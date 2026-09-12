// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/quality"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/logx"
)

// shutdownLogLine 是停机尾部行为测试的日志观测单元：zap JSON 单行解出的
// level/msg/error 三字段——"排空失败被显式上报"与"未宣称 clean shutdown"
// 都以机器可断言的日志面为契约。
type shutdownLogLine struct {
	Level string
	Msg   string
	Err   string
}

func readShutdownLog(t *testing.T, path string) []shutdownLogLine {
	t.Helper()
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	var lines []shutdownLogLine
	for _, ln := range strings.Split(string(content), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		var raw map[string]any
		require.NoError(t, json.Unmarshal([]byte(ln), &raw), "log line must be JSON: %s", ln)
		line := shutdownLogLine{}
		if s, ok := raw["level"].(string); ok {
			line.Level = s
		}
		if s, ok := raw["msg"].(string); ok {
			line.Msg = s
		}
		if s, ok := raw["error"].(string); ok {
			line.Err = s
		}
		lines = append(lines, line)
	}
	return lines
}

func newShutdownTestLogger(t *testing.T) (*logx.Logger, string) {
	t.Helper()
	// 不用 t.TempDir：zap 文件 sink 的句柄经 logx 无法释放（cfg.Build 不外露
	// closer），Windows 上打开中的文件令 TempDir 清理 RemoveAll 报错并使测试
	// 失败。独立临时目录 + 尽力而为清理（删除失败留一枚小日志文件于 OS 临时
	// 目录——升级路径：logx 提供 Close 后可回归 t.TempDir）。
	dir, err := os.MkdirTemp("", "c3api-shutdown-tail-")
	require.NoError(t, err)
	path := filepath.Join(dir, "shutdown.log")
	l, err := logx.New("info", path)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = l.Sync()
		_ = os.RemoveAll(dir)
	})
	return l, path
}

func testByte32(b byte) [32]byte {
	var a [32]byte
	for i := range a {
		a[i] = b
	}
	return a
}

// shutdownFakePG 是 quality.PGQualityWriter 的测试替身：failAll 注入持续
// 瞬时失败（非 poison，与真实 PG 不可达同形）；成功路径记录收到的行，供
// "clean 宣称属实"断言。
type shutdownFakePG struct {
	fail  error
	rows  []repository.RoutingQualityRow
	flows int
}

func (f *shutdownFakePG) UpsertQualityAndMarkDirty(_ context.Context, row repository.RoutingQualityRow) error {
	if f.fail != nil {
		return f.fail
	}
	f.rows = append(f.rows, row)
	return nil
}

func (f *shutdownFakePG) UpsertFlowSnapshot(_ context.Context, _ string, _ time.Time, _ int16, _ int64, rows []repository.RoutingFlowRow) error {
	if f.fail != nil {
		return f.fail
	}
	f.flows += len(rows)
	return nil
}

// newShutdownFixture 装配真实 quality-sync 组件：miniredis + 开放的
// recorder（一条 pending quality 行 + 一条 pending flow minute）+ 注入
// 失败模式的 PG fake + 持有该 worker 的 Manager（日志写独立临时文件，
// 供行为断言读取）。
func newShutdownFixture(t *testing.T, fail error) (wm *worker.Manager, rec *quality.Recorder, rdb *redis.Client, pg *shutdownFakePG, minute int64, log *logx.Logger, logPath string) {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb = redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	pg = &shutdownFakePG{fail: fail}
	rec, err := quality.NewRecorder(50000)
	require.NoError(t, err)

	fixed := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	w := quality.NewSyncWorker(rec, rdb, pg, quality.SyncConfig{InstanceSrc: "shutdown-tail-test", BatchSize: 10}, nil)
	w.SetClock(func() time.Time { return fixed })

	minute = fixed.Unix()
	k := quality.CanonicalKey(testByte32(11), testByte32(13), testByte32(11))
	qm := quality.NewQualityMinute(minute, k)
	qm.SetAttempts(3)
	qm.SetSuccesses(1)
	require.NoError(t, rec.EnqueueQualityMinute(qm))
	// v3-hygiene: edges-array ingestion is deleted — stage the pending flow
	// minute through the live FoldChain path (single terminal success edge),
	// exactly as production's proxy fold owner emits it.
	route, err := domain.RouteClassID(1, domain.FormatOpenAIChat, "m", domain.OpChatCompletions)
	require.NoError(t, err)
	fp, err := domain.CandidateFingerprint(1, 1, "api_key", "https://api.openai.com", "sk-upstream", "", "", "", false, "", "", "", "")
	require.NoError(t, err)
	rec.FlowOwner().FoldChain(minute, 1, func(i int) (domain.RouteClassIDVal, domain.CandidateFingerprintVal, int64, int64, int64, uint8, string, string, string, bool, bool) {
		return route, fp, 10, 0, 5, 1, "primary", "success", "", true, false
	})

	log, logPath = newShutdownTestLogger(t)
	wm = worker.New(log)
	require.NoError(t, wm.Register(w), "quality-sync worker must be registered with the manager")
	return wm, rec, rdb, pg, minute, log, logPath
}

// TestShutdownTail_IncompleteQualitySyncDrainNotClaimedClean 锁定 review
// blocker 的行为契约：quality-sync 排空不完整（PG 持续失败）时，
//   - 失败以 error 级日志显式上报（错误链含 "worker quality-sync close"
//     与 "drain incomplete"），不再 `_ =` 静默；
//   - 终态不宣称 "shutdown complete"；
//   - recorder 终态快照如实保留未落库 pending（finalization 不粉饰为
//     成功排空的终态）。
func TestShutdownTail_IncompleteQualitySyncDrainNotClaimedClean(t *testing.T) {
	// Given: 持续瞬时失败的 PG（非 poison），recorder 内一条 pending
	// quality 行 + 一条 pending flow minute。
	wm, rec, rdb, _, minute, log, logPath := newShutdownFixture(t, context.DeadlineExceeded)

	// When: 停机尾部在有限预算内运行（worker drain → recorder 收尾 →
	// Redis 释放）。
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	shutdownTail(shutdownCtx, wm, rec, rdb, log)
	require.NoError(t, log.Sync())

	// Then: 不完整的排空在 error 级显式可观测，错误链可归因到
	// quality-sync 的 drain incomplete。
	lines := readShutdownLog(t, logPath)
	drainSurfaced := false
	for _, ln := range lines {
		if ln.Level == "error" && strings.Contains(ln.Err, "drain incomplete") {
			drainSurfaced = true
			require.Contains(t, ln.Msg, "shutdown incomplete")
			require.Contains(t, ln.Err, "worker quality-sync close")
			require.Contains(t, ln.Err, "context deadline exceeded")
		}
	}
	require.True(t, drainSurfaced, "incomplete drain must be surfaced at error level, got %v", lines)

	// And: 终态不宣称干净退出。
	for _, ln := range lines {
		require.NotEqual(t, "shutdown complete", ln.Msg, "clean-shutdown claim forbidden after incomplete drain")
	}

	// And: 未落库 pending 保留在 recorder 终态快照——不是被粉饰掉的空终态。
	snap := rec.Snapshot()
	require.NotNil(t, snap)
	require.Len(t, snap.Quality, 1, "final snapshot must retain the unflushed quality minute")
	require.Contains(t, snap.Quality, minute)
	require.Len(t, snap.Flow, 1, "final snapshot must retain the unflushed flow minute")
	for _, rows := range snap.Quality {
		for _, qm := range rows {
			require.Equal(t, int64(3), qm.Attempts(), "retained row must be the original pending data")
			require.Equal(t, int64(1), qm.Successes())
		}
	}
}

// TestShutdownTail_CleanDrainClaimsShutdownComplete 反向锚定同一契约的
// 成功路径：排空真实落 PG fake（clean 宣称属实），终态 quality 快照为空而
// flow 快照保留已落库的 clean 分钟（单 owner 累计语义：成功不再 detach），
// "shutdown complete" 在场且无 error 级日志。
func TestShutdownTail_CleanDrainClaimsShutdownComplete(t *testing.T) {
	// Given: 健康 PG fake——quality-sync 的 Close 排空将把 pending 真实
	// 写入 fake。
	wm, rec, rdb, pg, minute, log, logPath := newShutdownFixture(t, nil)

	// When: 停机尾部运行（排空一次通过，无需超时预算）。
	shutdownTail(context.Background(), wm, rec, rdb, log)
	require.NoError(t, log.Sync())

	// Then: 排空真实落库——clean 宣称有事实支撑。
	require.Len(t, pg.rows, 1, "pending quality row must be flushed to PG on clean drain")
	require.Equal(t, int64(3), pg.rows[0].Attempts)
	require.Equal(t, int64(1), pg.rows[0].Successes)
	require.NotZero(t, pg.flows, "pending flow minute must be flushed to PG on clean drain")

	// And: 终态 quality 快照为空（无未落库残留），flow 快照保留已落库的
	// clean 分钟供诊断，clean 宣称在场，无 error 级日志。
	snap := rec.Snapshot()
	require.NotNil(t, snap)
	require.Empty(t, snap.Quality)
	require.Len(t, snap.Flow, 1, "final snapshot retains the clean flow minute for diagnostics")
	fm, ok := snap.Flow[minute]
	require.True(t, ok, "retained clean minute must be addressable by its minute")
	// v3-hygiene: rows are the live representation (edges-array input deleted) —
	// the retained minute carries the single folded success edge.
	rows := fm.FlowRows()
	require.Len(t, rows, 1, "retained minute preserves the folded edge")
	require.Equal(t, int64(10), rows[0].AccountID)
	require.Equal(t, "success", rows[0].Outcome)
	require.Equal(t, "primary", rows[0].Lane)
	require.Equal(t, int64(5), rows[0].Generation)
	require.Equal(t, int64(1), rows[0].ChainCount)

	lines := readShutdownLog(t, logPath)
	var msgs []string
	for _, ln := range lines {
		require.NotEqual(t, "error", ln.Level, "clean drain must not emit error-level logs, got %v", lines)
		msgs = append(msgs, ln.Msg)
	}
	require.Contains(t, msgs, "shutdown complete")
}
