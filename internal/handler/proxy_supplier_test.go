// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// M4c 门禁（impl-brief-proxy-pool-m4c §3 + spec-proxy-pool §3.6/§6.2 第 5 条）：
// **代理池供应商面** `/api/user/supplier/proxies` 的六个 op，走**真实链路**
// （`NewSupplierSurface`：RequireIdentity → RequireRole → SupplierScopeInject → 生成的
// supplier 路由 → 类型化直调管理面 AdminAPI handler → service → 仓储）。
//
// 纪律：
//   - 只用 require（不用 assert）；不 t.Parallel()（fakeStore 是共享可变状态）；
//   - **越域一律 404**：不是 403——404 不泄露「别人有这条代理」；
//   - 断言「没有副作用」时**再读一次**（响应码绿灯不代表数据没被改）；
//   - 不赌墙钟：探测只断言「非 null」。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/service"
)

// 两个供应商 uid：77 = 被测主体，88 = 另一个租户。
const (
	supplierUIDM4c = int64(77)
	otherUIDM4c    = int64(88)
)

// proxySupplierHarness 供应商面 + 管理面共用的同一份 store：管理面负责准备「平台代理」
// 与账号绑定，供应商面是被测对象。
type proxySupplierHarness struct {
	store    *fakeStore
	svc      *service.Service
	supplier http.Handler
	iss      *auth.Issuer
}

func newProxySupplierHarness(t *testing.T) *proxySupplierHarness {
	t.Helper()
	store := newFakeStore()
	svc := service.New(service.Deps{
		Store: store, Scheduler: fakeSched{}, Invalidate: service.NopInvalidator{},
		Publisher: nil, RuleReload: nil, Auth: &fakeKeys{}, Log: nil, EmailCodeStore: store,
	})
	api := New(svc)
	iss := auth.NewIssuer("s")
	users := fakeUsers{sn: domain.UserSnapshot{Status: domain.UserStatusActive, Role: domain.RoleSupplier}}
	return &proxySupplierHarness{
		store:    store,
		svc:      svc,
		supplier: NewSupplierSurface(api, iss, fakeSnapshot{fakeUsers: users, fakeMgmt: fakeMgmt{}}),
		iss:      iss,
	}
}

// call 以 uid 的身份打一次供应商面（JWT 由 iss 真签，作用域由中间件注入）。
func (h *proxySupplierHarness) call(t *testing.T, uid int64, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	tok, err := h.iss.Issue(uid, "s@example.com", string(domain.RoleSupplier), 0)
	require.NoError(t, err)
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, SupplierSurfaceBaseURL+path, http.NoBody)
	} else {
		req = httptest.NewRequest(method, SupplierSurfaceBaseURL+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.supplier.ServeHTTP(rec, req)
	return rec
}

// supplierCtx 某个供应商的作用域 ctx（校验落库事实用；与中间件注入的形状一致）。
func supplierCtx(uid int64) context.Context {
	return domain.WithAccountScope(context.Background(), domain.SupplierAccountScope(uid))
}

// readProxyAs 以某供应商的作用域读一条（越域 ⇒ 报错，镜像真实仓库的 ErrNotFound）。
func (h *proxySupplierHarness) readProxyAs(t *testing.T, uid, id int64) (domain.Proxy, error) {
	t.Helper()
	return h.store.GetProxy(supplierCtx(uid), id)
}

// createPlatformProxy 走**管理面**（无 scope ⇒ owner_uid IS NULL）建平台代理。
func (h *proxySupplierHarness) createPlatformProxy(t *testing.T, name string) int64 {
	t.Helper()
	id, err := h.svc.CreateProxy(context.Background(), service.ProxyInput{
		Name: name, Scheme: domain.ProxySchemeSOCKS5, Host: "platform.example.com", Port: 1080,
	})
	require.NoError(t, err)
	return id
}

// createSupplierProxy 走供应商面建一条自己的代理并返回 id（其余用例的前置）。
func (h *proxySupplierHarness) createSupplierProxy(t *testing.T, uid int64, name string) int64 {
	t.Helper()
	rec := h.call(t, uid, http.MethodPost, "/proxies",
		`{"name":"`+name+`","scheme":"socks5","host":"`+name+`.example.com","port":1080}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := decodeProxyBody(t, rec)
	id, ok := got["id"].(float64)
	require.True(t, ok, "创建响应必含 id：%v", got)
	return int64(id)
}

// TestSupplierProxyCreateOwnedByJWT ① 建 → 落库 owner = JWT 本人（读回 owner_uid 等于
// 自己），密码永不出；管理面（无 scope）仍看得到全部且回显归属（§3.6）。
func TestSupplierProxyCreateOwnedByJWT(t *testing.T) {
	h := newProxySupplierHarness(t)

	rec := h.call(t, supplierUIDM4c, http.MethodPost, "/proxies",
		`{"name":"office","scheme":"SOCKS5","host":"px.example.com","port":1080,
			"username":"u1","password":"p1","remark":"hk"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := decodeProxyBody(t, rec)
	requireNoPasswordKey(t, got)

	// 归属**只由作用域决定**：响应与落库都必须等于 JWT 本人。
	require.Equal(t, float64(supplierUIDM4c), got["owner_uid"],
		"供应商面创建的条目 owner_uid 必须等于 JWT 本人：%v", got)
	id, ok := got["id"].(float64)
	require.True(t, ok, "创建响应必含 id：%v", got)

	row, err := h.readProxyAs(t, supplierUIDM4c, int64(id))
	require.NoError(t, err)
	require.NotNil(t, row.OwnerUID)
	require.Equal(t, supplierUIDM4c, *row.OwnerUID, "落库 owner 必须是 JWT 本人（不是 body 能指定的）")
	require.Equal(t, "office", row.Name)
	require.Equal(t, "p1", row.Password, "凭据落库（读面仍不回显密码）")

	// 管理面视角：看得到这条，且 owner_uid 回显 = 该供应商（§3.6 管理员可见全部）。
	all, err := h.svc.ListProxies(context.Background())
	require.NoError(t, err)
	require.Len(t, all, 1)
	require.NotNil(t, all[0].OwnerUID)
	require.Equal(t, supplierUIDM4c, *all[0].OwnerUID)
}

// TestSupplierProxyListScopedToOwner ② 列表只出自己的：管理员建的平台代理与别的供应商的
// 都不出现（平台池不是公共池）。
func TestSupplierProxyListScopedToOwner(t *testing.T) {
	h := newProxySupplierHarness(t)
	h.createPlatformProxy(t, "platform")
	h.createSupplierProxy(t, otherUIDM4c, "others")
	h.createSupplierProxy(t, supplierUIDM4c, "mine")

	rowsOf := func(uid int64) []string {
		rec := h.call(t, uid, http.MethodGet, "/proxies", "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body struct {
			Rows []map[string]any `json:"rows"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
		out := make([]string, 0, len(body.Rows))
		for _, row := range body.Rows {
			out = append(out, row["name"].(string))
			require.Equal(t, float64(uid), row["owner_uid"], "列表里每一条的归属都必须是本人")
		}
		return out
	}
	require.Equal(t, []string{"mine"}, rowsOf(supplierUIDM4c),
		"平台代理与别的供应商的都不出现")
	require.Equal(t, []string{"others"}, rowsOf(otherUIDM4c))

	// 管理面仍是全量（作用域缺省 = 全部）。
	all, err := h.svc.ListProxies(context.Background())
	require.NoError(t, err)
	require.Len(t, all, 3)
}

// TestSupplierProxyCrossScopeIs404 ③ 取/改/删/探测**别人的**或**平台代理** ⇒ 404，且
// **不产生任何副作用**（再读仍是原值）；不存在的 id 同样是 404（不泄露存在性差异）。
func TestSupplierProxyCrossScopeIs404(t *testing.T) {
	h := newProxySupplierHarness(t)
	platformID := h.createPlatformProxy(t, "platform")
	otherID := h.createSupplierProxy(t, otherUIDM4c, "others")

	cases := []struct {
		name string
		id   int64
		body string // PUT 的 body（其它方法为空）
	}{
		{"平台代理", platformID, `{"name":"hijacked","scheme":"http","host":"evil.example.com","port":8080}`},
		{"别人的私池", otherID, `{"name":"hijacked","scheme":"http","host":"evil.example.com","port":8080}`},
		{"不存在", 90001, `{"name":"hijacked","scheme":"http","host":"evil.example.com","port":8080}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			suffix := "/" + strconv.FormatInt(tc.id, 10)
			for _, c := range []struct{ method, path, body string }{
				{http.MethodGet, "/proxies" + suffix, ""},
				{http.MethodPut, "/proxies" + suffix, tc.body},
				{http.MethodDelete, "/proxies" + suffix, ""},
				{http.MethodPost, "/proxies" + suffix + "/probe", ""},
			} {
				rec := h.call(t, supplierUIDM4c, c.method, c.path, c.body)
				require.Equal(t, http.StatusNotFound, rec.Code,
					"%s %s 越域必须 404（不泄露存在性）：%s", c.method, c.path, rec.Body.String())
			}
		})
	}

	// 无副作用：平台代理与别人的条目都还是原值，且没被探测过。
	adminRows, err := h.svc.ListProxies(context.Background())
	require.NoError(t, err)
	require.Len(t, adminRows, 2)
	for _, row := range adminRows {
		require.NotEqual(t, "hijacked", row.Name, "越域 PUT 不得改到别人的条目")
		require.Nil(t, row.LastProbeAt, "越域 probe 不得写探测列")
	}
	other, err := h.readProxyAs(t, otherUIDM4c, otherID)
	require.NoError(t, err)
	require.Equal(t, "others", other.Name)
	require.Equal(t, "others.example.com", other.Host)
	require.Nil(t, other.LastProbeAt, "越域 probe 不得产生任何副作用")
}

// TestSupplierProxyDeleteUnbindsOnlyOwnAccounts ④ 删除自己的 ⇒ 200，且解绑范围**随归属**：
// 只解绑自己的账号；同一代理上若有越权绑定（平台账号），那一行**不受影响**——这条在
// 数据层就成立（§3.6「删私池代理只解绑该供应商的账号」），不依赖绑定谓词是否被绕过。
func TestSupplierProxyDeleteUnbindsOnlyOwnAccounts(t *testing.T) {
	h := newProxySupplierHarness(t)
	proxyID := h.createSupplierProxy(t, supplierUIDM4c, "private")

	mine := proxyID
	// 正常绑定：自己的账号。
	h.store.accs[1] = &domain.Account{ID: 1, Name: "mine-acct", Enabled: true,
		SupplierUserID: supplierUIDM4c, ProxyID: &mine}
	// 越权绑定：平台账号（supplier_user_id = 0）指向了私池代理——谓词上不可能出现，
	// 但解绑连坐的范围必须**在数据层**就正确，不能指望上游不写出这种行。
	h.store.accs[2] = &domain.Account{ID: 2, Name: "platform-acct", Enabled: true, ProxyID: &mine}

	rec := h.call(t, supplierUIDM4c, http.MethodDelete, "/proxies/"+strconv.FormatInt(proxyID, 10), "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	deleted := decodeProxyBody(t, rec)
	require.Equal(t, true, deleted["deleted"])

	require.Nil(t, h.store.accs[1].ProxyID, "自己账号必须解绑（回落直连）")
	require.NotNil(t, h.store.accs[2].ProxyID, "平台账号的绑定不受私池删除影响（解绑范围随归属）")
	require.Equal(t, proxyID, *h.store.accs[2].ProxyID)

	// 软删后本主也读不到（= 「软删视同不存在」）。
	_, err := h.readProxyAs(t, supplierUIDM4c, proxyID)
	require.Error(t, err)
	rec = h.call(t, supplierUIDM4c, http.MethodGet, "/proxies/"+strconv.FormatInt(proxyID, 10), "")
	require.Equal(t, http.StatusNotFound, rec.Code)
}

// TestSupplierProxyProbeUnreachable ⑤ 拨不通不是 API 错误：仍 200 + last_probe_ok=false
// + 已脱敏错误文本。
//
// 地址取 `240.0.0.1`（Class E 保留段）：它**不在**供应商私网判据里（不是 10/8、172.16/12、
// 192.168/16、127/8、169.254/16、100.64/10、0/8、::1、fc00::/7、fe80::/10），故供应商面
// 允许创建；而它没有任何路由 ⇒ 拨测**确定性地**失败。刻意不用 192.0.2.0/24 之类的
// TEST-NET：凡"可路由但无主机"的地址在会接管出栈连接的宿主/沙箱上都可能假成功
// （连上去就 close），那样这条用例会把「拨得通」当结论。
func TestSupplierProxyProbeUnreachable(t *testing.T) {
	h := newProxySupplierHarness(t)
	rec := h.call(t, supplierUIDM4c, http.MethodPost, "/proxies",
		`{"name":"blackhole","scheme":"http","host":"240.0.0.1","port":9}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	id := int64(decodeProxyBody(t, rec)["id"].(float64))

	rec = h.call(t, supplierUIDM4c, http.MethodPost, "/proxies/"+strconv.FormatInt(id, 10)+"/probe", "")
	require.Equal(t, http.StatusOK, rec.Code, "拨不通不是 API 错误：%s", rec.Body.String())
	got := decodeProxyBody(t, rec)
	requireNoPasswordKey(t, got)
	require.Equal(t, false, got["last_probe_ok"])
	require.NotNil(t, got["last_probe_at"], "探测后必须回显探测时刻")
	probeErr, ok := got["last_probe_error"].(string)
	require.True(t, ok, "失败探测必须回显错误文本：%v", got)
	require.NotEmpty(t, probeErr)
}

// TestSupplierProxyRejectsPrivateHosts ⑥ 供应商建/改到私网、回环、链路本地、云元数据地址
// ⇒ 400（那是一个免费的内网穿透跳板）；**管理面仍可建内网出口**（内网拨测/专线是合理
// 需求）——判据只在「供应商」分支生效。
func TestSupplierProxyRejectsPrivateHosts(t *testing.T) {
	h := newProxySupplierHarness(t)
	for _, host := range []string{"10.0.0.1", "127.0.0.1", "169.254.169.254", "localhost"} {
		rec := h.call(t, supplierUIDM4c, http.MethodPost, "/proxies",
			`{"name":"inner","scheme":"http","host":"`+host+`","port":8080}`)
		require.Equal(t, http.StatusBadRequest, rec.Code,
			"供应商建 %s 必须 400（私网拒绝）：%s", host, rec.Body.String())
	}
	// 创建面拦住了还不够：改地址必须同判据（否则「先建公网名、再改成 127.0.0.1」就是绕过通路）。
	id := h.createSupplierProxy(t, supplierUIDM4c, "public")
	rec := h.call(t, supplierUIDM4c, http.MethodPut, "/proxies/"+strconv.FormatInt(id, 10),
		`{"name":"public","scheme":"http","host":"127.0.0.1","port":8080}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, "改地址同样拒私网：%s", rec.Body.String())

	// 对照：管理面（无 scope）建内网出口照旧允许。
	_, err := h.svc.CreateProxy(context.Background(), service.ProxyInput{
		Name: "lan", Scheme: domain.ProxySchemeHTTP, Host: "10.0.0.1", Port: 3128,
	})
	require.NoError(t, err, "管理员建内网出口是合理需求（判据只在供应商分支生效）")
}

// TestSupplierProxyNameUniquePerOwner ⑦ name 唯一域 = **每归属**（§2.5）：
//   - 同一供应商内重名 ⇒ 409；
//   - **两个供应商可以同名**（互不干扰，也不构成「猜别人有没有这条代理」的探测面）；
//   - 平台池内自己唯一（`owner_uid IS NULL` 那一组）；
//   - 跨租户看不到对方的条目（所以「同名是否冲突」这条信道本身也不再泄漏存在性）。
func TestSupplierProxyNameUniquePerOwner(t *testing.T) {
	h := newProxySupplierHarness(t)
	h.createSupplierProxy(t, supplierUIDM4c, "dup")

	// 同一供应商重名 ⇒ 409。
	rec := h.call(t, supplierUIDM4c, http.MethodPost, "/proxies",
		`{"name":"dup","scheme":"http","host":"dup2.example.com","port":8080}`)
	require.Equal(t, http.StatusConflict, rec.Code, "同一供应商内重名必须 409：%s", rec.Body.String())

	// **跨供应商同名 ⇒ 允许**（本阶段的核心改动）。
	h.createSupplierProxy(t, otherUIDM4c, "dup")

	// 平台池是另一组：管理员可用这个名字，且平台池内自己仍唯一。
	h.createPlatformProxy(t, "dup")
	_, err := h.svc.CreateProxy(context.Background(), service.ProxyInput{
		Name: "dup", Scheme: domain.ProxySchemeHTTP, Host: "p2.example.com", Port: 8080,
	})
	require.ErrorIs(t, err, service.ErrConflict, "平台池内重名仍冲突（唯一域 = 每归属，不是取消唯一）")

	// 改名撞同一归属内的已有 name ⇒ 409。
	otherID := h.createSupplierProxy(t, supplierUIDM4c, "second")
	rec = h.call(t, supplierUIDM4c, http.MethodPut, "/proxies/"+strconv.FormatInt(otherID, 10),
		`{"name":"dup","scheme":"http","host":"second.example.com","port":8080}`)
	require.Equal(t, http.StatusConflict, rec.Code, "改名撞同归属已有 name 必须 409：%s", rec.Body.String())

	// 两组各自一条 "dup"，互不干扰（列表按归属隔离，谁的 dup 只出现在谁那里）。
	namesOf := func(uid int64) []string {
		rec := h.call(t, uid, http.MethodGet, "/proxies", "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body struct {
			Rows []map[string]any `json:"rows"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		out := make([]string, 0, len(body.Rows))
		for _, row := range body.Rows {
			require.Equal(t, float64(uid), row["owner_uid"], "只能看到自己的条目")
			out = append(out, row["name"].(string))
		}
		return out
	}
	require.ElementsMatch(t, []string{"dup", "second"}, namesOf(supplierUIDM4c))
	require.Equal(t, []string{"dup"}, namesOf(otherUIDM4c), "别人的 dup 不出现在这里")
}

// TestSupplierProxyBodyCannotSetOwner 供应商面**不接受** `owner_uid`：写模型 ProxyConfig
// 里根本没有这个字段，而 `httpface.Decode` 是 `DisallowUnknownFields` ⇒ 带它进 body 直接
// 400（比「静默忽略」更强：归属不可能被写体影响）。落库归属的唯一来源是作用域，见
// TestSupplierProxyCreateOwnedByJWT。
func TestSupplierProxyBodyCannotSetOwner(t *testing.T) {
	h := newProxySupplierHarness(t)
	rec := h.call(t, supplierUIDM4c, http.MethodPost, "/proxies",
		`{"name":"forge","scheme":"http","host":"forge.example.com","port":8080,"owner_uid":88}`)
	require.Equal(t, http.StatusBadRequest, rec.Code,
		"写体出现 owner_uid 必须拒绝（归属只能由作用域决定）：%s", rec.Body.String())
	require.Empty(t, h.store.proxies, "被拒的请求不得落库")

	id := h.createSupplierProxy(t, supplierUIDM4c, "ok")
	rec = h.call(t, supplierUIDM4c, http.MethodPut, "/proxies/"+strconv.FormatInt(id, 10),
		`{"name":"ok","scheme":"http","host":"ok.example.com","port":8080,"owner_uid":88}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, "更新面同样不接受 owner_uid：%s", rec.Body.String())

	row, err := h.readProxyAs(t, supplierUIDM4c, id)
	require.NoError(t, err)
	require.NotNil(t, row.OwnerUID)
	require.Equal(t, supplierUIDM4c, *row.OwnerUID, "归属不得被写体改动")
}
