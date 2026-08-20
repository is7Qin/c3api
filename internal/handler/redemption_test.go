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
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	userapi "github.com/is7qin/c3api/internal/handler/user"
)

// validCode 校验码格式（XXXX-XXXX-XXXX-XXXX，字符集 32：大写 A-Z 去 I/O + 数字 2-9 去 0/1）。
func validCode(t *testing.T, code string) {
	t.Helper()
	parts := strings.Split(code, "-")
	require.Len(t, parts, 4, "code format: %s", code)
	const charset = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	for _, p := range parts {
		require.Len(t, p, 4, "code segment length: %s", code)
		for _, ch := range p {
			require.Contains(t, charset, string(ch), "code charset: %s", code)
		}
	}
}

// genCodes 管理面生成兑换码并返回完整响应（测试 helper）。
func genCodes(t *testing.T, doAdmin func(method, path, body, token string) *httptest.ResponseRecorder, body string) GenerateResponse {
	t.Helper()
	rec := doAdmin(http.MethodPost, "/api/admin/redemption-codes", body, "")
	require.Equal(t, 200, rec.Code, "generate: %s", rec.Body.String())
	var resp GenerateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotEmpty(t, resp.Codes)
	return resp
}

// TestGenerateRedemptionCodes 生成成功：count=3 → 完整码列表（格式/type/value/
// remark/max_uses 正确、互不重复）；静态 admin token 路径 created_by = 0（系统，
// 决策 5）。
func TestGenerateRedemptionCodes(t *testing.T) {
	_, _, do := newListTestRouter(t)
	rec := do(http.MethodPost, "/api/admin/redemption-codes",
		`{"type":"balance","value":100,"remark":"赠品","max_uses":3,"count":3}`)
	require.Equal(t, 200, rec.Code, "generate: %s", rec.Body.String())
	var resp GenerateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Codes, 3, "count=3 → 3 个码")
	seen := map[string]bool{}
	for i, c := range resp.Codes {
		validCode(t, c.Code)
		require.False(t, seen[c.Code], "code must be unique: %s", c.Code)
		seen[c.Code] = true
		require.True(t, c.ID > 0, "id 已分配（row %d）", i)
		require.Equal(t, RedemptionType("balance"), c.Type)
		require.Equal(t, 100.0, c.Value, "面值出参 USD 回显（100 USD 入参）")
		require.NotNil(t, c.Remark)
		require.Equal(t, "赠品", *c.Remark)
		require.Equal(t, 3, c.MaxUses)
		require.Zero(t, c.UsedCount)
		require.Equal(t, RedemptionStatus("active"), c.Status)
		require.Zero(t, c.CreatedBy, "静态 token 路径不注入 → 0 = 系统")
	}
}

// TestGenerateRedemptionCodesValidation 生成参数校验 400（handler decode + service
// validateGenerateRequest）：type 非法 / value ≤ 0 / temp_balance 缺
// resource_expires_at / count 越界。
func TestGenerateRedemptionCodesValidation(t *testing.T) {
	_, _, do := newListTestRouter(t)
	for _, tc := range []struct{ name, body string }{
		{"type 非法", `{"type":"bogus","value":100}`},
		{"value 0", `{"type":"balance","value":0}`},
		{"value 负数", `{"type":"balance","value":-1}`},
		{"concurrency 小数", `{"type":"concurrency","value":5.5}`},
		{"concurrency 0", `{"type":"concurrency","value":0}`},
		{"temp_balance 缺 resource_expires_at", `{"type":"temp_balance","value":100}`},
		{"count 越上界", `{"type":"balance","value":100,"count":1001}`},
		{"count 负数", `{"type":"balance","value":100,"count":-1}`},
		{"非 JSON", `{`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := do(http.MethodPost, "/api/admin/redemption-codes", tc.body)
			require.Equal(t, 400, rec.Code, "%s: %s", tc.name, rec.Body.String())
			var body map[string]any
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Contains(t, body, "error", "must be ErrorResponse JSON")
		})
	}
}

// TestRedemptionCodesList type/status 筛选 + 增强分页范式（page/page_size 绑定）+
// sort 白名单与枚举绑定 400。
func TestRedemptionCodesList(t *testing.T) {
	_, _, do := newListTestRouter(t)
	rec := do(http.MethodPost, "/api/admin/redemption-codes", `{"type":"balance","value":100,"count":2}`)
	require.Equal(t, 200, rec.Code, "generate balance: %s", rec.Body.String())
	rec = do(http.MethodPost, "/api/admin/redemption-codes", `{"type":"concurrency","value":5}`)
	require.Equal(t, 200, rec.Code, "generate concurrency: %s", rec.Body.String())
	var gen GenerateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &gen))
	concID := gen.Codes[0].ID

	// 全部
	rec = do(http.MethodGet, "/api/admin/redemption-codes", "")
	require.Equal(t, 200, rec.Code, "list all: %s", rec.Body.String())
	var list RedemptionCodeListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(3), list.Total)
	require.Len(t, list.Rows, 3)

	// type 筛选
	rec = do(http.MethodGet, "/api/admin/redemption-codes?type=balance", "")
	require.Equal(t, 200, rec.Code, "filter type: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(2), list.Total)
	for _, c := range list.Rows {
		require.Equal(t, RedemptionType("balance"), c.Type)
	}

	// 失效 1 个后 status 筛选
	rec = do(http.MethodPost, fmt.Sprintf("/api/admin/redemption-codes/%d/deactivate", concID), "")
	require.Equal(t, 200, rec.Code, "deactivate: %s", rec.Body.String())
	rec = do(http.MethodGet, "/api/admin/redemption-codes?status=disabled", "")
	require.Equal(t, 200, rec.Code, "filter status: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(1), list.Total)
	require.Equal(t, RedemptionType("concurrency"), list.Rows[0].Type)

	// 组合筛选 + 增强分页参数绑定
	rec = do(http.MethodGet, "/api/admin/redemption-codes?type=balance&status=active&page=1&page_size=10&sort=id&order=asc", "")
	require.Equal(t, 200, rec.Code, "combined: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, int64(2), list.Total)

	// page_size 越界 → 400（handler 校验）
	rec = do(http.MethodGet, "/api/admin/redemption-codes?page_size=1001", "")
	require.Equal(t, 400, rec.Code, "page_size 越界: %s", rec.Body.String())

	// 非法 sort → 400（service 白名单）
	rec = do(http.MethodGet, "/api/admin/redemption-codes?sort=bogus", "")
	require.Equal(t, 400, rec.Code, "invalid sort: %s", rec.Body.String())

	// 非法 type 枚举 → 400（参数绑定；service typ.Valid 双保险）
	rec = do(http.MethodGet, "/api/admin/redemption-codes?type=bogus", "")
	require.Equal(t, 400, rec.Code, "invalid type: %s", rec.Body.String())
}

// TestDeactivateRedemptionCodes 单码 + 批量失效：成功后可重复失效（幂等 no-op），
// 批量返回新失效数（已 disabled 不计）。
func TestDeactivateRedemptionCodes(t *testing.T) {
	_, _, do := newListTestRouter(t)
	rec := do(http.MethodPost, "/api/admin/redemption-codes", `{"type":"balance","value":100,"count":3}`)
	require.Equal(t, 200, rec.Code, "generate: %s", rec.Body.String())
	var resp GenerateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	ids := []int64{resp.Codes[0].ID, resp.Codes[1].ID, resp.Codes[2].ID}

	// 单码失效
	rec = do(http.MethodPost, fmt.Sprintf("/api/admin/redemption-codes/%d/deactivate", ids[0]), "")
	require.Equal(t, 200, rec.Code, "single deactivate: %s", rec.Body.String())
	var d DeactivateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &d))
	require.True(t, d.Deactivated)

	// 单码重复失效 no-op 成功（幂等重放友好，决策 6）
	rec = do(http.MethodPost, fmt.Sprintf("/api/admin/redemption-codes/%d/deactivate", ids[0]), "")
	require.Equal(t, 200, rec.Code, "repeat deactivate: %s", rec.Body.String())

	// 批量失效剩余 2 个 → 新失效数 2
	rec = do(http.MethodPost, "/api/admin/redemption-codes/batch-deactivate",
		`{"ids":[`+itoa(ids[1])+`,`+itoa(ids[2])+`]}`)
	require.Equal(t, 200, rec.Code, "batch deactivate: %s", rec.Body.String())
	var b BatchDeactivateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &b))
	require.Equal(t, 2, b.Deactivated)

	// 全部已失效后再批量 → no-op 0
	rec = do(http.MethodPost, "/api/admin/redemption-codes/batch-deactivate",
		`{"ids":[`+itoa(ids[0])+`,`+itoa(ids[1])+`]}`)
	require.Equal(t, 200, rec.Code, "batch no-op: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &b))
	require.Zero(t, b.Deactivated)

	// 空 ids / 越界 → 400（normalizeIDs 1-100）
	rec = do(http.MethodPost, "/api/admin/redemption-codes/batch-deactivate", `{"ids":[]}`)
	require.Equal(t, 400, rec.Code, "empty ids: %s", rec.Body.String())
	rec = do(http.MethodPost, "/api/admin/redemption-codes/batch-deactivate",
		`{"ids":[`+strings.TrimSuffix(strings.Repeat("1,", 101), ",")+`]}`)
	require.Equal(t, 400, rec.Code, "too many ids: %s", rec.Body.String())
}

// TestRedemptionCodesMissing404 单码失效 / 审计 / 批量失效缺失 id → 404 含缺失详情。
func TestRedemptionCodesMissing404(t *testing.T) {
	_, _, do := newListTestRouter(t)
	rec := do(http.MethodPost, "/api/admin/redemption-codes", `{"type":"balance","value":100}`)
	require.Equal(t, 200, rec.Code, "generate: %s", rec.Body.String())
	var resp GenerateResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	existID := resp.Codes[0].ID

	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/api/admin/redemption-codes/999/deactivate"},
		{http.MethodGet, "/api/admin/redemption-codes/999/uses"},
	} {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			rec := do(tc.method, tc.path, "")
			require.Equal(t, 404, rec.Code, "%s: %s", tc.path, rec.Body.String())
			require.Contains(t, errMsg(t, rec), "id=999 missing")
		})
	}

	// 批量含缺失 id → 404 含缺失详情（存在 id + 缺失 id）
	rec = do(http.MethodPost, "/api/admin/redemption-codes/batch-deactivate",
		`{"ids":[`+itoa(existID)+`,999]}`)
	require.Equal(t, 404, rec.Code, "batch missing: %s", rec.Body.String())
	require.Contains(t, errMsg(t, rec), "id=999 missing")
}

// TestRedeemThreeTypes 兑换三类型成功（经真实 /user 路由器 + JWT）：balance 加
// 余额、concurrency 0 特判设值、temp_balance 回执带 resource_expires_at；兑换后
// /api/user/auth/me 快照可见资源变更。
func TestRedeemThreeTypes(t *testing.T) {
	doAdmin, doUser, _ := newSharedRouters(t)
	token, userID := registerAndGet(t, doUser, "redeem@example.com")
	require.True(t, userID > 0)

	// balance：兑换成功回执 + 余额 += value（USD）
	gen := genCodes(t, doAdmin, `{"type":"balance","value":100}`)
	rec := doUser(http.MethodPost, "/api/user/redemptions", `{"code":"`+gen.Codes[0].Code+`"}`, token)
	require.Equal(t, 200, rec.Code, "redeem balance: %s", rec.Body.String())
	var r userapi.RedeemResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &r))
	require.Equal(t, userapi.RedemptionType("balance"), r.Applied.Type)
	require.Equal(t, 100.0, r.Applied.Value, "回执面值 USD 回显")
	require.Nil(t, r.Applied.ResourceExpiresAt, "balance 无资源到期")

	// concurrency：当前 0 → 直接设 value（决策 2）
	gen = genCodes(t, doAdmin, `{"type":"concurrency","value":5}`)
	rec = doUser(http.MethodPost, "/api/user/redemptions", `{"code":"`+gen.Codes[0].Code+`"}`, token)
	require.Equal(t, 200, rec.Code, "redeem concurrency: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &r))
	require.Equal(t, userapi.RedemptionType("concurrency"), r.Applied.Type)
	require.Equal(t, 5.0, r.Applied.Value, "回执并发数直出")

	// temp_balance：回执携带 resource_expires_at
	gen = genCodes(t, doAdmin, `{"type":"temp_balance","value":50,"resource_expires_at":"2030-01-01T00:00:00Z"}`)
	rec = doUser(http.MethodPost, "/api/user/redemptions", `{"code":"`+gen.Codes[0].Code+`"}`, token)
	require.Equal(t, 200, rec.Code, "redeem temp_balance: %s", rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &r))
	require.Equal(t, userapi.RedemptionType("temp_balance"), r.Applied.Type)
	require.Equal(t, 50.0, r.Applied.Value, "回执面值 USD 回显")
	require.NotNil(t, r.Applied.ResourceExpiresAt, "temp_balance 必带资源到期")

	// 兑换后快照生效（/api/user/auth/me 即时可见，决策 8 invalidate）
	rec = doUser(http.MethodGet, "/api/user/auth/me", "", token)
	require.Equal(t, 200, rec.Code, "me: %s", rec.Body.String())
	var me userapi.User
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &me))
	require.Equal(t, 100.0, *me.Balance, "balance 码已加余额（API 展示 USD：100 USD = $100）")
	require.Equal(t, 5, *me.MaxConcurrency, "concurrency 码 0 特判直接设值")
}

// TestRedeemConflictAndInvalid 重复兑换 → 409 already redeemed；不存在/已失效码 →
// 400 invalid code（统一不泄露状态细节，决策 7）。
func TestRedeemConflictAndInvalid(t *testing.T) {
	doAdmin, doUser, _ := newSharedRouters(t)
	token, _ := registerAndGet(t, doUser, "redeem2@example.com")

	gen := genCodes(t, doAdmin, `{"type":"balance","value":100}`)
	rec := doUser(http.MethodPost, "/api/user/redemptions", `{"code":"`+gen.Codes[0].Code+`"}`, token)
	require.Equal(t, 200, rec.Code, "first redeem: %s", rec.Body.String())

	// 重复兑换 → 409
	rec = doUser(http.MethodPost, "/api/user/redemptions", `{"code":"`+gen.Codes[0].Code+`"}`, token)
	require.Equal(t, 409, rec.Code, "repeat redeem: %s", rec.Body.String())
	require.Contains(t, errMsg(t, rec), "already redeemed")

	// 不存在的码 → 400 invalid code
	rec = doUser(http.MethodPost, "/api/user/redemptions", `{"code":"ABCD-EFGH-JKLM-NPQR"}`, token)
	require.Equal(t, 400, rec.Code, "unknown code: %s", rec.Body.String())
	require.Contains(t, errMsg(t, rec), "invalid code")

	// 已失效码 → 400 invalid code
	gen = genCodes(t, doAdmin, `{"type":"balance","value":100}`)
	rec = doAdmin(http.MethodPost, fmt.Sprintf("/api/admin/redemption-codes/%d/deactivate", gen.Codes[0].ID), "", "")
	require.Equal(t, 200, rec.Code, "deactivate: %s", rec.Body.String())
	rec = doUser(http.MethodPost, "/api/user/redemptions", `{"code":"`+gen.Codes[0].Code+`"}`, token)
	require.Equal(t, 400, rec.Code, "disabled code: %s", rec.Body.String())
	require.Contains(t, errMsg(t, rec), "invalid code")
}

// TestRedemptionUsesAndHistory 管理面审计（某码兑换记录）+ 用户面我的兑换记录
// （仅本人数据，含码的 type/remark）。
func TestRedemptionUsesAndHistory(t *testing.T) {
	doAdmin, doUser, _ := newSharedRouters(t)
	token, _ := registerAndGet(t, doUser, "hist@example.com")
	token2, _ := registerAndGet(t, doUser, "hist2@example.com")

	gen := genCodes(t, doAdmin, `{"type":"balance","value":100,"remark":"r1","count":2}`)
	c1, c2 := gen.Codes[0], gen.Codes[1]
	rec := doUser(http.MethodPost, "/api/user/redemptions", `{"code":"`+c1.Code+`"}`, token)
	require.Equal(t, 200, rec.Code, "user1 redeem: %s", rec.Body.String())
	rec = doUser(http.MethodPost, "/api/user/redemptions", `{"code":"`+c2.Code+`"}`, token2)
	require.Equal(t, 200, rec.Code, "user2 redeem: %s", rec.Body.String())

	// 管理面审计：码 1 恰 1 条记录（面值出参 USD 回显）
	rec = doAdmin(http.MethodGet, fmt.Sprintf("/api/admin/redemption-codes/%d/uses", c1.ID), "", "")
	require.Equal(t, 200, rec.Code, "admin uses: %s", rec.Body.String())
	var uses RedemptionUseListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &uses))
	require.Equal(t, int64(1), uses.Total)
	require.Equal(t, 100.0, uses.Rows[0].Value, "审计面值 USD 回显")

	// 用户面我的兑换记录：仅本人 1 条，含码的 type/remark（联查视图）
	rec = doUser(http.MethodGet, "/api/user/redemptions", "", token)
	require.Equal(t, 200, rec.Code, "my redemptions: %s", rec.Body.String())
	var mine userapi.RedemptionRecordListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &mine))
	require.Equal(t, int64(1), mine.Total, "仅本人记录（另一用户兑换不入列表）")
	require.Equal(t, c1.Code, mine.Rows[0].Code)
	require.Equal(t, userapi.RedemptionType("balance"), mine.Rows[0].CodeType)
	require.Equal(t, 100.0, mine.Rows[0].Value, "兑换记录面值 USD 回显")
	require.NotNil(t, mine.Rows[0].Remark)
	require.Equal(t, "r1", *mine.Rows[0].Remark)

	// 增强分页参数绑定（用户面）
	rec = doUser(http.MethodGet, "/api/user/redemptions?page=1&page_size=20&sort=id&order=desc", "", token)
	require.Equal(t, 200, rec.Code, "paged: %s", rec.Body.String())
}

// TestRedemptionValueUSDConversion 面值 API 边界换算（存储毫分不变）：
// balance/temp_balance 入参 USD → 存储毫分（1 USD = 100,000 毫分）；concurrency
// 并发数直存直出；出参回显换算（10 USD ↔ 1,000,000 毫分 roundtrip）。
func TestRedemptionValueUSDConversion(t *testing.T) {
	doAdmin, _, store := newSharedRouters(t)

	// balance：入参 10 USD → 存储 1,000,000 毫分；出参回显 10
	gen := genCodes(t, doAdmin, `{"type":"balance","value":10}`)
	require.Equal(t, 10.0, gen.Codes[0].Value, "出参 USD 回显")
	stored, err := store.GetCode(context.Background(), gen.Codes[0].ID)
	require.NoError(t, err)
	require.Equal(t, int64(1000000), stored.Value, "存储恒毫分（API 边界换算）")

	// temp_balance：入参 0.005 USD（= 500 毫分）→ 存储 500；出参回显 0.005
	gen = genCodes(t, doAdmin, `{"type":"temp_balance","value":0.005,"resource_expires_at":"2030-01-01T00:00:00Z"}`)
	require.Equal(t, 0.005, gen.Codes[0].Value, "出参 USD 回显")
	stored, err = store.GetCode(context.Background(), gen.Codes[0].ID)
	require.NoError(t, err)
	require.Equal(t, int64(500), stored.Value, "存储恒毫分")

	// concurrency：整数并发数直存直出（非货币，不换算）
	gen = genCodes(t, doAdmin, `{"type":"concurrency","value":5}`)
	require.Equal(t, 5.0, gen.Codes[0].Value, "出参并发数直出")
	stored, err = store.GetCode(context.Background(), gen.Codes[0].ID)
	require.NoError(t, err)
	require.Equal(t, int64(5), stored.Value, "存储恒并发数")

	// concurrency 小数 → 400（不静默取整）
	rec := doAdmin(http.MethodPost, "/api/admin/redemption-codes", `{"type":"concurrency","value":5.5}`, "")
	require.Equal(t, 400, rec.Code, "concurrency 小数必须 400: %s", rec.Body.String())
	require.Contains(t, errMsg(t, rec), "integer", "400 文案说明整数要求")
}
