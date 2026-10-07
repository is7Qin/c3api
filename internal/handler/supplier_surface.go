// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// 供应商面挂载骨架（spec 2026-10-09 §2.5/§6.1，T4）：第二 BaseURL
// /api/user/supplier 复用同一生成路由实现（字段层零差异化），前置 **default-
// deny 允许清单**（不暴露 /users、/settings、/pricing、/rules、/ops、/mail 等；
// 新增管理端点默认不对供应商暴露）+ **作用域注入**（供应商面恒 {OwnerUID:
// jwtUser, Set:true}）。
//
// 本文件提供**可测的挂载原语**；真实挂载与 service 层作用域 AND 进 WHERE 由
// T7 完成后接线（在作用域未落地前挂载会构成越权面，故此处不主动接入 server）。

import (
	"context"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
)

// SupplierSurfaceBaseURL 供应商面第二 BaseURL。
const SupplierSurfaceBaseURL = "/api/user/supplier"

// supplierSurfaceRoutes 供应商面允许的路由（相对 BaseURL；method + chi pattern）。
// 与 §2.5 路由子集一一对应：账号端点全量 + groups GET 只读候选 + templates GET 只读。
// default-deny ⇒ 未列出者不可达（组写面/assignments、模板写面、其余管理资源）。
var supplierSurfaceRoutes = []string{
	"GET /accounts",
	"POST /accounts",
	"POST /accounts/batch-update",
	"POST /accounts/batch-delete",
	"POST /accounts/batch-import-codex-oauth",
	"POST /accounts/batch-import-codex-pat",
	"GET /accounts/usage",
	"GET /accounts/{id}",
	"PATCH /accounts/{id}",
	"DELETE /accounts/{id}",
	"GET /accounts/{id}/ext",
	"PUT /accounts/{id}/ext",
	"GET /accounts/{id}/groups",
	"POST /accounts/{id}/recover",
	// groups GET 只读候选列表（账号页「选择分组」依赖）；组写面/assignments 不在子集内。
	"GET /groups",
	// templates GET 只读（提交账号需选模板）；模板写面不在子集内。
	"GET /templates",
	"GET /templates/{id}",
}

// SupplierSurfaceAllowlist 返回允许清单副本（顺序 = 声明序）。
func SupplierSurfaceAllowlist() []string {
	out := make([]string, len(supplierSurfaceRoutes))
	copy(out, supplierSurfaceRoutes)
	return out
}

// allowedRouter 由允许清单构造的 chi 路由（仅用于 Match 判定，无实际 handler）。
func allowedRouter() *chi.Mux {
	r := chi.NewRouter()
	for _, rt := range supplierSurfaceRoutes {
		method, pattern, ok := strings.Cut(rt, " ")
		if !ok {
			continue
		}
		r.Method(method, pattern, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	}
	return r
}

// SupplierSurfaceGuard 返回 default-deny 中间件：仅放行允许清单内的方法+路径，
// 其余 404（不泄漏存在性；新增管理端点默认不暴露）。full 为复用同一生成路由的
// 处理器（第二 BaseURL 实现）。请求路径前缀 SupplierSurfaceBaseURL 会被剥离后匹配。
func SupplierSurfaceGuard(full http.Handler) http.Handler {
	allowed := allowedRouter()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if after, ok := strings.CutPrefix(p, SupplierSurfaceBaseURL); ok {
			p = after
		}
		if p == "" {
			p = "/"
		}
		// 独立 RouteContext（seam 判定）：不污染请求真实路由上下文。
		if !allowed.Match(chi.NewRouteContext(), r.Method, p) {
			http.NotFound(w, r)
			return
		}
		full.ServeHTTP(w, r)
	})
}

// ctxKeyAccountScope 作用域注入键（单键单值，对齐 ctxKeyReqMeta 惯例）。
type ctxKeyAccountScope struct{}

// SupplierScopeInject 供应商面作用域注入：从已验证 JWT claims 取 user_id，注入
// {OwnerUID: jwtUser, Set:true}（§2.5 行层作用域）。必须位于 RequireJWT 之后。
// 无 claims（未鉴权）⇒ 不注入（下游按缺省管理面全量处理前应已由门控拒绝）。
func SupplierScopeInject(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if claims, ok := auth.ClaimsFrom(r.Context()); ok {
			ctx := context.WithValue(r.Context(), ctxKeyAccountScope{}, domain.SupplierAccountScope(claims.UserID))
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}

// AccountScopeFrom 读取注入的账号作用域（缺省 = 管理面全量 {Set:false}）。
// service 层每个账号读/写入口据此把作用域 AND 进 WHERE。
func AccountScopeFrom(ctx context.Context) domain.AccountScope {
	if s, ok := ctx.Value(ctxKeyAccountScope{}).(domain.AccountScope); ok {
		return s
	}
	return domain.PlatformAccountScope()
}
