// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"testing"

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

// TestRuntimeHealthSetProbeFnBackfillsProbe：装配序里 RuntimeHealth 先于其
// 依赖（codex 适配器构造需要 Health 进 FailureDeps）——Set* 事后回填是项目
// 装配惯例。契约：nil probe 恒失败（fail-closed）；回填后 doProbe 分发到注入
// 函数；必须在 Start 之前调用（Start 后 probe 循环并发读 probeFn，事后回填
// 构成数据竞争）。
func TestRuntimeHealthSetProbeFnBackfillsProbe(t *testing.T) {
	_, c := newHealthTestRedis(t)
	h := NewRuntimeHealth(c, "self-a", nil, nil, nil)

	err := h.doProbe(context.Background(), HealthKey{AccountID: 1, Quality: "*", Revision: 2})
	require.Error(t, err, "nil probe must fail-closed")

	called := false
	h.SetProbeFn(func(_ context.Context, key HealthKey) error {
		require.Equal(t, int64(1), key.AccountID)
		called = true
		return nil
	})
	require.NoError(t, h.doProbe(context.Background(), HealthKey{AccountID: 1, Quality: "*", Revision: 2}))
	require.True(t, called, "backfilled probe fn must be dispatched by doProbe")
}
