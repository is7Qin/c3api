// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// 供应商面挂载（spec 2026-10-09 §2.5/§2.6/§6.1）：第二 BaseURL
// /api/user/supplier 的路由**由 openapi 契约生成**——tag `supplier` 的显式 path
// （业务 5 op + 账号/分组/模板子集 17 op，共 22 op，生成到 internal/handler/supplier）。
//
//   - **tag 即边界（结构性 default-deny）**：未登记为 supplier 的 path 不进生成面
//     ⇒ 未注册 ⇒ 404（/users、/settings、/pricing、/rules、/ops、/mail、组写面/
//     assignments、模板写面…）。**没有**手写允许清单/guard 需要维护——新增管理端点
//     默认不对供应商暴露。
//   - **字段层零差异化**：账号子集的 requestBody/响应/query 参数在 openapi 里
//     $ref 复用管理面 components；运行时由生成的 wrapper 解析参数后**转发**到既有
//     管理面 ServerInterfaceWrapper（同一批 AdminAPI handler、同一
//     AccountConfigPatch/validateAccountPatch/accountFieldSpecs）。
//   - **作用域注入**：供应商面恒 {OwnerUID: jwtUser, Set:true}（仓储层每处 WHERE
//     AND 归属谓词，越域 ⇒ 404）。

import (
	"context"
	"net/http"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/handler/supplier"
)

// SupplierSurfaceBaseURL 供应商面第二 BaseURL（openapi 里该 tag 的 path 前缀）。
const SupplierSurfaceBaseURL = "/api/user/supplier"

// supplierSurface 供应商面接口适配器：实现生成的 supplier.ServerInterface（22 op）。
// 业务 op 直调 AdminAPI 自身方法（supplier_business.go）；账号/分组/模板 op 转发到
// 管理面生成的 ServerInterfaceWrapper——**参数无需手工映射**：两面 path/query/header
// 形状由同一批 components 定义，wrapper 自 `r` 解析（chi.URLParam 对 {id} 亦同名）。
type supplierSurface struct {
	api   *AdminAPI
	admin *ServerInterfaceWrapper
}

// ---- 账号/分组/模板子集（§2.5）：转发管理面生成 wrapper。----

// GetSupplierAccounts GET /api/user/supplier/accounts。
func (s supplierSurface) GetSupplierAccounts(w http.ResponseWriter, r *http.Request, _ supplier.GetSupplierAccountsParams) {
	s.admin.GetAccounts(w, r)
}

// PostSupplierAccounts POST /api/user/supplier/accounts。
func (s supplierSurface) PostSupplierAccounts(w http.ResponseWriter, r *http.Request) {
	s.admin.PostAccounts(w, r)
}

// GetSupplierAccountsUsage GET /api/user/supplier/accounts/usage。
func (s supplierSurface) GetSupplierAccountsUsage(w http.ResponseWriter, r *http.Request, _ supplier.GetSupplierAccountsUsageParams) {
	s.admin.GetAccountsUsage(w, r)
}

// PostSupplierAccountsBatchUpdate POST /api/user/supplier/accounts/batch-update。
func (s supplierSurface) PostSupplierAccountsBatchUpdate(w http.ResponseWriter, r *http.Request) {
	s.admin.PostAccountsBatchUpdate(w, r)
}

// PostSupplierAccountsBatchDelete POST /api/user/supplier/accounts/batch-delete。
func (s supplierSurface) PostSupplierAccountsBatchDelete(w http.ResponseWriter, r *http.Request) {
	s.admin.PostAccountsBatchDelete(w, r)
}

// PostSupplierAccountsBatchImportCodexOauth POST /api/user/supplier/accounts/batch-import-codex-oauth。
func (s supplierSurface) PostSupplierAccountsBatchImportCodexOauth(w http.ResponseWriter, r *http.Request) {
	s.admin.PostAccountsBatchImportCodexOauth(w, r)
}

// PostSupplierAccountsBatchImportCodexPat POST /api/user/supplier/accounts/batch-import-codex-pat。
func (s supplierSurface) PostSupplierAccountsBatchImportCodexPat(w http.ResponseWriter, r *http.Request) {
	s.admin.PostAccountsBatchImportCodexPat(w, r)
}

// GetSupplierAccountsId GET /api/user/supplier/accounts/{id}。
func (s supplierSurface) GetSupplierAccountsId(w http.ResponseWriter, r *http.Request, _ int64) {
	s.admin.GetAccountsId(w, r)
}

// PatchSupplierAccountsId PATCH /api/user/supplier/accounts/{id}（可选 If-Match 由
// 管理面 wrapper 自请求头解析——两面契约同构）。
func (s supplierSurface) PatchSupplierAccountsId(w http.ResponseWriter, r *http.Request, _ int64, _ supplier.PatchSupplierAccountsIdParams) {
	s.admin.PatchAccountsId(w, r)
}

// DeleteSupplierAccountsId DELETE /api/user/supplier/accounts/{id}。
func (s supplierSurface) DeleteSupplierAccountsId(w http.ResponseWriter, r *http.Request, _ int64) {
	s.admin.DeleteAccountsId(w, r)
}

// GetSupplierAccountsIdExt GET /api/user/supplier/accounts/{id}/ext。
func (s supplierSurface) GetSupplierAccountsIdExt(w http.ResponseWriter, r *http.Request, _ int64) {
	s.admin.GetAccountsIdExt(w, r)
}

// PutSupplierAccountsIdExt PUT /api/user/supplier/accounts/{id}/ext。
func (s supplierSurface) PutSupplierAccountsIdExt(w http.ResponseWriter, r *http.Request, _ int64) {
	s.admin.PutAccountsIdExt(w, r)
}

// GetSupplierAccountsIdGroups GET /api/user/supplier/accounts/{id}/groups。
func (s supplierSurface) GetSupplierAccountsIdGroups(w http.ResponseWriter, r *http.Request, _ int64) {
	s.admin.GetAccountsIdGroups(w, r)
}

// PostSupplierAccountsIdRecover POST /api/user/supplier/accounts/{id}/recover。
func (s supplierSurface) PostSupplierAccountsIdRecover(w http.ResponseWriter, r *http.Request, _ int64) {
	s.admin.PostAccountsIdRecover(w, r)
}

// GetSupplierGroups GET /api/user/supplier/groups（只读候选列表；组写面不登记）。
func (s supplierSurface) GetSupplierGroups(w http.ResponseWriter, r *http.Request, _ supplier.GetSupplierGroupsParams) {
	s.admin.GetGroups(w, r)
}

// GetSupplierTemplates GET /api/user/supplier/templates（只读；模板写面不登记）。
func (s supplierSurface) GetSupplierTemplates(w http.ResponseWriter, r *http.Request, _ supplier.GetSupplierTemplatesParams) {
	s.admin.GetTemplates(w, r)
}

// GetSupplierTemplatesId GET /api/user/supplier/templates/{id}（只读）。
func (s supplierSurface) GetSupplierTemplatesId(w http.ResponseWriter, r *http.Request, _ int64) {
	s.admin.GetTemplatesId(w, r)
}

// ---- 供应商业务面（§6.1/§6.2）：直调既有实现（supplier_business.go）。----

// GetSupplierOverview GET /api/user/supplier/overview。
func (s supplierSurface) GetSupplierOverview(w http.ResponseWriter, r *http.Request) {
	s.api.GetSupplierOverview(w, r)
}

// GetSupplierEarnings GET /api/user/supplier/earnings（生成面 params 与业务实现
// 同名同构，直透）。
func (s supplierSurface) GetSupplierEarnings(w http.ResponseWriter, r *http.Request, params supplier.GetSupplierEarningsParams) {
	s.api.GetSupplierEarnings(w, r, params)
}

// GetSupplierChunks GET /api/user/supplier/chunks。
func (s supplierSurface) GetSupplierChunks(w http.ResponseWriter, r *http.Request, params supplier.GetSupplierChunksParams) {
	s.api.GetSupplierChunks(w, r, params)
}

// GetSupplierSettlements GET /api/user/supplier/settlements。
func (s supplierSurface) GetSupplierSettlements(w http.ResponseWriter, r *http.Request, params supplier.GetSupplierSettlementsParams) {
	s.api.GetSupplierSettlements(w, r, params)
}

// PostSupplierSettlement POST /api/user/supplier/settlements（申请结算）。
func (s supplierSurface) PostSupplierSettlement(w http.ResponseWriter, r *http.Request) {
	s.api.PostSupplierSettlement(w, r)
}

// SupplierSurfaceHandler 供应商面生成路由（tag `supplier` 的 22 op，绝对路径、
// 无 BaseURL；故 HandlerWithOptions 直接按 spec 路径注册）。业务 op 走本包 handler，
// 账号/分组/模板 op 走得同一批生成 wrapper——**暴露面唯一事实源 = openapi**。
func (h *AdminAPI) SupplierSurfaceHandler() http.Handler {
	badRequest := func(w http.ResponseWriter, r *http.Request, err error) {
		httpface.WriteErr(w, http.StatusBadRequest, err.Error())
	}
	return supplier.HandlerWithOptions(supplierSurface{
		api:   h,
		admin: &ServerInterfaceWrapper{Handler: h, ErrorHandlerFunc: badRequest},
	}, supplier.ChiServerOptions{ErrorHandlerFunc: badRequest})
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

// NewSupplierSurface 组装供应商面完整链路（挂载于 /api/user/supplier/*）：
//
//	RequireJWT(iss,users) → RequireRole(users, SupplierSurfaceRoles...) →
//	SupplierScopeInject → 生成路由（tag `supplier` 的 22 op）
//
// 可达集唯一事实源 = domain.SupplierSurfaceRoles()（§2.6）；暴露面唯一事实源 =
// openapi 里 tag `supplier` 的登记（本函数不再持有任何手写路由清单/guard）。
func NewSupplierSurface(api *AdminAPI, iss *auth.Issuer, users auth.UserStatusProvider) http.Handler {
	var h http.Handler = api.SupplierSurfaceHandler()
	h = SupplierScopeInject(h)
	h = auth.RequireRole(users, domain.SupplierSurfaceRoles()...)(h)
	h = auth.RequireJWT(iss, users)(h)
	return h
}
