// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestAccountWriteTriState 三态写语义表：可空标量 null = 清空、缺席 = 不变；
// 集合 [] = 清空、null = 不变；不可空标量 null → 400；可空标量 "" → 400；
// group_ids 元素规则（重复 / <=0 → 400）在 PATCH 与 CREATE 两面一致。
func TestAccountWriteTriState(t *testing.T) {
	_, _, do := newListTestRouter(t)

	rec := do(http.MethodPost, "/api/admin/templates", `{"name":"t1","base_url":"https://api.openai.com","supported_formats":["openai-chat"]}`)
	require.Equal(t, 200, rec.Code, "create template: %s", rec.Body.String())
	rec = do(http.MethodPost, "/api/admin/groups", `{"name":"g1"}`)
	require.Equal(t, 200, rec.Code, "create group: %s", rec.Body.String())
	var groupResp domain.Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groupResp))
	gID := strconv.FormatInt(groupResp.ID, 10)

	// 基准账号：带 base_url + 分组
	rec = do(http.MethodPost, "/api/admin/accounts",
		`{"name":"acc1","template_id":1,"upstream_key":"sk-x","base_url":"https://acc.example.com","group_ids":[`+gID+`]}`)
	require.Equal(t, 200, rec.Code, "create account: %s", rec.Body.String())
	var created domain.Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	idStr := strconv.FormatInt(created.ID, 10)
	rev := created.LifecycleRevision

	get := func() domain.Account {
		rec := do(http.MethodGet, "/api/admin/accounts/"+idStr, "")
		require.Equal(t, 200, rec.Code)
		var got domain.Account
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		return got
	}
	getGroups := func() []int64 {
		rec := do(http.MethodGet, "/api/admin/accounts/"+idStr+"/groups", "")
		require.Equal(t, 200, rec.Code)
		var ag AccountGroupsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ag))
		return ag.GroupIds
	}

	// base_url: null → 清空
	rec = do(http.MethodPatch, "/api/admin/accounts/"+idStr, `{"base_url":null,"name":"acc1"}`)
	require.Equal(t, 200, rec.Code, "null clears: %s", rec.Body.String())
	require.Nil(t, get().BaseURL)

	// base_url 缺席 → 不变（先落值，再缺席补丁确认保持）
	rec = do(http.MethodPatch, "/api/admin/accounts/"+idStr, `{"base_url":"https://keep.example.com"}`)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = do(http.MethodPatch, "/api/admin/accounts/"+idStr, `{"name":"renamed"}`)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	got := get()
	require.Equal(t, "renamed", got.Name)
	require.NotNil(t, got.BaseURL)
	require.Equal(t, "https://keep.example.com", *got.BaseURL, "缺席 = 不变")
	require.Equal(t, rev+3, got.LifecycleRevision)

	// group_ids: [] → 清空
	rec = do(http.MethodPatch, "/api/admin/accounts/"+idStr, `{"group_ids":[]}`)
	require.Equal(t, 200, rec.Code, "empty clears: %s", rec.Body.String())
	require.Empty(t, getGroups())

	// group_ids: null → 不变（先塞回，再 null 确认保持；需带其他字段凑非空补丁）
	rec = do(http.MethodPatch, "/api/admin/accounts/"+idStr, `{"group_ids":[`+gID+`]}`)
	require.Equal(t, 200, rec.Code, rec.Body.String())
	rec = do(http.MethodPatch, "/api/admin/accounts/"+idStr, `{"name":"renamed2","group_ids":null}`)
	require.Equal(t, 200, rec.Code, "null group_ids: %s", rec.Body.String())
	require.Equal(t, []int64{groupResp.ID}, getGroups(), "null = 不变")

	// 不可空标量 null → 400（具名字段）
	for _, body := range []string{
		`{"name":null}`, `{"template_id":null}`, `{"upstream_key":null}`,
		`{"max_concurrency":null}`, `{"enabled":null}`, `{"upstream_cost_multiplier":null}`,
	} {
		rec = do(http.MethodPatch, "/api/admin/accounts/"+idStr, body)
		require.Equal(t, 400, rec.Code, "%s must 400: %s", body, rec.Body.String())
	}

	// 可空标量 "" → 400
	for _, body := range []string{`{"base_url":""}`, `{"cache_domain":""}`} {
		rec = do(http.MethodPatch, "/api/admin/accounts/"+idStr, body)
		require.Equal(t, 400, rec.Code, "%s must 400: %s", body, rec.Body.String())
	}

	// group_ids 元素规则（PATCH 面）：重复 / <=0 → 400
	for _, body := range []string{
		`{"group_ids":[` + gID + `,` + gID + `]}`,
		`{"group_ids":[0]}`, `{"group_ids":[-1]}`,
	} {
		rec = do(http.MethodPatch, "/api/admin/accounts/"+idStr, body)
		require.Equal(t, 400, rec.Code, "%s must 400: %s", body, rec.Body.String())
	}

	// group_ids 元素规则（CREATE 面与 PATCH 一致）
	for _, body := range []string{
		`{"name":"dup","template_id":1,"upstream_key":"sk-x","group_ids":[` + gID + `,` + gID + `]}`,
		`{"name":"zero","template_id":1,"upstream_key":"sk-x","group_ids":[0]}`,
	} {
		rec = do(http.MethodPost, "/api/admin/accounts", body)
		require.Equal(t, 400, rec.Code, "%s must 400: %s", body, rec.Body.String())
	}
}

// TestAccountPatchIfMatch If-Match 前置条件：匹配 → 生效；陈旧 → 412；缺席 →
// 生效；语法非法（W/、多值、*、非整数）→ 400。
func TestAccountPatchIfMatch(t *testing.T) {
	h, store, _, _ := newLifecycleTestHandler(t)
	r := newIfMatchRouter(h)

	do := func(id, body, ifMatch string, setHeader bool) *httptest.ResponseRecorder {
		return doPatch(r, id, body, ifMatch, setHeader)
	}

	// 缺席 → 生效
	rec := do("1", `{"name":"no-fence"}`, "", false)
	require.Equal(t, 200, rec.Code, "absent If-Match applies: %s", rec.Body.String())
	require.Equal(t, int64(6), store.accs[1].LifecycleRevision)

	// 匹配 → 生效
	rec = do("1", `{"name":"fenced"}`, "6", true)
	require.Equal(t, 200, rec.Code, "matching If-Match applies: %s", rec.Body.String())
	require.Equal(t, int64(7), store.accs[1].LifecycleRevision)

	// 引号包裹 → 生效
	rec = do("1", `{"name":"quoted"}`, `"7"`, true)
	require.Equal(t, 200, rec.Code, "quoted If-Match applies: %s", rec.Body.String())

	// 陈旧 → 412（无状态变更）
	rec = do("1", `{"name":"stale"}`, "7", true)
	require.Equal(t, 412, rec.Code, "stale If-Match must 412: %s", rec.Body.String())
	require.Equal(t, "quoted", store.accs[1].Name, "412 不得变更")

	// 语法非法 → 400
	for _, v := range []string{`W/"8"`, "8,9", "*", "abc", ""} {
		rec = do("1", `{"name":"x"}`, v, true)
		require.Equal(t, 400, rec.Code, "If-Match %q must 400: %s", v, rec.Body.String())
	}
}

// TestAccountBatchUpdateRevisions 批量响应携带每账号的新配置代际。
func TestAccountBatchUpdateRevisions(t *testing.T) {
	_, _, do := newListTestRouter(t)

	rec := do(http.MethodPost, "/api/admin/templates", `{"name":"t1","base_url":"https://api.openai.com","supported_formats":["openai-chat"]}`)
	require.Equal(t, 200, rec.Code, "create template: %s", rec.Body.String())
	var ids []int64
	for _, name := range []string{"b1", "b2"} {
		rec = do(http.MethodPost, "/api/admin/accounts", `{"name":"`+name+`","template_id":1,"upstream_key":"sk-x"}`)
		require.Equal(t, 200, rec.Code, "create %s: %s", name, rec.Body.String())
		var created domain.Account
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
		ids = append(ids, created.ID)
	}

	rec = do(http.MethodPost, "/api/admin/accounts/batch-update",
		`{"ids":[`+strconv.FormatInt(ids[0], 10)+`,`+strconv.FormatInt(ids[1], 10)+`],"fields":{"max_concurrency":3}}`)
	require.Equal(t, 200, rec.Code, "batch update: %s", rec.Body.String())
	var up AccountBatchUpdateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &up))
	require.Equal(t, 2, up.Updated)
	require.Len(t, up.Items, 2, "响应携带每账号代际")
	byID := map[int64]int64{}
	for _, item := range up.Items {
		byID[item.AccountId] = item.LifecycleRevision
	}
	for _, id := range ids {
		rev, ok := byID[id]
		require.True(t, ok, "account %d must be present", id)
		require.Equal(t, int64(2), rev, "创建 rev=1 → 批量写后 rev=2")
		rec := do(http.MethodGet, "/api/admin/accounts/"+strconv.FormatInt(id, 10), "")
		require.Equal(t, 200, rec.Code)
		var got domain.Account
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		require.Equal(t, rev, got.LifecycleRevision, "回显代际与响应一致")
	}
}

// TestAccountCreateEnabledFalseAndFilter 显式 enabled:false 在创建时生效；
// enabled 列表过滤只返回匹配行。
func TestAccountCreateEnabledFalseAndFilter(t *testing.T) {
	hh, _, _ := newEnabledFilterRouter(t)

	rec := hh.do(http.MethodPost, "/api/admin/accounts", `{"name":"on","template_id":1,"upstream_key":"sk-x"}`)
	require.Equal(t, 200, rec.Code, "create default: %s", rec.Body.String())
	var on domain.Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &on))
	require.True(t, on.Enabled, "创建默认启用")

	rec = hh.do(http.MethodPost, "/api/admin/accounts", `{"name":"off","template_id":1,"upstream_key":"sk-x","enabled":false}`)
	require.Equal(t, 200, rec.Code, "create disabled: %s", rec.Body.String())
	var off domain.Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &off))
	require.False(t, off.Enabled, "显式 enabled:false 生效")

	rec = hh.do(http.MethodGet, "/api/admin/accounts?enabled=true", "")
	require.Equal(t, 200, rec.Code)
	var list AccountListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Rows, 1)
	require.Equal(t, "on", *list.Rows[0].Name)

	rec = hh.do(http.MethodGet, "/api/admin/accounts?enabled=false", "")
	require.Equal(t, 200, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Rows, 1)
	require.Equal(t, "off", *list.Rows[0].Name)

	rec = hh.do(http.MethodGet, "/api/admin/accounts", "")
	require.Equal(t, 200, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Rows, 2, "缺席 = 不过滤")
}

// TestAccountCreateDefaults 创建写时默认全集。
func TestAccountCreateDefaults(t *testing.T) {
	svc := serviceForDefaults(t)

	acc, err := createDefaultAccount(svc)
	require.NoError(t, err)
	require.True(t, acc.Enabled, "enabled=true")
	require.Equal(t, 10000, domain.MultBp(acc.UpstreamCostMultiplierBp), "upstream_cost_multiplier=1 → 10000bp")
	require.Nil(t, acc.CacheDomain, "cache_domain=NULL")
	require.Nil(t, acc.BaseURL, "base_url=NULL")
	require.Equal(t, 7, acc.MaxConcurrency, "max_concurrency = 注入默认")
	require.Equal(t, int64(1), acc.LifecycleRevision, "lifecycle_revision=1")
	require.Equal(t, int64(1), acc.IdentityRevision, "identity_revision=1")
	groups, err := svc.GetAccountGroups(context.Background(), acc.ID)
	require.NoError(t, err)
	require.Empty(t, groups, "无分组")
}

// TestAccountResponseExposesGenerations 账号读面必须回显两个代际：C
// （lifecycle_revision，客户端 If-Match 令牌）与 K（identity_revision，身份代际）。
// 二者缺一，前端就无法做乐观并发控制、也无法观测身份写入。
func TestAccountResponseExposesGenerations(t *testing.T) {
	_, _, do := newListTestRouter(t)

	rec := do(http.MethodPost, "/api/admin/templates", `{"name":"t1","base_url":"https://api.openai.com","supported_formats":["openai-chat"]}`)
	require.Equal(t, 200, rec.Code, "create template: %s", rec.Body.String())
	rec = do(http.MethodPost, "/api/admin/accounts", `{"name":"acc1","template_id":1,"upstream_key":"sk-x"}`)
	require.Equal(t, 200, rec.Code, "create account: %s", rec.Body.String())
	var created Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.NotNil(t, created.LifecycleRevision)
	require.Equal(t, int64(1), *created.LifecycleRevision, "创建回显 C=1")
	require.NotNil(t, created.IdentityRevision, "响应必须回显 K")
	require.Equal(t, int64(1), *created.IdentityRevision, "创建回显 K=1")

	rec = do(http.MethodGet, fmt.Sprintf("/api/admin/accounts/%d", *created.ID), "")
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var got Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.NotNil(t, got.IdentityRevision, "详情必须回显 K")
	require.Equal(t, int64(1), *got.IdentityRevision)

	// AccountView 是平铺结构：漏拷即列表编辑回显恒缺——同一转换函数里
	// 少拷一个字段不会有编译错误，只会在编辑回显时静默缺失。
	rec = do(http.MethodGet, "/api/admin/accounts", "")
	require.Equal(t, 200, rec.Code, rec.Body.String())
	var list AccountListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Rows, 1)
	require.NotNil(t, list.Rows[0].LifecycleRevision, "列表视图必须平铺回显 C")
	require.NotNil(t, list.Rows[0].IdentityRevision, "列表视图必须平铺回显 K")
	require.Equal(t, int64(1), *list.Rows[0].IdentityRevision)
}
