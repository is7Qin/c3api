// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/scheduler"
)

// fakeRuntimeProvider 小 fake（service 包既有测试全部 sched=nil
// 构造，本测试首建注入先例；接口仅 Runtime 用，Runtimes 零值实现）。
type fakeRuntimeProvider struct {
	ri scheduler.RuntimeInfo
	ok bool
}

func (f *fakeRuntimeProvider) Runtime(accountID int64) (scheduler.RuntimeInfo, bool) {
	return f.ri, f.ok
}
func (f *fakeRuntimeProvider) Runtimes() []scheduler.AccountRuntime { return nil }

// TestListAccountViewsMergesRuntimeMetrics 列表视图的运行时指标合并：
// Concurrency/ErrRate/ErrCount 与调度器内存同源（快照原子读）；sched 未装配
// 与 Runtime 未命中 → 零值回退。生命周期（enabled/failed_at）直接来自持久
// 字段，视图不做运行时覆盖。
func TestListAccountViewsMergesRuntimeMetrics(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	tpl, err := fs.CreateTemplate(ctx, &domain.Template{
		Name: "t", BaseURL: "https://t.example.com",
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
	})
	require.NoError(t, err)
	_, err = fs.CreateAccount(ctx, &domain.Account{
		Name: "a1", TemplateID: tpl.ID, UpstreamKey: "sk-1", Enabled: true,
	})
	require.NoError(t, err)

	fake := &fakeRuntimeProvider{ri: scheduler.RuntimeInfo{
		Concurrency: 2, ErrRate: 0.5, ErrCount: 3,
	}, ok: true}
	svc := &Service{store: fs, sched: fake, log: nil}

	views, total, err := svc.ListAccountViews(ctx, repository.ListQuery{})
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, views, 1)
	v := views[0]
	require.Equal(t, int64(2), v.Concurrency, "并发合并（内存权威）")
	require.Equal(t, 0.5, v.ErrRate, "err_rate 合并")
	require.Equal(t, 3, v.ErrCount, "err_count 合并")
	require.True(t, v.Enabled, "生命周期字段直取持久值")

	// sched 未装配（既有构造形态）→ 指标零值
	svcNil := &Service{store: fs, log: nil}
	views, _, err = svcNil.ListAccountViews(ctx, repository.ListQuery{})
	require.NoError(t, err)
	require.Zero(t, views[0].Concurrency, "sched nil → 零值")

	// Runtime 未命中（快照外账号/快照未加载）→ 指标零值
	svcMiss := &Service{store: fs, sched: &fakeRuntimeProvider{ok: false}, log: nil}
	views, _, err = svcMiss.ListAccountViews(ctx, repository.ListQuery{})
	require.NoError(t, err)
	require.Zero(t, views[0].Concurrency, "Runtime 未命中 → 零值")
}
