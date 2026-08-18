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
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/scheduler"
)

// fakeRuntimeProvider 小 fake（A-4，评审 O-2——service 包既有测试全部 sched=nil
// 构造，本测试首建注入先例；接口仅 Runtime 用，Runtimes 零值实现）。
type fakeRuntimeProvider struct {
	ri scheduler.RuntimeInfo
	ok bool
}

func (f *fakeRuntimeProvider) Runtime(accountID int64) (scheduler.RuntimeInfo, bool) { return f.ri, f.ok }
func (f *fakeRuntimeProvider) Runtimes() []scheduler.AccountRuntime                  { return nil }

// TestListAccountViewsMergesRuntimeStatus A-4：列表显示 = 调度器内存权威——
// DB active + 内存 429+冷却 → AccountView.Status/CooldownUntil 与 Runtime 一致
//（回写丢失/失败时管理端不再显示 DB 镜像的 active——用户报告"active 却恒 429"
// 的展示盲区）；Concurrency/ErrRate/ErrCount 既有合并回归。sched 未装配
//（既有构造）与 Runtime 未命中 → 回退 DB 值（既有行为不变）。
func TestListAccountViewsMergesRuntimeStatus(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	tpl, err := fs.CreateTemplate(ctx, &domain.Template{
		Name: "t", BaseURL: "https://t.example.com",
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
	})
	require.NoError(t, err)
	acc, err := fs.CreateAccount(ctx, &domain.Account{
		Name: "a1", TemplateID: tpl.ID, UpstreamKey: "sk-1", Status: domain.StatusActive,
	})
	require.NoError(t, err)
	require.Equal(t, domain.StatusActive, acc.Status, "DB 落 active（回写丢失场景的数据源形态）")

	cd := time.Now().Add(5 * time.Hour)
	fake := &fakeRuntimeProvider{ri: scheduler.RuntimeInfo{
		Status: domain.Status429, CooldownUntil: &cd,
		Concurrency: 2, ErrRate: 0.5, ErrCount: 3,
	}, ok: true}
	svc := &Service{store: fs, sched: fake, log: nil}

	views, total, err := svc.ListAccountViews(ctx, repository.ListQuery{})
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, views, 1)
	v := views[0]
	require.Equal(t, domain.Status429, v.Status, "DB active + 内存 429 → 显示 429（内存权威）")
	require.Equal(t, &cd, v.CooldownUntil, "冷却随内存合并")
	require.Equal(t, int64(2), v.Concurrency, "并发合并（既有）")
	require.Equal(t, 0.5, v.ErrRate, "err_rate 合并（既有）")
	require.Equal(t, 3, v.ErrCount, "err_count 合并（既有）")

	// sched 未装配（既有构造形态）→ 回退 DB 值
	svcNil := &Service{store: fs, log: nil}
	views, _, err = svcNil.ListAccountViews(ctx, repository.ListQuery{})
	require.NoError(t, err)
	require.Equal(t, domain.StatusActive, views[0].Status, "sched nil → DB 值（既有行为）")
	require.Nil(t, views[0].CooldownUntil)

	// Runtime 未命中（快照外账号/快照未加载）→ 回退 DB 值
	svcMiss := &Service{store: fs, sched: &fakeRuntimeProvider{ok: false}, log: nil}
	views, _, err = svcMiss.ListAccountViews(ctx, repository.ListQuery{})
	require.NoError(t, err)
	require.Equal(t, domain.StatusActive, views[0].Status, "Runtime 未命中 → DB 值")
	require.Nil(t, views[0].CooldownUntil)
}
