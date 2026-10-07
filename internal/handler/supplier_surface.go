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
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/handler/supplier"
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
	// 供应商业务面（§6.1/§6.2）：概览/收益明细/冻结桶/结算单 + 申请结算。
	"GET /overview",
	"GET /earnings",
	"GET /chunks",
	"GET /settlements",
	"POST /settlements",
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

// 作用域注入键/读值下沉 domain（叶子包）——service/repository 与 handler 共用同一
// 访问器（domain.WithAccountScope/domain.AccountScopeFrom），避免 handler→service
// 反向依赖（§2.5）。见 internal/domain/account_scope.go。

// SupplierScopeInject 供应商面作用域注入：从已验证 JWT claims 取 user_id，注入
// {OwnerUID: jwtUser, Set:true}（§2.5 行层作用域）。必须位于 RequireJWT 之后。
// 无 claims（未鉴权）⇒ 不注入（下游按缺省管理面全量处理前应已由门控拒绝）。
func SupplierScopeInject(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if claims, ok := auth.ClaimsFrom(r.Context()); ok {
			ctx := domain.WithAccountScope(r.Context(), domain.SupplierAccountScope(claims.UserID))
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}

// AccountScopeFrom 读取注入的账号作用域（缺省 = 管理面全量 {Set:false}）。
// service/repository 层经 domain.AccountScopeFrom 读取同一键（此处为 handler 面
// 便捷别名，保持既有引用）。
func AccountScopeFrom(ctx context.Context) domain.AccountScope {
	return domain.AccountScopeFrom(ctx)
}

// SupplierSurfaceRouter 供应商面复用路由（第二 BaseURL /api/user/supplier）：
// 以**同一生成 ServerInterfaceWrapper** 注册允许清单内的账号/分组/模板端点——
// 字段层**零差异化**（同一 AccountConfigPatch/validateAccountPatch/accountFieldSpecs），
// 仅作用域由 ctx 注入（repository 每处 WHERE AND 归属谓词）。
//
// 仅注册 allowlist（default-deny）：未列出的管理端点（/users、/settings、
// /pricing、/rules、/ops、/mail、组写面/assignments、模板写面）**根本不注册**
// ⇒ 404（安全默认方向：新增管理端点默认不对供应商暴露）。
func (h *AdminAPI) SupplierSurfaceRouter() http.Handler {
	siw := &ServerInterfaceWrapper{
		Handler: h,
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			httpface.WriteErr(w, http.StatusBadRequest, err.Error())
		},
	}
	r := chi.NewRouter()
	// 账号端点全量（复用）。
	r.Get("/api/user/supplier/accounts", siw.GetAccounts)
	r.Post("/api/user/supplier/accounts", siw.PostAccounts)
	r.Post("/api/user/supplier/accounts/batch-update", siw.PostAccountsBatchUpdate)
	r.Post("/api/user/supplier/accounts/batch-delete", siw.PostAccountsBatchDelete)
	r.Post("/api/user/supplier/accounts/batch-import-codex-oauth", siw.PostAccountsBatchImportCodexOauth)
	r.Post("/api/user/supplier/accounts/batch-import-codex-pat", siw.PostAccountsBatchImportCodexPat)
	r.Get("/api/user/supplier/accounts/usage", siw.GetAccountsUsage)
	r.Get("/api/user/supplier/accounts/{id}", siw.GetAccountsId)
	r.Patch("/api/user/supplier/accounts/{id}", siw.PatchAccountsId)
	r.Delete("/api/user/supplier/accounts/{id}", siw.DeleteAccountsId)
	r.Get("/api/user/supplier/accounts/{id}/ext", siw.GetAccountsIdExt)
	r.Put("/api/user/supplier/accounts/{id}/ext", siw.PutAccountsIdExt)
	r.Get("/api/user/supplier/accounts/{id}/groups", siw.GetAccountsIdGroups)
	r.Post("/api/user/supplier/accounts/{id}/recover", siw.PostAccountsIdRecover)
	// groups GET 只读候选列表（账号页「选择分组」依赖）；组写面/assignments 不注册。
	r.Get("/api/user/supplier/groups", siw.GetGroups)
	// templates GET 只读（提交账号需选模板）；模板写面不注册。
	r.Get("/api/user/supplier/templates", siw.GetTemplates)
	r.Get("/api/user/supplier/templates/{id}", siw.GetTemplatesId)
	// 供应商业务面（§6.1/§6.2；生成面 supplier.ServerInterface，实现见
	// supplier_business.go）。
	bsiw := &supplier.ServerInterfaceWrapper{
		Handler: h,
		ErrorHandlerFunc: func(w http.ResponseWriter, r *http.Request, err error) {
			httpface.WriteErr(w, http.StatusBadRequest, err.Error())
		},
	}
	r.Get("/api/user/supplier/overview", bsiw.GetSupplierOverview)
	r.Get("/api/user/supplier/earnings", bsiw.GetSupplierEarnings)
	r.Get("/api/user/supplier/chunks", bsiw.GetSupplierChunks)
	r.Get("/api/user/supplier/settlements", bsiw.GetSupplierSettlements)
	r.Post("/api/user/supplier/settlements", bsiw.PostSupplierSettlement)
	return r
}

// NewSupplierSurface 组装供应商面完整链路（挂载于 /api/user/supplier/*）：
//
//	RequireJWT(iss,users) → RequireRole(users, SupplierSurfaceRoles...) →
//	SupplierScopeInject → SupplierSurfaceGuard(default-deny) → 复用路由
//
// 门控可达集唯一事实源 = domain.SupplierSurfaceRoles()（§2.6）。
func NewSupplierSurface(api *AdminAPI, iss *auth.Issuer, users auth.UserStatusProvider) http.Handler {
	var h http.Handler = SupplierSurfaceGuard(api.SupplierSurfaceRouter())
	h = SupplierScopeInject(h)
	h = auth.RequireRole(users, domain.SupplierSurfaceRoles()...)(h)
	h = auth.RequireJWT(iss, users)(h)
	return h
}
