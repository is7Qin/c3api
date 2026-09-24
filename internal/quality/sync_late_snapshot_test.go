// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/logx"
)

// retentionRejectingPG 让 flow 快照写入被观测保留守卫拒绝（哨兵），quality 面
// 不参与本用例。
type retentionRejectingPG struct{}

func (retentionRejectingPG) UpsertQualityRow(context.Context, repository.RoutingQualityRow) error {
	return nil
}

func (retentionRejectingPG) UpsertFlowSnapshot(context.Context, string, time.Time, int16, int64, []repository.RoutingFlowRow) error {
	return repository.ErrRoutingSnapshotBeyondRetention
}

// B5（可观测面）：超出观测保留期的快照被拒后必须**可观测地丢弃**，不得照抄
// "旧序号静默 no-op"的形状（那是幂等，正确；超期是数据丢失）。
//
// 三处可观测面各断言一次：
//  1. 专用哨兵 repository.ErrRoutingSnapshotBeyondRetention（PG 侧用例断言）
//  2. Warn 日志（带 minute + instance_src）
//  3. 进程丢弃计数 quality.RoutingLateSnapshotDropped()
//
// 外加"不再重试"：该分钟分区已被 retention DROP，重试永不成功，故结清租约
// （否则每轮 flush 都会 Warn + 计数，把可观测性变成刷屏）。
func TestQualitySync_LateSnapshotBeyondRetentionIsObservableDrop(t *testing.T) {
	_, rdb := newMiniRedis(t)
	rec, err := NewRecorder(50000)
	require.NoError(t, err)
	fixed := time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
	minute := fixed.Unix()

	// 日志落盘捕获（仓库既有惯例：目录自建自清，清理失败不阻断——Windows 上
	// zap 仍持有句柄，故 RemoveAll 的错误被忽略）。
	logDir, err := os.MkdirTemp("", "quality-late-snapshot-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(logDir) })
	logPath := filepath.Join(logDir, "sync.log")
	logger, err := logx.New("warn", logPath)
	require.NoError(t, err)

	w := NewSyncWorker(rec, rdb, retentionRejectingPG{}, SyncConfig{InstanceSrc: "src-late-drop", BatchSize: 10}, logger, nil)
	w.SetClock(func() time.Time { return fixed })
	require.NoError(t, foldConsumerRows(rec.FlowOwner(), minute, []repository.RoutingFlowRow{{
		IdentityVersion: 1, TerminalMinute: fixed, Ordinal: 1, Lane: "primary",
		AccountID: 7, TransitionReason: "init", Outcome: "success", IsTerminal: true, Generation: 1, ChainCount: 3,
	}}))

	before := RoutingLateSnapshotDropped()
	w.doPG(context.Background())
	require.Equal(t, before+1, RoutingLateSnapshotDropped(),
		"the process drop counter must count the rejected snapshot exactly once")

	require.NoError(t, logger.Sync())
	raw, err := os.ReadFile(logPath)
	require.NoError(t, err)
	logText := string(raw)
	require.Contains(t, logText, "dropped beyond observation retention", "the drop must be warned, not silent")
	require.Contains(t, logText, fixed.Format(time.RFC3339), "the warn must carry the minute")
	require.Contains(t, logText, "src-late-drop", "the warn must carry instance_src")

	require.Empty(t, rec.FlowOwner().pgCandidateMinutes(),
		"a rejected snapshot must be settled, not retried every cycle forever")
}
