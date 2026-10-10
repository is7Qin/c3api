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

// fakeMgmt 管理 key 快照 provider（供应商面 mk- 路径用例）：按 Bearer mk-… 明文
// 查表；仅 status==active 命中（模拟 proxy.Auth.AuthenticateManagement）。
type fakeMgmt struct {
	metas map[string]domain.ManagementKeyMeta
}

func (f fakeMgmt) AuthenticateManagement(r *http.Request) (domain.ManagementKeyMeta, bool) {
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || !strings.HasPrefix(raw, "mk-") {
		return domain.ManagementKeyMeta{}, false
	}
	m, ok := f.metas[raw]
	if !ok || m.Status != domain.ManagementKeyStatusActive {
		return domain.ManagementKeyMeta{}, false
	}
	return m, true
}

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

// TestSupplierSurfaceParamContractPreserved S1：类型化直调后，非法 query / id /
// header 仍由生成 wrapper 与 PatchAccountsId 的 parseIfMatch 判定，响应码与改造前
// 一致（400），证明「同一次请求只解析一次」未改变对外契约。
func TestSupplierSurfaceParamContractPreserved(t *testing.T) {
	api := newTestHandler(t)
	h := api.SupplierSurfaceHandler()

	// 非法 query（limit 非整数）⇒ 生成 wrapper 绑定失败 ⇒ 400。
	req := httptest.NewRequest(http.MethodGet, "/api/user/supplier/accounts?limit=abc", http.NoBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// 非法 id（非整数）⇒ 400。
	req = httptest.NewRequest(http.MethodGet, "/api/user/supplier/accounts/not-an-id", http.NoBody)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())

	// 非法 If-Match ⇒ PatchAccountsId 的 parseIfMatch 判定 ⇒ 400。
	req = httptest.NewRequest(http.MethodPatch, "/api/user/supplier/accounts/1", strings.NewReader(`{"name":"x"}`))
	req.Header.Set("If-Match", `"not-a-number"`)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// TestSupplierManagementKeyReachesFunds 管理 key（mk-）与 JWT 同权（spec 2026-10-09
// A5）：供应商 owner 的 mk- 穿过供应商面鉴权链（不 401/403），进入资金 handler——
// 资金 actor = owner（RequireIdentity 注入 FundsActor）。静态 token 专用 403 门已随
// 静态管理面 token 删除（§4.5）。
func TestSupplierManagementKeyReachesFunds(t *testing.T) {
	api := newTestHandler(t)
	iss := auth.NewIssuer("s")
	active := domain.UserSnapshot{Status: domain.UserStatusActive, Role: domain.RoleSupplier}
	mgmt := fakeMgmt{metas: map[string]domain.ManagementKeyMeta{
		"mk-sup": {ID: 1, UserID: 77, Status: domain.ManagementKeyStatusActive},
	}}
	h := NewSupplierSurface(api, iss, fakeSnapshot{fakeUsers{sn: active}, mgmt})

	// mk- 供应商 owner 打资金写命令 ⇒ 穿过门控，进入生成面（非 401/403）。
	req := httptest.NewRequest(http.MethodPost, SupplierSurfaceBaseURL+"/settlements",
		strings.NewReader(`{"amount_millis":1,"request_key":"k"}`))
	req.Header.Set("Authorization", "Bearer mk-sup")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.NotContains(t, []int{http.StatusUnauthorized, http.StatusForbidden}, rec.Code,
		"mk- 不得被门控拦下（资金 Actor 来自 owner）：%s", rec.Body.String())

	// 无效 mk-（查表失败）⇒ 401（前缀先判、失败不回退 JWT）。
	req = httptest.NewRequest(http.MethodGet, SupplierSurfaceBaseURL+"/overview", http.NoBody)
	req.Header.Set("Authorization", "Bearer mk-ghost")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code, "未知 mk- 必须 401")

	// 其他无效凭证 ⇒ 401（不产生 403）。
	req = httptest.NewRequest(http.MethodPost, SupplierSurfaceBaseURL+"/settlements", strings.NewReader(`{}`))
	req.Header.Set("Authorization", "Bearer not-a-jwt")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code, "其他无效凭证仍 401")
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
	svc := service.New(service.Deps{Store: store, Scheduler: fakeSched{}, Invalidate: service.NopInvalidator{}, Auth: &fakeKeys{}, EmailCodeStore: store})
	api := New(svc)

	iss := auth.NewIssuer("s")
	active := domain.UserSnapshot{Status: domain.UserStatusActive, Role: domain.RoleSupplier}
	h := NewSupplierSurface(api, iss, fakeSnapshot{fakeUsers{sn: active}, fakeMgmt{}})

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

// fakeSnapshot 合并鉴权快照 provider（users + mgmt，spec 2026-10-09 §4.3）：
// NewSupplierSurface 现需单一 SnapshotProvider（同一 auth 只传一次）。嵌入两个
// fake 即同时满足两接口——资金同权用例用真实 mgmt metas，JWT-only 用例传空 fakeMgmt。
type fakeSnapshot struct {
	fakeUsers
	fakeMgmt
}

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
	h := NewSupplierSurface(api, iss, fakeSnapshot{fakeUsers{sn: active}, fakeMgmt{}})

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
	plain := NewSupplierSurface(api, iss, fakeSnapshot{fakeUsers{sn: domain.UserSnapshot{Status: domain.UserStatusActive, Role: domain.RoleUser}}, fakeMgmt{}})
	req = httptest.NewRequest(http.MethodGet, SupplierSurfaceBaseURL+"/accounts", http.NoBody)
	req.Header.Set("Authorization", "Bearer "+userTok)
	rec = httptest.NewRecorder()
	plain.ServeHTTP(rec, req)
	require.Equal(t, http.StatusForbidden, rec.Code, "user 角色不得进入供应商面")
}
