// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
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
	f.accExts[2] = &domain.AccountExt{AccountID: 2, CredentialType: credential.TypeCodexPAT, PATKey: strPtr("pat-ok")}
	f.accExts[3] = &domain.AccountExt{AccountID: 3, CredentialType: credential.TypeCodexPAT, PATKey: strPtr("pat-bad")}
	f.accExts[4] = &domain.AccountExt{AccountID: 4, CredentialType: credential.TypeCodexPAT, PATKey: strPtr("pat-net")}
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
