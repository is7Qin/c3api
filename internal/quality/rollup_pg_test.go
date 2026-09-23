// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

// rollup worker 真实 PG 集成：走完整链路——quality/flow 事实落 instance 分钟
// 表（同事务标 dirty）→ worker.runOnce 经选择缝滚成 rollup 表 + watermark
// 推进 + dirty 清除 → 读面（QueryQuality/FlowRollupStats）可见聚合行；二轮
// 纯 no-op。未设 TEST_DATABASE_URL 则 skip（全仓 PG 测试契约）。独立 schema
// （quality_rollup_test）与同库其它 PG 测试隔离。

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

const rollupPGTestSchema = "quality_rollup_test"

func rollupPGRepos(t *testing.T) *repository.Repository {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	if strings.Contains(dsn, "?") {
		dsn += "&search_path=" + rollupPGTestSchema
	} else {
		dsn += "?search_path=" + rollupPGTestSchema
	}
	ctx := context.Background()
	pool, err := repository.OpenPG(ctx, dsn, 5)
	require.NoError(t, err)
	t.Cleanup(pool.Close)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.ExecContext(ctx, `DROP SCHEMA IF EXISTS `+rollupPGTestSchema+` CASCADE; CREATE SCHEMA `+rollupPGTestSchema+`;`)
	require.NoError(t, err)
	// 收尾必须 DROP 本 schema：routing 表若残留，同库 information_schema.columns
	// 按表名计数会翻倍，打爆 repository 侧分区 bootstrap 测试的列存在断言。
	t.Cleanup(func() {
		_, _ = db.ExecContext(context.Background(), `DROP SCHEMA IF EXISTS `+rollupPGTestSchema+` CASCADE;`)
	})
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, db), false)
	require.NoError(t, err)
	return repos
}

func TestRollupWorkerPG(t *testing.T) {
	repos := rollupPGRepos(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, now))

	rc, err := domain.RouteClassID(1, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	require.NoError(t, err)
	qc, err := domain.QualityClassID(domain.CallerChat, domain.FormatOpenAIChat, "gpt-4o", domain.OpChatCompletions)
	require.NoError(t, err)
	fp, err := domain.CandidateFingerprint(1, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-one", "", "", "", false, "inst", "sess", "thr", "win")
	require.NoError(t, err)

	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, repository.RoutingQualityRow{
		IdentityVersion: int16(domain.RoutingIdentityVersion), RouteClassID: rc, QualityClassID: qc,
		CandidateFingerprint: fp, InstanceSrc: "pg-rollup-1", BucketMinute: now,
		AbsoluteSequence: 1, Attempts: 5, Successes: 3, Count429: 1, InputTokens: 100, Calls: 2,
	}))
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "pg-rollup-1", now, int16(domain.RoutingIdentityVersion), 1,
		[]repository.RoutingFlowRow{{
			IdentityVersion: int16(domain.RoutingIdentityVersion), RouteClassID: rc, TerminalMinute: now,
			Ordinal: 1, Lane: "primary", AccountID: 7, TransitionReason: "plan", Outcome: "success",
			IsTerminal: true, Generation: 1, ChainCount: 3,
		}}))

	dirtyQ, err := repos.Partitions.IsDirty(ctx, "quality", 1, now)
	require.NoError(t, err)
	require.True(t, dirtyQ, "前置：事实写入已标脏")

	w := NewRollupWorker(repos.Partitions, RollupConfig{Interval: time.Hour}, nil)
	w.runOnce(ctx)

	st := w.Stats().(RollupStats)
	require.Equal(t, int64(1), st.QualityRolled)
	require.Zero(t, st.Failed)
	require.Equal(t, now.UnixMilli(), st.WatermarkQualityUnixMs)

	// 状态推进：dirty 清除 + watermark 落位（成功事务的可观测契约）。
	dirtyQ, err = repos.Partitions.IsDirty(ctx, "quality", 1, now)
	require.NoError(t, err)
	require.False(t, dirtyQ)
	wmQ, err := repos.Partitions.GetWatermark(ctx, "quality", 1)
	require.NoError(t, err)
	require.True(t, wmQ.UTC().Truncate(time.Minute).Equal(now))

	// 读面可见聚合行（routing API 的数据源不再为空）。
	qStats, err := repos.Partitions.QueryQualityRollupStats(ctx, rc, 1, now, now.Add(time.Minute))
	require.NoError(t, err)
	require.Len(t, qStats, 1)
	require.Equal(t, int64(5), qStats[0].Attempts)
	require.Equal(t, int64(3), qStats[0].Successes)
	fStats, err := repos.Partitions.QueryFlowRollupStats(ctx, rc, 1, now, now.Add(time.Minute))
	require.NoError(t, err)
	require.Len(t, fStats, 1)
	require.Equal(t, int64(3), fStats[0].ChainCount)

	// 二轮 no-op：无脏分钟 → 不重复滚、watermark 不动。
	w.runOnce(ctx)
	st2 := w.Stats().(RollupStats)
	require.Equal(t, int64(1), st2.QualityRolled)

	// 迟到事实重标脏（同桶新 sequence > 旧）→ 等于 watermark 的桶可重算
	// （选择缝 >= 下界 + advanceWatermarkTx 接受相等）。
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, repository.RoutingQualityRow{
		IdentityVersion: int16(domain.RoutingIdentityVersion), RouteClassID: rc, QualityClassID: qc,
		CandidateFingerprint: fp, InstanceSrc: "pg-rollup-1", BucketMinute: now,
		AbsoluteSequence: 2, Attempts: 8, Successes: 5, Calls: 3,
	}))
	w.runOnce(ctx)
	qStats, err = repos.Partitions.QueryQualityRollupStats(ctx, rc, 1, now, now.Add(time.Minute))
	require.NoError(t, err)
	require.Len(t, qStats, 1)
	require.Equal(t, int64(8), qStats[0].Attempts, "重算覆盖（绝对值语义，非累加）")

	// watermark 之下的迟到脏分钟也必须被消费，且不能倒退 watermark。
	late := now.Add(-time.Hour)
	require.NoError(t, repos.Partitions.UpsertQualityAndMarkDirty(ctx, repository.RoutingQualityRow{
		IdentityVersion: int16(domain.RoutingIdentityVersion), RouteClassID: rc, QualityClassID: qc,
		CandidateFingerprint: fp, InstanceSrc: "pg-rollup-2", BucketMinute: late,
		AbsoluteSequence: 1, Attempts: 2, Successes: 1,
	}))
	w.runOnce(ctx)
	st3 := w.Stats().(RollupStats)
	require.Equal(t, int64(3), st3.QualityRolled, "低于 watermark 的脏分钟也被消费")
	require.Zero(t, st3.Failed)
	dirtyLate, err := repos.Partitions.IsDirty(ctx, "quality", 1, late)
	require.NoError(t, err)
	require.False(t, dirtyLate)
	wmQ, err = repos.Partitions.GetWatermark(ctx, "quality", 1)
	require.NoError(t, err)
	require.True(t, wmQ.UTC().Truncate(time.Minute).Equal(now), "迟到桶不倒退 watermark")
}
