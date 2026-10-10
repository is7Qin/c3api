// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package auth

import (
	"context"
	"net/http"
	"strings"

	"github.com/is7qin/c3api/internal/domain"
)

// ManagementKeyProvider 管理 key 鉴权快照面（proxy.Auth 实现，spec 2026-10-09）：
// 仅认 Authorization: Bearer mk-…；前缀先判、失败不回退 JWT（由调用方纪律保证）。
type ManagementKeyProvider interface {
	AuthenticateManagement(r *http.Request) (domain.ManagementKeyMeta, bool)
}

// SnapshotProvider 单一鉴权快照面（spec 2026-10-09 §4.3）：合并用户状态与
// 管理 key 两个 provider——同一 auth 只传一次。proxy.Auth 同时实现两者。
type SnapshotProvider interface {
	UserStatusProvider
	ManagementKeyProvider
}

// RequireIdentity /user 组（含供应商面）统一身份鉴权中间件（spec 2026-10-09 §4.3）：
// Bearer 凭证既可为 JWT（eyJ…）亦可为管理 key（mk-…），二者均以 owner/user 身份
// 放行。单一 SnapshotProvider（users + mgmt 合并）——同一 auth 只传一次。前缀先判：
//   - mk- 开头 → 管理 key 快照查表（失败/禁用 → 401，**不回落 JWT**），owner 快照
//     缺失/非 active → 401（fail-closed）；成功注入 Claims{UserID, Ver:sn.TokenVersion,
//     Role:sn.Role} + FundsActor{UserID, TokenVersion:sn.TokenVersion}。
//   - 非 mk- → 既有 JWT 校验（Verify + 快照 status/TokenVersion），同样注入 Claims +
//     FundsActor。
//
// 两条分支都注入 ctxClaimsKey（下游 RequireRole / SupplierScopeInject / handler 读
// auth.ClaimsFrom）与 FundsActor（资金命令 actor = owner，token_version = owner 快照
// 当前值——与 JWT 路径逐位一致，资金写事务复核通过）。不改动导出的 RequireJWT
// （他处/测试仍用）。
func RequireIdentity(iss *Issuer, p SnapshotProvider) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// fail-closed：必需 provider 缺失（未装配）⇒ 401，绝不 panic。合并后的
			// SnapshotProvider 同时承载用户快照（两条分支都要查）与管理 key 查表
			// （mk- 分支）。nil 接口/指针解引用会 panic（评审 MAJOR），故调用前显式判空。
			if p == nil {
				writeUnauthorized(w)
				return
			}
			raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || raw == "" {
				writeUnauthorized(w)
				return
			}
			// 管理 key 分支：仅 mk- 前缀进入；失败/禁用直接 401（不得回落 JWT——
			// 否则 §5.2 前缀判别不成立、A3 推理面隔离失效）。
			if strings.HasPrefix(raw, "mk-") {
				meta, ok := p.AuthenticateManagement(r)
				if !ok {
					writeUnauthorized(w)
					return
				}
				// owner 快照校验（fail-closed）：缺失/非 active → 401；owner 降权
				// 由快照 role 实时反映（key 仍可作用于其降权后面）。
				sn, ok := p.UserSnapshot(meta.UserID)
				if !ok || sn.Status != domain.UserStatusActive {
					writeUnauthorized(w)
					return
				}
				claims := &Claims{UserID: meta.UserID, Role: string(sn.Role), Ver: sn.TokenVersion}
				ctx := context.WithValue(r.Context(), ctxClaimsKey{}, claims)
				ctx = domain.WithFundsActor(ctx, domain.FundsActor{UserID: meta.UserID, TokenVersion: sn.TokenVersion})
				next.ServeHTTP(w, r.WithContext(ctx))
				return
			}
			// JWT 分支（与 RequireJWT 同校验口径），追加 FundsActor 注入。iss 缺失
			// ⇒ 401（fail-closed，不解引用 nil *Issuer）。
			if iss == nil {
				writeUnauthorized(w)
				return
			}
			claims, err := iss.Verify(raw)
			if err != nil {
				writeUnauthorized(w)
				return
			}
			sn, ok := p.UserSnapshot(claims.UserID)
			if !ok || sn.Status != domain.UserStatusActive || sn.TokenVersion != claims.Ver {
				writeUnauthorized(w)
				return
			}
			ctx := context.WithValue(r.Context(), ctxClaimsKey{}, claims)
			ctx = domain.WithFundsActor(ctx, domain.FundsActor{UserID: claims.UserID, TokenVersion: sn.TokenVersion})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
