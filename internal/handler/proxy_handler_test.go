// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"github.com/is7qin/c3api/internal/service"
)

// M4a 门禁（spec-proxy-pool §9.4 API 部分 + 简报 §3）：六个 `/proxies` 端点的
// 状态码与回显。
//
// 纪律：
//   - 只用 require（不用 assert）；
//   - 不 t.Parallel()（fakeStore 是共享可变状态，并行会互相踩）；
//   - **不赌墙钟**：探测时刻只断言「非 null」（`last_probe_at` 由 service 的注入
//     时钟产生，时钟精度的确定性由 service 的 proxy_test.go 覆盖；handler 只做
//     投影，断言具体时刻等于把两层的职责搅在一起）。

// newProxyRouter 装配被测面：fakeStore + service.New（与 newTestHandler 同款，
// 仅多返回 store 供断言落库值）+ /api/admin 前缀的 chi 路由。
func newProxyRouter(t *testing.T) (http.Handler, *fakeStore) {
	t.Helper()
	store := newFakeStore()
	svc := service.New(service.Deps{
		Store: store, Scheduler: fakeSched{}, Invalidate: service.NopInvalidator{},
		Publisher: nil, RuleReload: nil, Auth: &fakeKeys{}, Log: nil, EmailCodeStore: store,
	})
	r := chi.NewRouter()
	r.Mount("/", New(svc).Router())
	return r, store
}

// doProxy 发一次管理面请求。body 为空串 ⇒ 无 body。
func doProxy(r http.Handler, method, path, body string) *httptest.ResponseRecorder {
	var req *http.Request
	if body == "" {
		req = httptest.NewRequest(method, path, http.NoBody)
	} else {
		req = httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// decodeProxyBody 把响应体解成 map（键存在性断言的唯一可靠方式：结构体解出来
// 无法区分「键缺席」与「零值」）。
func decodeProxyBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var got map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got), "response must be a JSON object: %s", rec.Body.String())
	return got
}

// requireNoPasswordKey 断言响应里**绝无** password 键（spec §3.3：密码永不出）。
func requireNoPasswordKey(t *testing.T, got map[string]any) {
	t.Helper()
	_, has := got["password"]
	require.False(t, has, "read model must not carry a password key: %v", got)
	_, has = got["Password"]
	require.False(t, has, "read model must not carry a Password key: %v", got)
}

// createProxy 走端点建一条并返回其 id。
func createProxy(t *testing.T, r http.Handler, body string) int64 {
	t.Helper()
	rec := doProxy(r, http.MethodPost, "/api/admin/proxies", body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := decodeProxyBody(t, rec)
	id, ok := got["id"].(float64)
	require.True(t, ok, "create response must carry numeric id: %v", got)
	return int64(id)
}

// TestProxyCreateListGet 建 → 列表 → 单取：状态码、回显、id 稳定序。
func TestProxyCreateListGet(t *testing.T) {
	r, _ := newProxyRouter(t)

	first := createProxy(t, r, `{"name":"a-socks","scheme":"socks5","host":"10.0.0.1","port":1080,
		"username":"u1","password":"p1","remark":"hk"}`)
	second := createProxy(t, r, `{"name":"b-http","scheme":"HTTP","host":"10.0.0.2","port":3128}`)
	require.Greater(t, second, first, "id must increase")

	// 单取：非密字段原样回显，密码只以 has_password 出现。
	rec := doProxy(r, http.MethodGet, "/api/admin/proxies/"+strconv.FormatInt(first, 10), "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	one := decodeProxyBody(t, rec)
	requireNoPasswordKey(t, one)
	require.EqualValues(t, first, one["id"])
	require.Equal(t, "a-socks", one["name"])
	require.Equal(t, "socks5", one["scheme"])
	require.Equal(t, "10.0.0.1", one["host"])
	require.EqualValues(t, 1080, one["port"])
	require.Equal(t, "u1", one["username"])
	require.Equal(t, true, one["has_password"])
	require.Equal(t, true, one["enabled"], "Create 缺省 enabled=true")
	require.Equal(t, "hk", one["remark"])
	require.Equal(t, false, one["last_probe_ok"])
	require.Nil(t, one["last_probe_at"], "从未探测 ⇒ null（不是零值时间）")
	require.Nil(t, one["last_probe_error"])
	require.Contains(t, one, "created_at")
	require.Contains(t, one, "updated_at")
	require.NotContains(t, rec.Body.String(), "p1", "密码不得出现在任何响应里")

	// 列表：id 升序稳定序；第二条无认证 ⇒ username/has_password 均为「无」。
	rec = doProxy(r, http.MethodGet, "/api/admin/proxies", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var list struct {
		Rows []map[string]any `json:"rows"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Rows, 2)
	require.EqualValues(t, first, list.Rows[0]["id"])
	require.EqualValues(t, second, list.Rows[1]["id"])
	for _, row := range list.Rows {
		requireNoPasswordKey(t, row)
	}
	require.Equal(t, "http", list.Rows[1]["scheme"], "scheme 小写归一")
	require.Nil(t, list.Rows[1]["username"], "无认证 ⇒ null")
	require.Equal(t, false, list.Rows[1]["has_password"])
}

// TestProxyCredentialTriState 凭据三态（spec §3.3）：缺席 = 保留 / null = 清空 /
// 值 = 落值 / "" = 400。username 与 password 同规矩（同一份三态语义）。
func TestProxyCredentialTriState(t *testing.T) {
	r, store := newProxyRouter(t)
	id := createProxy(t, r, `{"name":"p","scheme":"socks5","host":"10.0.0.9","port":1080,
		"username":"u1","password":"p1"}`)
	path := "/api/admin/proxies/" + strconv.FormatInt(id, 10)

	// 只改 remark ⇒ 两列凭据都不变（缺席 = 不变；这是「改备注不能顺手把凭据抹掉」）。
	rec := doProxy(r, http.MethodPut, path, `{"name":"p","scheme":"socks5","host":"10.0.0.9","port":1080,"remark":"r2"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	put := decodeProxyBody(t, rec)
	requireNoPasswordKey(t, put)
	require.Equal(t, "u1", put["username"], "username 缺席 ⇒ 保留")
	require.Equal(t, true, put["has_password"], "password 缺席 ⇒ 保留")
	require.Equal(t, "r2", put["remark"])
	require.Equal(t, "p1", store.proxies[id].p.Password, "落库值未被抹")

	// 显式 null ⇒ 清空（空串哨兵已取消：清空只有一个拼法 null）。
	rec = doProxy(r, http.MethodPut, path, `{"name":"p","scheme":"socks5","host":"10.0.0.9","port":1080,
		"username":null,"password":null}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	put = decodeProxyBody(t, rec)
	require.Nil(t, put["username"], "null ⇒ 无认证")
	require.Equal(t, false, put["has_password"], "null ⇒ 密码清空")
	require.Equal(t, "", store.proxies[id].p.Password, "落库值已清")
	require.Equal(t, "r2", store.proxies[id].p.Remark, "本次没提 remark ⇒ 保留上一步的值")

	// 非空值 ⇒ 落值。
	rec = doProxy(r, http.MethodPut, path, `{"name":"p","scheme":"socks5h","host":"10.0.0.9","port":1080,
		"username":"u2","password":"p2"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	put = decodeProxyBody(t, rec)
	require.Equal(t, "u2", put["username"])
	require.Equal(t, true, put["has_password"])
	require.Equal(t, "socks5h", put["scheme"], "socks5h 不归一成 socks5")

	// "" ⇒ 400（与账号写面同一口径：可空标量的空串是错误，不是清空）。
	for _, field := range []string{"username", "password", "remark"} {
		body := `{"name":"p","scheme":"socks5","host":"10.0.0.9","port":1080,"` + field + `":""}`
		rec := doProxy(r, http.MethodPut, path, body)
		require.Equal(t, http.StatusBadRequest, rec.Code, "%s: empty string must be 400", field)
	}

	// enabled 是不可空标量：null ⇒ 400（null 不是 enabled 的一种状态）。
	rec = doProxy(r, http.MethodPut, path, `{"name":"p","scheme":"socks5","host":"10.0.0.9","port":1080,"enabled":null}`)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// enabled 缺席 ⇒ 不变；给出即落值。
	rec = doProxy(r, http.MethodPut, path, `{"name":"p","scheme":"socks5","host":"10.0.0.9","port":1080,"enabled":false}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, false, decodeProxyBody(t, rec)["enabled"])
}

// TestProxyConflict404AndSoftDelete name 冲突 ⇒ 409；不存在/已软删 ⇒ 404；
// 软删后再取同一 id ⇒ 404（软删 = 视同不存在）。
func TestProxyConflict404AndSoftDelete(t *testing.T) {
	r, _ := newProxyRouter(t)
	body := `{"name":"dup","scheme":"socks5","host":"10.0.0.1","port":1080}`
	id := createProxy(t, r, body)

	// 同名再建（大小写/空白归一后同名同样冲突：service 归一后落库，唯一索引裁决）。
	require.Equal(t, http.StatusConflict, doProxy(r, http.MethodPost, "/api/admin/proxies", body).Code)

	// 改名撞别人 ⇒ 409。
	other := createProxy(t, r, `{"name":"other","scheme":"http","host":"10.0.0.2","port":8080}`)
	rec := doProxy(r, http.MethodPut, "/api/admin/proxies/"+strconv.FormatInt(other, 10),
		`{"name":"dup","scheme":"http","host":"10.0.0.2","port":8080}`)
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	// 不存在的 id：四个带 id 的端点全 404（probe 也不例外：条目不存在才是错误）。
	missing := "/api/admin/proxies/9999"
	require.Equal(t, http.StatusNotFound, doProxy(r, http.MethodGet, missing, "").Code)
	require.Equal(t, http.StatusNotFound, doProxy(r, http.MethodPut, missing,
		`{"name":"x","scheme":"http","host":"10.0.0.3","port":8080}`).Code)
	require.Equal(t, http.StatusNotFound, doProxy(r, http.MethodDelete, missing, "").Code)
	require.Equal(t, http.StatusNotFound, doProxy(r, http.MethodPost, missing+"/probe", "").Code)

	// 软删：200 + deleted=true，随后单取/再删 404。
	path := "/api/admin/proxies/" + strconv.FormatInt(id, 10)
	rec = doProxy(r, http.MethodDelete, path, "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, true, decodeProxyBody(t, rec)["deleted"])
	require.Equal(t, http.StatusNotFound, doProxy(r, http.MethodGet, path, "").Code)
	require.Equal(t, http.StatusNotFound, doProxy(r, http.MethodDelete, path, "").Code)
}

// TestProxyValidation400 非法输入 ⇒ 400（判据全在 service，本用例只钉线格式
// 到状态码的映射，不重复实现一遍值域规则）。
func TestProxyValidation400(t *testing.T) {
	r, _ := newProxyRouter(t)
	cases := map[string]string{
		"scheme 不在白名单":       `{"name":"x","scheme":"ftp","host":"10.0.0.1","port":1080}`,
		"scheme 缺失":          `{"name":"x","host":"10.0.0.1","port":1080}`,
		"name 空":             `{"name":"  ","scheme":"http","host":"10.0.0.1","port":1080}`,
		"host 空":             `{"name":"x","scheme":"http","host":"","port":1080}`,
		"host 带 @（userinfo）": `{"name":"x","scheme":"http","host":"u@h","port":1080}`,
		"host 带 /":           `{"name":"x","scheme":"http","host":"h/p","port":1080}`,
		"port 0":             `{"name":"x","scheme":"http","host":"10.0.0.1","port":0}`,
		"port 越界":            `{"name":"x","scheme":"http","host":"10.0.0.1","port":70000}`,
		"凭据控制字符":             `{"name":"x","scheme":"http","host":"10.0.0.1","port":1080,"password":"a\u0007b"}`,
	}
	for name, body := range cases {
		rec := doProxy(r, http.MethodPost, "/api/admin/proxies", body)
		require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", name, rec.Body.String())
	}
	// IPv6 去方括号是**成功**路径（不能只测拒绝）：[::1] 落库为 ::1。
	id := createProxy(t, r, `{"name":"v6","scheme":"socks5","host":"[::1]","port":1080}`)
	rec := doProxy(r, http.MethodGet, "/api/admin/proxies/"+strconv.FormatInt(id, 10), "")
	require.Equal(t, "::1", decodeProxyBody(t, rec)["host"])
}

// TestProxyProbe 探测：可达 ⇒ last_probe_ok=true；不可达 ⇒ 仍是 200 + ok=false +
// 脱敏后的错误文本（拨不通是探测结论，不是 API 错误）。
func TestProxyProbe(t *testing.T) {
	r, _ := newProxyRouter(t)

	// 可达：真起一个 TCP 监听（只拨 TCP，无握手 ⇒ 无需实现任何协议）。
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer ln.Close()
	addr, ok := ln.Addr().(*net.TCPAddr)
	require.True(t, ok)
	upID := createProxy(t, r, `{"name":"up","scheme":"http","host":"127.0.0.1","port":`+
		strconv.Itoa(addr.Port)+`}`)

	rec := doProxy(r, http.MethodPost, "/api/admin/proxies/"+strconv.FormatInt(upID, 10)+"/probe", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	up := decodeProxyBody(t, rec)
	requireNoPasswordKey(t, up)
	require.Equal(t, true, up["last_probe_ok"], "可达 ⇒ last_probe_ok=true")
	require.NotNil(t, up["last_probe_at"], "探测后必须回显探测时刻")
	require.Nil(t, up["last_probe_error"], "成功探测无错误文本")

	// 不可达：拿一个刚关掉的端口（拒连）。
	dead, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	deadAddr, ok := dead.Addr().(*net.TCPAddr)
	require.True(t, ok)
	require.NoError(t, dead.Close())

	// 凭据刻意取成「与地址同形」的串：这样 service 的脱敏路径**可被观察**
	// （拨测错误文本里必然出现 127.0.0.1，被替换成 ***）。否则这段文本天然不含
	// 凭据，用例会假绿。
	downID := createProxy(t, r, `{"name":"down","scheme":"http","host":"127.0.0.1","port":`+
		strconv.Itoa(deadAddr.Port)+`,"username":"127.0.0.1","password":"127.0.0.1"}`)
	rec = doProxy(r, http.MethodPost, "/api/admin/proxies/"+strconv.FormatInt(downID, 10)+"/probe", "")
	require.Equal(t, http.StatusOK, rec.Code, "拨不通不是 API 错误：%s", rec.Body.String())
	down := decodeProxyBody(t, rec)
	requireNoPasswordKey(t, down)
	require.Equal(t, false, down["last_probe_ok"])
	probeErr, ok := down["last_probe_error"].(string)
	require.True(t, ok, "失败探测必须回显错误文本：%v", down)
	require.NotEmpty(t, probeErr)
	require.Contains(t, probeErr, "***", "凭据必须被脱敏成 ***")
	require.NotContains(t, probeErr, "127.0.0.1", "脱敏后不得残留地址里的凭据原文")

	// 探测结论在**更新**后仍然保留（spec §6.3：last_probe_* 只由 probe 写）。
	rec = doProxy(r, http.MethodPut, "/api/admin/proxies/"+strconv.FormatInt(upID, 10),
		`{"name":"up","scheme":"http","host":"127.0.0.1","port":`+strconv.Itoa(addr.Port)+`,"remark":"改备注"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	kept := decodeProxyBody(t, rec)
	require.Equal(t, true, kept["last_probe_ok"], "改备注不得把探测结论抹成「从未探测」")
	require.NotNil(t, kept["last_probe_at"])
}

// TestProxiesKeepOffSupplierSurface **管理面**代理池路径不得出现在供应商面/用户面
// 生成物里：tag 机制的结构性保证，用两处断言钉住（源文本 + 可达性），防止将来有人把
// path 标成两个 tag 而无人察觉。
//
// M4c 之后供应商面有了**自己的**代理池路径（`/api/user/supplier/proxies`，tag supplier），
// 故断言从「源文本里没有 /proxies 子串」收紧为「源文本里没有**管理面前缀**的 /proxies
// 注册」「没有 /api/admin/ 前缀」——管理面那六条 op 依然只在 proxies tag 下。
func TestProxiesKeepOffSupplierSurface(t *testing.T) {
	for _, file := range []string{
		filepath.Join("supplier", "api.gen.go"),
		filepath.Join("user", "api.gen.go"),
	} {
		raw, err := os.ReadFile(file)
		require.NoError(t, err)
		// 管理面生成物里的注册行是 `options.BaseURL+"/proxies"`（BaseURL = /admin）：
		// 这个字面量出现在供应商面/用户面即说明 /proxies 混进了别的 tag。
		require.NotContains(t, string(raw), `options.BaseURL+"/proxies"`,
			"%s 是 -include-tags 生成物：管理面 /proxies 只属 proxies tag，不得进入", file)
		// 管理面代理池的**路径字面量**同样不得出现（注释里提到别的管理端点属正常文档，
		// 故这里只钉路径字面量，不钉 "/api/admin/" 子串）。
		require.NotContains(t, string(raw), `"/api/admin/proxies`,
			"%s 不得出现管理面代理池路径", file)
	}
	// 结构层面（未登记 ⇒ 未注册 ⇒ 404）由 TestSupplierSurfaceStructuralDefaultDeny
	// 的 blocked 清单覆盖；这里补一条「openapi 里 /proxies 的 tag 恰为 [proxies]」，
	// 让「有意登记」这件事本身也被断言，而不是只依赖生成物。
	raw, err := os.ReadFile(filepath.Join("..", "..", "openapi", "openapi.yaml"))
	require.NoError(t, err)
	var doc struct {
		Paths map[string]map[string]yaml.Node `yaml:"paths"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &doc))
	seen := 0
	for path, item := range doc.Paths {
		if path != "/proxies" && !strings.HasPrefix(path, "/proxies/") {
			continue
		}
		for key, node := range item {
			if _, isMethod := openapiMethods[key]; !isMethod {
				continue
			}
			var op struct {
				Tags []string `yaml:"tags"`
			}
			require.NoError(t, node.Decode(&op))
			require.Equal(t, []string{"proxies"}, op.Tags,
				"%s %s 的 tag 必须恰为 [proxies]（多挂一个 supplier 就绕开了 default-deny）", strings.ToUpper(key), path)
			seen++
		}
	}
	require.Equal(t, 6, seen, "六个 op：GET/POST /proxies + GET/PUT/DELETE /proxies/{id} + POST /proxies/{id}/probe")
}
