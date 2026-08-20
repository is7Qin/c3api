// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPostRulesBatchDelete 批量删除规则：成功 / 重复 ids 去重 / 空 ids 400 / 超长 400。
func TestPostRulesBatchDelete(t *testing.T) {
	do := rulesDo(t)

	ids := make([]int64, 0, 3)
	for i := 1; i <= 3; i++ {
		rec := do(http.MethodPost, "/api/admin/rules", `{
			"name":"r`+itoa(int64(i))+`","priority":`+itoa(int64(i*10))+`,
			"when":{"kind":"5xx"},"then":{"status":"unhealthy"}}`)
		require.Equal(t, 201, rec.Code, "create rule: %s", rec.Body.String())
		var created Rule
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
		ids = append(ids, created.ID)
	}

	// 成功：删前两个 → 200 {"deleted":2}
	rec := do(http.MethodPost, "/api/admin/rules/batch-delete", `{"ids":[`+itoa(ids[0])+`,`+itoa(ids[1])+`]}`)
	require.Equal(t, 200, rec.Code, "batch delete: %s", rec.Body.String())
	var del BatchDeleteResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &del))
	require.Equal(t, 2, del.Deleted)

	// 列表确认 1、2 已删，剩 3
	rec = do(http.MethodGet, "/api/admin/rules", "")
	require.Equal(t, 200, rec.Code)
	var list RuleListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(1), list.Total, "only one rule left: %s", rec.Body.String())

	// 重复 ids 去重 → {"deleted":1}
	rec = do(http.MethodPost, "/api/admin/rules/batch-delete", `{"ids":[`+itoa(ids[2])+`,`+itoa(ids[2])+`]}`)
	require.Equal(t, 200, rec.Code, "dup ids: %s", rec.Body.String())
	var del2 BatchDeleteResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &del2))
	require.Equal(t, 1, del2.Deleted)

	// 空 ids → 400
	rec = do(http.MethodPost, "/api/admin/rules/batch-delete", `{"ids":[]}`)
	require.Equal(t, 400, rec.Code, "empty ids: %s", rec.Body.String())

	// 超长（101 条）→ 400
	over := make([]int64, 101)
	for i := range over {
		over[i] = int64(i + 1)
	}
	body, err := json.Marshal(map[string]any{"ids": over})
	require.NoError(t, err)
	rec = do(http.MethodPost, "/api/admin/rules/batch-delete", string(body))
	require.Equal(t, 400, rec.Code, "overlong ids: %s", rec.Body.String())
}

// TestPostRulesBatchDeleteMissing 缺 id → 404，响应含缺失 id。
func TestPostRulesBatchDeleteMissing(t *testing.T) {
	do := rulesDo(t)

	rec := do(http.MethodPost, "/api/admin/rules/batch-delete", `{"ids":[999]}`)
	require.Equal(t, 404, rec.Code, "missing id: %s", rec.Body.String())
	var errBody map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errBody))
	require.Contains(t, errBody["error"], "999", "404 must carry missing id: %s", rec.Body.String())
}
