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

// TestPutPricingMatrixFields 手动设价矩阵 22 列（API 边界换算：USD/1M ↔ 毫分、
// fast 正常值 ↔ 万分数）：全设 → 响应与列表 roundtrip；缺省 → nil；负数/超界
// fast_multiplier → 400。
func TestPutPricingMatrixFields(t *testing.T) {
	h, do := newPricingRouter(t, nil)

	// 全矩阵设价（priority 4 + flex 4 + above 13 + fast 1 = 22；USD/1M 正常值）
	body := `{
		"prompt_price_per_million":1.0,"completion_price_per_million":2.0,
		"priority_prompt_price_per_million":1.1,"priority_completion_price_per_million":2.1,
		"priority_cache_read_price_per_million":1.11,"priority_cache_creation_price_per_million":2.11,
		"flex_prompt_price_per_million":1.2,"flex_completion_price_per_million":2.2,
		"flex_cache_read_price_per_million":1.21,"flex_cache_creation_price_per_million":2.21,
		"above_threshold":128000,
		"above_prompt_price_per_million":1.3,"above_completion_price_per_million":2.3,
		"above_cache_read_price_per_million":1.31,"above_cache_creation_price_per_million":2.31,
		"above_priority_prompt_price_per_million":1.4,"above_priority_completion_price_per_million":2.4,
		"above_priority_cache_read_price_per_million":1.41,"above_priority_cache_creation_price_per_million":2.41,
		"above_flex_prompt_price_per_million":1.5,"above_flex_completion_price_per_million":2.5,
		"above_flex_cache_read_price_per_million":1.51,"above_flex_cache_creation_price_per_million":2.51,
		"fast_multiplier":2.0
	}`
	rec := do(http.MethodPut, "/admin/pricing?model=matrix-model", body)
	require.Equal(t, 200, rec.Code, "put matrix: %s", rec.Body.String())
	var p Pricing
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	require.Equal(t, 1.1, *p.PriorityPromptPricePerMillion, "priority_prompt 回显（USD/1M）")
	require.Equal(t, 2.11, *p.PriorityCacheCreationPricePerMillion)
	require.Equal(t, 2.2, *p.FlexCompletionPricePerMillion)
	require.Equal(t, int64(128000), *p.AboveThreshold, "above_threshold 回显（tokens int）")
	require.Equal(t, 2.3, *p.AboveCompletionPricePerMillion)
	require.Equal(t, 1.4, *p.AbovePriorityPromptPricePerMillion)
	require.Equal(t, 2.41, *p.AbovePriorityCacheCreationPricePerMillion)
	require.Equal(t, 1.5, *p.AboveFlexPromptPricePerMillion)
	require.Equal(t, 2.51, *p.AboveFlexCacheCreationPricePerMillion)
	require.Equal(t, 2.0, *p.FastMultiplier, "fast_multiplier 正常值回显（2.0 ↔ 20000）")

	// 列表 roundtrip
	rec = do(http.MethodGet, "/admin/pricing?model=matrix-model", "")
	require.Equal(t, 200, rec.Code, "list: %s", rec.Body.String())
	var list PricingListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Len(t, list.Rows, 1)
	require.Equal(t, 1.3, *list.Rows[0].AbovePromptPricePerMillion, "列表 roundtrip above_prompt")
	require.Equal(t, 2.0, *list.Rows[0].FastMultiplier)

	// 部分设价覆盖：其余矩阵字段清空（PUT 全量替换，nil = 清空）
	rec = do(http.MethodPut, "/admin/pricing?model=matrix-model",
		`{"prompt_price_per_million":1,"completion_price_per_million":2,"above_threshold":1000}`)
	require.Equal(t, 200, rec.Code, "partial overwrite: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p))
	require.Equal(t, int64(1000), *p.AboveThreshold, "保留显式 above_threshold")
	require.Nil(t, p.PriorityPromptPricePerMillion, "未提供矩阵价 → nil（清空）")
	require.Nil(t, p.AbovePromptPricePerMillion)
	require.Nil(t, p.FastMultiplier)

	// 矩阵字段负数 → 400（service 校验）
	for _, tc := range []struct{ name, field string }{
		{"priority_prompt", `"priority_prompt_price_per_million":-0.01`},
		{"flex_completion", `"flex_completion_price_per_million":-0.01`},
		{"above_threshold", `"above_threshold":-1`},
		{"above_priority_cache_creation", `"above_priority_cache_creation_price_per_million":-0.01`},
		{"above_flex_prompt", `"above_flex_prompt_price_per_million":-0.01`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(http.MethodPut, "/admin/pricing?model=matrix-model",
				fmt.Sprintf(`{"prompt_price_per_million":1,"completion_price_per_million":2,%s}`, tc.field))
			require.Equal(t, 400, rec.Code, "%s: %s", tc.name, rec.Body.String())
		})
	}

	// fast_multiplier 越界（0 与 >10）→ 400
	rec = do(http.MethodPut, "/admin/pricing?model=matrix-model",
		`{"prompt_price_per_million":1,"completion_price_per_million":2,"fast_multiplier":0}`)
	require.Equal(t, 400, rec.Code, "fast 0: %s", rec.Body.String())
	rec = do(http.MethodPut, "/admin/pricing?model=matrix-model",
		`{"prompt_price_per_million":1,"completion_price_per_million":2,"fast_multiplier":10.0001}`)
	require.Equal(t, 400, rec.Code, "fast >10: %s", rec.Body.String())

	// 幂等：svc 快照重载后 GetPrice 读到矩阵价（计费读路径）
	got, err := h.svc.GetPrice("matrix-model")
	require.NoError(t, err)
	require.Equal(t, int64(1000), *got.AboveThreshold, "快照读矩阵价")
}

// TestPricingBoundaryConversion 价格列/倍率 API 边界换算表驱动（与 balance
// 毫分↔USD 同构）：USD/1M ↔ 毫分（150000 ↔ 1.5 round 双向）、fast 正常值 ↔
// 万分数（60000 ↔ 6.0）。
func TestPricingBoundaryConversion(t *testing.T) {
	// 价格列：毫分 → USD（展示）与 USD → 毫分（输入）
	for _, c := range []struct {
		millis int64
		usd    float64
	}{
		{150000, 1.5},
		{100000, 1.0},
		{0, 0.0},
		{250000, 2.5},
		{1, 0.00001},
	} {
		require.Equal(t, c.usd, millisToUSD(c.millis), "毫分 %d → USD", c.millis)
		require.Equal(t, c.millis, usdToMillis(c.usd), "USD %v → 毫分（round）", c.usd)
	}
	// fast 倍率：万分数 → 正常值 与 正常值 → 万分数（round 双向）
	for _, c := range []struct {
		mult int64
		norm float64
	}{
		{60000, 6.0},
		{20000, 2.0},
		{15000, 1.5},
		{10000, 1.0},
		{0, 0.0},
	} {
		v := multI64ToNormalPtr(&c.mult)
		require.NotNil(t, v)
		require.Equal(t, c.norm, *v, "万分数 %d → 正常值", c.mult)
		in := c.norm
		back := normalToMultI64Ptr(&in)
		require.NotNil(t, back)
		require.Equal(t, c.mult, *back, "正常值 %v → 万分数（round）", c.norm)
	}
	// nil 透传（缺省 = 清空该价/无倍率）
	require.Nil(t, millisToUSDPtr(nil))
	require.Nil(t, usdToMillisPtr(nil))
	require.Nil(t, multI64ToNormalPtr(nil))
	require.Nil(t, normalToMultI64Ptr(nil))
	// 组/专属倍率 int 换算（multToNormal/normalToMult 同构复用）
	require.Equal(t, 1.5, multToNormal(15000))
	require.Equal(t, 15000, normalToMult(1.5))
	require.Equal(t, 0.0, multToNormal(0))
	require.Equal(t, 100000, normalToMult(10.0))
}

// TestAdminUserBalance 管理面用户：balance USD 换算（创建/更新/列表/详情回显）。
// 价格倍率按组（T3.5 修正）经 /admin/groups/{id}/assignments 设置，用户本体
// 无倍率字段（见 TestGroupAssignmentMultipliers）。
func TestAdminUserBalance(t *testing.T) {
	doAdmin, doUser, _ := newSharedRouters(t)

	// 创建：balance 1.5 USD → 150000 毫分
	rec := doAdmin(http.MethodPost, "/admin/users",
		`{"email":"bill@example.com","password":"s3cret-pass","balance":1.5}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "create: %s", rec.Body.String())
	var created User
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.Equal(t, 1.5, *created.Balance, "创建响应 Balance 回显 USD")

	// 列表回显 USD
	rec = doAdmin(http.MethodGet, "/admin/users?email=bill", "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var list UserListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(1), list.Total)
	require.Equal(t, 1.5, *list.Rows[0].Balance, "列表 Balance USD")

	// 用户面 /user/auth/me 同语义 USD
	token, _ := loginUser(t, doUser, "bill@example.com")
	rec = doUser(http.MethodGet, "/user/auth/me", "", token)
	require.Equal(t, http.StatusOK, rec.Code, "me: %s", rec.Body.String())
	var me struct {
		Balance *float64 `json:"Balance"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &me))
	require.Equal(t, 1.5, *me.Balance, "/user/auth/me Balance USD")

	// 更新 balance 0.25 USD（毫分边界 25000）
	rec = doAdmin(http.MethodPut, "/admin/users/"+itoa(*created.ID),
		`{"balance":0.25}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "update: %s", rec.Body.String())
	var updated User
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	require.Equal(t, 0.25, *updated.Balance, "更新后 Balance USD")

	// 非法：负余额 → 400
	rec = doAdmin(http.MethodPut, "/admin/users/"+itoa(*created.ID), `{"balance":-1}`, "")
	require.Equal(t, 400, rec.Code, "negative balance: %s", rec.Body.String())
}

// loginUser 登录拿 JWT（handler 测试 helper）。
func loginUser(t *testing.T, doUser func(method, path, body, token string) *httptest.ResponseRecorder, email string) (string, int64) {
	t.Helper()
	rec := doUser(http.MethodPost, "/user/auth/login",
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
// 显式 2.0 → 回显；PUT 0 = 免费；PUT 超界 → 400；用户面 /user/groups 回显。
func TestGroupMultiplier(t *testing.T) {
	doAdmin, doUser, _ := newSharedRouters(t)

	// POST 缺省 → service 归一 10000 = ×1 → API 回显 1.0（正常值换算）
	rec := doAdmin(http.MethodPost, "/admin/groups", `{"name":"g-default"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "create: %s", rec.Body.String())
	var g Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
	require.Equal(t, 1.0, *g.PriceMultiplier, "缺省 → 组默认 ×1（1.0 正常值）")

	// POST 显式倍率 2.0（→ 万分数 20000 入库 → 2.0 回显）
	rec = doAdmin(http.MethodPost, "/admin/groups", `{"name":"g-mult","price_multiplier":2.0}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "create mult: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
	require.Equal(t, 2.0, *g.PriceMultiplier)

	// POST 显式 0.0 = 免费组（T3.5 修正：API 可表达显式 0，不落 ×1）
	rec = doAdmin(http.MethodPost, "/admin/groups", `{"name":"g-free","price_multiplier":0.0}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "create free: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
	require.Equal(t, 0.0, *g.PriceMultiplier, "POST 显式 0 = 免费组")

	// PUT 0 = 免费
	rec = doAdmin(http.MethodPut, "/admin/groups/"+itoa(*g.ID), `{"name":"g-free","price_multiplier":0}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "put free: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
	require.Equal(t, 0.0, *g.PriceMultiplier, "PUT 显式 0 = 免费")

	// 边界换算 round：1.5 → 15000 入库；10 → 100000（×10 上限）
	rec = doAdmin(http.MethodPut, "/admin/groups/"+itoa(*g.ID), `{"name":"g-free","price_multiplier":1.5}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "put 1.5: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
	require.Equal(t, 1.5, *g.PriceMultiplier, "1.5 ↔ 15000 换算回显")
	rec = doAdmin(http.MethodPut, "/admin/groups/"+itoa(*g.ID), `{"name":"g-free","price_multiplier":10}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "put 10: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
	require.Equal(t, 10.0, *g.PriceMultiplier, "10 ↔ 100000 上限换算回显")

	// PUT 超界 → 400
	rec = doAdmin(http.MethodPut, "/admin/groups/"+itoa(*g.ID), `{"name":"g-free","price_multiplier":10.1}`, "")
	require.Equal(t, 400, rec.Code, "multiplier too large: %s", rec.Body.String())
	rec = doAdmin(http.MethodPost, "/admin/groups", `{"name":"g-neg","price_multiplier":-1}`, "")
	require.Equal(t, 400, rec.Code, "multiplier negative: %s", rec.Body.String())

	// 用户面 /user/groups 回显倍率（正常值）
	token, _ := registerAndGet(t, doUser, "gm@example.com")
	rec = doUser(http.MethodGet, "/user/groups", "", token)
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
	rec := doAdmin(http.MethodPost, "/admin/groups", `{"name":"ga-mult"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "create group: %s", rec.Body.String())
	var g Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
	var uids []int64
	for _, email := range []string{"ga1@example.com", "ga2@example.com"} {
		rec = doAdmin(http.MethodPost, "/admin/users", `{"email":"`+email+`","password":"s3cret-pass"}`, "")
		require.Equal(t, http.StatusOK, rec.Code, "create user: %s", rec.Body.String())
		var u User
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &u))
		uids = append(uids, *u.ID)
	}

	// 授予两人 + 设置专属倍率：u1 2.0（→20000）、u2 0.5（→5000）
	body := fmt.Sprintf(`{"user_ids":[%d,%d],"multipliers":{"%d":2.0,"%d":0.5}}`, uids[0], uids[1], uids[0], uids[1])
	rec = doAdmin(http.MethodPut, "/admin/groups/"+itoa(*g.ID)+"/assignments", body, "")
	require.Equal(t, http.StatusOK, rec.Code, "set mults: %s", rec.Body.String())
	var resp GroupAssignmentsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.ElementsMatch(t, uids, resp.UserIds)
	require.NotNil(t, resp.Multipliers)
	require.Equal(t, 2.0, *(*resp.Multipliers)[itoa(uids[0])], "u1 专属倍率 2.0 回显")
	require.Equal(t, 0.5, *(*resp.Multipliers)[itoa(uids[1])], "u2 专属倍率 0.5 回显")

	// 清除 u1 专属倍率（null → 未设置 → 回退组倍率）；u2 未列出 → 沿用
	body = fmt.Sprintf(`{"user_ids":[%d,%d],"multipliers":{"%d":null}}`, uids[0], uids[1], uids[0])
	rec = doAdmin(http.MethodPut, "/admin/groups/"+itoa(*g.ID)+"/assignments", body, "")
	require.Equal(t, http.StatusOK, rec.Code, "clear mult: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Nil(t, (*resp.Multipliers)[itoa(uids[0])], "null = 清除为未设置")
	require.Equal(t, 0.5, *(*resp.Multipliers)[itoa(uids[1])], "未列出的用户沿用既有倍率")

	// 0 = 免费（正常值 0）
	body = fmt.Sprintf(`{"user_ids":[%d],"multipliers":{"%d":0}}`, uids[0], uids[0])
	rec = doAdmin(http.MethodPut, "/admin/groups/"+itoa(*g.ID)+"/assignments", body, "")
	require.Equal(t, http.StatusOK, rec.Code, "free mult: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, 0.0, *(*resp.Multipliers)[itoa(uids[0])], "0 = 免费")

	// 越界 → 400
	body = fmt.Sprintf(`{"user_ids":[%d],"multipliers":{"%d":10.1}}`, uids[0], uids[0])
	rec = doAdmin(http.MethodPut, "/admin/groups/"+itoa(*g.ID)+"/assignments", body, "")
	require.Equal(t, 400, rec.Code, "multiplier too large: %s", rec.Body.String())
	body = fmt.Sprintf(`{"user_ids":[%d],"multipliers":{"%d":-0.1}}`, uids[0], uids[0])
	rec = doAdmin(http.MethodPut, "/admin/groups/"+itoa(*g.ID)+"/assignments", body, "")
	require.Equal(t, 400, rec.Code, "multiplier negative: %s", rec.Body.String())
	// multipliers key 不在 user_ids → 400
	body = fmt.Sprintf(`{"user_ids":[%d],"multipliers":{"999999":1.0}}`, uids[0])
	rec = doAdmin(http.MethodPut, "/admin/groups/"+itoa(*g.ID)+"/assignments", body, "")
	require.Equal(t, 400, rec.Code, "unknown uid in multipliers: %s", rec.Body.String())
}

// TestServiceTierPolicySettings service_tier_policy_priority/flex/fast 三个 key：
// 默认 passthrough；三值可设；非法 → 400。
func TestServiceTierPolicySettings(t *testing.T) {
	_, _, do := newListTestRouter(t)

	// GET 默认值包含三个 policy key
	rec := do(http.MethodGet, "/admin/settings", "")
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
		rec := do(http.MethodPut, "/admin/settings", `{"key":"service_tier_policy_priority","value":"`+v+`"}`)
		require.Equal(t, 200, rec.Code, "set %s: %s", v, rec.Body.String())
		rec = do(http.MethodPut, "/admin/settings", `{"key":"service_tier_policy_fast","value":"`+v+`"}`)
		require.Equal(t, 200, rec.Code, "set fast %s: %s", v, rec.Body.String())
	}

	// 非法值 → 400
	rec = do(http.MethodPut, "/admin/settings", `{"key":"service_tier_policy_flex","value":"bogus"}`)
	require.Equal(t, 400, rec.Code, "invalid policy value: %s", rec.Body.String())
	rec = do(http.MethodPut, "/admin/settings", `{"key":"service_tier_policy_fast","value":"bogus"}`)
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
	store.stats = []*domain.StatBucket{
		{UserID: userID, Model: "gpt-4o", RequestCount: 1, Cost: 500, TotalTokens: 30},
	}
	store.mu.Unlock()
	win := "from=" + base.Add(-time.Hour).Format(time.RFC3339) + "&to=" + base.Add(time.Hour).Format(time.RFC3339)

	// 管理面 /admin/usage_logs
	rec := doAdmin(http.MethodGet, "/admin/usage_logs?"+win, "", "")
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

	// 用户面 /user/usage_logs 同字段
	rec = doUser(http.MethodGet, "/user/usage_logs?"+win, "", token)
	require.Equal(t, http.StatusOK, rec.Code)
	var ul userapi.UserLogsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ul))
	require.Len(t, ul.Rows, 1)
	require.Equal(t, int64(500), *ul.Rows[0].Cost, "user log cost 回显")
	require.Equal(t, int64(700), *ul.Rows[0].RawCost, "user log raw_cost 回显")

	// 管理面 /admin/stats
	rec = doAdmin(http.MethodGet, "/admin/stats", "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	var buckets []StatBucket
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &buckets))
	require.Len(t, buckets, 1)
	// rewrite spec 2026-08-14 破坏性变更：/stats Cost 毫分 → USD（/1e5）
	require.InDelta(t, 0.005, *buckets[0].Cost, 1e-9, "stat bucket cost USD 口径（500 毫分 /1e5）")
}
