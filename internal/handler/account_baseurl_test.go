// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestAccountBaseURLContract 账号级 base_url 契约面（C3 响应面）：
// create/update 透传；create/update 路径空串归一 null（不落库）；
// 列表响应含 base_url（toAPIAccountView 平铺逐字段拷贝——缺则编辑回显
// 恒缺字段，前端保存静默清空）。
func TestAccountBaseURLContract(t *testing.T) {
	_, _, do := newListTestRouter(t)

	rec := do(http.MethodPost, "/api/admin/templates", `{"name":"t1","base_url":"https://api.openai.com","supported_formats":["openai-chat"]}`)
	require.Equal(t, 200, rec.Code, "create template: %s", rec.Body.String())

	// 创建带 base_url → 响应回显
	rec = do(http.MethodPost, "/api/admin/accounts", `{"name":"acc1","template_id":1,"upstream_key":"sk-x","base_url":"https://acc.example.com"}`)
	require.Equal(t, 200, rec.Code, "create account: %s", rec.Body.String())
	var created domain.Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.NotNil(t, created.BaseURL)
	require.Equal(t, "https://acc.example.com", *created.BaseURL)

	// PUT 全量替换带 base_url → 生效
	rec = do(http.MethodPut, "/api/admin/accounts/"+strconv.FormatInt(created.ID, 10),
		`{"name":"acc1","template_id":1,"upstream_key":"sk-x","base_url":"https://acc2.example.com"}`)
	require.Equal(t, 200, rec.Code, "update: %s", rec.Body.String())
	var updated domain.Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	require.NotNil(t, updated.BaseURL)
	require.Equal(t, "https://acc2.example.com", *updated.BaseURL)

	// 列表响应含 base_url（C3——toAPIAccountView 平铺拷贝）
	rec = do(http.MethodGet, "/api/admin/accounts", "")
	require.Equal(t, 200, rec.Code)
	var list AccountListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.NotEmpty(t, list.Rows)
	require.NotNil(t, list.Rows[0].BaseURL, "列表响应必须含 base_url（toAPIAccountView 平铺）")
	require.Equal(t, "https://acc2.example.com", *list.Rows[0].BaseURL)

	// create 路径空串归一 null（"" 与 null 合并为「继承模板」，防 "" 落库）
	rec = do(http.MethodPost, "/api/admin/accounts", `{"name":"acc2","template_id":1,"upstream_key":"sk-y","base_url":""}`)
	require.Equal(t, 200, rec.Code, "create with empty base_url: %s", rec.Body.String())
	var created2 domain.Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created2))
	require.Nil(t, created2.BaseURL, "create 空串 → 归一 null（继承模板）")

	// PUT 空串归一 null
	rec = do(http.MethodPut, "/api/admin/accounts/"+strconv.FormatInt(created.ID, 10),
		`{"name":"acc1","template_id":1,"upstream_key":"sk-x","base_url":""}`)
	require.Equal(t, 200, rec.Code, "update with empty base_url: %s", rec.Body.String())
	var updated2 domain.Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated2))
	require.Nil(t, updated2.BaseURL, "update 空串 → 归一 null（继承模板）")
}

// TestPostAccountsBatchUpdateBaseURL 批量 patch 的 base_url 三态（C1）：
// 非空 → 落值；"" → 清空（透传不归一——与 create 路径语义分写）；
// 只带 base_url 的 patch 合法（空 fields 判定含 BaseURL）。
func TestPostAccountsBatchUpdateBaseURL(t *testing.T) {
	_, _, do := newListTestRouter(t)

	rec := do(http.MethodPost, "/api/admin/templates", `{"name":"t1","base_url":"https://api.openai.com","supported_formats":["openai-chat"]}`)
	require.Equal(t, 200, rec.Code, "create template: %s", rec.Body.String())
	rec = do(http.MethodPost, "/api/admin/accounts", `{"name":"acc1","template_id":1,"upstream_key":"sk-x"}`)
	require.Equal(t, 200, rec.Code, "create account: %s", rec.Body.String())
	var created domain.Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	idStr := strconv.FormatInt(created.ID, 10)

	// 只带 base_url 的 patch 合法（空 fields 判定：BaseURL 提供算「有字段」）
	rec = do(http.MethodPost, "/api/admin/accounts/batch-update", `{"ids":[`+idStr+`],"fields":{"base_url":"https://batch.example.com"}}`)
	require.Equal(t, 200, rec.Code, "patch with only base_url must be legal: %s", rec.Body.String())
	var up BatchUpdateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &up))
	require.Equal(t, 1, up.Updated)

	// 非空 → 落值（GET 确认）
	rec = do(http.MethodGet, "/api/admin/accounts/"+idStr, "")
	require.Equal(t, 200, rec.Code)
	var got domain.Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.NotNil(t, got.BaseURL)
	require.Equal(t, "https://batch.example.com", *got.BaseURL)

	// "" → 清空（透传不归一；空串 = 清空语义）
	rec = do(http.MethodPost, "/api/admin/accounts/batch-update", `{"ids":[`+idStr+`],"fields":{"base_url":""}}`)
	require.Equal(t, 200, rec.Code, "batch clear base_url: %s", rec.Body.String())
	rec = do(http.MethodGet, "/api/admin/accounts/"+idStr, "")
	require.Equal(t, 200, rec.Code)
	var cleared domain.Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cleared))
	require.Nil(t, cleared.BaseURL, "批量空串 → 清空（落 NULL = 继承模板）")

	// 非法 base_url（无 scheme）→ 400（validateAccountPatch 复用 validateBaseURL）
	rec = do(http.MethodPost, "/api/admin/accounts/batch-update", `{"ids":[`+idStr+`],"fields":{"base_url":"no-scheme"}}`)
	require.Equal(t, 400, rec.Code, "invalid base_url: %s", rec.Body.String())
}
