// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package auth

import "context"

// ctxAdminUserIDKey 管理面鉴权中间件写入的 platform_admin 用户 id
// （兑换码 created_by 用，决策 5：0 = 系统）。管理 key（mk-）路径同样写入 owner id。
//
// 键是**本包私有**：写入只经 WithUserID、读取只经 UserIDFromContext，外部无法
// 自造 key 伪造身份。它与 ctxClaimsKey 同款（私有 key + 取值访问器），区别是
// 这里存的是一个已鉴权通过的 uid 快照，而不是完整 claims。
type ctxAdminUserIDKey struct{}

// WithUserID 把**已鉴权**的用户 id 注入 context。只有鉴权中间件该调用它——
// 它不做任何校验，写进去的值会被 UserIDFromContext 原样读出。
func WithUserID(ctx context.Context, userID int64) context.Context {
	return context.WithValue(ctx, ctxAdminUserIDKey{}, userID)
}

// UserIDFromContext 取管理面鉴权注入的用户 id（JWT 或管理 key mk- 皆注入；
// 兑换码生成 created_by 用）；未注入 → (0, false)。
func UserIDFromContext(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(ctxAdminUserIDKey{}).(int64)
	return id, ok
}
