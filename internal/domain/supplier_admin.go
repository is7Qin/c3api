// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"context"
	"time"
)

// 资金命令的具名操作者（spec 2026-10-09 §6.2/§6.5 I5）。资金写命令（supplier_request /
// approve / reject / claim / paid / confirm-failed / admin_request）一律要求具名 JWT
// 操作者；**静态 admin token 无 uid/token_version ⇒ 403**（不能担保可锁 users 行、
// 也无法复核 token_version）。具名操作者经 context 从管理面鉴权中间件注入，并在写
// 事务内锁 users 行后复核 status/role/token_version。
type FundsActor struct {
	// UserID 具名操作者（JWT claims.user_id）。
	UserID int64
	// TokenVersion 签名请求的 token_version（JWT claims.ver；复核 DB 当前值是否相等，
	// 撤权/改密后旧票在写事务内被拒）。
	TokenVersion int64
}

// fundsActorCtxKey 资金操作者上下文键（单键单值）。
type fundsActorCtxKey struct{}

// WithFundsActor 注入资金操作者（管理面 JWT 鉴权路径）。
func WithFundsActor(ctx context.Context, a FundsActor) context.Context {
	return context.WithValue(ctx, fundsActorCtxKey{}, a)
}

// FundsActorFrom 读取资金操作者；缺省（静态 admin token / 无鉴权）⇒ ok=false。
func FundsActorFrom(ctx context.Context) (FundsActor, bool) {
	a, ok := ctx.Value(fundsActorCtxKey{}).(FundsActor)
	return a, ok
}

// SupplierBalancePatch 管理面 PATCH 逐供应商配置（§6.3：**仅** share_bp /
// freeze_hours，不接受金额）。指针非 nil = 落值；Clear* = 落 NULL（回继承）。
type SupplierBalancePatch struct {
	ShareBp          *int
	ClearShareBp     bool
	FreezeHours      *int
	ClearFreezeHours bool
}

// SupplierSettlementFilter 管理面结算单列表过滤（§6.3；status/kind 可空）。
type SupplierSettlementFilter struct {
	Status *SupplierSettlementStatus
	Kind   *SupplierSettlementKind
}

// SupplierBalance 管理面余额列表行（§6.3/§2.4 运维可见性）。
type SupplierBalance struct {
	SupplierUserID    int64
	Available         int64
	LifetimeCredited  int64
	LifetimePaid      int64
	ShareBp           *int
	FreezeHours       *int
	LatestAvailableAt *time.Time
	BucketRows        int64
}
