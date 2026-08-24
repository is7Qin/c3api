// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	userapi "github.com/is7qin/c3api/internal/handler/user"
)

// 本文件覆盖 Phase 5 T4 契约扩展：pricing 矩阵 22 字段（PUT 解码 + 响应/列表
// 回显）、用户余额 USD 换算与 price_multiplier（null = 清除）、组倍率、
// service_tier_policy 设置校验、usagelog/stat 计费字段回显。

// TestAdminUserBalance 管理面用户：balance USD 换算（创建/更新/列表/详情回显）。
// 价格倍率按组（T3.5 修正）经 /api/admin/groups/{id}/assignments 设置，用户本体
// 无倍率字段（见 TestGroupAssignmentMultipliers）。
func TestAdminUserBalance(t *testing.T) {
	doAdmin, doUser, _ := newSharedRouters(t)

	// 创建：balance 1.5 USD → 150000 毫分
	rec := doAdmin(http.MethodPost, "/api/admin/users",
		`{"email":"bill@example.com","password":"s3cret-pass","balance":1.5}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "create: %s", rec.Body.String())
	var created User
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.Equal(t, 1.5, *created.Balance, "创建响应 Balance 回显 USD")

	// 列表回显 USD
	rec = doAdmin(http.MethodGet, "/api/admin/users?email=bill", "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var list UserListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(1), list.Total)
	require.Equal(t, 1.5, *list.Rows[0].Balance, "列表 Balance USD")

	// 用户面 /api/user/auth/me 同语义 USD
	token, _ := loginUser(t, doUser, "bill@example.com")
	rec = doUser(http.MethodGet, "/api/user/auth/me", "", token)
	require.Equal(t, http.StatusOK, rec.Code, "me: %s", rec.Body.String())
	var me struct {
		Balance *float64 `json:"Balance"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &me))
	require.Equal(t, 1.5, *me.Balance, "/api/user/auth/me Balance USD")

	// 更新 balance 0.25 USD（毫分边界 25000）
	rec = doAdmin(http.MethodPut, "/api/admin/users/"+itoa(*created.ID),
		`{"balance":0.25}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "update: %s", rec.Body.String())
	var updated User
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	require.Equal(t, 0.25, *updated.Balance, "更新后 Balance USD")

	// 非法：负余额 → 400
	rec = doAdmin(http.MethodPut, "/api/admin/users/"+itoa(*created.ID), `{"balance":-1}`, "")
	require.Equal(t, 400, rec.Code, "negative balance: %s", rec.Body.String())
}

// loginUser 登录拿 JWT（handler 测试 helper）。
func loginUser(t *testing.T, doUser func(method, path, body, token string) *httptest.ResponseRecorder, email string) (string, int64) {
	t.Helper()
	rec := doUser(http.MethodPost, "/api/user/auth/login",
		`{"email":"`+email+`","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "login: %s", rec.Body.String())
	var resp struct {
		Token string `json:"token"`
		User  User   `json:"user"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.Token, *resp.User.ID
}

// TestGroupMultiplier 组倍率：POST 缺省/null → ×1（1.0 正常值回显）；POST
// 显式 2.0 → 回显；PUT 0 = 免费；PUT 超界 → 400；用户面 /api/user/groups 回显。
func TestGroupMultiplier(t *testing.T) {
	doAdmin, doUser, _ := newSharedRouters(t)

	// POST 缺省 → service 归一 10000 = ×1 → API 回显 1.0（正常值换算）
	rec := doAdmin(http.MethodPost, "/api/admin/groups", `{"name":"g-default"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "create: %s", rec.Body.String())
	var g Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
	require.Equal(t, 1.0, *g.PriceMultiplier, "缺省 → 组默认 ×1（1.0 正常值）")

	// POST 显式倍率 2.0（→ 万分数 20000 入库 → 2.0 回显）
	rec = doAdmin(http.MethodPost, "/api/admin/groups", `{"name":"g-mult","price_multiplier":2.0}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "create mult: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
	require.Equal(t, 2.0, *g.PriceMultiplier)

	// POST 显式 0.0 = 免费组（T3.5 修正：API 可表达显式 0，不落 ×1）
	rec = doAdmin(http.MethodPost, "/api/admin/groups", `{"name":"g-free","price_multiplier":0.0}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "create free: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
	require.Equal(t, 0.0, *g.PriceMultiplier, "POST 显式 0 = 免费组")

	// PUT 0 = 免费
	rec = doAdmin(http.MethodPut, "/api/admin/groups/"+itoa(*g.ID), `{"name":"g-free","price_multiplier":0}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "put free: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
	require.Equal(t, 0.0, *g.PriceMultiplier, "PUT 显式 0 = 免费")

	// 边界换算 round：1.5 → 15000 入库；10 → 100000（×10 上限）
	rec = doAdmin(http.MethodPut, "/api/admin/groups/"+itoa(*g.ID), `{"name":"g-free","price_multiplier":1.5}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "put 1.5: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
	require.Equal(t, 1.5, *g.PriceMultiplier, "1.5 ↔ 15000 换算回显")
	rec = doAdmin(http.MethodPut, "/api/admin/groups/"+itoa(*g.ID), `{"name":"g-free","price_multiplier":10}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "put 10: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
	require.Equal(t, 10.0, *g.PriceMultiplier, "10 ↔ 100000 上限换算回显")

	// PUT 超界 → 400
	rec = doAdmin(http.MethodPut, "/api/admin/groups/"+itoa(*g.ID), `{"name":"g-free","price_multiplier":10.1}`, "")
	require.Equal(t, 400, rec.Code, "multiplier too large: %s", rec.Body.String())
	rec = doAdmin(http.MethodPost, "/api/admin/groups", `{"name":"g-neg","price_multiplier":-1}`, "")
	require.Equal(t, 400, rec.Code, "multiplier negative: %s", rec.Body.String())

	// 用户面 /api/user/groups 回显倍率（正常值）
	token, _ := registerAndGet(t, doUser, "gm@example.com")
	rec = doUser(http.MethodGet, "/api/user/groups", "", token)
	require.Equal(t, http.StatusOK, rec.Code, "user groups: %s", rec.Body.String())
	var groups []Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groups))
	require.NotEmpty(t, groups)
	for _, gr := range groups {
		require.NotNil(t, gr.PriceMultiplier, "用户面组回显倍率")
	}
}

// TestGroupAssignmentMultipliers 用户-组专属倍率（T3.5 修正核心：按组挂载，
// 用户在不同组可有不同倍率）：PUT /groups/{id}/assignments 的 multipliers 设置/
// 清除；response 回显 post-state；越界/未知用户 → 400。
func TestGroupAssignmentMultipliers(t *testing.T) {
	doAdmin, _, _ := newSharedRouters(t)

	// 建组 + 两用户
	rec := doAdmin(http.MethodPost, "/api/admin/groups", `{"name":"ga-mult"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "create group: %s", rec.Body.String())
	var g Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
	var uids []int64
	for _, email := range []string{"ga1@example.com", "ga2@example.com"} {
		rec = doAdmin(http.MethodPost, "/api/admin/users", `{"email":"`+email+`","password":"s3cret-pass"}`, "")
		require.Equal(t, http.StatusOK, rec.Code, "create user: %s", rec.Body.String())
		var u User
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &u))
		uids = append(uids, *u.ID)
	}

	// 授予两人 + 设置专属倍率：u1 2.0（→20000）、u2 0.5（→5000）
	body := fmt.Sprintf(`{"user_ids":[%d,%d],"multipliers":{"%d":2.0,"%d":0.5}}`, uids[0], uids[1], uids[0], uids[1])
	rec = doAdmin(http.MethodPut, "/api/admin/groups/"+itoa(*g.ID)+"/assignments", body, "")
	require.Equal(t, http.StatusOK, rec.Code, "set mults: %s", rec.Body.String())
	var resp GroupAssignmentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.ElementsMatch(t, uids, resp.UserIds)
	require.NotNil(t, resp.Multipliers)
	require.Equal(t, 2.0, *(*resp.Multipliers)[itoa(uids[0])], "u1 专属倍率 2.0 回显")
	require.Equal(t, 0.5, *(*resp.Multipliers)[itoa(uids[1])], "u2 专属倍率 0.5 回显")

	// 清除 u1 专属倍率（null → 未设置 → 回退组倍率）；u2 未列出 → 沿用
	body = fmt.Sprintf(`{"user_ids":[%d,%d],"multipliers":{"%d":null}}`, uids[0], uids[1], uids[0])
	rec = doAdmin(http.MethodPut, "/api/admin/groups/"+itoa(*g.ID)+"/assignments", body, "")
	require.Equal(t, http.StatusOK, rec.Code, "clear mult: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Nil(t, (*resp.Multipliers)[itoa(uids[0])], "null = 清除为未设置")
	require.Equal(t, 0.5, *(*resp.Multipliers)[itoa(uids[1])], "未列出的用户沿用既有倍率")

	// 0 = 免费（正常值 0）
	body = fmt.Sprintf(`{"user_ids":[%d],"multipliers":{"%d":0}}`, uids[0], uids[0])
	rec = doAdmin(http.MethodPut, "/api/admin/groups/"+itoa(*g.ID)+"/assignments", body, "")
	require.Equal(t, http.StatusOK, rec.Code, "free mult: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 0.0, *(*resp.Multipliers)[itoa(uids[0])], "0 = 免费")

	// 越界 → 400
	body = fmt.Sprintf(`{"user_ids":[%d],"multipliers":{"%d":10.1}}`, uids[0], uids[0])
	rec = doAdmin(http.MethodPut, "/api/admin/groups/"+itoa(*g.ID)+"/assignments", body, "")
	require.Equal(t, 400, rec.Code, "multiplier too large: %s", rec.Body.String())
	body = fmt.Sprintf(`{"user_ids":[%d],"multipliers":{"%d":-0.1}}`, uids[0], uids[0])
	rec = doAdmin(http.MethodPut, "/api/admin/groups/"+itoa(*g.ID)+"/assignments", body, "")
	require.Equal(t, 400, rec.Code, "multiplier negative: %s", rec.Body.String())
	// multipliers key 不在 user_ids → 400
	body = fmt.Sprintf(`{"user_ids":[%d],"multipliers":{"999999":1.0}}`, uids[0])
	rec = doAdmin(http.MethodPut, "/api/admin/groups/"+itoa(*g.ID)+"/assignments", body, "")
	require.Equal(t, 400, rec.Code, "unknown uid in multipliers: %s", rec.Body.String())
}

// TestServiceTierPolicySettings service_tier_policy_priority/flex/fast 三个 key：
// 默认 passthrough；三值可设；非法 → 400。
func TestServiceTierPolicySettings(t *testing.T) {
	_, _, do := newListTestRouter(t)

	// GET 默认值包含三个 policy key
	rec := do(http.MethodGet, "/api/admin/settings", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var settings []Setting
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &settings))
	seen := map[string]string{}
	for _, s := range settings {
		seen[*s.Key] = *s.Value
	}
	require.Equal(t, "passthrough", seen["service_tier_policy_priority"], "priority 默认 passthrough")
	require.Equal(t, "passthrough", seen["service_tier_policy_flex"], "flex 默认 passthrough")
	require.Equal(t, "passthrough", seen["service_tier_policy_fast"], "fast 默认 passthrough")

	// 三值均可设
	for _, v := range []string{"passthrough", "strip", "reject"} {
		rec := do(http.MethodPut, "/api/admin/settings", `{"key":"service_tier_policy_priority","value":"`+v+`"}`)
		require.Equal(t, 200, rec.Code, "set %s: %s", v, rec.Body.String())
		rec = do(http.MethodPut, "/api/admin/settings", `{"key":"service_tier_policy_fast","value":"`+v+`"}`)
		require.Equal(t, 200, rec.Code, "set fast %s: %s", v, rec.Body.String())
	}

	// 非法值 → 400
	rec = do(http.MethodPut, "/api/admin/settings", `{"key":"service_tier_policy_flex","value":"bogus"}`)
	require.Equal(t, 400, rec.Code, "invalid policy value: %s", rec.Body.String())
	rec = do(http.MethodPut, "/api/admin/settings", `{"key":"service_tier_policy_fast","value":"bogus"}`)
	require.Equal(t, 400, rec.Code, "invalid fast policy value: %s", rec.Body.String())
}

// TestLogsStatsBillingFields usagelog cost/billing_tier/above_hit/overdraft 与
// StatBucket cost 经管理面/用户面端点回显。
func TestLogsStatsBillingFields(t *testing.T) {
	doAdmin, doUser, store := newSharedRouters(t)
	token, userID := registerAndGet(t, doUser, "lb@example.com")

	base := time.Now().UTC().Truncate(time.Second)
	store.mu.Lock()
	store.logs = []*domain.UsageLog{
		{
			ID: 1, UserID: userID, RequestID: "r-bill", Model: "gpt-4o",
			Format: domain.FormatOpenAIChat, StatusCode: 200,
			InputTokens: 10, OutputTokens: 20, TotalTokens: 30,
			Cost: 500, RawCost: 700, BillingTier: "fast", AboveHit: true, Overdraft: false,
			CreatedAt: base,
		},
	}
	store.mu.Unlock()
	win := "from=" + base.Add(-time.Hour).Format(time.RFC3339) + "&to=" + base.Add(time.Hour).Format(time.RFC3339)

	// 管理面 /api/admin/usage_logs
	rec := doAdmin(http.MethodGet, "/api/admin/usage_logs?"+win, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var logs LogsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &logs))
	require.Len(t, logs.Rows, 1)
	r := logs.Rows[0]
	require.Equal(t, int64(500), *r.Cost, "log cost 回显（毫分）")
	require.Equal(t, int64(700), *r.RawCost, "log raw_cost 回显（毫分——乘倍率前原始成本）")
	require.Equal(t, "fast", *r.BillingTier, "log billing_tier 回显")
	require.True(t, *r.AboveHit)
	require.False(t, *r.Overdraft)

	// 用户面 /api/user/usage_logs 同字段
	rec = doUser(http.MethodGet, "/api/user/usage_logs?"+win, "", token)
	require.Equal(t, http.StatusOK, rec.Code)
	var ul userapi.UserLogsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ul))
	require.Len(t, ul.Rows, 1)
	require.Equal(t, int64(500), *ul.Rows[0].Cost, "user log cost 回显")
	require.Equal(t, int64(700), *ul.Rows[0].RawCost, "user log raw_cost 回显")
}
