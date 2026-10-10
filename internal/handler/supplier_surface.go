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
//     $ref 复用管理面 components；生成代码对同构标量生成同底层类型别名
//     （ListLimit/AccountEnabled 等）。运行时由生成的 supplier wrapper 解析**一次**
//     参数后，**类型化直调**同一批 AdminAPI handler（同一 AccountConfigPatch/
//     validateAccountPatch/accountFieldSpecs）。
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
// 业务 op 直调 AdminAPI 自身方法（supplier_business.go）；账号/分组/模板 op 由生成的
// supplier wrapper 解析**一次**参数后，**类型化直调**同一 AdminAPI 实现——id 透传、
// query/header 显式映射到管理面参数类型，不再从 `r` 二次解析（消除两套生成路由的
// chi 参数名 / query / header 规则长期同构的隐含耦合；编译器能发现**字段名/类型**
// 不匹配——但具名 struct literal 仍可省略字段，**字段遗漏由测试/评审兜底**）。
type supplierSurface struct {
	api *AdminAPI
}

// ---- 账号/分组/模板子集（§2.5）：类型化直调管理面 AdminAPI 实现。----

// GetSupplierAccounts GET /api/user/supplier/accounts。
func (s supplierSurface) GetSupplierAccounts(w http.ResponseWriter, r *http.Request, params supplier.GetSupplierAccountsParams) {
	s.api.GetAccounts(w, r, GetAccountsParams{
		Limit:      params.Limit,
		Offset:     params.Offset,
		Name:       params.Name,
		Sort:       params.Sort,
		Order:      (*GetAccountsParamsOrder)(params.Order),
		TemplateId: params.TemplateId,
		Enabled:    params.Enabled,
	})
}

// PostSupplierAccounts POST /api/user/supplier/accounts（body 由 AdminAPI 自 r 读一次）。
func (s supplierSurface) PostSupplierAccounts(w http.ResponseWriter, r *http.Request) {
	s.api.PostAccounts(w, r)
}

// GetSupplierAccountsUsage GET /api/user/supplier/accounts/usage。
func (s supplierSurface) GetSupplierAccountsUsage(w http.ResponseWriter, r *http.Request, params supplier.GetSupplierAccountsUsageParams) {
	s.api.GetAccountsUsage(w, r, GetAccountsUsageParams{
		AccountIds: params.AccountIds,
		From:       params.From,
		To:         params.To,
		Window:     params.Window,
		Timezone:   params.Timezone,
	})
}

// PostSupplierAccountsBatchUpdate POST /api/user/supplier/accounts/batch-update。
func (s supplierSurface) PostSupplierAccountsBatchUpdate(w http.ResponseWriter, r *http.Request) {
	s.api.PostAccountsBatchUpdate(w, r)
}

// PostSupplierAccountsBatchDelete POST /api/user/supplier/accounts/batch-delete。
func (s supplierSurface) PostSupplierAccountsBatchDelete(w http.ResponseWriter, r *http.Request) {
	s.api.PostAccountsBatchDelete(w, r)
}

// PostSupplierAccountsBatchImportCodexOauth POST /api/user/supplier/accounts/batch-import-codex-oauth。
func (s supplierSurface) PostSupplierAccountsBatchImportCodexOauth(w http.ResponseWriter, r *http.Request) {
	s.api.PostAccountsBatchImportCodexOauth(w, r)
}

// PostSupplierAccountsBatchImportCodexPat POST /api/user/supplier/accounts/batch-import-codex-pat。
func (s supplierSurface) PostSupplierAccountsBatchImportCodexPat(w http.ResponseWriter, r *http.Request) {
	s.api.PostAccountsBatchImportCodexPat(w, r)
}

// GetSupplierAccountsId GET /api/user/supplier/accounts/{id}。
func (s supplierSurface) GetSupplierAccountsId(w http.ResponseWriter, r *http.Request, id int64) {
	s.api.GetAccountsId(w, r, id)
}

// PatchSupplierAccountsId PATCH /api/user/supplier/accounts/{id}（可选 If-Match 显式
// 映射到管理面参数——两面契约同构）。
func (s supplierSurface) PatchSupplierAccountsId(w http.ResponseWriter, r *http.Request, id int64, params supplier.PatchSupplierAccountsIdParams) {
	s.api.PatchAccountsId(w, r, id, PatchAccountsIdParams{IfMatch: params.IfMatch})
}

// DeleteSupplierAccountsId DELETE /api/user/supplier/accounts/{id}。
func (s supplierSurface) DeleteSupplierAccountsId(w http.ResponseWriter, r *http.Request, id int64) {
	s.api.DeleteAccountsId(w, r, id)
}

// GetSupplierAccountsIdExt GET /api/user/supplier/accounts/{id}/ext。
func (s supplierSurface) GetSupplierAccountsIdExt(w http.ResponseWriter, r *http.Request, id int64) {
	s.api.GetAccountsIdExt(w, r, id)
}

// PutSupplierAccountsIdExt PUT /api/user/supplier/accounts/{id}/ext。
func (s supplierSurface) PutSupplierAccountsIdExt(w http.ResponseWriter, r *http.Request, id int64) {
	s.api.PutAccountsIdExt(w, r, id)
}

// GetSupplierAccountsIdGroups GET /api/user/supplier/accounts/{id}/groups。
func (s supplierSurface) GetSupplierAccountsIdGroups(w http.ResponseWriter, r *http.Request, id int64) {
	s.api.GetAccountsIdGroups(w, r, id)
}

// PostSupplierAccountsIdRecover POST /api/user/supplier/accounts/{id}/recover。
func (s supplierSurface) PostSupplierAccountsIdRecover(w http.ResponseWriter, r *http.Request, id int64) {
	s.api.PostAccountsIdRecover(w, r, id)
}

// GetSupplierGroups GET /api/user/supplier/groups（只读候选列表；组写面不登记）。
func (s supplierSurface) GetSupplierGroups(w http.ResponseWriter, r *http.Request, params supplier.GetSupplierGroupsParams) {
	s.api.GetGroups(w, r, GetGroupsParams{
		Limit:  params.Limit,
		Offset: params.Offset,
		Name:   params.Name,
		Sort:   params.Sort,
		Order:  (*GetGroupsParamsOrder)(params.Order),
	})
}

// GetSupplierTemplates GET /api/user/supplier/templates（只读；模板写面不登记）。
func (s supplierSurface) GetSupplierTemplates(w http.ResponseWriter, r *http.Request, params supplier.GetSupplierTemplatesParams) {
	s.api.GetTemplates(w, r, GetTemplatesParams{
		Limit:  params.Limit,
		Offset: params.Offset,
		Name:   params.Name,
		Sort:   params.Sort,
		Order:  (*GetTemplatesParamsOrder)(params.Order),
	})
}

// GetSupplierTemplatesId GET /api/user/supplier/templates/{id}（只读）。
func (s supplierSurface) GetSupplierTemplatesId(w http.ResponseWriter, r *http.Request, id int64) {
	s.api.GetTemplatesId(w, r, id)
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
// 账号/分组/模板 op 由生成 wrapper 解析一次参数后类型化直调同一批 AdminAPI 实现
// ——**暴露面唯一事实源 = openapi**。
func (h *AdminAPI) SupplierSurfaceHandler() http.Handler {
	badRequest := func(w http.ResponseWriter, r *http.Request, err error) {
		httpface.WriteErr(w, http.StatusBadRequest, err.Error())
	}
	return supplier.HandlerWithOptions(supplierSurface{api: h}, supplier.ChiServerOptions{ErrorHandlerFunc: badRequest})
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
//	RequireIdentity(iss,p) → RequireRole(p, SupplierSurfaceRoles...) →
//	SupplierScopeInject → 生成路由（tag `supplier` 的 22 op）
//
// 可达集唯一事实源 = domain.SupplierSurfaceRoles()（§2.6）；暴露面唯一事实源 =
// openapi 里 tag `supplier` 的登记（本函数不再持有任何手写路由清单/guard）。p 为
// 单一鉴权快照面（users + mgmt 合并，proxy.Auth 实现）。
//
// 资金命令（POST /api/user/supplier/settlements）与 JWT 同权（spec 2026-10-09 A5）：
// RequireIdentity 两条分支（JWT / 管理 key mk-）均注入 FundsActor（actor = owner，
// token_version = owner 快照当前值），写事务内复核通过——静态 token 专用资金门与
// 其 403 语义已随静态管理面 token 删除（§4.5）。
func NewSupplierSurface(api *AdminAPI, iss *auth.Issuer, p auth.SnapshotProvider) http.Handler {
	var h http.Handler = api.SupplierSurfaceHandler()
	h = SupplierScopeInject(h)
	h = auth.RequireRole(p, domain.SupplierSurfaceRoles()...)(h)
	h = auth.RequireIdentity(iss, p)(h)
	return h
}
