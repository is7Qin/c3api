// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/handler/httpface"
)

// rulesDo 构造带 admin token 中间件的 /admin 路由，返回请求执行函数（/rules CRUD 往返）。
func rulesDo(t *testing.T) func(method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := newTestHandler(t)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler { // admin token 中间件
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "Bearer admin-tok" {
				httpface.WriteErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Mount("/", h.Router())
	return func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-tok")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
}

// TestRulesCRUD /api/admin/rules 全流程：创建 → 冲突 → 校验失败 → 列表 → 部分更新 → 删除。
func TestRulesCRUD(t *testing.T) {
	do := rulesDo(t)

	// 创建（201）
	rec := do(http.MethodPost, "/api/admin/rules", `{
		"name":"r1","priority":10,"enabled":true,
		"when":{"kind":"5xx"},
		"then":{"status":"unhealthy","cooldown":"5s"}}`)
	require.Equal(t, 201, rec.Code, "create rule: %s", rec.Body.String())
	var created Rule
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.Equal(t, "r1", created.Name)
	require.Equal(t, 10, created.Priority)
	require.Equal(t, "5xx", created.When["kind"], "when round-trip")
	require.Equal(t, "unhealthy", created.Then["status"], "then round-trip")

	// priority 冲突 → 409
	rec = do(http.MethodPost, "/api/admin/rules", `{
		"name":"r2","priority":10,
		"when":{"kind":"ok"},"then":{"status":"active"}}`)
	require.Equal(t, 409, rec.Code, "priority conflict: %s", rec.Body.String())

	// when 未知键 → 400
	rec = do(http.MethodPost, "/api/admin/rules", `{
		"name":"r3","priority":20,
		"when":{"kind":"5xx","bogus":1},
		"then":{"status":"unhealthy"}}`)
	require.Equal(t, 400, rec.Code, "unknown when key: %s", rec.Body.String())

	// then 无动作 → 400
	rec = do(http.MethodPost, "/api/admin/rules", `{
		"name":"r4","priority":21,
		"when":{"kind":"5xx"},"then":{}}`)
	require.Equal(t, 400, rec.Code, "empty then: %s", rec.Body.String())

	// 列表：priority 升序 + {total, rows}
	rec = do(http.MethodGet, "/api/admin/rules", "")
	require.Equal(t, 200, rec.Code)
	var list RuleListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(1), list.Total)
	require.Len(t, list.Rows, 1)
	require.Equal(t, 10, list.Rows[0].Priority)

	// enabled 过滤
	rec = do(http.MethodGet, "/api/admin/rules?enabled=false", "")
	require.Equal(t, 200, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(0), list.Total, "无 disabled 规则")

	// 部分更新：name 变更，when/then 保持
	rec = do(http.MethodPut, "/api/admin/rules/"+itoa(created.ID), `{"name":"r1-renamed"}`)
	require.Equal(t, 200, rec.Code, "update rule: %s", rec.Body.String())
	var updated Rule
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	require.Equal(t, "r1-renamed", updated.Name)
	require.Equal(t, "5xx", updated.When["kind"], "when 未提供保持原值")
	require.Equal(t, "5s", updated.Then["cooldown"], "then 未提供保持原值")

	// PUT 404 含 id
	rec = do(http.MethodPut, "/api/admin/rules/999", `{"name":"x"}`)
	require.Equal(t, 404, rec.Code, "update missing: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "id=999 missing")

	// 删除（204）+ 404
	rec = do(http.MethodDelete, "/api/admin/rules/"+itoa(created.ID), "")
	require.Equal(t, 204, rec.Code)
	rec = do(http.MethodDelete, "/api/admin/rules/"+itoa(created.ID), "")
	require.Equal(t, 404, rec.Code, "delete missing: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "id="+itoa(created.ID)+" missing")

	// 删除后列表为空
	rec = do(http.MethodGet, "/api/admin/rules", "")
	require.Equal(t, 200, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(0), list.Total)
}

// TestRulesDisabledToggle enabled 字段往返（缺省 true 与显式 false）。
func TestRulesDisabledToggle(t *testing.T) {
	do := rulesDo(t)

	rec := do(http.MethodPost, "/api/admin/rules", `{
		"name":"r1","priority":10,
		"when":{"kind":"ok"},"then":{"status":"active"}}`)
	require.Equal(t, 201, rec.Code)
	var created Rule
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.True(t, created.Enabled, "enabled 缺省 true")

	rec = do(http.MethodPut, "/api/admin/rules/"+itoa(created.ID), `{"enabled":false}`)
	require.Equal(t, 200, rec.Code)
	var updated Rule
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	require.False(t, updated.Enabled)

	rec = do(http.MethodGet, "/api/admin/rules?enabled=false", "")
	require.Equal(t, 200, rec.Code)
	var list RuleListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(1), list.Total, "disabled 规则按 enabled=false 过滤可见")
}
