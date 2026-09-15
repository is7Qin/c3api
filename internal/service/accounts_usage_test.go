// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestAccountsGatewayUsageAssembly 网关聚合组装（/api/admin/accounts/usage
// 查询面的 gateway 栏）：items 恒 = ids 全量（顺序 = ids 顺序）、无记录账号
// gateway 补零。upstream 栏由 handler fan-out 另行装配（见 handler 侧测试）。
func TestAccountsGatewayUsageAssembly(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore()
	// usage_logs 种子（a1/a2 有记录；a5 无记录）
	base := time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)
	f.logs = []*domain.UsageLog{
		{RequestID: "u1", AccountID: 1, Format: domain.FormatOpenAIChat, ErrorType: domain.ErrNone, TotalTokens: 100, Cost: 1000, RawCost: 2000, CreatedAt: base},
		{RequestID: "u2", AccountID: 2, Format: domain.FormatOpenAIChat, ErrorType: domain.ErrNone, TotalTokens: 50, Cost: 500, RawCost: 600, CreatedAt: base},
	}
	svc := &Service{store: f}

	from := base.Add(-time.Hour)
	to := base.Add(time.Hour)
	items, err := svc.AccountsGatewayUsage(ctx, []int64{1, 2, 5}, from, to)
	require.NoError(t, err)
	require.Len(t, items, 3, "items 恒 = ids 全量（顺序 = ids 顺序）")
	ids := []int64{1, 2, 5}
	for i, it := range items {
		require.Equal(t, ids[i], it.AccountID, "items 顺序 = ids 顺序")
	}

	// a1：gateway 聚合
	require.Equal(t, int64(1), items[0].Gateway.Requests)
	require.Equal(t, int64(1000), items[0].Gateway.Cost)
	require.Equal(t, int64(2000), items[0].Gateway.RawCost)
	require.Equal(t, int64(100), items[0].Gateway.TotalTokens)

	// a5：无记录 → gateway 全 0（前端免补零）
	require.Equal(t, int64(0), items[2].Gateway.Requests)
	require.Equal(t, int64(0), items[2].Gateway.Cost)
	require.Equal(t, int64(0), items[2].Gateway.RawCost)
	require.Equal(t, int64(0), items[2].Gateway.TotalTokens)
	require.Nil(t, items[2].Upstream)
	require.Nil(t, items[2].UpstreamError)
}

// TestAccountsGatewayUsageTimeFiltering 时间过滤透传（半开区间 [from, to)——
// 边界语义由 repo/fake 承担；service 直透不做二次过滤——仅断言透传）。
func TestAccountsGatewayUsageTimeFiltering(t *testing.T) {
	ctx := context.Background()
	f := newFakeStore()
	base := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	f.logs = []*domain.UsageLog{
		{RequestID: "t1", AccountID: 1, Format: domain.FormatOpenAIChat, ErrorType: domain.ErrNone, CreatedAt: base.Add(time.Hour)},
	}
	svc := &Service{store: f}
	items, err := svc.AccountsGatewayUsage(ctx, []int64{1}, base, base.Add(30*time.Minute))
	require.NoError(t, err)
	require.Equal(t, int64(0), items[0].Gateway.Requests, "窗外行不计入（透传半开区间）")
	items, err = svc.AccountsGatewayUsage(ctx, []int64{1}, base, base.Add(2*time.Hour))
	require.NoError(t, err)
	require.Equal(t, int64(1), items[0].Gateway.Requests, "窗内行计入")
}
