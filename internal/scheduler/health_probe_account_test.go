// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestSchedulerProbeAccountComposesSnapshotAuthority（blocker：probe 执行面需要
// "选号门与探测同一权威视图"的取号入口）：
//   - 已发布快照中的账号必须可解析，返回静态快照只读拷贝（Template/revision/
//     UpstreamKey/Ext 齐备，非裸写发布视图）。
//   - 快照外账号（已删/未加载）返回 ok=false——探测侧 fail-closed，stale
//     PROBING 记录不可能经缺失账号变 READY。
func TestSchedulerProbeAccountComposesSnapshotAuthority(t *testing.T) {
	chat := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"})
	s := newTestScheduler(t, []*domain.Account{acc(1, chat, 4)})

	got, ok := s.ProbeAccount(1)
	require.True(t, ok, "snapshot account must be probe-resolvable")
	require.NotNil(t, got.Template, "ProbeAccount must carry the template")
	require.Equal(t, int64(1), got.ID)
	require.Equal(t, int64(1), got.LifecycleRevision)
	require.Equal(t, "k", got.UpstreamKey)

	_, ok = s.ProbeAccount(42)
	require.False(t, ok, "unknown account must fail-closed for the probe")
}

// TestRuntimeHealthStartInjectsProbe：probe 是 Start 期依赖（组合根在
// sched/codex 就绪后构造真 probe，Start 期一次性交接，无回填）。契约：nil
// probe 恒失败（fail-closed）；Start 交接后 doProbe 分发到注入函数。
func TestRuntimeHealthStartInjectsProbe(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h := NewRuntimeHealth(c, "self-a", nil, nil)

	err := h.doProbe(context.Background(), HealthKey{AccountID: 1, Quality: "*", IdentityRevision: 2})
	require.Error(t, err, "nil probe must fail-closed")

	called := false
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, h.Start(ctx, func(_ context.Context, key HealthKey) error {
		require.Equal(t, int64(1), key.AccountID)
		called = true
		return nil
	}))
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer closeCancel()
		require.NoError(t, h.Close(closeCtx))
	})
	require.NoError(t, h.doProbe(context.Background(), HealthKey{AccountID: 1, Quality: "*", IdentityRevision: 2}))
	require.True(t, called, "Start-injected probe fn must be dispatched by doProbe")
}
