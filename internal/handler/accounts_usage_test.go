// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/internal/service"
)

// hUsageSnap 逐账号可编程快照数据源（handler 装配矩阵注入面——service
// CodexUsageSnapshotter 接口）。
type hUsageSnap struct {
	snaps map[int64]*domain.CodexUsageSnapshot
	errs  map[int64]error
}

func (s *hUsageSnap) GetUsageSnapshot(ctx context.Context, cred *domain.AccountCredential) (*domain.CodexUsageSnapshot, error) {
	return s.snaps[cred.AccountID], s.errs[cred.AccountID]
}

// newUsageTestHandler /admin/accounts/usage 专用测试装配（h.now 注入 + 可编程
// 快照数据源）。
func newUsageTestHandler(t *testing.T, now time.Time, snap *hUsageSnap) (*AdminAPI, *fakeStore) {
	t.Helper()
	store := newFakeStore()
	svc := service.New(store, fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil)
	svc.SetUsageSnapshotter(snap)
	h := New(svc)
	h.now = func() time.Time { return now }
	return h, store
}

// getUsage 发 GET /admin/accounts/usage 请求。
func getUsage(h *AdminAPI, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/admin/accounts/usage?"+query, nil)
	rec := httptest.NewRecorder()
	h.Router().ServeHTTP(rec, req)
	return rec
}

// TestGetAccountsUsageValidation 参数校验矩阵：>100（去重后计）→ 400、空 ids →
// 400、非数字 → 400、重复去重唯一、非法时间格式 → 400、from>to → 400。
func TestGetAccountsUsageValidation(t *testing.T) {
	h, _ := newUsageTestHandler(t, time.Date(2026, 8, 18, 15, 30, 0, 0, time.UTC), &hUsageSnap{})

	ids101 := make([]string, 101)
	for i := range ids101 {
		ids101[i] = strconv.Itoa(i + 1)
	}
	dup101 := append(append([]string{}, ids101[:100]...), "1") // 100 唯一 + 1 重复 = 101 原始

	cases := []struct {
		name  string
		query string
		want  int
	}{
		{"缺 account_ids", "", 400},
		{"空串", "account_ids=", 400},
		{"非数字", "account_ids=abc", 400},
		{"混入非数字", "account_ids=1,2,x", 400},
		{"101 唯一 → 400", "account_ids=" + strings.Join(ids101, ","), 400},
		{"101 原始含重复 → 400（normalizeIDs 惯例：原始长度检查在前）", "account_ids=" + strings.Join(dup101, ","), 400},
		{"非法 from 格式", "account_ids=1&from=not-a-time", 400},
		{"非法 to 格式", "account_ids=1&to=2026-13-99T00:00:00Z", 400},
		{"from > to", "account_ids=1&from=2026-08-18T12:00:00Z&to=2026-08-18T11:00:00Z", 400},
		{"from == to", "account_ids=1&from=2026-08-18T11:00:00Z&to=2026-08-18T11:00:00Z", 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := getUsage(h, tc.query)
			require.Equal(t, tc.want, rec.Code, "body: %s", rec.Body.String())
		})
	}
}

// TestGetAccountsUsageDefaultsAndOrdering 缺省时间注入（from=UTC 当日零点、
// to=now——h.now 注入固定时钟，无真实 now 漂移）+ items 顺序 = 去重后
// account_ids 顺序 + 聚合与全量补零。
func TestGetAccountsUsageDefaultsAndOrdering(t *testing.T) {
	now := time.Date(2026, 8, 18, 15, 30, 0, 0, time.UTC)
	h, store := newUsageTestHandler(t, now, &hUsageSnap{})
	base := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC) // 当日窗内
	store.logs = []*domain.UsageLog{
		{RequestID: "d1", AccountID: 3, Format: domain.FormatOpenAIChat, ErrorType: domain.ErrNone, TotalTokens: 10, Cost: 100, RawCost: 200, CreatedAt: base},
		{RequestID: "d2", AccountID: 1, Format: domain.FormatOpenAIChat, ErrorType: domain.ErrNone, TotalTokens: 100, Cost: 1000, RawCost: 3000, CreatedAt: base},
		{RequestID: "d3", AccountID: 1, Format: domain.FormatOpenAIChat, ErrorType: domain.ErrNone, TotalTokens: 50, Cost: 500, RawCost: 1500, CreatedAt: base.Add(2 * time.Hour)},
	}

	// 重复 id（3,1,2,1）→ 去重后 [3,1,2]（顺序保持首次出现序）
	rec := getUsage(h, "account_ids=3,1,2,1")
	require.Equal(t, 200, rec.Code, "body: %s", rec.Body.String())

	var resp AccountsUsageResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Items, 3, "items 恒 = 去重后 ids 全量")
	require.Equal(t, []int64{3, 1, 2}, []int64{resp.Items[0].AccountId, resp.Items[1].AccountId, resp.Items[2].AccountId}, "items 顺序 = 去重后 account_ids 顺序")

	// 缺省时间注入断言（fake 收参 = handler 注入值）
	require.Equal(t, []int64{3, 1, 2}, store.aggIDs, "去重后 ids 传入 repo")
	require.True(t, store.aggFrom.Equal(now.UTC().Truncate(24*time.Hour)), "缺省 from = UTC 当日零点（%v）", store.aggFrom)
	require.True(t, store.aggTo.Equal(now), "缺省 to = now（注入时钟）（%v）", store.aggTo)

	// 聚合 + 补零：a3（1 行）、a1（2 行）、a2（无记录全 0）
	g3 := resp.Items[0].Gateway
	require.Equal(t, int64(1), g3.Requests)
	require.Equal(t, float64(0.001), g3.CostUsd, "100 毫分 /1e5 = 0.001 USD")
	require.Equal(t, float64(0.002), g3.RawCostUsd, "200 毫分 /1e5 = 0.002 USD")
	require.Equal(t, int64(10), g3.TotalTokens)
	g1 := resp.Items[1].Gateway
	require.Equal(t, int64(2), g1.Requests)
	require.Equal(t, float64(0.015), g1.CostUsd)
	require.Equal(t, float64(0.045), g1.RawCostUsd)
	require.Equal(t, int64(150), g1.TotalTokens)
	require.Equal(t, int64(0), resp.Items[2].Gateway.Requests, "无记录账号 gateway 全 0（前端免补零）")
	require.Equal(t, float64(0), resp.Items[2].Gateway.CostUsd)
	require.Equal(t, float64(0), resp.Items[2].Gateway.RawCostUsd)
	require.Equal(t, int64(0), resp.Items[2].Gateway.TotalTokens)
}

// TestGetAccountsUsageExplicitRange 显式 from/to 透传（不做"当天"注入）。
func TestGetAccountsUsageExplicitRange(t *testing.T) {
	h, store := newUsageTestHandler(t, time.Date(2026, 8, 18, 15, 30, 0, 0, time.UTC), &hUsageSnap{})
	from := "2026-08-17T00:00:00Z"
	to := "2026-08-17T23:59:59Z"
	rec := getUsage(h, "account_ids=1&from="+from+"&to="+to)
	require.Equal(t, 200, rec.Code, "body: %s", rec.Body.String())
	wantFrom, _ := time.Parse(time.RFC3339, from)
	wantTo, _ := time.Parse(time.RFC3339, to)
	require.True(t, store.aggFrom.Equal(wantFrom), "显式 from 透传（%v）", store.aggFrom)
	require.True(t, store.aggTo.Equal(wantTo), "显式 to 透传（%v）", store.aggTo)
}

// TestGetAccountsUsageUpstreamAssembly 响应面 upstream 装配（task 3 接线）：
// api-key → upstream:null + upstream_error:null 显式输出；codex 成功 → 快照；
// codex fatal → auth_expired；上游失败 → upstream_unavailable；单账号失败其余
// 正常（整响应 200）。
func TestGetAccountsUsageUpstreamAssembly(t *testing.T) {
	now := time.Date(2026, 8, 18, 15, 30, 0, 0, time.UTC)
	h, store := newUsageTestHandler(t, now, &hUsageSnap{
		snaps: map[int64]*domain.CodexUsageSnapshot{2: {
			PlanType:  "chatgpt-plus",
			Credits:   &domain.CodexCredits{Balance: strPtr("12.50")},
			RateLimit: &domain.CodexRateLimit{UsedPercent: 42}, // ResetAt nil（零值守卫）
		}},
		errs: map[int64]error{3: sdkbridge.ErrAuthExpired, 4: sdkbridge.ErrUpstream},
	})
	store.accExts[2] = &domain.AccountExt{AccountID: 2, CredentialType: credential.TypeCodexPAT, CodexPATKey: strPtr("pat-ok")}
	store.accExts[3] = &domain.AccountExt{AccountID: 3, CredentialType: credential.TypeCodexPAT, CodexPATKey: strPtr("pat-bad")}
	store.accExts[4] = &domain.AccountExt{AccountID: 4, CredentialType: credential.TypeCodexPAT, CodexPATKey: strPtr("pat-net")}

	rec := getUsage(h, "account_ids=1,2,3,4")
	require.Equal(t, 200, rec.Code, "单账号快照失败不整批失败: %s", rec.Body.String())
	var resp AccountsUsageResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Items, 4)

	// a1 api-key：upstream/upstream_error 显式 null（非缺席）
	require.Nil(t, resp.Items[0].Upstream)
	require.Nil(t, resp.Items[0].UpstreamError)
	require.Contains(t, rec.Body.String(), `"upstream":null`, "api-key upstream 显式 null")
	require.Contains(t, rec.Body.String(), `"upstream_error":null`, "api-key upstream_error 显式 null")

	// a2 codex 成功：快照直透（plan_type/credits.balance）+ error null
	require.NotNil(t, resp.Items[1].Upstream)
	require.NotNil(t, resp.Items[1].Upstream.PlanType)
	require.Equal(t, "chatgpt-plus", *resp.Items[1].Upstream.PlanType)
	require.NotNil(t, resp.Items[1].Upstream.Credits)
	require.NotNil(t, resp.Items[1].Upstream.Credits.Balance)
	require.Equal(t, "12.50", *resp.Items[1].Upstream.Credits.Balance)
	require.Nil(t, resp.Items[1].UpstreamError)
	// ResetAt nil → 契约层显式 null（非虚假 0001-01-01——T3-1 零值泄漏端到端修复）
	require.Contains(t, rec.Body.String(), `"reset_at":null`, "nil ResetAt → reset_at null")
	require.NotContains(t, rec.Body.String(), "0001-01-01", "零值时间戳不外泄")

	// a3 fatal → auth_expired
	require.Nil(t, resp.Items[2].Upstream)
	require.NotNil(t, resp.Items[2].UpstreamError)
	require.Equal(t, AccountUsageItemUpstreamError("auth_expired"), *resp.Items[2].UpstreamError)

	// a4 上游失败 → upstream_unavailable
	require.Nil(t, resp.Items[3].Upstream)
	require.NotNil(t, resp.Items[3].UpstreamError)
	require.Equal(t, AccountUsageItemUpstreamError("upstream_unavailable"), *resp.Items[3].UpstreamError)
}
