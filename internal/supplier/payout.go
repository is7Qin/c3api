// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package supplier

// payout.go 付款风控门（spec 2026-10-09 §6.5 C1）：纯谓词，便于表驱动断言。
// 权威信号由 repository 在 claim 事务内直查 PG（新专用探针）后填入 BillingProbe；
// 本文件只做「放行谓词」判定，缺一项即失败闭合。

import (
	"errors"
	"fmt"
	"time"
)

// PayoutGateConfig 付款风控门阈值（§6.4/§6.5 四键 + 计费开关）。
type PayoutGateConfig struct {
	// BillingEnabled 计费链开关：false ⇒ 消费不落 usage、探针无意义 ⇒ 拒绝放行。
	BillingEnabled bool
	// MaxBacklogRows billed 健康探针 backlog 上限（行）。
	MaxBacklogRows int
	// MaxBacklogAge 队头最长滞留。
	MaxBacklogAge time.Duration
	// MaxObserveAge 健康观测最大陈旧。
	MaxObserveAge time.Duration
}

// BillingProbe 扣费链健康探针结果（单语句快照）。
type BillingProbe struct {
	// BacklogRows 未扣费且计价为正的行数。
	BacklogRows int64
	// OldestCreatedAt 队头 created_at（backlogRows=0 时为 nil）。
	OldestCreatedAt *time.Time
	// ObservedAt 查询快照采样时刻（DB 时钟）。
	ObservedAt time.Time
}

// ErrPayoutGate 付款风控门拒绝（失败闭合，不置 paying）。
var ErrPayoutGate = errors.New("supplier: payout risk gate rejected")

// EvaluatePayoutGate 放行谓词（§6.5，单语句快照；dbNow = claim 事务 DB 时间）：
//
//	billing_enabled
//	AND backlog_rows <= max_backlog_rows
//	AND (backlog_rows = 0 OR db_now - oldest_created_at <= max_backlog_age)
//	AND 0 <= db_now - observed_at <= max_observe_age
//
// 任一不满足（含探针缺结果 / 负或未来时间 / 过期）⇒ 拒绝。
func EvaluatePayoutGate(cfg PayoutGateConfig, p BillingProbe, dbNow time.Time) error {
	if !cfg.BillingEnabled {
		return fmt.Errorf("%w: billing disabled", ErrPayoutGate)
	}
	if p.BacklogRows < 0 {
		return fmt.Errorf("%w: negative backlog rows", ErrPayoutGate)
	}
	if p.BacklogRows > int64(cfg.MaxBacklogRows) {
		return fmt.Errorf("%w: backlog_rows %d > %d", ErrPayoutGate, p.BacklogRows, cfg.MaxBacklogRows)
	}
	if p.BacklogRows > 0 {
		if p.OldestCreatedAt == nil {
			return fmt.Errorf("%w: backlog_rows %d but oldest_created_at missing", ErrPayoutGate, p.BacklogRows)
		}
		if age := dbNow.Sub(*p.OldestCreatedAt); age < 0 || age > cfg.MaxBacklogAge {
			return fmt.Errorf("%w: backlog head age %s out of [0, %s]", ErrPayoutGate, age, cfg.MaxBacklogAge)
		}
	}
	observeAge := dbNow.Sub(p.ObservedAt)
	if observeAge < 0 || observeAge > cfg.MaxObserveAge {
		return fmt.Errorf("%w: observe age %s out of [0, %s]", ErrPayoutGate, observeAge, cfg.MaxObserveAge)
	}
	return nil
}
