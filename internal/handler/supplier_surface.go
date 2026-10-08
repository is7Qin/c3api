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
// chi 参数名 / query / header 规则长期同构的隐含耦合；编译器能发现映射遗漏）。
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

// SupplierFundsPaths 供应商面上**资金写命令**的方法 + 路径集合（spec §6.5 I5 /
// A24①）：这些命令一律要求具名 JWT 操作者，静态 admin token 明确 403。
//
// 供应商面（openapi tag `supplier`，22 op）目前只登记一条资金命令——
// `POST /api/user/supplier/settlements`（supplier_request）。其余六条
// （approve/reject/claim/confirmed-failed/paid/admin-request）只在管理面
// `/api/admin/supplier/*` 上，由 `requireFundsActor`（supplier_admin.go）逐条 403。
// 本表是「供应商面资金入口」的唯一事实源：新增资金 op 到供应商面时须一并登记，
// 否则它会绕过本 middleware 的静态 token 拒绝（落到 RequireJWT 的 401，而非
// 契约要求的 403）。
var SupplierFundsPaths = []struct {
	Method string
	Path   string
}{
	{http.MethodPost, SupplierSurfaceBaseURL + "/settlements"},
}

// supplierFundsPathSet 资金路径查表（`METHOD PATH` → 命中）。
var supplierFundsPathSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(SupplierFundsPaths))
	for _, p := range SupplierFundsPaths {
		m[p.Method+" "+p.Path] = struct{}{}
	}
	return m
}()

// SupplierStaticTokenFundsGate 资金入口的静态 admin token 拒绝（§6.5 I5 / A24①）：
// 请求携带**已配置的静态 admin token** 且命中资金写命令 ⇒ **403**（该凭证无 uid，
// 无法担保在资金事务内锁定并复核 users 行）。
//
// 为什么必须在本 middleware 而不是资金 handler 里：供应商面最外层是
// RequireJWT（非 JWT 凭据 ⇒ 401），静态 admin token 不是 JWT ⇒ 在到达生成面之前
// 就已被 401 短死，永远到不了 handler 的 403。契约要求的「七种资金命令逐条 403」
// 因此只能在**鉴权链最外层**兑现。
//
// 其他无效凭证仍走 401（门控语义不变：不泄漏「该 token 是资金入口专用」）；未配置
// 静态 token（空）时本 middleware 恒不放行任何请求到 403 分支——空值守卫与
// adminAuth 同款（空 token 永不匹配）。
func SupplierStaticTokenFundsGate(adminToken string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if adminToken == "" || r.Header.Get("Authorization") != "Bearer "+adminToken {
				next.ServeHTTP(w, r)
				return
			}
			if _, ok := supplierFundsPathSet[r.Method+" "+r.URL.Path]; !ok {
				// 静态 token 打非资金面：交回既有鉴权链 ⇒ 401（与改造前一致，
				// 不扩大 403 面）。
				next.ServeHTTP(w, r)
				return
			}
			httpface.WriteErr(w, http.StatusForbidden, "funds commands require a named JWT operator")
		})
	}
}

// NewSupplierSurface 组装供应商面完整链路（挂载于 /api/user/supplier/*）：
//
//	SupplierStaticTokenFundsGate(adminToken) → RequireJWT(iss,users) →
//	RequireRole(users, SupplierSurfaceRoles...) →
//	SupplierScopeInject → 生成路由（tag `supplier` 的 22 op）
//
// 可达集唯一事实源 = domain.SupplierSurfaceRoles()（§2.6）；暴露面唯一事实源 =
// openapi 里 tag `supplier` 的登记（本函数不再持有任何手写路由清单/guard）；资金
// 入口的静态 token 拒绝面唯一事实源 = SupplierFundsPaths（见其注释）。
//
// adminToken = 部署配置的静态管理面 token（空 = 未启用静态鉴权）。
func NewSupplierSurface(api *AdminAPI, iss *auth.Issuer, users auth.UserStatusProvider, adminToken string) http.Handler {
	var h http.Handler = api.SupplierSurfaceHandler()
	h = SupplierScopeInject(h)
	h = auth.RequireRole(users, domain.SupplierSurfaceRoles()...)(h)
	h = auth.RequireJWT(iss, users)(h)
	h = SupplierStaticTokenFundsGate(adminToken)(h)
	return h
}
