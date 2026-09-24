// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
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

// B2（cutoff 内晚到重算正确）：同一分钟第二个实例晚到 → 重标脏 → 整分钟幂等重算，
// rollup 反映两实例之和。quality 暂存层保留全分钟重算语义，故晚到天然正确
// （S1）；本用例断言该语义在观测保留期内成立（分钟取当前分钟，前置断言它确实
// 落在截止之内）。
func TestRoutingQualityLateArrivalRecomputeWithinRetentionPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	minute := now.Add(-time.Minute)
	require.True(t, minute.After(domain.RoutingObservationCutoff(now, 7)),
		"fixture precondition: the late minute must sit inside the 7-day observation cutoff")
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	rcA, _, qc1, _, fps := windowVals(t)
	fp := fps["cur"]

	row := func(instance string, attempts, successes, seq int64) repository.RoutingQualityRow {
		return repository.RoutingQualityRow{
			IdentityVersion: 1, RouteClassID: rcA, QualityClassID: qc1, CandidateFingerprint: fp,
			InstanceSrc: instance, BucketMinute: minute, AbsoluteSequence: seq,
			Attempts: attempts, Successes: successes, TTFTN: attempts,
		}
	}
	// 实例 A 先到并滚一次。
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, row("src-early", 10, 6, 1)))
	require.NoError(t, repos.Partitions.RollupQuality(ctx, minute, 1))
	var attempts int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT attempts FROM routing_quality_rollup WHERE bucket_minute=$1`, minute).Scan(&attempts))
	require.Equal(t, int64(10), attempts)

	// 实例 B 晚到同一分钟：重标脏 → 重算整分钟。
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, row("src-late", 4, 3, 1)))
	dirty, err := repos.Partitions.IsDirty(ctx, "quality", 1, minute)
	require.NoError(t, err)
	require.True(t, dirty, "late arrival must re-mark the minute dirty")
	require.NoError(t, repos.Partitions.RollupQuality(ctx, minute, 1))
	require.NoError(t, pool.QueryRow(ctx, `SELECT attempts FROM routing_quality_rollup WHERE bucket_minute=$1`, minute).Scan(&attempts))
	require.Equal(t, int64(14), attempts, "whole-minute recompute must include the late instance")
}

// B4：dirty 有界（双向负例）。提交前删 dirty=false 且**严格早于**水位的行；
// dirty=true 的旧分钟（晚到重算待办）与恰好 == 水位的行必须存活。
func TestRoutingDirtyMinuteBoundedCleanupPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	m0, m1, m2, m3 := now.Add(-3*time.Minute), now.Add(-2*time.Minute), now.Add(-time.Minute), now
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	rcA, _, qc1, _, fps := windowVals(t)
	fp := fps["cur"]

	// 四个分钟各自一行事实 + 脏位。
	for i, minute := range []time.Time{m0, m1, m2, m3} {
		require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, repository.RoutingQualityRow{
			IdentityVersion: 1, RouteClassID: rcA, QualityClassID: qc1, CandidateFingerprint: fp,
			InstanceSrc: "src-b4", BucketMinute: minute, AbsoluteSequence: int64(i + 1),
			Attempts: 1, Successes: 1,
		}))
	}

	// 滚 m1 → 水位 = m1；清理不删任何行（没有更老的 clean 行）。
	require.NoError(t, repos.Partitions.RollupQuality(ctx, m1, 1))
	require.Equal(t, int64(4), dirtyRowCount(t, ctx, pool, "quality"))

	// 滚 m3 → 水位 = m3；m1（dirty=false 且 < 水位）被删，m0/m2（仍 dirty）与
	// m3（== 水位）存活。
	require.NoError(t, repos.Partitions.RollupQuality(ctx, m3, 1))
	var minutes []time.Time
	rows, err := pool.Query(ctx, `SELECT bucket_minute FROM routing_dirty_minute WHERE kind='quality' ORDER BY bucket_minute`)
	require.NoError(t, err)
	for rows.Next() {
		var b time.Time
		require.NoError(t, rows.Scan(&b))
		minutes = append(minutes, b.UTC())
	}
	rows.Close()
	require.NoError(t, rows.Err())
	require.Equal(t, []time.Time{m0, m2, m3}, minutes,
		"clean minute strictly below the watermark is deleted; dirty older minutes and the watermark minute survive")
}

// dirtyRowCount 统计 dirty 表某 kind 的行数（有界性断言用）。
func dirtyRowCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, kind string) int64 {
	t.Helper()
	var n int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_dirty_minute WHERE kind=$1`, kind).Scan(&n))
	return n
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
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_flow_rollup WHERE instance_src=$1 AND terminal_minute=$2`, "src-B5", now).Scan(&edgeRows))
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

// B6：watermark / compiler_state 构造有界——主键/单行约束 + 行数断言。flow 道
// 删除后 watermark 只剩 quality 一行；compiler_state 由 CHECK (id = 1) 锁死单行；
// dirty 表不再出现 flow 行。
func TestRoutingStateTablesConstructivelyBoundedPG(t *testing.T) {
	repos, pool := newRoutingRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))
	rcA, _, qc1, _, fps := windowVals(t)
	fp := fps["cur"]

	// flow 快照不再写 dirty（S3：dirty 仅剩 quality）。
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-B6", now, 1, 1, []repository.RoutingFlowRow{{
		IdentityVersion: 1, RouteClassID: rcA, TerminalMinute: now, Ordinal: 1, Lane: "primary",
		AccountID: 10, PreviousOutcome: "", TransitionReason: "init", Outcome: "success",
		IsTerminal: true, Generation: 1, ChainCount: 1,
	}}))
	require.Equal(t, int64(0), dirtyRowCount(t, ctx, pool, "flow"))

	// watermark：滚一个 quality 分钟 → 恰好一行（kind=quality），重复滚不增行。
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, repository.RoutingQualityRow{
		IdentityVersion: 1, RouteClassID: rcA, QualityClassID: qc1, CandidateFingerprint: fp,
		InstanceSrc: "src-B6", BucketMinute: now, AbsoluteSequence: 1, Attempts: 1, Successes: 1,
	}))
	require.NoError(t, repos.Partitions.RollupQuality(ctx, now, 1))
	var watermarkRows int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_rollup_watermark`).Scan(&watermarkRows))
	require.Equal(t, int64(1), watermarkRows, "watermark holds exactly one row after the flow lane was removed")
	var kinds []string
	rows, err := pool.Query(ctx, `SELECT kind FROM routing_rollup_watermark`)
	require.NoError(t, err)
	for rows.Next() {
		var k string
		require.NoError(t, rows.Scan(&k))
		kinds = append(kinds, k)
	}
	rows.Close()
	require.NoError(t, rows.Err())
	require.Equal(t, []string{"quality"}, kinds)

	// compiler_state：PK (id, identity_version) + CHECK (id = 1)。
	for i := 0; i < 2; i++ {
		_, err = pool.Exec(ctx, `INSERT INTO routing_compiler_state (id, identity_version, desired_generation, published_generation, updated_at) VALUES (1, 1, $1, $1, now()) ON CONFLICT (id, identity_version) DO UPDATE SET desired_generation = EXCLUDED.desired_generation`, int64(i+1))
		require.NoError(t, err)
	}
	var compilerRows int64
	require.NoError(t, pool.QueryRow(ctx, `SELECT COUNT(*) FROM routing_compiler_state`).Scan(&compilerRows))
	require.Equal(t, int64(1), compilerRows, "compiler_state is a single row by primary key")
	_, err = pool.Exec(ctx, `INSERT INTO routing_compiler_state (id, identity_version, desired_generation, published_generation, updated_at) VALUES (2, 1, 1, 1, now())`)
	require.Error(t, err, "the id = 1 CHECK must forbid a second compiler_state row")
	require.Contains(t, err.Error(), "check constraint")
}
