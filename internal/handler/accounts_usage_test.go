// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/internal/service"
)

// hUsageSnap 逐账号可编程快照数据源（handler 装配矩阵注入面——handler
// CodexUsageProber 接口，经 OpsOptions.UsageSnap 构造注入）。
type hUsageSnap struct {
	snaps map[int64]*domain.CodexUsageSnapshot
	errs  map[int64]error
}

func (s *hUsageSnap) GetUsageSnapshot(ctx context.Context, cred *domain.AccountCredential) (*domain.CodexUsageSnapshot, error) {
	return s.snaps[cred.AccountID], s.errs[cred.AccountID]
}

// newUsageTestHandler /api/admin/accounts/usage 专用测试装配（h.now 注入 + 可编程
// 快照数据源）。请求时区经 `timezone` 查询参数注入（request-tz：无装配级时区）。
func newUsageTestHandler(t *testing.T, now time.Time, snap CodexUsageProber) (*AdminAPI, *fakeStore) {
	t.Helper()
	store := newFakeStore()
	svc := service.New(store, fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil, service.ServiceDeps{EmailCodeStore: store})
	h := New(svc, OpsOptions{UsageSnap: snap})
	h.now = func() time.Time { return now }
	return h, store
}

// getUsage 发 GET /api/admin/accounts/usage 请求。
func getUsage(h *AdminAPI, query string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/admin/accounts/usage?"+query, nil)
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

// TestGetAccountsUsageDefaultsAndOrdering 缺省时间注入（from=统计时区当日零点，
// 未配置 = UTC；to=now——h.now 注入固定时钟，无真实 now 漂移）+ items 顺序 =
// 去重后 account_ids 顺序 + 聚合与全量补零。
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

// TestGetAccountsUsageUpstreamAssembly 响应面 upstream 装配：
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
	// ResetAt nil → 契约层显式 null（非虚假 0001-01-01——零值泄漏端到端修复）
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

// TestGetAccountsUsageNilProber 未装配降级（UsageSnap nil = 旧 service
// nil-setter 语义）：codex 账号 → upstream/upstream_error 双 null（不 panic，
// 不整批失败）。生产组合根恒装配 codexAdapter，此路径生产不可达。
func TestGetAccountsUsageNilProber(t *testing.T) {
	now := time.Date(2026, 8, 18, 15, 30, 0, 0, time.UTC)
	h, store := newUsageTestHandler(t, now, nil)
	store.accExts[1] = &domain.AccountExt{AccountID: 1, CredentialType: credential.TypeCodexPAT, CodexPATKey: strPtr("pat-ok")}

	rec := getUsage(h, "account_ids=1")
	require.Equal(t, 200, rec.Code, "body: %s", rec.Body.String())
	var resp AccountsUsageResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Len(t, resp.Items, 1)
	require.Nil(t, resp.Items[0].Upstream, "未装配 → codex 账号 null 快照")
	require.Nil(t, resp.Items[0].UpstreamError)
}

// TestAssembleUpstreamStoreErrorIsolated store 故障隔离（原 service 侧
// 用例搬迁）：GetAccountExt 非 ErrNotFound 错误 → 该账号 upstream null +
// upstream_error null（不误标上游问题）+ 批内其余账号正常。
func TestAssembleUpstreamStoreErrorIsolated(t *testing.T) {
	h, store := newUsageTestHandler(t, time.Date(2026, 8, 18, 15, 30, 0, 0, time.UTC), &hUsageSnap{
		snaps: map[int64]*domain.CodexUsageSnapshot{1: {PlanType: "chatgpt-plus"}},
	})
	store.accExts[1] = &domain.AccountExt{AccountID: 1, CredentialType: credential.TypeCodexPAT, CodexPATKey: strPtr("pat-ok")}
	store.accExtErr[2] = errors.New("store down") // 非 ErrNotFound store 故障

	items := []domain.AccountUsage{{AccountID: 1}, {AccountID: 2}}
	h.assembleUpstream(context.Background(), items)
	require.NotNil(t, items[0].Upstream, "其余账号正常")
	require.Nil(t, items[0].UpstreamError)
	require.Nil(t, items[1].Upstream, "store 故障账号 → null 快照")
	require.Nil(t, items[1].UpstreamError, "store 故障账号 → null 标记（不误标上游问题）")
}

// gatedSnap 批内并行装配注入面：并发在途计数 + 闸门（第 2 个并发调用到达即
// close twoInFlight 信号——断言并行实际发生，channel 同步禁 sleep）。
type gatedSnap struct {
	mu          sync.Mutex
	inflight    int
	maxInFlight int
	signaled    bool
	twoInFlight chan struct{}
	release     chan struct{}
	snaps       map[int64]*domain.CodexUsageSnapshot
	errs        map[int64]error
}

func (g *gatedSnap) GetUsageSnapshot(ctx context.Context, cred *domain.AccountCredential) (*domain.CodexUsageSnapshot, error) {
	g.mu.Lock()
	g.inflight++
	if g.inflight > g.maxInFlight {
		g.maxInFlight = g.inflight
	}
	// 首达 2 即发信号（inflight 可因闸门放行后回落再回升——仅首次 close）
	if g.inflight == 2 && !g.signaled {
		g.signaled = true
		close(g.twoInFlight)
	}
	g.mu.Unlock()
	<-g.release
	g.mu.Lock()
	g.inflight--
	g.mu.Unlock()
	return g.snaps[cred.AccountID], g.errs[cred.AccountID]
}

func (g *gatedSnap) max() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.maxInFlight
}

// TestAssembleUpstreamParallel 批内并行装配（原 service 侧用例搬迁）：
// N 账号 errgroup 有界并发（8）——闸门证明 ≥2 账号快照并发在途（串行装配恒
// 1）；结果正确 + 顺序 = ids 顺序稳定 + 单账号失败不整批失败。
func TestAssembleUpstreamParallel(t *testing.T) {
	snap := &gatedSnap{
		twoInFlight: make(chan struct{}),
		release:     make(chan struct{}),
		snaps:       map[int64]*domain.CodexUsageSnapshot{},
		errs:        map[int64]error{4: sdkbridge.ErrAuthExpired}, // 单账号失败
	}
	for i := int64(1); i <= 8; i++ {
		snap.snaps[i] = &domain.CodexUsageSnapshot{PlanType: "chatgpt-plus"}
	}
	h, store := newUsageTestHandler(t, time.Date(2026, 8, 18, 15, 30, 0, 0, time.UTC), snap)
	for i := int64(1); i <= 8; i++ {
		store.accExts[i] = &domain.AccountExt{AccountID: i, CredentialType: credential.TypeCodexPAT, CodexPATKey: strPtr("pat-ok")}
	}
	items := make([]domain.AccountUsage, 0, 8)
	for i := int64(1); i <= 8; i++ {
		items = append(items, domain.AccountUsage{AccountID: i})
	}

	release := sync.OnceFunc(func() { close(snap.release) })
	t.Cleanup(release) // 失败路径防 goroutine 泄漏
	done := make(chan struct{})
	go func() {
		h.assembleUpstream(context.Background(), items)
		close(done)
	}()

	select {
	case <-snap.twoInFlight:
		// 并行实际发生
	case <-time.After(5 * time.Second):
		t.Fatal("批内装配未并行（5s 内无 ≥2 并发在途）")
	}
	release()
	<-done

	require.GreaterOrEqual(t, snap.max(), 2, "批内快照装配并行（errgroup 有界并发）")
	for i, it := range items {
		require.Equal(t, int64(i+1), it.AccountID, "items 顺序 = ids 顺序（并行装配保序）")
	}
	for i := range items {
		if items[i].AccountID == 4 {
			require.Nil(t, items[i].Upstream)
			require.NotNil(t, items[i].UpstreamError)
			require.Equal(t, domain.UpstreamErrorAuthExpired, *items[i].UpstreamError)
			continue
		}
		require.NotNil(t, items[i].Upstream, "账号 %d 快照正常", items[i].AccountID)
		require.Nil(t, items[i].UpstreamError)
	}
}
func strPtr(s string) *string { return &s }
