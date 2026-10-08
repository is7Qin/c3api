// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/is7qin/c3api/internal/config"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
	"github.com/is7qin/c3api/internal/supplier"
	"github.com/is7qin/c3api/internal/worker"
)

// supplierViewReloader 把 *supplier.ViewLoader 转成非 nil 的
// service.SupplierViewReloader；nil loader 返回**真 nil 接口**（nil *ViewLoader
// 装箱成非 nil 接口会让 Service 的 nil 守卫失效 ⇒ 写面 Reload 打到 nil receiver
// panic——typed-nil 教训同 main 的 invBalances）。
func supplierViewReloader(l *supplier.ViewLoader) service.SupplierViewReloader {
	if l == nil {
		return nil
	}
	return l
}

// supplierCreditDrainBudget 记账链 Close 排空的独立总预算（不得复用
// drainCycleBudget 的 500ms——那是单消费周期预算，§5.5）。
const supplierCreditDrainBudget = 5 * time.Second

// supplierViewStaleThreshold 财务视图陈旧告警阈值（>0；stale_age 超此触发 Warn）。
const supplierViewStaleThreshold = 60 * time.Second

// supplierWorkersFor 组装供应商收益链 worker（spec 2026-10-09 §5.5）：
// 注册序 **thaw → credit**（thaw 先注册 ⇒ 反向排空时最后关；credit 后注册 ⇒ 早于
// thaw 关）。关闭态（enabled=false）返回 nil（G1：关闭态零注册——append 不贡献
// 元素）。freeze_enabled=false 时仅 credit（解冻 worker 不注册，存量释放由 credit
// 周期执行，§5.4）。抽成纯函数以便对 enabled×freeze_enabled 组合做切片断言。
func supplierWorkersFor(enabled, freezeEnabled bool, thaw, credit worker.Worker) []worker.Worker {
	if !enabled {
		return nil
	}
	var ws []worker.Worker
	if freezeEnabled && thaw != nil {
		ws = append(ws, thaw)
	}
	if credit != nil {
		ws = append(ws, credit)
	}
	return ws
}

// validateSupplierOverrides 启动期 override 集合校验（spec 2026-10-09 §5.5/A16⑦）：
// 遍历 supplier_balances，按全局 g + 其 override 重算桶上界，越界即拒绝启动并
// **列出违规 uid**（share_bp / freeze_hours 值域同批校验）。
func validateSupplierOverrides(ctx context.Context, repo *repository.SupplierRepo, g time.Duration) error {
	overrides, err := repo.ListSupplierOverrides(ctx)
	if err != nil {
		return fmt.Errorf("list overrides: %w", err)
	}
	var violations []string
	for _, o := range overrides {
		if o.FreezeHours != nil {
			if verr := config.ValidateSupplierFreezeHours(*o.FreezeHours, g); verr != nil {
				violations = append(violations, fmt.Sprintf("uid=%d freeze_hours=%d: %v", o.UID, *o.FreezeHours, verr))
			}
		}
		if o.ShareBp != nil {
			if verr := config.ValidateSupplierShareBp(*o.ShareBp); verr != nil {
				violations = append(violations, fmt.Sprintf("uid=%d share_bp=%d: %v", o.UID, *o.ShareBp, verr))
			}
		}
	}
	if len(violations) > 0 {
		sort.Strings(violations)
		return fmt.Errorf("supplier override set incompatible with thaw_granularity %s: %v", g, violations)
	}
	return nil
}

// validateSupplierDisabled 停用 liability 校验（spec 2026-10-09 §5.5 / I6 / A23）：
// enabled=false 且存在残留冻结桶 / 未记账 usage backlog / available>0 / 在途单
// （pending|approved|paying）⇒ 拒绝启动并打印分项规模。理由：enabled=false 时
// credit/thaw 链根本不存在 ⇒ 残留为「债权不可达」；必须让操作员看见。
func validateSupplierDisabled(ctx context.Context, repo *repository.SupplierRepo) error {
	l, err := repo.SupplierLiability(ctx)
	if err != nil {
		return fmt.Errorf("supplier liability probe: %w", err)
	}
	if l.Zero() {
		return nil
	}
	return fmt.Errorf("supplier disabled but outstanding liability remains: frozen_chunks_rows=%d uncredited_usage_rows=%d available=%d inflight_settlements=%d",
		l.ChunksRows, l.UncreditedRows, l.Available, l.InFlightSettlements)
}
