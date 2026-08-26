// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
)

// UserStatusProvider 用户快照（status+role 单次查找；proxy.Auth 实现；用户
// 变更走 invalidate → Auth.Reload 全量刷新，RequireJWT/adminAuth 不用 DB
// 直查——评审定夺②）。
type UserStatusProvider interface {
	UserSnapshot(userID int64) (domain.UserSnapshot, bool)
}

type ctxClaimsKey struct{}

// ClaimsFrom 从请求 context 取已验证 claims（RequireJWT 之后可用）。
func ClaimsFrom(ctx context.Context) (*Claims, bool) {
	c, ok := ctx.Value(ctxClaimsKey{}).(*Claims)
	return c, ok
}

// RequireJWT /user 组中间件：验证 Bearer JWT + 内存快照用户状态校验。
// 用户禁用 → 立即拒绝（快照刷新走 invalidate）；JWT 24h 长时效仅作快照
// 失效后的最终兜底（评审定夺②，用户决策 2026-08-11）。
func RequireJWT(iss *Issuer, users UserStatusProvider) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || raw == "" {
				writeUnauthorized(w)
				return
			}
			claims, err := iss.Verify(raw)
			if err != nil {
				writeUnauthorized(w)
				return
			}
			// 快照用户状态校验：禁用即拒（无需等 JWT 过期）；快照缺失
			// （启动首刷失败 / Reload 失败保留旧快照 / NOTIFY 丢失）同样拒绝
			// ——fail-closed，与 /admin 面（internal/server/middleware.go adminAuth）
			// 及 Balances.BalanceOf 缺失 → 402 纪律一致。行为变化注记：启动
			// 首刷失败（DB 挂）时 /user 全拒——/user 端点 DB-backed 反正 500，
			// 实际影响≈0。
			// token_version 快照比对（spec 2026-08-25-jwt-password-revocation）：
			// claims.Ver ≠ 快照版本 → 401（改密/重置密码后该用户全部既有 JWT
			// 立即失效；与 status 同款 fail-closed 语义——快照缺失本就拒绝）。
			sn, ok := users.UserSnapshot(claims.UserID)
			if !ok || sn.Status != domain.UserStatusActive || sn.TokenVersion != claims.Ver {
				writeUnauthorized(w)
				return
			}
			ctx := context.WithValue(r.Context(), ctxClaimsKey{}, claims)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireRole 端点级 RBAC 声明：claims.Role 必须 ∈ 允许角色。当前两级角色下
// 用户面端点不区分角色（platform_admin 也可用 /user 面），本中间件供后续
// 细分权限使用（/admin 组鉴权 = 静态 token OR platform_admin JWT）。
func RequireRole(roles ...domain.Role) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			claims, ok := ClaimsFrom(r.Context())
			if !ok {
				httpface.WriteErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			for _, role := range roles {
				if claims.Role == string(role) {
					next.ServeHTTP(w, r)
					return
				}
			}
			httpface.WriteErr(w, http.StatusForbidden, "forbidden")
		})
	}
}

func writeUnauthorized(w http.ResponseWriter) {
	httpface.WriteErr(w, http.StatusUnauthorized, "unauthorized")
}
