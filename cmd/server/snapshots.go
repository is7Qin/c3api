// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/handler"
	"github.com/is7qin/c3api/internal/proxy"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/service"
	"github.com/is7qin/c3api/internal/snapshot"
)

// 五路快照 → snapshot.Snapshot 适配器（装配侧，与 schedGroupPub 同模式——
// 模块包不 import snapshot，粘合放最外层）：包装各模块既有 Reload（零重写），
// 声明变更 scope。当前 scope 接线：仅 settings 变更经注册表分发（ScopeSettings
// → auth：gate 预算 N 即时重算，#36 缺口）；其余变更类型仍走 invalidate 去抖
// 器（合并语义），注册表不重复接管（避免同快照双 reload）。其余四路无 scope
// 声明 = 纯启动就绪 + 状态追踪快照。

// authSnapshot 鉴权快照（key/user 状态 + gate 预算）。
type authSnapshot struct{ a *proxy.Auth }

func (s authSnapshot) Name() string                     { return "auth" }
func (s authSnapshot) Scopes() []string                 { return []string{snapshot.ScopeSettings} }
func (s authSnapshot) Reload(ctx context.Context) error { return s.a.Reload(ctx) }

// schedSnapshot 调度器快照（组/账号/模板/Selection 路由）。
type schedSnapshot struct{ s *scheduler.Scheduler }

func (s schedSnapshot) Name() string     { return "scheduler" }
func (s schedSnapshot) Scopes() []string { return nil }
func (s schedSnapshot) Reload(ctx context.Context) error {
	return s.s.InvalidateAllSyncCtx(ctx)
}

// ruleSnapshot 规则引擎快照（启用规则表 + 窗口计数重建）。
type ruleSnapshot struct{ e *rule.RuleEngine }

func (s ruleSnapshot) Name() string                     { return "rules" }
func (s ruleSnapshot) Scopes() []string                 { return nil }
func (s ruleSnapshot) Reload(ctx context.Context) error { return s.e.Reload(ctx) }

// balanceSnapshot 余额 + 倍率快照（billing.enabled 才注册）。
type balanceSnapshot struct{ b *billing.Balances }

func (s balanceSnapshot) Name() string                     { return "balances" }
func (s balanceSnapshot) Scopes() []string                 { return nil }
func (s balanceSnapshot) Reload(ctx context.Context) error { return s.b.Reload(ctx) }

// pricingSnapshot 价格表快照（service pricing + image_price + function_price
// 缓存；与 price_sync_cron / 管理端改价同一刷新目标——Task A 双线 + 价格表
// 三件套：三快照同路径首刷，重启后计费读零 DB 即时可用）。
type pricingSnapshot struct{ svc *service.Service }

func (s pricingSnapshot) Name() string     { return "pricing" }
func (s pricingSnapshot) Scopes() []string { return nil }
func (s pricingSnapshot) Reload(ctx context.Context) error {
	if err := s.svc.ReloadPricingCtx(ctx); err != nil {
		return err
	}
	if err := s.svc.ReloadImagePricingCtx(ctx); err != nil {
		return err
	}
	return s.svc.ReloadFunctionPricingCtx(ctx)
}

// snapshotStates 注册表状态 → /api/admin/ops/workers 响应映射（LastError error
// 接口 JSON 不可用 → 字符串；snapshot.Status 值拷贝，调用方安全持有）。
func snapshotStates(st []snapshot.Status) []handler.SnapshotState {
	out := make([]handler.SnapshotState, 0, len(st))
	for _, s := range st {
		ss := handler.SnapshotState{Name: s.Name, LastReload: s.LastReload}
		// 契约类型（handler/api.gen.go 生成）Scopes 为 *[]string：nil scope
		// 保持省略（纯启动/状态快照），非 nil 取副本地址（s 为 range 值拷贝，
		// 每次迭代独立变量，取址安全）。
		if s.Scopes != nil {
			ss.Scopes = &s.Scopes
		}
		if s.LastError != nil {
			e := s.LastError.Error()
			ss.LastError = &e
		}
		out = append(out, ss)
	}
	return out
}
