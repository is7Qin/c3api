// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/service"
)

// staticAdminToken 测试用静态管理面 token（非 JWT）。用于验证供应商面资金入口
// 的静态 token 拒绝（§6.5 I5 / A24①）。
const staticAdminToken = "static-admin-token-for-test"

// openapiMethods OpenAPI path item 里的 HTTP 方法键（其余键如 parameters/summary
// 是 path 级字段，不是操作）。
var openapiMethods = map[string]struct{}{
	"get": {}, "post": {}, "put": {}, "patch": {}, "delete": {},
	"head": {}, "options": {}, "trace": {},
}

// openapiSupplierRoutes 供应商面**应有登记集**（method + path）的**唯一事实源**：
// 直接解析 `openapi/openapi.yaml` 里 tag `supplier` 的操作（不再是与 yaml 并存的
// 手抄守卫副本）。yaml 多挂一条而 Go 侧漂移也会被抓——只要 tag 是 supplier，
// 生成面就必须登记；反之未登记的 op 不会出现在本清单里。只读测试用 yaml.v3
// （生产包不依赖 yaml）。
func openapiSupplierRoutes(t *testing.T) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "openapi", "openapi.yaml"))
	require.NoError(t, err, "读取 openapi 契约（供应商面登记集的唯一事实源）")
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &doc), "解析 openapi.yaml")
	var out []string
	for path, item := range doc.Paths {
		for key, node := range item {
			if _, ok := openapiMethods[key]; !ok {
				continue
			}
			var op struct {
				Tags []string `yaml:"tags"`
			}
			if err := node.Decode(&op); err != nil {
				continue
			}
			for _, tag := range op.Tags {
				if tag == "supplier" {
					out = append(out, strings.ToUpper(key)+" "+path)
					break
				}
			}
		}
	}
	sort.Strings(out)
	return out
}

// walkRoutes 收集已注册路由（method + pattern）。
func walkRoutes(t *testing.T, h http.Handler) []string {
	t.Helper()
	mux, ok := h.(chi.Routes)
	require.True(t, ok, "供应商面必须是 chi 路由（生成面 HandlerWithOptions）")
	var out []string
	require.NoError(t, chi.Walk(mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		out = append(out, method+" "+route)
		return nil
	}))
	sort.Strings(out)
	return out
}

// TestSupplierSurfaceRegisteredRoutes A14④（结构性 default-deny）：生成面**只**注册
// tag `supplier` 的 op——登记集恰为 openapi 的供应商面清单，多一条少一条都算失败。
// 「未登记 ⇒ 未注册 ⇒ 404」即 default-deny 的全部机制（无手写允许清单/guard）。
func TestSupplierSurfaceRegisteredRoutes(t *testing.T) {
	api := newTestHandler(t)
	got := walkRoutes(t, api.SupplierSurfaceHandler())

	want := openapiSupplierRoutes(t)
	require.Equal(t, want, got, "供应商面登记集必须 = openapi 中 tag supplier 的 path 集")
}

// TestSupplierSurfaceStructuralDefaultDeny A14④：管理面资源在供应商面**不可达**
// ——未登记（结构性 404）；组写面/assignments、模板写面、新增管理端点同理。
func TestSupplierSurfaceStructuralDefaultDeny(t *testing.T) {
	api := newTestHandler(t)
	h := api.SupplierSurfaceHandler()
	mux := h.(chi.Routes)

	reachable := []struct{ method, path string }{
		{"GET", "/api/user/supplier/accounts"},
		{"POST", "/api/user/supplier/accounts"},
		{"GET", "/api/user/supplier/accounts/usage"},
		{"POST", "/api/user/supplier/accounts/batch-update"},
		{"POST", "/api/user/supplier/accounts/batch-delete"},
		{"POST", "/api/user/supplier/accounts/batch-import-codex-oauth"},
		{"POST", "/api/user/supplier/accounts/batch-import-codex-pat"},
		{"GET", "/api/user/supplier/accounts/1"},
		{"PATCH", "/api/user/supplier/accounts/1"},
		{"DELETE", "/api/user/supplier/accounts/1"},
		{"GET", "/api/user/supplier/accounts/1/ext"},
		{"PUT", "/api/user/supplier/accounts/1/ext"},
		{"GET", "/api/user/supplier/accounts/1/groups"},
		{"POST", "/api/user/supplier/accounts/1/recover"},
		// groups GET 只读候选（可达）、templates GET 只读（可达）。
		{"GET", "/api/user/supplier/groups"},
		{"GET", "/api/user/supplier/templates"},
		{"GET", "/api/user/supplier/templates/1"},
		// 业务面（可达；空态零值/空列表，不 404）。
		{"GET", "/api/user/supplier/overview"},
		{"GET", "/api/user/supplier/earnings"},
		{"GET", "/api/user/supplier/chunks"},
		{"GET", "/api/user/supplier/settlements"},
	}
	for _, tc := range reachable {
		// 可达性按**路由命中**判定（避免与作用域 404——如越域 id——混淆：
		// 生成面命中即证明「已登记」，语义层的 404 另由作用域测试覆盖）。
		require.True(t, mux.Match(chi.NewRouteContext(), tc.method, tc.path),
			"%s %s 必须命中生成面（登记在 openapi 的供应商面）", tc.method, tc.path)
	}

	// 无路径参数的登记端点须真正跑通（≤ 证明登记的是**同一批管理面实现**，
	// 而非占位 stub）：账号/分组/模板列表在缺省作用域（管理面全量）下 200。
	// **usage 不在此列**：它的 200 依赖「无作用域 = 全量」这一管理面语义，拿它
	// 当「登记的是真实现」的证据等于把越域可读编码成期望行为（A14③ 反例）。
	// usage 的**作用域**语义由 TestSupplierUsageScopeDeniesForeignAccountIDs
	// （供应商 JWT + 他人 account_ids ⇒ 404）与账户作用域 PG 用例覆盖。
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/user/supplier/accounts"},
		{"GET", "/api/user/supplier/groups"},
		{"GET", "/api/user/supplier/templates"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, http.NoBody)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "%s %s: %s", tc.method, tc.path, rec.Body.String())
	}

	blocked := []struct{ method, path string }{
		// 管理面资源 ⇒ 未登记 ⇒ 404。
		{"GET", "/api/user/supplier/users"},
		{"GET", "/api/user/supplier/settings"},
		{"GET", "/api/user/supplier/prices"},
		{"GET", "/api/user/supplier/rules"},
		{"GET", "/api/user/supplier/ops/workers"},
		{"GET", "/api/user/supplier/mail/templates"},
		{"GET", "/api/user/supplier/redemption-codes"},
		{"GET", "/api/user/supplier/keys"},
		{"GET", "/api/user/supplier/stats/trend"},
		{"GET", "/api/user/supplier/routing/flow"},
		{"GET", "/api/user/supplier/supplier/settlements"},
		{"GET", "/api/user/supplier/supplier/balances"},
		// 组写面/assignments ⇒ 不可达。
		{"POST", "/api/user/supplier/groups"},
		{"PUT", "/api/user/supplier/groups/1"},
		{"DELETE", "/api/user/supplier/groups/1"},
		{"GET", "/api/user/supplier/groups/1/assignments"},
		{"PUT", "/api/user/supplier/groups/1/assignments"},
		{"POST", "/api/user/supplier/groups/batch-delete"},
		// 模板写面 ⇒ 不可达。
		{"POST", "/api/user/supplier/templates"},
		{"PUT", "/api/user/supplier/templates/1"},
		{"DELETE", "/api/user/supplier/templates/1"},
		{"GET", "/api/user/supplier/templates/1/ext"},
		{"POST", "/api/user/supplier/templates/batch-delete"},
		// 反向断言：假设新增一个管理端点 ⇒ 默认不对供应商暴露（未登记）。
		{"GET", "/api/user/supplier/brand-new-admin-thing"},
	}
	for _, tc := range blocked {
		req := httptest.NewRequest(tc.method, tc.path, http.NoBody)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		// 未登记 ⇒ chi 404；同路径仅登记了其它方法时 405。二者都表示「不可达」。
		require.Contains(t, []int{http.StatusNotFound, http.StatusMethodNotAllowed}, rec.Code,
			"%s %s 必须不可达（default-deny）", tc.method, tc.path)
	}
}

// TestSupplierFundsStaticTokenForbidden §6.5 I5 / A24①：供应商面的资金写命令必须
// **拒绝静态 admin token（403）**——改造前它在 RequireJWT 处先被 401 短死，永远到
// 不了 handler 的 403；该凭证无 uid，无法担保资金事务内锁定并复核 users 行。
// 其他无效凭证仍 401（门控语义不变）；静态 token 打**非资金**面也仍 401
// （不扩大 403 面）。具名供应商 JWT 照常可达（403 只针对静态凭证）。
func TestSupplierFundsStaticTokenForbidden(t *testing.T) {
	api := newTestHandler(t)
	iss := auth.NewIssuer("s")
	active := domain.UserSnapshot{Status: domain.UserStatusActive, Role: domain.RoleSupplier}
	h := NewSupplierSurface(api, iss, fakeUsers{sn: active}, staticAdminToken)

	// ① 资金命令 + 静态 admin token ⇒ **403**（不是 401）。
	req := httptest.NewRequest(http.MethodPost, SupplierSurfaceBaseURL+"/settlements", strings.NewReader(`{"amount_millis":1,"request_key":"k"}`))
	req.Header.Set("Authorization", "Bearer "+staticAdminToken)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code,
		"静态 admin token 打资金命令必须 403（A24①）：%s", rec.Body.String())

	// ② 每条登记的资金路径都必须 403（SupplierFundsPaths 即契约面）。
	for _, fp := range SupplierFundsPaths {
		req := httptest.NewRequest(fp.Method, fp.Path, strings.NewReader(`{"amount_millis":1,"request_key":"k"}`))
		req.Header.Set("Authorization", "Bearer "+staticAdminToken)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		require.Equal(t, http.StatusForbidden, rec.Code, "%s %s 必须 403", fp.Method, fp.Path)
	}

	// ③ 静态 token 打非资金面 ⇒ 仍 401（既有语义；不把 403 面扩大到读路径）。
	req = httptest.NewRequest(http.MethodGet, SupplierSurfaceBaseURL+"/overview", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+staticAdminToken)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code, "静态 token 非资金面仍是 401")

	// ④ 其他无效凭证 ⇒ 401（不因本改造变成 403）。
	req = httptest.NewRequest(http.MethodPost, SupplierSurfaceBaseURL+"/settlements", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer not-the-admin-token")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code, "其他无效凭证仍 401")

	// ⑤ 未配置静态 token（空）⇒ 任何 Bearer 都走 JWT 门控（401），不产生 403。
	noStatic := NewSupplierSurface(api, iss, fakeUsers{sn: active}, "")
	req = httptest.NewRequest(http.MethodPost, SupplierSurfaceBaseURL+"/settlements", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer ")
	rec = httptest.NewRecorder()
	noStatic.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// ⑥ 具名供应商 JWT ⇒ 穿过静态 token 门（不 403/401），进入资金 handler。
	tok, err := iss.Issue(77, "s@example.com", string(domain.RoleSupplier), 0)
	require.NoError(t, err)
	req = httptest.NewRequest(http.MethodPost, SupplierSurfaceBaseURL+"/settlements",
		strings.NewReader(`{"amount_millis":1,"request_key":"k"}`))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.NotContains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, rec.Code,
		"具名 JWT 不得被静态 token 门拦下（资金 Actor 来自 claims）：%s", rec.Body.String())
}

// TestSupplierUsageScopeDeniesForeignAccountIDs A14③（C1 回归）：供应商 JWT 调
// `/accounts/usage?account_ids=<他人 id>` ⇒ **404**（整批作用域前置校验，不补零、
// 不泄漏存在性）；本属账号 ⇒ 200。旧实现（无 owner 谓词）在此处返回 200 + 他人
// 用量——本用例是该漏洞的固定回归面。
func TestSupplierUsageScopeDeniesForeignAccountIDs(t *testing.T) {
	store := newFakeStore()
	// 账号 1 归他人（uid 88：平台管理员另兼供应商）；账号 2 归本人（uid 77）。
	store.accs[1] = &domain.Account{ID: 1, Name: "other", Enabled: true, SupplierUserID: 88}
	store.accs[2] = &domain.Account{ID: 2, Name: "mine", Enabled: true, SupplierUserID: 77}
	svc := service.New(service.Deps{Store: store, Scheduler: fakeSched{}, Invalidate: service.NopInvalidator{}, Keys: &fakeKeys{}, EmailCodeStore: store})
	api := New(svc)

	iss := auth.NewIssuer("s")
	active := domain.UserSnapshot{Status: domain.UserStatusActive, Role: domain.RoleSupplier}
	h := NewSupplierSurface(api, iss, fakeUsers{sn: active}, staticAdminToken)

	tok, err := iss.Issue(77, "s@example.com", string(domain.RoleSupplier), 0)
	require.NoError(t, err)
	call := func(query string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet,
			SupplierSurfaceBaseURL+"/accounts/usage?"+query, http.NoBody)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// 他人 id（越域）⇒ 404（且不得是 200——补零即泄漏）。
	rec := call("account_ids=1&window=24h")
	require.Equal(t, http.StatusNotFound, rec.Code, "越域 account_ids 必须 404：%s", rec.Body.String())

	// 混合（本人 + 他人）⇒ 整批 404（任一越域即整事务失败）。
	rec = call("account_ids=2,1&window=24h")
	require.Equal(t, http.StatusNotFound, rec.Code, "混合批任一越域必须整批 404：%s", rec.Body.String())

	// 不存在（非本人所有）⇒ 同样 404（不泄漏存在性差异）。
	rec = call("account_ids=999999&window=24h")
	require.Equal(t, http.StatusNotFound, rec.Code)

	// 本属账号 ⇒ 200（无记录账号零值补齐仍成立）。
	rec = call("account_ids=2&window=24h")
	require.Equal(t, http.StatusOK, rec.Code, "本属账号必须 200：%s", rec.Body.String())
}

// TestAccountScopeFromDefault 缺省 = 管理面全量。
func TestAccountScopeFromDefault(t *testing.T) {
	s := AccountScopeFrom(httptest.NewRequest(http.MethodGet, "/", nil).Context())
	require.False(t, s.Set, "缺省管理面全量")
	require.True(t, s.MatchesOwner(0))
	require.True(t, s.MatchesOwner(999))
}

type fakeUsers struct{ sn domain.UserSnapshot }

func (f fakeUsers) UserSnapshot(int64) (domain.UserSnapshot, bool) { return f.sn, true }

// TestSupplierScopeInject 作用域注入：RequireJWT → SupplierScopeInject ⇒
// AccountScopeFrom = {jwtUser, true}（越域归属被阻）。
func TestSupplierScopeInject(t *testing.T) {
	iss := auth.NewIssuer("s")
	tok, err := iss.Issue(77, "s@example.com", string(domain.RoleSupplier), 0)
	require.NoError(t, err)
	users := fakeUsers{sn: domain.UserSnapshot{Status: domain.UserStatusActive, Role: domain.RoleSupplier}}
	var got domain.AccountScope
	h := auth.RequireJWT(iss, users)(SupplierScopeInject(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = AccountScopeFrom(r.Context())
	})))
	req := httptest.NewRequest(http.MethodGet, SupplierSurfaceBaseURL+"/accounts", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	h.ServeHTTP(httptest.NewRecorder(), req)
	require.True(t, got.Set)
	require.Equal(t, int64(77), got.OwnerUID)
	require.True(t, got.MatchesOwner(77))
	require.False(t, got.MatchesOwner(88), "他人归属越域")
}

// TestNewSupplierSurfaceGate 整链门控（A13②方向）：无凭据 ⇒ 401 在生成面之前短死
// （不泄漏路由存在性）；具名供应商 JWT ⇒ 可达；user 角色 ⇒ 403。
func TestNewSupplierSurfaceGate(t *testing.T) {
	api := newTestHandler(t)
	iss := auth.NewIssuer("s")
	active := domain.UserSnapshot{Status: domain.UserStatusActive, Role: domain.RoleSupplier}
	h := NewSupplierSurface(api, iss, fakeUsers{sn: active}, staticAdminToken)

	// 未鉴权 ⇒ 401（RequireJWT 最外层）。
	req := httptest.NewRequest(http.MethodGet, SupplierSurfaceBaseURL+"/accounts", http.NoBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// 具名供应商 JWT ⇒ 可达（生成面已注册）。
	tok, err := iss.Issue(77, "s@example.com", string(domain.RoleSupplier), 0)
	require.NoError(t, err)
	req = httptest.NewRequest(http.MethodGet, SupplierSurfaceBaseURL+"/accounts", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.NotContains(t, []int{http.StatusUnauthorized, http.StatusNotFound}, rec.Code)

	// 非供应商角色（user）⇒ 403（RequireRole 快照基）。
	userTok, err := iss.Issue(88, "u@example.com", string(domain.RoleUser), 0)
	require.NoError(t, err)
	plain := NewSupplierSurface(api, iss, fakeUsers{sn: domain.UserSnapshot{Status: domain.UserStatusActive, Role: domain.RoleUser}}, staticAdminToken)
	req = httptest.NewRequest(http.MethodGet, SupplierSurfaceBaseURL+"/accounts", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+userTok)
	rec = httptest.NewRecorder()
	plain.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code, "user 角色不得进入供应商面")
}

// TestSupplierStaticTokenFundsForbidden 静态 admin token 命中供应商面**资金写命令**
// ⇒ 403（§6.5 I5 / A24①）；打非资金路径仍按原鉴权链 ⇒ 401（不扩大 403 面）。
func TestSupplierStaticTokenFundsForbidden(t *testing.T) {
	api := newTestHandler(t)
	iss := auth.NewIssuer("s")
	active := domain.UserSnapshot{Status: domain.UserStatusActive, Role: domain.RoleSupplier}
	h := NewSupplierSurface(api, iss, fakeUsers{sn: active}, staticAdminToken)

	// 资金写命令 + 静态 token ⇒ 403（最外层资金门拒绝，不进生成面）。
	req := httptest.NewRequest(http.MethodPost, SupplierSurfaceBaseURL+"/settlements", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+staticAdminToken)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code, "静态 admin token 不得执行资金命令")

	// 非资金路径 + 静态 token ⇒ 交回既有鉴权链（非 JWT）⇒ 401（非 403）。
	req = httptest.NewRequest(http.MethodGet, SupplierSurfaceBaseURL+"/settlements", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+staticAdminToken)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code, "静态 token 打非资金面应为 401")
}
