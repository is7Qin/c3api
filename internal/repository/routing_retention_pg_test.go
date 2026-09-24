// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/scheduler"
)

// A2：窗口与地板同源。按 scheduler.BaselineLookback 播种：基线窗
// [M-BaselineLookback, M-CurrentWindowLen) 内的行被读到，M-BaselineLookback-1m
// 与 M-CurrentWindowLen（半开上界）不被读到；当前窗 [M-CurrentWindowLen, M)
// 同理（下界含、M 不含）。
//
// 播种偏移全部由共享常量推导（不写 24h/5m 字面量）——常量若被改动，本测试的
// 边界会随之移动，而断言仍然成立；这正是"同源"的可观测形式。
// A2 的 config 子句（observation_retention_days=1 启动失败、=2 通过）由
// internal/config 的 TestRoutingObservationRetentionFloor 覆盖。
func TestRoutingQualityWindowBoundarySeedPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	m := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, m))
	require.NoError(t, repos.Partitions.EnsureRoutingFactPartitions(ctx, m.Add(-scheduler.BaselineLookback-24*time.Hour), m.Add(24*time.Hour)))

	rcA, _, qc1, _, fps := windowVals(t)
	fp := fps["cur"]
	baseFrom := m.Add(-scheduler.BaselineLookback)                                          // 基线窗下界（含）
	baseTo := m.Add(-scheduler.CurrentWindowLen)                                            // 基线窗上界（半开，不含）
	curFrom := m.Add(-scheduler.CurrentWindowLen)                                           // 当前窗下界（含）
	seedQualityFactRow(t, pool, rcA, qc1, fp, "src-seed", baseFrom.Add(-time.Minute), 7, 7) // 基线窗外（更老）
	seedQualityFactRow(t, pool, rcA, qc1, fp, "src-seed", baseFrom, 1, 1)                   // 基线窗内下界
	seedQualityFactRow(t, pool, rcA, qc1, fp, "src-seed", baseTo.Add(-time.Minute), 1, 1)   // 基线窗内上界-1m
	seedQualityFactRow(t, pool, rcA, qc1, fp, "src-seed", baseTo, 7, 7)                     // 基线窗外上界（== 当前窗下界，含）
	seedQualityFactRow(t, pool, rcA, qc1, fp, "src-seed", curFrom.Add(time.Minute), 5, 5)   // 当前窗内
	seedQualityFactRow(t, pool, rcA, qc1, fp, "src-seed", m, 7, 7)                          // 当前窗上界（不含）

	keys := []repository.WindowHotKey{{RouteClassID: rcA, Fingerprint: fp}}
	baseline, err := repos.Partitions.QueryBaselineTruncated(ctx, 1, m, keys)
	require.NoError(t, err)
	require.Len(t, baseline, 1)
	require.Equal(t, int64(2), baseline[0].Attempts,
		"baseline must read only the two rows inside [M-BaselineLookback, M-CurrentWindowLen)")
	require.Equal(t, int64(2), baseline[0].Successes)

	// 当前窗 [M-CurrentWindowLen, M)：下界含（该分钟正是基线窗的半开上界——两窗
	// 无缝无重叠），M 本身不含。
	cur, err := repos.Partitions.QueryCurrentWindowStats(ctx, 1, m)
	require.NoError(t, err)
	require.Len(t, cur, 1)
	require.Equal(t, int64(12), cur[0].Attempts,
		"current window must read the boundary minute (inclusive lower bound) plus the inner row, and exclude M")
}

// B5（存储面）：snapshot_state 有界。cutoff 前被删、cutoff 内 state 存活且旧序号
// 重放仍**静默**被拒（幂等语义）；cutoff 外的写入被**拒**且返回专用哨兵
// （可观测的 Warn + 计数在 quality 侧断言——见 quality 包的同名用例）。
func TestRoutingFlowSnapshotStateBoundedPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	rc := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	repos.Partitions.SetRoutingObservationRetentionDays(7)

	rows := []repository.RoutingFlowRow{{
		IdentityVersion: 1, RouteClassID: rc, TerminalMinute: now, Ordinal: 1, Lane: "primary",
		AccountID: 10, PreviousOutcome: "", TransitionReason: "init", Outcome: "success",
		IsTerminal: true, Generation: 1, ChainCount: 5,
	}}
	// cutoff 内写入成功，state 建立。
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-B5", now, 1, 2, rows))
	var seq int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT highest_sequence FROM routing_flow_snapshot_state WHERE instance_src=$1 AND terminal_minute=$2`, "src-B5", now).Scan(&seq))
	require.Equal(t, int64(2), seq)

	// cutoff 内旧序号重放：**静默**被拒（tx.Commit 返回 nil 的幂等语义，不是错误）。
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-B5", now, 1, 1, nil))
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-B5", now, 1, 2, nil))
	require.NoError(t, pool.QueryRow(ctx, `SELECT highest_sequence FROM routing_flow_snapshot_state WHERE instance_src=$1 AND terminal_minute=$2`, "src-B5", now).Scan(&seq))
	require.Equal(t, int64(2), seq, "replayed older/equal sequence must not advance state")
	var edgeRows int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_fact WHERE instance_src=$1 AND terminal_minute=$2`, "src-B5", now).Scan(&edgeRows))
	require.Equal(t, int64(1), edgeRows, "replayed snapshot must not mutate the shard")

	// cutoff 外写入：拒绝 + 哨兵（1 天余量，时钟抖动无法翻转判定）。
	outside := now.AddDate(0, 0, -8)
	err := repos.Partitions.UpsertFlowSnapshot(ctx, "src-B5", outside, 1, 1, rows)
	require.ErrorIs(t, err, repository.ErrRoutingSnapshotBeyondRetention)
	var outsideStates int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_snapshot_state WHERE terminal_minute=$1`, outside).Scan(&outsideStates))
	require.Equal(t, int64(0), outsideStates, "rejected snapshot must not leave state behind (no re-create after cleanup)")

	// 清理：cutoff 前的遗留 state 行被删，cutoff 内的存活。
	_, err = pool.Exec(ctx, `INSERT INTO routing_flow_snapshot_state (terminal_minute, instance_src, identity_version, highest_sequence, updated_at) VALUES ($1, 'src-B5', 1, 9, now())`, outside)
	require.NoError(t, err)
	n, err := repos.Partitions.DeleteRoutingFlowSnapshotStateBefore(ctx, domain.RoutingObservationCutoff(now, 7))
	require.NoError(t, err)
	require.Equal(t, 1, n, "exactly the beyond-cutoff state row is removed")
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_snapshot_state WHERE terminal_minute=$1`, outside).Scan(&outsideStates))
	require.Equal(t, int64(0), outsideStates)
	var insideStates int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_snapshot_state WHERE terminal_minute=$1`, now).Scan(&insideStates))
	require.Equal(t, int64(1), insideStates, "state inside the cutoff must survive the sweep")
}

// B6：compiler_state 构造有界——CHECK (id = 1) + 主键锁死单行，行数断言。
func TestRoutingStateTablesConstructivelyBoundedPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))

	// compiler_state：PK (id, identity_version) + CHECK (id = 1)。
	for i := 0; i < 2; i++ {
		_, err := pool.Exec(ctx, `INSERT INTO routing_compiler_state (id, identity_version, desired_generation, published_generation, updated_at) VALUES (1, 1, $1, $1, now()) ON CONFLICT (id, identity_version) DO UPDATE SET desired_generation = EXCLUDED.desired_generation`, int64(i+1))
		require.NoError(t, err)
	}
	var compilerRows int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_compiler_state`).Scan(&compilerRows))
	require.Equal(t, int64(1), compilerRows, "compiler_state is a single row by primary key")
	_, err := pool.Exec(ctx, `INSERT INTO routing_compiler_state (id, identity_version, desired_generation, published_generation, updated_at) VALUES (2, 1, 1, 1, now())`)
	require.Error(t, err, "the id = 1 CHECK must forbid a second compiler_state row")
	require.Contains(t, err.Error(), "check constraint")
}
