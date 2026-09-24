// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package usage

// retention worker 调度测试：短 ticker + fake PartitionManager 记录调用参数。

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

var errBoom = errors.New("boom")

// fakePartitionManager 四表计数 fake（usage_logs / err_logs / usage_stats /
// usage_entity_stats 各自独立计数——四表独立调度断言：cutoff 独立、失败隔离；
// entity_stats 与 usage_stats 共用 StatsRetentionDays 同一循环 DROP+预建）。
type fakePartitionManager struct {
	mu        sync.Mutex
	drops     []time.Time // usage_logs cutoff 参数
	nows      []time.Time // usage_logs ensure 的 now 参数
	ensures   []time.Time // usage_logs ensure 的 until 参数
	edrops    []time.Time // err_logs cutoff 参数
	enows     []time.Time // err_logs ensure 的 now 参数
	eensures  []time.Time // err_logs ensure 的 until 参数
	sdrops    []time.Time // usage_stats cutoff 参数
	snows     []time.Time // usage_stats ensure 的 now 参数
	sensures  []time.Time // usage_stats ensure 的 until 参数
	esdrops   []time.Time // usage_entity_stats cutoff 参数（与 usage_stats 同 StatsRetentionDays）
	esnows    []time.Time // usage_entity_stats ensure 的 now 参数
	esensures []time.Time // usage_entity_stats ensure 的 until 参数
	rdeletes  []time.Time // redemption_uses 批删 cutoff 参数
	rdrops    []time.Time // routing 观测分区 drops（quality fact / flow fact 同一 cutoff）
	rstateDel []time.Time // routing_flow_snapshot_state 有界删 cutoff 参数
	rnows     []time.Time // routing fact ensures
	dropErr   error       // usage_logs drop 失败注入
	edropErr  error       // err_logs drop 失败注入（失败隔离断言）
	sdropErr  error       // usage_stats drop 失败注入（失败隔离断言）
	esdropErr error       // usage_entity_stats drop 失败注入（失败隔离断言，与 sdrop 独立）
	rdelErr   error       // redemption_uses 批删失败注入（失败隔离断言）
	ensureErr error

	rpartCount  int       // RoutingFactPartitionStats 回传的分区数
	rpartOldest time.Time // RoutingFactPartitionStats 回传的最老分区下界（零值 = 无分区）
	rpartErr    error     // RoutingFactPartitionStats 失败注入（失败不覆盖上轮值断言）
}

func (f *fakePartitionManager) DropUsageLogPartitionsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.drops = append(f.drops, cutoff)
	return 0, f.dropErr
}

func (f *fakePartitionManager) EnsureUsageLogPartitions(ctx context.Context, now, until time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nows = append(f.nows, now)
	f.ensures = append(f.ensures, until)
	return f.ensureErr
}

func (f *fakePartitionManager) DropErrLogPartitionsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edrops = append(f.edrops, cutoff)
	return 0, f.edropErr
}

func (f *fakePartitionManager) EnsureErrLogPartitions(ctx context.Context, now, until time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.enows = append(f.enows, now)
	f.eensures = append(f.eensures, until)
	return f.ensureErr
}

func (f *fakePartitionManager) DropUsageStatsPartitionsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sdrops = append(f.sdrops, cutoff)
	return 0, f.sdropErr
}

func (f *fakePartitionManager) EnsureUsageStatsPartitions(ctx context.Context, now, until time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snows = append(f.snows, now)
	f.sensures = append(f.sensures, until)
	return f.ensureErr
}

func (f *fakePartitionManager) DropUsageEntityStatsPartitionsBefore(ctx context.Context, cutoff time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.esdrops = append(f.esdrops, cutoff)
	return 0, f.esdropErr
}

func (f *fakePartitionManager) EnsureUsageEntityStatsPartitions(ctx context.Context, now, until time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.esnows = append(f.esnows, now)
	f.esensures = append(f.esensures, until)
	return f.ensureErr
}

func (f *fakePartitionManager) DeleteRedemptionUsesBefore(ctx context.Context, cutoff time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rdeletes = append(f.rdeletes, cutoff)
	return 0, f.rdelErr
}

func (f *fakePartitionManager) EnsureRoutingFactPartitions(ctx context.Context, now, until time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rnows = append(f.rnows, now)
	return f.ensureErr
}
func (f *fakePartitionManager) DropRoutingQualityFactBefore(ctx context.Context, cutoff time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rdrops = append(f.rdrops, cutoff)
	return 0, nil
}
func (f *fakePartitionManager) DropRoutingFlowFactBefore(ctx context.Context, cutoff time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rdrops = append(f.rdrops, cutoff)
	return 0, nil
}
func (f *fakePartitionManager) DeleteRoutingFlowSnapshotStateBefore(ctx context.Context, cutoff time.Time) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rstateDel = append(f.rstateDel, cutoff)
	return 0, nil
}
func (f *fakePartitionManager) RoutingFactPartitionStats(ctx context.Context) (int, time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.rpartCount, f.rpartOldest, f.rpartErr
}

func (f *fakePartitionManager) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.drops), len(f.ensures)
}

func (f *fakePartitionManager) errCounts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.edrops), len(f.eensures)
}

func (f *fakePartitionManager) statsCounts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sdrops), len(f.sensures)
}

func (f *fakePartitionManager) entityStatsCounts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.esdrops), len(f.esensures)
}

// waitCounts 轮询直到 drops/ensures 达到目标（短 ticker 调度断言）。
func waitCounts(t *testing.T, f *fakePartitionManager, wantDrops, wantEnsures int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		d, e := f.counts()
		if d >= wantDrops && e >= wantEnsures {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	d, e := f.counts()
	require.Failf(t, "worker did not tick in time", "drops=%d ensures=%d (want %d/%d)", d, e, wantDrops, wantEnsures)
}

func TestRetentionWorkerTicks(t *testing.T) {
	pm := &fakePartitionManager{}
	w := NewRetention(RetentionConfig{LogRetentionDays: 30, ErrLogRetentionDays: 7, StatsRetentionDays: 180, TickerInterval: 20 * time.Millisecond}, pm, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, w.Start(ctx))

	// 启动即巡检一次 + 至少 2 个 tick（drop + ensure 每轮都调——两表各自）
	waitCounts(t, pm, 2, 2)
	cancel()
	require.NoError(t, w.Close(ctx))

	pm.mu.Lock()
	defer pm.mu.Unlock()
	// cutoff 语义：usage_logs now - 30 天（日粒度，容忍 ±1 天边界）
	now := time.Now().UTC()
	cut := now.AddDate(0, 0, -30).Truncate(24 * time.Hour)
	got := pm.drops[0].UTC().Truncate(24 * time.Hour)
	require.True(t, cut.Equal(got) || cut.Add(-24*time.Hour).Equal(got) || cut.Add(24*time.Hour).Equal(got),
		"cutoff = now-30d 日粒度，got=%v want≈%v", pm.drops[0], cut)
	// err_logs cutoff 独立：now - 7 天（ErrLogRetentionDays 独立保留期）
	ecut := now.AddDate(0, 0, -7).Truncate(24 * time.Hour)
	egot := pm.edrops[0].UTC().Truncate(24 * time.Hour)
	require.True(t, ecut.Equal(egot) || ecut.Add(-24*time.Hour).Equal(egot) || ecut.Add(24*time.Hour).Equal(egot),
		"err_logs cutoff = now-7d 独立保留期，got=%v want≈%v", pm.edrops[0], ecut)
	require.Len(t, pm.edrops, len(pm.drops), "三表每轮各自 DROP（同一调度循环）")
	// usage_stats cutoff 独立：now - 180 天（StatsRetentionDays 独立长保留）
	scut := now.AddDate(0, 0, -180).Truncate(24 * time.Hour)
	sgot := pm.sdrops[0].UTC().Truncate(24 * time.Hour)
	require.True(t, scut.Equal(sgot) || scut.Add(-24*time.Hour).Equal(sgot) || scut.Add(24*time.Hour).Equal(sgot),
		"usage_stats cutoff = now-180d 独立长保留期，got=%v want≈%v", pm.sdrops[0], scut)
	// usage_entity_stats 共用 StatsRetentionDays：cutoff 同 usage_stats，各自 DROP+预建（同一循环）
	require.Len(t, pm.esdrops, len(pm.sdrops), "usage_entity_stats 与 usage_stats 同 StatsRetentionDays 各自 DROP")
	esgot := pm.esdrops[0].UTC().Truncate(24 * time.Hour)
	require.True(t, scut.Equal(esgot) || scut.Add(-24*time.Hour).Equal(esgot) || scut.Add(24*time.Hour).Equal(esgot),
		"usage_entity_stats cutoff = now-180d 与 usage_stats 共用 StatsRetentionDays，got=%v want≈%v", pm.esdrops[0], scut)
	// until 语义：now + 1 天（预建当日/明日分区）
	until := pm.ensures[0]
	require.WithinDuration(t, time.Now().AddDate(0, 0, 1), until, 2*time.Second)
	// now 语义：ensure 的 now = 巡检时刻（与 until 同源，边界
	// 由同一 now 推导）
	require.WithinDuration(t, time.Now(), pm.nows[0], 2*time.Second)
	require.Len(t, pm.eensures, len(pm.ensures), "四表各自预建分区（usage_logs/err_logs/stats/entity）")
	require.Len(t, pm.sensures, len(pm.ensures), "usage_stats 独立预建分区")
	require.Len(t, pm.esensures, len(pm.sensures), "usage_entity_stats 独立预建分区（与 usage_stats 同循环）")
	require.WithinDuration(t, time.Now(), pm.snows[0], 2*time.Second)
	require.WithinDuration(t, time.Now(), pm.esnows[0], 2*time.Second)
}

// TestRetentionWorkerStartsWithImmediateRun 启动即巡检（不等到第一个 tick）。
func TestRetentionWorkerStartsWithImmediateRun(t *testing.T) {
	pm := &fakePartitionManager{}
	w := NewRetention(RetentionConfig{LogRetentionDays: 7, TickerInterval: time.Hour}, pm, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, w.Start(ctx))
	waitCounts(t, pm, 1, 1) // 不等 ticker（1h）即完成一轮
	cancel()
	require.NoError(t, w.Close(ctx))
}

// TestRetentionWorkerZeroRetention SkipsDrop LogRetentionDays<=0 → 不删除，
// 只预建分区（旧 janitorLoop 同语义）；三表独立：usage_logs 0 天不删除、
// err_logs 30 天删除、usage_stats 未配置（0）不删除。
func TestRetentionWorkerZeroRetentionSkipsDrop(t *testing.T) {
	pm := &fakePartitionManager{}
	w := NewRetention(RetentionConfig{LogRetentionDays: 0, ErrLogRetentionDays: 30, TickerInterval: 20 * time.Millisecond}, pm, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, w.Start(ctx))
	waitCounts(t, pm, 0, 2)
	cancel()
	require.NoError(t, w.Close(ctx))
	pm.mu.Lock()
	defer pm.mu.Unlock()
	require.Len(t, pm.drops, 0, "usage_logs 保留天数 0 → 不删除")
	require.NotEmpty(t, pm.edrops, "err_logs 保留天数 30 → 独立删除")
	require.NotEmpty(t, pm.eensures, "err_logs 预建分区")
	require.Empty(t, pm.sdrops, "usage_stats 未配置保留天数 → 不删除")
	require.NotEmpty(t, pm.sensures, "usage_stats 仍预建分区（保留期仅管删除）")
	require.Empty(t, pm.esdrops, "usage_entity_stats 未配置保留天数 → 不删除（与 usage_stats 共用 StatsRetentionDays）")
	require.NotEmpty(t, pm.esensures, "usage_entity_stats 仍预建分区（保留期仅管删除，与 usage_stats 同循环）")
}

// TestRetentionWorkerStatsFailureIsolated 扩展：usage_stats DROP 失败不影响
// 明细两表（四表逐表错误隔离——180 天清理失败不连带 30 天/7 天清理）；
// usage_entity_stats 与 usage_stats 共用 StatsRetentionDays 但各自独立错误隔离——
// stats 失败不影响 entity，反之亦然（同一循环内逐表隔离）。
func TestRetentionWorkerStatsFailureIsolated(t *testing.T) {
	pm := &fakePartitionManager{sdropErr: errBoom}
	w := NewRetention(RetentionConfig{LogRetentionDays: 30, ErrLogRetentionDays: 7, StatsRetentionDays: 180, TickerInterval: 20 * time.Millisecond}, pm, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, w.Start(ctx))
	waitCounts(t, pm, 2, 2)
	sd, se := pm.statsCounts()
	require.GreaterOrEqual(t, sd, 2, "usage_stats drop 失败仍每轮重试")
	require.GreaterOrEqual(t, se, 2, "usage_stats ensure 不受 drop 失败影响")
	ed, ee := pm.errCounts()
	require.GreaterOrEqual(t, ed, 2, "err_logs 不受 usage_stats 失败影响")
	require.GreaterOrEqual(t, ee, 2)
	esd, ese := pm.entityStatsCounts()
	require.GreaterOrEqual(t, esd, 2, "usage_entity_stats 不受 usage_stats 失败影响（同一 StatsRetentionDays 各自隔离）")
	require.GreaterOrEqual(t, ese, 2)
	cancel()
	require.NoError(t, w.Close(ctx))
}

// TestRetentionWorkerErrLogsFailureIsolated：一表 DROP 失败不影响另一表
// （err_logs drop 失败 → usage_logs 仍正常 drop/ensure，下轮重试各自独立）。
func TestRetentionWorkerErrLogsFailureIsolated(t *testing.T) {
	pm := &fakePartitionManager{edropErr: errBoom}
	w := NewRetention(RetentionConfig{LogRetentionDays: 30, ErrLogRetentionDays: 7, TickerInterval: 20 * time.Millisecond}, pm, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, w.Start(ctx))
	waitCounts(t, pm, 2, 2)
	ed, ee := pm.errCounts()
	require.GreaterOrEqual(t, ed, 2, "err_logs drop 失败仍每轮重试")
	require.GreaterOrEqual(t, ee, 2, "err_logs ensure 不受 drop 失败影响")
	cancel()
	require.NoError(t, w.Close(ctx))
}

// TestRetentionWorkerEntityStatsFailureIsolated 四表隔离：usage_entity_stats DROP 失败不影响
// usage_stats 及明细两表（同一 StatsRetentionDays 各自错误隔离——entity 失败不连带 cube）。
func TestRetentionWorkerEntityStatsFailureIsolated(t *testing.T) {
	pm := &fakePartitionManager{esdropErr: errBoom}
	w := NewRetention(RetentionConfig{LogRetentionDays: 30, ErrLogRetentionDays: 7, StatsRetentionDays: 180, TickerInterval: 20 * time.Millisecond}, pm, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, w.Start(ctx))
	waitCounts(t, pm, 2, 2)
	esd, ese := pm.entityStatsCounts()
	require.GreaterOrEqual(t, esd, 2, "usage_entity_stats drop 失败仍每轮重试")
	require.GreaterOrEqual(t, ese, 2, "usage_entity_stats ensure 不受 drop 失败影响")
	sd, se := pm.statsCounts()
	require.GreaterOrEqual(t, sd, 2, "usage_stats 不受 usage_entity_stats 失败影响")
	require.GreaterOrEqual(t, se, 2)
	cancel()
	require.NoError(t, w.Close(ctx))
}

// TestRetentionWorkerErrorTolerated 单轮失败不中断循环（Warn + 下轮重试）。
func TestRetentionWorkerErrorTolerated(t *testing.T) {
	pm := &fakePartitionManager{dropErr: errBoom, ensureErr: errBoom}
	w := NewRetention(RetentionConfig{LogRetentionDays: 30, TickerInterval: 20 * time.Millisecond}, pm, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, w.Start(ctx))
	waitCounts(t, pm, 2, 2) // 失败后仍继续 tick
	cancel()
	require.NoError(t, w.Close(ctx))
}

// TestRetentionWorkerCloseIdempotent Close 未 Start 也安全 + 重复 Close 幂等
// （worker.Worker 契约）。
func TestRetentionWorkerCloseIdempotent(t *testing.T) {
	pm := &fakePartitionManager{}
	w := NewRetention(RetentionConfig{LogRetentionDays: 30}, pm, nil)
	require.NoError(t, w.Close(context.Background()))
	require.NoError(t, w.Close(context.Background()))
}

// TestRetentionWorkerStartTwiceFails 重复 Start 报错（Recorder 同契约）。
func TestRetentionWorkerStartTwiceFails(t *testing.T) {
	pm := &fakePartitionManager{}
	w := NewRetention(RetentionConfig{LogRetentionDays: 30}, pm, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, w.Start(ctx))
	require.Error(t, w.Start(ctx))
}

// TestRetentionWorkerDeletesRedemptionUses：redemption_uses 批删并入周期
// 任务——每轮巡检都调 DeleteRedemptionUsesBefore，cutoff = now - 90 天（TTL
// 定死，非配置项）。
func TestRetentionWorkerDeletesRedemptionUses(t *testing.T) {
	pm := &fakePartitionManager{}
	w := NewRetention(RetentionConfig{LogRetentionDays: 30, TickerInterval: 20 * time.Millisecond}, pm, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, w.Start(ctx))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pm.mu.Lock()
		n := len(pm.rdeletes)
		pm.mu.Unlock()
		if n >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	require.NoError(t, w.Close(ctx))

	pm.mu.Lock()
	defer pm.mu.Unlock()
	require.GreaterOrEqual(t, len(pm.rdeletes), 2, "每轮巡检都执行 redemption_uses 批删")
	// cutoff 语义：now - 90 天（日粒度，容忍 ±1 天边界）
	now := time.Now().UTC()
	cut := now.AddDate(0, 0, -redemptionUseRetentionDays).Truncate(24 * time.Hour)
	got := pm.rdeletes[0].UTC().Truncate(24 * time.Hour)
	require.True(t, cut.Equal(got) || cut.Add(-24*time.Hour).Equal(got) || cut.Add(24*time.Hour).Equal(got),
		"redemption_uses cutoff = now-90d（TTL 定死），got=%v want≈%v", pm.rdeletes[0], cut)
}

// TestRetentionWorkerRedemptionDeleteFailureIsolated：批删失败不连带分区
// 三表（逐表错误隔离同语义），下轮重试。
func TestRetentionWorkerRedemptionDeleteFailureIsolated(t *testing.T) {
	pm := &fakePartitionManager{rdelErr: errBoom}
	w := NewRetention(RetentionConfig{LogRetentionDays: 30, ErrLogRetentionDays: 7, StatsRetentionDays: 180, TickerInterval: 20 * time.Millisecond}, pm, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, w.Start(ctx))
	waitCounts(t, pm, 2, 2) // 分区三表 drop/ensure 不受批删失败影响
	cancel()
	require.NoError(t, w.Close(ctx))

	pm.mu.Lock()
	defer pm.mu.Unlock()
	require.GreaterOrEqual(t, len(pm.rdeletes), 2, "批删失败仍每轮重试")
	require.Len(t, pm.edrops, len(pm.drops), "err_logs 不受 redemption_uses 批删失败影响")
	require.Len(t, pm.sdrops, len(pm.drops), "usage_stats 不受 redemption_uses 批删失败影响")
	require.Len(t, pm.esdrops, len(pm.drops), "usage_entity_stats 不受 redemption_uses 批删失败影响（四表同一循环）")
}

// TestRetentionRoutingPartitionBackstop 保留期兜底（spec §6 / 判据 A17）：路由两张
// 事实表的分区概况必须进观测面，且最老分区早于观测保留 cutoff 时告警。
//
// 为什么这是必要的：事实表的有界性完全依赖于本 worker 在跑（与已下线的重算机械
// 是同一种依赖形状）。worker 停摆或 DROP 持续失败时分区会**静默**无界增长——没有这个
// 观测面，磁盘会被填满而任何指标都不动。正常巡检下「最老分区早于 cutoff」不可能发生，
// 一旦出现就是该失效的直接信号。
func TestRetentionRoutingPartitionBackstop(t *testing.T) {
	// 情形 1：最老分区在 cutoff 内 → 记录观测值，不告警。
	oldestIn := domain.RoutingObservationCutoff(time.Now(), 7).Add(24 * time.Hour)
	logger, out := newTestErrLogLogger(t)
	pm := &fakePartitionManager{rpartCount: 12, rpartOldest: oldestIn}
	w := NewRetention(RetentionConfig{RoutingObservationRetentionDays: 7}, pm, logger)
	w.runOnce()
	st := w.Stats().(RetentionWorkerStats)
	require.Equal(t, int64(12), st.PartitionCount, "分区数进观测面")
	require.Equal(t, oldestIn.UnixMilli(), st.OldestPartitionUnixMs, "最老分区时刻进观测面")
	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.NotContains(t, string(b), "older than the observation cutoff", "cutoff 内不得告警")

	// 情形 2：最老分区早于 cutoff → 告警触发（分区 DROP 未生效的兜底信号）。
	logger2, out2 := newTestErrLogLogger(t)
	pm2 := &fakePartitionManager{rpartCount: 99, rpartOldest: domain.RoutingObservationCutoff(time.Now(), 7).Add(-48 * time.Hour)}
	w2 := NewRetention(RetentionConfig{RoutingObservationRetentionDays: 7}, pm2, logger2)
	w2.runOnce()
	require.Equal(t, int64(99), w2.Stats().(RetentionWorkerStats).PartitionCount)
	require.NoError(t, logger2.Sync())
	b2, err := os.ReadFile(out2)
	require.NoError(t, err)
	require.Contains(t, string(b2), "older than the observation cutoff",
		"最老分区早于 cutoff 必须告警——否则分区无界增长无声发生")

	// 情形 3：无分区 → oldest 记 0。零值 time 的 UnixMilli 是巨大负数，必须归 0，
	// 否则运维会看到公元 1 年的「最老分区」。
	pm3 := &fakePartitionManager{rpartCount: 0}
	w3 := NewRetention(RetentionConfig{RoutingObservationRetentionDays: 7}, pm3, nil)
	w3.runOnce()
	require.Zero(t, w3.Stats().(RetentionWorkerStats).OldestPartitionUnixMs, "无分区 = 0")

	// 情形 4：查询失败 → 保留上轮值（与 lastDrop* 同观测纪律），lastPatrol 仍推进。
	pm4 := &fakePartitionManager{rpartCount: 7, rpartOldest: domain.RoutingObservationCutoff(time.Now(), 7).Add(time.Hour)}
	w4 := NewRetention(RetentionConfig{RoutingObservationRetentionDays: 7}, pm4, nil)
	w4.runOnce()
	require.Equal(t, int64(7), w4.Stats().(RetentionWorkerStats).PartitionCount)
	pm4.rpartErr = errors.New("stats boom")
	w4.runOnce()
	st4 := w4.Stats().(RetentionWorkerStats)
	require.Equal(t, int64(7), st4.PartitionCount, "失败轮保留上轮值")
	require.NotZero(t, st4.LastPatrolUnixMs, "失败轮 lastPatrol 仍推进")
}
