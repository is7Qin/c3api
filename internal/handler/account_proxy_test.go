// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
)

// account_proxy_test.go 账号面 `proxy_id`/`proxy_name` 的读写契约（spec-proxy-pool
// §3.6 / §6.2 第 4·6 条，M1e）：
//
//   - 写面：`proxy_id` 与 `supplier_user_id` 共用同一个三态投影（缺省 = 不变 /
//     null = 解绑 / >0 = 绑定 / <=0 非 null ⇒ 400）；**供应商面可写**，越界由 service
//     的唯一绑定谓词兜住并一律 404（不是 400——400 会把「绑自己的代理」误杀）。
//   - 读面：两个字段同生共死，且只回显**对当前调用方可见**的代理。供应商面看到
//     「被管理员绑了平台代理」的账号时，键为 null 且 JSON 里不出现该代理的名字。
//   - 平铺拷贝陷阱：列表页走 AccountView（平铺结构），两字段必须逐字段拷贝。

const (
	proxyTestSupplierUID  = int64(77)
	proxyTestPlatformName = "pool-platform-exit"
	proxyTestPrivateName  = "pool-supplier-exit"
)

// seedHandlerProxy 在替身里建一条代理（owner>0 ⇒ 该供应商私池；0 ⇒ 平台代理），
// 并把落库行一并返回：handler 用例需要它以"仓储展开后的条目"身份出现在账号行上
// （见 stageAccountProxy）。
func seedHandlerProxy(t *testing.T, store *fakeStore, owner int64, name string) (int64, *domain.Proxy) {
	t.Helper()
	ctx := context.Background()
	if owner > 0 {
		ctx = domain.WithAccountScope(ctx, domain.SupplierAccountScope(owner))
	}
	id, err := store.CreateProxy(ctx, repository.ProxyCreate{
		ProxyFields: repository.ProxyFields{
			Name: name, Scheme: domain.ProxySchemeHTTP, Host: "proxy.example.com", Port: 3128, Enabled: true,
		},
	})
	require.NoError(t, err)
	row := store.proxies[id].p // 落库行（归属已由作用域钉死）
	return id, &row
}

// stageAccountProxy 表达「仓储已按调用方 scope 展开（或未展开）这一条」：`Account.Proxy`
// 是**读面装配的结果**，规则只有一处（repository 的 expandProxies，spec §3.6），门禁是
// `TestPGAccountReadExpandsProxy`。handler 只把它投影出去（C2/S6 之后这里没有第二份
// 可见性判定），故用例直接给出读路径该看到的输入：px = nil 即"对本调用方不可见/绑定缺失"。
func stageAccountProxy(store *fakeStore, accID int64, px *domain.Proxy) {
	store.accs[accID].Proxy = px
}

// seedHandlerAccount 直接在替身里插一个账号（owner 0 = 平台自有）。
func seedHandlerAccount(store *fakeStore, id int64, name string, owner int64) {
	store.accs[id] = &domain.Account{
		ID: id, Name: name, TemplateID: 1, UpstreamKey: "sk-" + name,
		MaxConcurrency: 8, Enabled: true, SupplierUserID: owner,
		LifecycleRevision: 1, IdentityRevision: 1,
	}
}

// accountProxyFixture 账号 + 两条代理（平台一条、供应商 77 私池一条）+ 两个 API 面。
type accountProxyFixture struct {
	api      *AdminAPI
	store    *fakeStore
	platform int64
	private  int64
	// platformRow/privateRow 两条代理的落库行：用例以它们充当「仓储展开后的条目」。
	platformRow *domain.Proxy
	privateRow  *domain.Proxy
}

func newAccountProxyFixture(t *testing.T) accountProxyFixture {
	t.Helper()
	store := newFakeStore()
	store.tpls[1] = &domain.Template{ID: 1, Name: "codex-tpl", CredentialType: credential.TypeCodexOAuth,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}}
	svc := service.New(service.Deps{Store: store, Scheduler: fakeSched{}, Invalidate: service.NopInvalidator{},
		Auth: &fakeKeys{}, EmailCodeStore: store})
	platform, platformRow := seedHandlerProxy(t, store, 0, proxyTestPlatformName)
	private, privateRow := seedHandlerProxy(t, store, proxyTestSupplierUID, proxyTestPrivateName)
	return accountProxyFixture{
		api:         New(svc),
		store:       store,
		platform:    platform,
		private:     private,
		platformRow: platformRow,
		privateRow:  privateRow,
	}
}

// TestAccountProxyAdminPatch 管理面写面三态 + 回显（两个字段必须同时出现）。
func TestAccountProxyAdminPatch(t *testing.T) {
	f := newAccountProxyFixture(t)
	seedHandlerAccount(f.store, 1, "acc-admin", 0)

	t.Run("bind echoes id and name", func(t *testing.T) {
		// 读路径输入：管理面（无 scope）⇒ 仓储展开这条行（expandProxies）。
		stageAccountProxy(f.store, 1, f.platformRow)
		rec := doImport(t, f.api, http.MethodPatch, "/api/admin/accounts/1",
			`{"proxy_id":`+itoa(f.platform)+`}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var acc Account
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))
		require.NotNil(t, acc.ProxyId, "绑定后回显 proxy_id")
		require.Equal(t, f.platform, *acc.ProxyId)
		require.NotNil(t, acc.ProxyName, "同生共死：名字一并回显")
		require.Equal(t, proxyTestPlatformName, *acc.ProxyName)
	})

	t.Run("absent leaves the binding alone", func(t *testing.T) {
		rec := doImport(t, f.api, http.MethodPatch, "/api/admin/accounts/1", `{"name":"acc-admin-renamed"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var acc Account
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))
		require.NotNil(t, acc.ProxyId, "缺省 ⇒ 绑定不变")
		require.Equal(t, f.platform, *acc.ProxyId)
	})

	t.Run("explicit null unbinds", func(t *testing.T) {
		rec := doImport(t, f.api, http.MethodPatch, "/api/admin/accounts/1", `{"proxy_id":null}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var acc Account
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))
		require.Nil(t, acc.ProxyId, "解绑 ⇒ proxy_id 为 null")
		require.Nil(t, acc.ProxyName, "解绑 ⇒ proxy_name 也为 null（不许留半个）")
		raw, err := json.Marshal(acc)
		require.NoError(t, err)
		require.Contains(t, string(raw), `"proxy_id":null`, "键必须在，值为 null（缺键 = 调用方无法区分语义）")
		require.Contains(t, string(raw), `"proxy_name":null`)
	})

	t.Run("zero is 400 not unbind", func(t *testing.T) {
		rec := doImport(t, f.api, http.MethodPatch, "/api/admin/accounts/1", `{"proxy_id":0}`)
		require.Equal(t, http.StatusBadRequest, rec.Code, "<=0 且非 null ⇒ 400（清空只有 null 一个拼法）：%s", rec.Body.String())
	})

	t.Run("missing proxy is 404", func(t *testing.T) {
		rec := doImport(t, f.api, http.MethodPatch, "/api/admin/accounts/1", `{"proxy_id":999999}`)
		require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	})

	t.Run("absent proxy_id on other fields stays null", func(t *testing.T) {
		seedHandlerAccount(f.store, 2, "acc-unbound", 0)
		rec := doImport(t, f.api, http.MethodGet, "/api/admin/accounts/2", "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var acc Account
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))
		require.Nil(t, acc.ProxyId, "未绑定 ⇒ null")
		require.Nil(t, acc.ProxyName)
	})
}

// TestAccountProxyListEchoesBoth 平铺拷贝陷阱：列表页（AccountView 是平铺结构）必须
// 逐行回显两个字段；漏拷即「列表恒 null、编辑页恒 null」——base_url 同款教训。
func TestAccountProxyListEchoesBoth(t *testing.T) {
	f := newAccountProxyFixture(t)
	seedHandlerAccount(f.store, 1, "acc-list-bound", 0)
	seedHandlerAccount(f.store, 2, "acc-list-free", 0)
	stageAccountProxy(f.store, 1, f.platformRow) // 仓储已展开（管理面）
	rec := doImport(t, f.api, http.MethodPatch, "/api/admin/accounts/1", `{"proxy_id":`+itoa(f.platform)+`}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = doImport(t, f.api, http.MethodGet, "/api/admin/accounts", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list AccountListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Rows, 2)

	byName := make(map[string]AccountView, len(list.Rows))
	for _, row := range list.Rows {
		byName[*row.Name] = row
	}
	bound := byName["acc-list-bound"]
	require.NotNil(t, bound.ProxyId, "列表页必须回显 proxy_id（toAPIAccountView 平铺拷贝）")
	require.Equal(t, f.platform, *bound.ProxyId)
	require.NotNil(t, bound.ProxyName, "列表页必须回显 proxy_name")
	require.Equal(t, proxyTestPlatformName, *bound.ProxyName)

	free := byName["acc-list-free"]
	require.Nil(t, free.ProxyId)
	require.Nil(t, free.ProxyName)
}

// TestAccountProxySupplierSurface 供应商面的读写差异（§3.6 + §6.2 第 6 条）：
// 能绑自己的代理（200），不能碰平台代理（404），看不见不是自己的代理（键为 null
// 且 JSON 里不出现它的名字）。
func TestAccountProxySupplierSurface(t *testing.T) {
	f := newAccountProxyFixture(t)

	iss := auth.NewIssuer("s")
	active := domain.UserSnapshot{Status: domain.UserStatusActive, Role: domain.RoleSupplier}
	h := NewSupplierSurface(f.api, iss, fakeSnapshot{fakeUsers{sn: active}, fakeMgmt{}})
	tok, err := iss.Issue(proxyTestSupplierUID, "sup@example.com", string(domain.RoleSupplier), 0)
	require.NoError(t, err)

	call := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, SupplierSurfaceBaseURL+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+tok)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	t.Run("binds its own proxy", func(t *testing.T) {
		seedHandlerAccount(f.store, 10, "acc-sup-own", proxyTestSupplierUID)
		// 仓储对"供应商本人的代理"照常展开（TestPGAccountReadExpandsProxy）。
		stageAccountProxy(f.store, 10, f.privateRow)
		rec := call(http.MethodPatch, "/accounts/10", `{"proxy_id":`+itoa(f.private)+`}`)
		require.Equal(t, http.StatusOK, rec.Code,
			"供应商绑自己的代理是合法写（不是 400）：%s", rec.Body.String())
		var acc Account
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))
		require.NotNil(t, acc.ProxyId)
		require.Equal(t, f.private, *acc.ProxyId)
		require.NotNil(t, acc.ProxyName)
		require.Equal(t, proxyTestPrivateName, *acc.ProxyName)

		// 读路径同样回显（GET /accounts/{id}）。
		rec = call(http.MethodGet, "/accounts/10", "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))
		require.NotNil(t, acc.ProxyId)
		require.Equal(t, f.private, *acc.ProxyId)
	})

	t.Run("platform proxy is 404 not 403", func(t *testing.T) {
		seedHandlerAccount(f.store, 11, "acc-sup-platform-try", proxyTestSupplierUID)
		rec := call(http.MethodPatch, "/accounts/11", `{"proxy_id":`+itoa(f.platform)+`}`)
		require.Equal(t, http.StatusNotFound, rec.Code,
			"平台代理只有管理员能分配：越界 404（403 会承认「有这条代理」）: %s", rec.Body.String())
	})

	t.Run("admin-bound platform proxy stays invisible", func(t *testing.T) {
		seedHandlerAccount(f.store, 12, "acc-sup-admin-bound", proxyTestSupplierUID)
		// 管理员（无 scope）把平台代理绑到供应商账号上——合法写（§3.6 平台代理 → 任意账号）。
		rec := doImport(t, f.api, http.MethodPatch, "/api/admin/accounts/12",
			`{"proxy_id":`+itoa(f.platform)+`}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		// 供应商读这一行：仓储**不展开**（expandProxies 的 scope 谓词——平台池不是公共
		// 池），于是输入是 `ProxyID 非 nil + Proxy nil`。这里把这个输入摆在行上，
		// 断言 handler 不再自己判一遍（C2/S6 删掉了那份 owner 判断）：两个键都是 null，
		// 且响应体里**不出现**该代理名。
		stageAccountProxy(f.store, 12, nil)
		rec = call(http.MethodGet, "/accounts/12", "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		body := rec.Body.String()
		require.NotContains(t, body, proxyTestPlatformName,
			"平台代理名不得出现在供应商面响应里（名字即泄漏面）：%s", body)
		require.Contains(t, body, `"proxy_id":null`, "键在、值为 null：%s", body)
		require.Contains(t, body, `"proxy_name":null`)

		var acc Account
		require.NoError(t, json.Unmarshal([]byte(body), &acc))
		require.Nil(t, acc.ProxyId)
		require.Nil(t, acc.ProxyName)
		// 绑定事实本身没被可见性改写（列还在）。
		require.NotNil(t, f.store.accs[12].ProxyID)
		require.Equal(t, f.platform, *f.store.accs[12].ProxyID)

		// 管理面读同一行：仓储展开 ⇒ 看得见。差异只由**仓储的作用域谓词**决定，handler
		// 对同一份输入只做同一次投影（若这层还留着第二份判定，这里就会是 null）。
		stageAccountProxy(f.store, 12, f.platformRow)
		rec = doImport(t, f.api, http.MethodGet, "/api/admin/accounts/12", "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))
		require.NotNil(t, acc.ProxyName)
		require.Equal(t, proxyTestPlatformName, *acc.ProxyName)
	})

	t.Run("supplier list page hides the platform proxy per row", func(t *testing.T) {
		// 一页两行：10 = 自己的代理（仓储展开），12 = 平台代理（仓储对供应商不展开）。
		stageAccountProxy(f.store, 10, f.privateRow)
		stageAccountProxy(f.store, 12, nil)
		rec := call(http.MethodGet, "/accounts", "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		body := rec.Body.String()
		require.NotContains(t, body, proxyTestPlatformName, "列表页同样是泄漏面：%s", body)
		require.Contains(t, body, proxyTestPrivateName, "自己的代理名照常回显：%s", body)
	})

	t.Run("create response projects the same way", func(t *testing.T) {
		// 创建面的响应体走的是 toAPIAccount（与单读同一函数），故"仓储未展开 ⇒ 两个
		// null"在此按投影本身钉住：POST 面不可能绕过那份契约（它没有第二条装配路径）。
		pid := f.platform
		created := toAPIAccount(&domain.Account{ID: 99, Name: "acc-created", ProxyID: &pid})
		require.Nil(t, created.ProxyId, "创建响应：仓储未展开 ⇒ proxy_id 为 null")
		require.Nil(t, created.ProxyName, "创建响应：仓储未展开 ⇒ proxy_name 为 null")
	})
}

// TestAccountPatchFromBodyProxyID 字段级：与 supplier_user_id 共用同一份三态投影，
// 但供应商面**不再** 400（归属面禁区不适用于出口绑定）。
func TestAccountPatchFromBodyProxyID(t *testing.T) {
	mgmt := context.Background()
	supplier := domain.WithAccountScope(mgmt, domain.SupplierAccountScope(proxyTestSupplierUID))

	body := AccountConfigPatch{}
	require.NoError(t, json.Unmarshal([]byte(`{"proxy_id":5}`), &body))
	p, err := accountPatchFromBody(supplier, &body)
	require.NoError(t, err)
	require.NotNil(t, p.ProxyID)
	require.Equal(t, int64(5), *p.ProxyID, "供应商面可写：投影照常落值")

	body = AccountConfigPatch{}
	require.NoError(t, json.Unmarshal([]byte(`{"proxy_id":null}`), &body))
	p, err = accountPatchFromBody(supplier, &body)
	require.NoError(t, err)
	require.NotNil(t, p.ProxyID)
	require.Equal(t, int64(0), *p.ProxyID, "显式 null ⇒ &0（repo 层「落 NULL」编码）")

	body = AccountConfigPatch{}
	require.NoError(t, json.Unmarshal([]byte(`{"name":"x"}`), &body))
	p, err = accountPatchFromBody(supplier, &body)
	require.NoError(t, err)
	require.Nil(t, p.ProxyID, "缺席 ⇒ 不变")

	body = AccountConfigPatch{}
	require.NoError(t, json.Unmarshal([]byte(`{"proxy_id":-1}`), &body))
	_, err = accountPatchFromBody(mgmt, &body)
	require.Error(t, err, "<=0 且非 null ⇒ 400")
}
