// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// usage_logs / err_logs 查询面（消费面改名裁决：log → usage 语义；错误审计
// err_logs 独立查询面——/usage_logs 与 /err_logs API）。
//
// 两条读路径都是 keyset 分页、成本与跨度无关（矩阵 CostCap = 0 = 无上限），但
// **coverage 与 trend 面同一纪律**（spec §4.5 R7b 裁决）：from 早于保证存留的
// 分区 cutoff 时被 DROP 的行会不可见地消失——「不静默残缺」必须覆盖全部读路径，
// 而不是只覆盖 zone-grouped 的两条。判定的唯一点是 domain.Admit。

import (
	"context"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// admitLogsWindow 明细分页面的窗口判定（KindUsageList / KindErrlogList）：from/to
// 契约必填（openapi required ⇒ 生成物为 time.Time，四个 handler 恒传 &params.From/&params.To）。
// nil 只可能来自内部/测试调用，按"未给窗口"处理 ⇒ 送零值进 Admit 的 step 1（零值 → window_invalid），
// **不在此处另立宽容分支**——判定的唯一点是 domain.Admit，任何"跳过判定"的分支都是本 spec 要消灭的静默路径。
func (s *Service) admitLogsWindow(kind domain.StatsKindID, from, to *time.Time) error {
	var f, t time.Time
	if from != nil {
		f = *from
	}
	if to != nil {
		t = *to
	}
	// zone = UTC：这两条 kind 无分组（GroupingNone），时区不参与判定与执行。
	_, err := s.admitStats(kind, time.UTC, f, t)
	return err
}

// QueryUsages usage_logs 计费明细分页查询（/usage_logs API；错误行含
// abort/failover 半异常标记——error_type 过滤保留）。keyset 游标分页：
// 返回行可能含 limit+1 探测行，next_cursor 组装在 handler。
// 返回 []*domain.UsageLog 直透（不再擦除为 []any——spec 2026-08-17 边界收敛，
// 类型信息保留，handler 断言删除）。
func (s *Service) QueryUsages(ctx context.Context, q repository.UsageQuery) ([]*domain.UsageLog, error) {
	if err := s.admitLogsWindow(domain.KindUsageList, q.From, q.To); err != nil {
		return nil, err
	}
	return s.store.QueryUsages(ctx, q)
}

// QueryErrLogs err_logs 错误明细分页查询（/err_logs API：完整错误面——拒绝 +
// 异常双轨，status_code/error_type 全值；行类型同为 *domain.UsageLog——
// err_logs 表复用该领域类型）。keyset 游标分页同 QueryUsages；覆盖率依据
// Retention.ErrLog（只读 err_logs 一张表——与 usage_logs 不是同一个 basis）。
func (s *Service) QueryErrLogs(ctx context.Context, q repository.ErrLogQuery) ([]*domain.UsageLog, error) {
	if err := s.admitLogsWindow(domain.KindErrlogList, q.From, q.To); err != nil {
		return nil, err
	}
	return s.store.QueryErrLogs(ctx, q)
}
