// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// spec-proxy-import §6：批量导入的两个面（管理面 `/api/admin/proxies/import`、
// 供应商面 `/api/user/supplier/proxies/import`）。
//
// 纪律：
//   - 只用 require；不并行（fakeStore 是共享可变状态）；
//   - **不替身数据库**（C2/S8）：handler 的 fakeStore 不模拟名字唯一性，故"逐行冲突"
//     只能靠**注入点**验证映射（proxyCreateErr），不靠造两条同名数据假装它在查库；
//   - 分层判据按面断言：线格式错 ⇒ 整请求 400（并点名行号）；值域错 ⇒ 该行失败、其余
//     照建、整体 200。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/repository"
)

// postProxyImport 打管理面的导入端点。
func postProxyImport(r http.Handler, body string) *httptest.ResponseRecorder {
	return doProxy(r, http.MethodPost, "/api/admin/proxies/import", body)
}

// decodeImport 解导入响应（结构体解，字段语义由契约保证）。
func decodeImport(t *testing.T, rec *httptest.ResponseRecorder) ProxyImportResponse {
	t.Helper()
	var got ProxyImportResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got), "response must be ProxyImportResponse: %s", rec.Body.String())
	return got
}

// TestAdminProxyImportEnvelope 信封与线格式：坏请求整单 400（不降级成逐行结果），
// 且逐行请求里的错误要**点名行号**——几十行里"这个请求错了"没法定位。
func TestAdminProxyImportEnvelope(t *testing.T) {
	r, store := newProxyRouter(t)

	t.Run("rows 缺失", func(t *testing.T) {
		rec := postProxyImport(r, `{}`)
		require.Equal(t, http.StatusBadRequest, rec.Code)
	})

	t.Run("rows 为空数组", func(t *testing.T) {
		rec := postProxyImport(r, `{"rows":[]}`)
		require.Equal(t, http.StatusBadRequest, rec.Code, "空导入是调用方 bug，不是 no-op")
	})

	t.Run("超过上限", func(t *testing.T) {
		rows := make([]string, 0, 101)
		for i := 0; i < 101; i++ {
			rows = append(rows, fmt.Sprintf(`{"name":"n%d","scheme":"http","host":"h.example.com","port":8080}`, i))
		}
		rec := postProxyImport(r, `{"rows":[`+strings.Join(rows, ",")+`]}`)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Contains(t, rec.Body.String(), "rows must be 1..100")
	})

	t.Run("行内三态拼错", func(t *testing.T) {
		// 契约：可空标量 "" = 400（清空只有 null 一个拼法）。第 1 行（0 基）故意拼错，
		// 错误必须点名 rows[1]，而不是笼统的 "invalid json"。
		rec := postProxyImport(r, `{"rows":[
			{"name":"ok","scheme":"http","host":"h.example.com","port":8080},
			{"name":"bad","scheme":"http","host":"h.example.com","port":8080,"username":""}
		]}`)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Contains(t, rec.Body.String(), "rows[1]")
	})

	t.Run("未知字段被严格解码拒绝", func(t *testing.T) {
		rec := postProxyImport(r, `{"rows":[{"name":"a","scheme":"http","host":"h.example.com","port":8080,"owner_uid":7}]}`)
		require.Equal(t, http.StatusBadRequest, rec.Code, "写体里没有 owner_uid：带上它必须被拒，而不是静默忽略")
	})

	require.Empty(t, store.proxies, "被拒的请求不得落库")
}

// TestAdminProxyImportPartialSuccess 部分成功：值域错误只让那一行失败，整体 200；
// 行序 / 下标 / 名字 / reason 三分类都要能对上（前端靠这些把结果标回原文行）。
func TestAdminProxyImportPartialSuccess(t *testing.T) {
	r, store := newProxyRouter(t)

	rec := postProxyImport(r, `{"rows":[
		{"name":"imp-1","scheme":"socks5","host":"10.1.0.1","port":1080},
		{"name":"imp-2","scheme":"socks5","host":"10.1.0.2","port":70000},
		{"name":"imp-3","scheme":"http","host":"10.1.0.3","port":8080,"enabled":false}
	]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := decodeImport(t, rec)
	require.Equal(t, 2, got.Created)
	require.Equal(t, 1, got.Failed)
	require.Len(t, got.Rows, 3, "行数必须与请求一致")

	require.Equal(t, 0, got.Rows[0].Index)
	require.True(t, got.Rows[0].Ok)
	require.Equal(t, "imp-1", got.Rows[0].Name)
	require.NotNil(t, got.Rows[0].Id)
	require.Nil(t, got.Rows[0].Reason, "成功行不出现 reason")
	require.Nil(t, got.Rows[0].Error)

	require.Equal(t, 1, got.Rows[1].Index)
	require.False(t, got.Rows[1].Ok)
	require.Equal(t, "imp-2", got.Rows[1].Name)
	require.Nil(t, got.Rows[1].Id)
	require.NotNil(t, got.Rows[1].Reason)
	require.Equal(t, Invalid, *got.Rows[1].Reason)
	require.NotNil(t, got.Rows[1].Error)
	require.Contains(t, *got.Rows[1].Error, "port", "字段级说明必须透出（导入界面靠它定位）")

	require.Equal(t, 2, got.Rows[2].Index)
	require.True(t, got.Rows[2].Ok)

	// 落库事实（不是只看状态码）：两条进去了，显式 enabled=false 生效。
	require.Len(t, store.proxies, 2)
	var disabled int
	for _, row := range store.proxies {
		if !row.p.Enabled {
			disabled++
		}
	}
	require.Equal(t, 1, disabled, "显式 enabled=false 必须生效")
}

// TestAdminProxyImportConflictReason 逐行冲突的分类：fakeStore 不模拟唯一性（C2/S8），
// 故用注入点验证"仓储 ErrConflict ⇒ 该行 reason=conflict"这一层映射。
func TestAdminProxyImportConflictReason(t *testing.T) {
	r, store := newProxyRouter(t)
	store.proxyCreateErr = fmt.Errorf("%w: name=%q", repository.ErrConflict, "dup")

	rec := postProxyImport(r, `{"rows":[
		{"name":"dup","scheme":"http","host":"h.example.com","port":8080},
		{"name":"dup2","scheme":"http","host":"h.example.com","port":8080}
	]}`)
	require.Equal(t, http.StatusOK, rec.Code, "请求本身合法 ⇒ 200，冲突是逐行结果")
	got := decodeImport(t, rec)
	require.Equal(t, 0, got.Created)
	require.Equal(t, 2, got.Failed)
	for _, row := range got.Rows {
		require.NotNil(t, row.Reason)
		require.Equal(t, Conflict, *row.Reason)
		require.NotNil(t, row.Error)
	}
}

// TestSupplierProxyImport 供应商面：同一实现（wrapper 直调），归属恒为 JWT 本人；
// 私网行只让那一行失败。这条同时证明**路由**没被 `/proxies/{id}` 吃掉
// （否则会得到 405/404 或单个条目，而不是导入结果）。
func TestSupplierProxyImport(t *testing.T) {
	h := newProxySupplierHarness(t)

	rec := h.call(t, supplierUIDM4c, http.MethodPost, "/proxies/import", `{"rows":[
		{"name":"sp-pub","scheme":"socks5","host":"203.0.113.7","port":1080},
		{"name":"sp-priv","scheme":"socks5","host":"10.0.0.9","port":1080}
	]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got := decodeImport(t, rec)
	require.Equal(t, 1, got.Created)
	require.Equal(t, 1, got.Failed)
	require.True(t, got.Rows[0].Ok)
	require.NotNil(t, got.Rows[1].Reason)
	require.Equal(t, Invalid, *got.Rows[1].Reason)

	// 归属只由作用域决定：落库行属于 JWT 本人，且别人读不到。
	id := *got.Rows[0].Id
	stored, err := h.readProxyAs(t, supplierUIDM4c, id)
	require.NoError(t, err)
	require.NotNil(t, stored.OwnerUID)
	require.Equal(t, supplierUIDM4c, *stored.OwnerUID)
	_, err = h.readProxyAs(t, otherUIDM4c, id)
	require.ErrorIs(t, err, repository.ErrNotFound, "跨租户读不到（404 不泄露存在性）")
}
