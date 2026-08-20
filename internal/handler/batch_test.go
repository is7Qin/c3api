// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestPostTemplatesBatchDelete 批量删除：成功 / 空 ids 400 / 超长 400 /
// 重复 ids 去重。
func TestPostTemplatesBatchDelete(t *testing.T) {
	_, _, do := newListTestRouter(t)

	for _, name := range []string{"t1", "t2", "t3"} {
		rec := do(http.MethodPost, "/api/admin/templates", `{"name":"`+name+`","base_url":"https://api.openai.com","supported_formats":["openai-chat"]}`)
		require.Equal(t, 200, rec.Code, "create %s: %s", name, rec.Body.String())
	}

	// 成功：{"ids":[1,2]} → 200 {"deleted":2}
	rec := do(http.MethodPost, "/api/admin/templates/batch-delete", `{"ids":[1,2]}`)
	require.Equal(t, 200, rec.Code, "batch delete: %s", rec.Body.String())
	var del BatchDeleteResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &del))
	require.Equal(t, 2, del.Deleted)

	// 列表确认 1、2 已删，剩 3
	rec = do(http.MethodGet, "/api/admin/templates", "")
	require.Equal(t, 200, rec.Code)
	var list TemplateListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(1), list.Total, "only template 3 left: %s", rec.Body.String())

	// 空 ids → 400
	rec = do(http.MethodPost, "/api/admin/templates/batch-delete", `{"ids":[]}`)
	require.Equal(t, 400, rec.Code, "empty ids: %s", rec.Body.String())

	// 超长（101 条）→ 400
	ids := make([]int64, 101)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	body, err := json.Marshal(map[string]any{"ids": ids})
	require.NoError(t, err)
	rec = do(http.MethodPost, "/api/admin/templates/batch-delete", string(body))
	require.Equal(t, 400, rec.Code, "overlong ids: %s", rec.Body.String())

	// 重复 ids 去重 → {"deleted":1}
	rec = do(http.MethodPost, "/api/admin/templates/batch-delete", `{"ids":[3,3,3]}`)
	require.Equal(t, 200, rec.Code, "dup ids: %s", rec.Body.String())
	var del2 BatchDeleteResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &del2))
	require.Equal(t, 1, del2.Deleted)
}

// TestPostTemplatesBatchDeleteMissing 缺 id → 404，响应含缺失 id。
func TestPostTemplatesBatchDeleteMissing(t *testing.T) {
	_, _, do := newListTestRouter(t)

	rec := do(http.MethodPost, "/api/admin/templates/batch-delete", `{"ids":[999]}`)
	require.Equal(t, 404, rec.Code, "missing id: %s", rec.Body.String())
	var errBody map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
	require.Contains(t, errBody["error"], "999", "404 must carry missing id: %s", rec.Body.String())
}

// TestPostTemplatesBatchUpdate 批量更新：成功（改名 + base_url 生效）/
// 非法 supported_formats 400 / 空 fields 400。
func TestPostTemplatesBatchUpdate(t *testing.T) {
	_, _, do := newListTestRouter(t)

	rec := do(http.MethodPost, "/api/admin/templates", `{"name":"t1","base_url":"https://api.openai.com","supported_formats":["openai-chat"]}`)
	require.Equal(t, 200, rec.Code, "create: %s", rec.Body.String())

	// 成功：改名 + base_url → 200 {"updated":1}
	rec = do(http.MethodPost, "/api/admin/templates/batch-update", `{"ids":[1],"fields":{"name":"t1-v2","base_url":"https://api.openai.com/v2"}}`)
	require.Equal(t, 200, rec.Code, "batch update: %s", rec.Body.String())
	var up BatchUpdateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &up))
	require.Equal(t, 1, up.Updated)

	// GET 确认已生效
	rec = do(http.MethodGet, "/api/admin/templates/1", "")
	require.Equal(t, 200, rec.Code, "get: %s", rec.Body.String())
	var tpl domain.Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))
	require.Equal(t, "t1-v2", tpl.Name)
	require.Equal(t, "https://api.openai.com/v2", tpl.BaseURL)

	// 非法 supported_formats → 400（service validateTemplatePatch）
	rec = do(http.MethodPost, "/api/admin/templates/batch-update", `{"ids":[1],"fields":{"supported_formats":["bogus"]}}`)
	require.Equal(t, 400, rec.Code, "invalid supported_formats: %s", rec.Body.String())

	// 空 fields → 400
	rec = do(http.MethodPost, "/api/admin/templates/batch-update", `{"ids":[1],"fields":{}}`)
	require.Equal(t, 400, rec.Code, "empty fields: %s", rec.Body.String())
}

// TestPostAccountsBatchUpdate 批量更新账号：成功（status=disabled 生效）/
// group_ids 提供（替换）/ 空数组（清空）/ null（不变）/ 非法 status 400 /
// 空 fields 400 / 缺 id 404。
func TestPostAccountsBatchUpdate(t *testing.T) {
	_, _, do := newListTestRouter(t)

	rec := do(http.MethodPost, "/api/admin/templates", `{"name":"t1","base_url":"https://api.openai.com","supported_formats":["openai-chat"]}`)
	require.Equal(t, 200, rec.Code, "create template: %s", rec.Body.String())
	ids := make([]int64, 0, 2)
	for i := 1; i <= 2; i++ {
		rec = do(http.MethodPost, "/api/admin/accounts", `{"name":"acc`+itoa(int64(i))+`","template_id":1,"upstream_key":"sk-x"}`)
		require.Equal(t, 200, rec.Code, "create account: %s", rec.Body.String())
		var created domain.Account
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
		ids = append(ids, created.ID)
	}
	rec = do(http.MethodPost, "/api/admin/groups", `{"name":"g1"}`)
	require.Equal(t, 200, rec.Code, "create group: %s", rec.Body.String())
	var groupResp domain.Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groupResp))
	gID := groupResp.ID

	// 成功：{"ids":[...],"fields":{"status":"disabled"}} → 200 {"updated":2}
	rec = do(http.MethodPost, "/api/admin/accounts/batch-update",
		`{"ids":[`+itoa(ids[0])+`,`+itoa(ids[1])+`],"fields":{"status":"disabled"}}`)
	require.Equal(t, 200, rec.Code, "batch update: %s", rec.Body.String())
	var up BatchUpdateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &up))
	require.Equal(t, 2, up.Updated)

	// GET 确认 status 已生效
	rec = do(http.MethodGet, "/api/admin/accounts/"+itoa(ids[0]), "")
	require.Equal(t, 200, rec.Code, "get: %s", rec.Body.String())
	var acc domain.Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))
	require.Equal(t, domain.StatusDisabled, acc.Status)

	// group_ids 提供 → 替换生效（回显核对）
	rec = do(http.MethodPost, "/api/admin/accounts/batch-update",
		`{"ids":[`+itoa(ids[0])+`,`+itoa(ids[1])+`],"fields":{"group_ids":[`+itoa(gID)+`]}}`)
	require.Equal(t, 200, rec.Code, "batch set groups: %s", rec.Body.String())
	rec = do(http.MethodGet, "/api/admin/accounts/"+itoa(ids[0])+"/groups", "")
	require.Equal(t, 200, rec.Code, "get groups: %s", rec.Body.String())
	var ag AccountGroupsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ag))
	require.Equal(t, []int64{gID}, ag.GroupIds, "批量 group_ids 替换生效")

	// group_ids: [] → 清空
	rec = do(http.MethodPost, "/api/admin/accounts/batch-update",
		`{"ids":[`+itoa(ids[0])+`],"fields":{"group_ids":[]}}`)
	require.Equal(t, 200, rec.Code, "batch clear groups: %s", rec.Body.String())
	rec = do(http.MethodGet, "/api/admin/accounts/"+itoa(ids[0])+"/groups", "")
	require.Equal(t, 200, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ag))
	require.Empty(t, ag.GroupIds, "[] = 清空")

	// group_ids: null → 不变（先塞回 g1，再 null 不动；null 不算提供字段，
	// 需带其他字段才非空 fields）
	rec = do(http.MethodPost, "/api/admin/accounts/batch-update",
		`{"ids":[`+itoa(ids[0])+`],"fields":{"group_ids":[`+itoa(gID)+`]}}`)
	require.Equal(t, 200, rec.Code, "re-set groups: %s", rec.Body.String())
	rec = do(http.MethodPost, "/api/admin/accounts/batch-update",
		`{"ids":[`+itoa(ids[0])+`],"fields":{"name":"acc1","group_ids":null}}`)
	require.Equal(t, 200, rec.Code, "null group_ids: %s", rec.Body.String())
	rec = do(http.MethodGet, "/api/admin/accounts/"+itoa(ids[0])+"/groups", "")
	require.Equal(t, 200, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ag))
	require.Equal(t, []int64{gID}, ag.GroupIds, "null = 不变")

	// 非法 status 枚举 → 400（handler 显式校验）
	rec = do(http.MethodPost, "/api/admin/accounts/batch-update",
		`{"ids":[`+itoa(ids[0])+`],"fields":{"status":"bogus"}}`)
	require.Equal(t, 400, rec.Code, "invalid status: %s", rec.Body.String())

	// 空 fields → 400
	rec = do(http.MethodPost, "/api/admin/accounts/batch-update",
		`{"ids":[`+itoa(ids[0])+`],"fields":{}}`)
	require.Equal(t, 400, rec.Code, "empty fields: %s", rec.Body.String())

	// 全 null fields → 400（null 不算提供）
	rec = do(http.MethodPost, "/api/admin/accounts/batch-update",
		`{"ids":[`+itoa(ids[0])+`],"fields":{"name":null,"group_ids":null}}`)
	require.Equal(t, 400, rec.Code, "all-null fields: %s", rec.Body.String())

	// 缺 id → 404
	rec = do(http.MethodPost, "/api/admin/accounts/batch-update", `{"ids":[999],"fields":{"name":"x"}}`)
	require.Equal(t, 404, rec.Code, "missing id: %s", rec.Body.String())
}

// TestAccountGroupsCreateUpdate 单账号创建/更新带 group_ids（替换语义映射）：
// 创建带分组 → 回显；PUT 替换/清空/不变；GET /accounts/{id}/groups 404。
func TestAccountGroupsCreateUpdate(t *testing.T) {
	_, _, do := newListTestRouter(t)

	rec := do(http.MethodPost, "/api/admin/templates", `{"name":"t1","base_url":"https://api.openai.com","supported_formats":["openai-chat"]}`)
	require.Equal(t, 200, rec.Code, "create template: %s", rec.Body.String())
	rec = do(http.MethodPost, "/api/admin/groups", `{"name":"g1"}`)
	require.Equal(t, 200, rec.Code, "create group: %s", rec.Body.String())
	var g domain.Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
	// 第二个组（验证替换语义「只留所选」）
	rec = do(http.MethodPost, "/api/admin/groups", `{"name":"g2"}`)
	require.Equal(t, 200, rec.Code, "create group2: %s", rec.Body.String())
	var g2 domain.Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g2))

	// 创建带分组
	rec = do(http.MethodPost, "/api/admin/accounts",
		`{"name":"a1","template_id":1,"upstream_key":"sk-x","group_ids":[`+itoa(g.ID)+`,`+itoa(g2.ID)+`]}`)
	require.Equal(t, 200, rec.Code, "create with groups: %s", rec.Body.String())
	var acc domain.Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))
	rec = do(http.MethodGet, "/api/admin/accounts/"+itoa(acc.ID)+"/groups", "")
	require.Equal(t, 200, rec.Code, "echo: %s", rec.Body.String())
	var ag AccountGroupsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ag))
	require.ElementsMatch(t, []int64{g.ID, g2.ID}, ag.GroupIds, "创建带分组生效")

	// 创建不带分组 → 无分组
	rec = do(http.MethodPost, "/api/admin/accounts", `{"name":"a2","template_id":1,"upstream_key":"sk-x"}`)
	require.Equal(t, 200, rec.Code)
	var acc2 domain.Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc2))
	rec = do(http.MethodGet, "/api/admin/accounts/"+itoa(acc2.ID)+"/groups", "")
	require.Equal(t, 200, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ag))
	require.Empty(t, ag.GroupIds, "创建不带 group_ids = 无分组")

	// PUT 替换：只留 g1（g2 被移除）
	rec = do(http.MethodPut, "/api/admin/accounts/"+itoa(acc.ID),
		`{"name":"a1","template_id":1,"upstream_key":"sk-x","group_ids":[`+itoa(g.ID)+`]}`)
	require.Equal(t, 200, rec.Code, "put replace: %s", rec.Body.String())
	rec = do(http.MethodGet, "/api/admin/accounts/"+itoa(acc.ID)+"/groups", "")
	require.Equal(t, 200, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ag))
	require.Equal(t, []int64{g.ID}, ag.GroupIds, "PUT 替换 = 只留所选")

	// PUT 清空（[]）
	rec = do(http.MethodPut, "/api/admin/accounts/"+itoa(acc.ID), `{"name":"a1","template_id":1,"upstream_key":"sk-x","group_ids":[]}`)
	require.Equal(t, 200, rec.Code, "put clear: %s", rec.Body.String())
	rec = do(http.MethodGet, "/api/admin/accounts/"+itoa(acc.ID)+"/groups", "")
	require.Equal(t, 200, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ag))
	require.Empty(t, ag.GroupIds, "PUT [] = 清空")

	// PUT 缺省 group_ids = 不变（仍为空）
	rec = do(http.MethodPut, "/api/admin/accounts/"+itoa(acc.ID), `{"name":"a1","template_id":1,"upstream_key":"sk-x"}`)
	require.Equal(t, 200, rec.Code, "put without group_ids: %s", rec.Body.String())
	rec = do(http.MethodGet, "/api/admin/accounts/"+itoa(acc.ID)+"/groups", "")
	require.Equal(t, 200, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ag))
	require.Empty(t, ag.GroupIds, "PUT 缺省 = 不变")

	// 创建带缺失组 → 404 含 id
	rec = do(http.MethodPost, "/api/admin/accounts", `{"name":"a3","template_id":1,"upstream_key":"sk-x","group_ids":[999]}`)
	require.Equal(t, 404, rec.Code, "create missing group: %s", rec.Body.String())

	// GET /accounts/{id}/groups 缺账号 → 404
	rec = do(http.MethodGet, "/api/admin/accounts/999/groups", "")
	require.Equal(t, 404, rec.Code, "missing account groups: %s", rec.Body.String())
}

// TestPostGroupsBatchDeleteMissing 批量删除分组缺 id → 404（service 先
// GetGroup 逐 id 存在性检查）。
func TestPostGroupsBatchDeleteMissing(t *testing.T) {
	_, _, do := newListTestRouter(t)

	rec := do(http.MethodPost, "/api/admin/groups/batch-delete", `{"ids":[999]}`)
	require.Equal(t, 404, rec.Code, "missing id: %s", rec.Body.String())
}

// TestPostAccountsBatchDelete 批量删除账号：成功 / 重复 ids 去重 / 缺 id 404。
func TestPostAccountsBatchDelete(t *testing.T) {
	_, _, do := newListTestRouter(t)

	rec := do(http.MethodPost, "/api/admin/templates", `{"name":"t1","base_url":"https://api.openai.com","supported_formats":["openai-chat"]}`)
	require.Equal(t, 200, rec.Code, "create template: %s", rec.Body.String())
	ids := make([]int64, 0, 3)
	for i := 1; i <= 3; i++ {
		rec = do(http.MethodPost, "/api/admin/accounts", `{"name":"acc`+itoa(int64(i))+`","template_id":1,"upstream_key":"sk-x"}`)
		require.Equal(t, 200, rec.Code, "create account: %s", rec.Body.String())
		var created domain.Account
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
		ids = append(ids, created.ID)
	}

	// 成功：删除前两个 → 200 {"deleted":2}
	rec = do(http.MethodPost, "/api/admin/accounts/batch-delete", `{"ids":[`+itoa(ids[0])+`,`+itoa(ids[1])+`]}`)
	require.Equal(t, 200, rec.Code, "batch delete: %s", rec.Body.String())
	var del BatchDeleteResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &del))
	require.Equal(t, 2, del.Deleted)

	// 重复 ids 去重 → {"deleted":1}
	rec = do(http.MethodPost, "/api/admin/accounts/batch-delete", `{"ids":[`+itoa(ids[2])+`,`+itoa(ids[2])+`]}`)
	require.Equal(t, 200, rec.Code, "dup ids: %s", rec.Body.String())
	var del2 BatchDeleteResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &del2))
	require.Equal(t, 1, del2.Deleted)

	// 缺 id → 404，响应含缺失 id
	rec = do(http.MethodPost, "/api/admin/accounts/batch-delete", `{"ids":[999]}`)
	require.Equal(t, 404, rec.Code, "missing id: %s", rec.Body.String())
	var errBody map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
	require.Contains(t, errBody["error"], "999", "404 must carry missing id: %s", rec.Body.String())
}

// TestPostGroupsBatchDelete 批量删除分组：成功（service 先 GetGroup 逐 id
// 前置检查，再事务批量删）/ 缺 id 404。
func TestPostGroupsBatchDelete(t *testing.T) {
	_, _, do := newListTestRouter(t)

	ids := make([]int64, 0, 2)
	for i := 1; i <= 2; i++ {
		rec := do(http.MethodPost, "/api/admin/groups", `{"name":"g`+itoa(int64(i))+`"}`)
		require.Equal(t, 200, rec.Code, "create group: %s", rec.Body.String())
		// Phase 3a：创建响应为 Group 本体（无 key 字段）
		var created domain.Group
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
		ids = append(ids, created.ID)
	}

	// 成功 → 200 {"deleted":2}
	rec := do(http.MethodPost, "/api/admin/groups/batch-delete", `{"ids":[`+itoa(ids[0])+`,`+itoa(ids[1])+`]}`)
	require.Equal(t, 200, rec.Code, "batch delete: %s", rec.Body.String())
	var del BatchDeleteResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &del))
	require.Equal(t, 2, del.Deleted)
}

// TestPostGroupsBatchUpdate 批量更新分组：成功（改名生效）/ 空 fields 400。
func TestPostGroupsBatchUpdate(t *testing.T) {
	_, _, do := newListTestRouter(t)

	rec := do(http.MethodPost, "/api/admin/groups", `{"name":"g1"}`)
	require.Equal(t, 200, rec.Code, "create: %s", rec.Body.String())

	// 成功：改名 → 200 {"updated":1}
	rec = do(http.MethodPost, "/api/admin/groups/batch-update", `{"ids":[1],"fields":{"name":"g1-v2"}}`)
	require.Equal(t, 200, rec.Code, "batch update: %s", rec.Body.String())
	var up BatchUpdateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &up))
	require.Equal(t, 1, up.Updated)

	// GET 确认已生效
	rec = do(http.MethodGet, "/api/admin/groups/1", "")
	require.Equal(t, 200, rec.Code, "get: %s", rec.Body.String())
	var g domain.Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
	require.Equal(t, "g1-v2", g.Name)

	// 空 fields → 400
	rec = do(http.MethodPost, "/api/admin/groups/batch-update", `{"ids":[1],"fields":{}}`)
	require.Equal(t, 400, rec.Code, "empty fields: %s", rec.Body.String())
}
