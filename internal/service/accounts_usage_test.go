// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/sdkbridge"
)

// perAccountSnap 逐账号可编程快照数据源（AccountsUsage 装配矩阵注入面——
// 按账号区分成功/两种失败）。
type perAccountSnap struct {
	mu    sync.Mutex
	snaps map[int64]*domain.CodexUsageSnapshot
	errs  map[int64]error
}

func (p *perAccountSnap) GetUsageSnapshot(ctx context.Context, cred *domain.AccountCredential) (*domain.CodexUsageSnapshot, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snaps[cred.AccountID], p.errs[cred.AccountID]
}

// TestAccountsUsageAssembly upstream 装配矩阵（/admin/accounts/usage 查询面）：
// api-key 无凭据 → null 快照/null 标记（"缺失"不进 auth_expired）；codex 成功 →
// 快照/null；codex 失败 fatal → null/auth_expired；失败上游 → null/
// upstream_unavailable；**单账号失败不整批失败**——其余账号照常返回。
// 同时覆盖：items 顺序 = ids 顺序、无记录账号 gateway 补零。
func TestAccountsUsageAssembly(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore()
	// usage_logs 种子（a1/a2 有记录；a5 无记录）
	base := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	f.logs = []*domain.UsageLog{
		{RequestID: "u1", AccountID: 1, Format: domain.FormatOpenAIChat, ErrorType: domain.ErrNone, TotalTokens: 100, Cost: 1000, RawCost: 2000, CreatedAt: base},
		{RequestID: "u2", AccountID: 2, Format: domain.FormatOpenAIChat, ErrorType: domain.ErrNone, TotalTokens: 50, Cost: 500, RawCost: 600, CreatedAt: base},
	}
	// codex 账号 ext 行（2 = 成功、3 = fatal、4 = 上游失败）
	f.accExts[2] = &domain.AccountExt{AccountID: 2, CredentialType: credential.TypeCodexPAT, CodexPATKey: strPtr("pat-ok")}
	f.accExts[3] = &domain.AccountExt{AccountID: 3, CredentialType: credential.TypeCodexPAT, CodexPATKey: strPtr("pat-bad")}
	f.accExts[4] = &domain.AccountExt{AccountID: 4, CredentialType: credential.TypeCodexPAT, CodexPATKey: strPtr("pat-net")}
	snap := &perAccountSnap{
		snaps: map[int64]*domain.CodexUsageSnapshot{2: {PlanType: "chatgpt-plus"}},
		errs:  map[int64]error{3: sdkbridge.ErrAuthExpired, 4: sdkbridge.ErrUpstream},
	}
	svc := &Service{store: f}
	svc.SetUsageSnapshotter(snap)

	from := base.Add(-time.Hour)
	to := base.Add(time.Hour)
	items, err := svc.AccountsUsage(ctx, []int64{1, 2, 3, 4, 5}, from, to)
	require.NoError(t, err, "单账号 upstream 失败不整批失败")
	require.Len(t, items, 5, "items 恒 = ids 全量（顺序 = ids 顺序）")
	ids := []int64{1, 2, 3, 4, 5}
	for i, it := range items {
		require.Equal(t, ids[i], it.AccountID, "items 顺序 = ids 顺序")
	}

	// a1：api-key（无 ext 行）→ gateway 聚合 + nil/nil
	require.Equal(t, int64(1), items[0].Gateway.Requests)
	require.Equal(t, int64(1000), items[0].Gateway.Cost)
	require.Equal(t, int64(2000), items[0].Gateway.RawCost)
	require.Equal(t, int64(100), items[0].Gateway.TotalTokens)
	require.Nil(t, items[0].Upstream, "api-key 无凭据 → nil 快照")
	require.Nil(t, items[0].UpstreamError, "api-key 无凭据 → nil 标记（缺失不进 auth_expired）")

	// a2：codex 成功 → 快照/nil
	require.NotNil(t, items[1].Upstream)
	require.Equal(t, "chatgpt-plus", items[1].Upstream.PlanType)
	require.Nil(t, items[1].UpstreamError)

	// a3：fatal → null/auth_expired
	require.Nil(t, items[2].Upstream)
	require.NotNil(t, items[2].UpstreamError)
	require.Equal(t, domain.UpstreamErrorAuthExpired, *items[2].UpstreamError)

	// a4：上游错误 → null/upstream_unavailable
	require.Nil(t, items[3].Upstream)
	require.NotNil(t, items[3].UpstreamError)
	require.Equal(t, domain.UpstreamErrorUpstreamUnavailable, *items[3].UpstreamError)

	// a5：无记录 + api-key → gateway 全 0 + nil/nil（前端免补零）
	require.Equal(t, int64(0), items[4].Gateway.Requests)
	require.Equal(t, int64(0), items[4].Gateway.Cost)
	require.Equal(t, int64(0), items[4].Gateway.RawCost)
	require.Equal(t, int64(0), items[4].Gateway.TotalTokens)
	require.Nil(t, items[4].Upstream)
	require.Nil(t, items[4].UpstreamError)
}

// usageBatchResult 装配调用结果通道载体（err 真实透传断言——不静默吞掉）。
type usageBatchResult struct {
	items []domain.AccountUsage
	err   error
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

// TestAccountsUsageParallelAssembly 批内并行装配（T2-1/N1）：N 账号 errgroup
// 有界并发（8）——闸门证明 ≥2 账号快照并发在途（串行装配恒 1）；结果正确 +
// 顺序 = ids 顺序稳定 + 单账号失败不整批失败。
func TestAccountsUsageParallelAssembly(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore()
	base := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	for i := int64(1); i <= 8; i++ {
		f.accExts[i] = &domain.AccountExt{AccountID: i, CredentialType: credential.TypeCodexPAT, CodexPATKey: strPtr("pat-ok")}
	}
	snap := &gatedSnap{
		twoInFlight: make(chan struct{}),
		release:     make(chan struct{}),
		snaps:       map[int64]*domain.CodexUsageSnapshot{},
		errs:        map[int64]error{4: sdkbridge.ErrAuthExpired}, // 单账号失败
	}
	for i := int64(1); i <= 8; i++ {
		snap.snaps[i] = &domain.CodexUsageSnapshot{PlanType: "chatgpt-plus"}
	}
	svc := &Service{store: f}
	svc.SetUsageSnapshotter(snap)

	release := sync.OnceFunc(func() { close(snap.release) })
	t.Cleanup(release) // 失败路径防 goroutine 泄漏
	done := make(chan usageBatchResult)
	go func() {
		items, err := svc.AccountsUsage(ctx, []int64{1, 2, 3, 4, 5, 6, 7, 8}, base.Add(-time.Hour), base.Add(time.Hour))
		done <- usageBatchResult{items: items, err: err}
	}()

	select {
	case <-snap.twoInFlight:
		// 并行实际发生
	case <-time.After(5 * time.Second):
		t.Fatal("批内装配未并行（5s 内无 ≥2 并发在途）")
	}
	release()
	res := <-done
	require.NoError(t, res.err, "装配错误必须真实断言（不静默吞掉）")
	items := res.items

	require.GreaterOrEqual(t, snap.max(), 2, "批内快照装配并行（errgroup 有界并发）")
	ids := []int64{1, 2, 3, 4, 5, 6, 7, 8}
	for i, it := range items {
		require.Equal(t, ids[i], it.AccountID, "items 顺序 = ids 顺序（并行装配保序）")
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

// TestAccountsUsageStoreErrorIsolated store 故障隔离（T2-2）：GetAccountExt
// 非 ErrNotFound 错误 → 该账号 upstream null + upstream_error null（不误标
// 上游问题）+ 批内其余账号正常（整批不失败）。
func TestAccountsUsageStoreErrorIsolated(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore()
	f.accExts[1] = &domain.AccountExt{AccountID: 1, CredentialType: credential.TypeCodexPAT, CodexPATKey: strPtr("pat-ok")}
	f.accExts[2] = &domain.AccountExt{AccountID: 2, CredentialType: credential.TypeCodexPAT, CodexPATKey: strPtr("pat-ok")}
	f.accExtErr[2] = errors.New("store down") // 非 ErrNotFound store 故障
	snap := &perAccountSnap{
		snaps: map[int64]*domain.CodexUsageSnapshot{1: {PlanType: "chatgpt-plus"}},
	}
	svc := &Service{store: f}
	svc.SetUsageSnapshotter(snap)

	items, err := svc.AccountsUsage(ctx, []int64{1, 2}, time.Now().Add(-time.Hour), time.Now())
	require.NoError(t, err, "store 故障不整批失败")
	require.NotNil(t, items[0].Upstream, "其余账号正常")
	require.Nil(t, items[0].UpstreamError)
	require.Nil(t, items[1].Upstream, "store 故障账号 → null 快照")
	require.Nil(t, items[1].UpstreamError, "store 故障账号 → null 标记（不误标上游问题）")
}

// TestAccountsUsageTimeFiltering 时间过滤透传（半开区间 [from, to)——边界语义
// 由 repo/fake 承担；service 直透不做二次过滤——仅断言透传）。
func TestAccountsUsageTimeFiltering(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore()
	base := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	f.logs = []*domain.UsageLog{
		{RequestID: "t1", AccountID: 1, Format: domain.FormatOpenAIChat, ErrorType: domain.ErrNone, CreatedAt: base.Add(time.Hour)},
	}
	svc := &Service{store: f}
	items, err := svc.AccountsUsage(ctx, []int64{1}, base, base.Add(30*time.Minute))
	require.NoError(t, err)
	require.Equal(t, int64(0), items[0].Gateway.Requests, "窗外行不计入（透传半开区间）")
	items, err = svc.AccountsUsage(ctx, []int64{1}, base, base.Add(2*time.Hour))
	require.NoError(t, err)
	require.Equal(t, int64(1), items[0].Gateway.Requests, "窗内行计入")
}
