// SPDX-License-Identifier: AGPL-3.0-or-later
package usage

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A4 + B1 + B2 + B3：路由观测保留与 usage_stats **解耦**，且三个观测 store
// 各自有界。
//
//   - A4：改 observation_retention_days 只动 routing 三张分区表的 cutoff，
//     usage_stats/usage_entity_stats 恒为 StatsRetentionDays（180d）——观测深度
//     不再搭聚合统计长保留的车。
//   - B1：S2′（routing_flow_fact）cutoff 前分钟分区 DROP，cutoff 内全保留
//     （DROP 由 repository.DropTablePartitionsBefore 按分区名日期判定；此处断言
//     传参口径）。
//   - B2：routing_quality_instance_minute 同 cutoff。
//   - B3：routing_quality_rollup 同 cutoff。
//
// 外加 snapshot_state 的有界删（普通表，同一 cutoff 的 DELETE）。
func TestRoutingObservationRetentionDecoupledFromStats(t *testing.T) {
	for _, days := range []int{2, 7} {
		t.Run(fmt.Sprintf("observation_days=%d", days), func(t *testing.T) {
			pm := &fakePartitionManager{}
			w := NewRetention(RetentionConfig{
				LogRetentionDays:                30,
				ErrLogRetentionDays:             7,
				StatsRetentionDays:              180,
				RoutingObservationRetentionDays: days,
				TickerInterval:                  time.Hour, // 启动即巡检一轮即可
			}, pm, nil)
			ctx, cancel := context.WithCancel(context.Background())
			require.NoError(t, w.Start(ctx))
			waitCounts(t, pm, 1, 1)
			cancel()
			require.NoError(t, w.Close(ctx))

			pm.mu.Lock()
			defer pm.mu.Unlock()
			now := time.Now().UTC()
			wantRouting := now.AddDate(0, 0, -days)
			require.Len(t, pm.rdrops, 3, "观测三张分区表各自 DROP（quality instance / quality rollup / flow rollup）")
			for i, got := range pm.rdrops {
				require.WithinDurationf(t, wantRouting, got, 5*time.Second,
					"routing observation cutoff #%d must be now-%dd (observation_retention_days), got %v", i, days, got)
			}
			require.Len(t, pm.rstateDel, 1, "snapshot_state 有界删每轮一次")
			require.WithinDuration(t, wantRouting, pm.rstateDel[0], 5*time.Second,
				"snapshot_state cutoff 与分区 DROP 同源")

			// usage_stats / usage_entity_stats 不受观测天数影响：仍是 StatsRetentionDays。
			require.NotEmpty(t, pm.sdrops)
			require.WithinDuration(t, now.AddDate(0, 0, -180), pm.sdrops[0], 5*time.Second,
				"usage_stats cutoff must stay now-180d regardless of the observation depth")
			require.Len(t, pm.esdrops, len(pm.sdrops))
			require.WithinDuration(t, now.AddDate(0, 0, -180), pm.esdrops[0], 5*time.Second)
		})
	}
}

// TestRoutingObservationRetentionDisabled：观测天数 <= 0 = 不删除（保留期仅管
// 删除，分区预建照常）——与其它表 <= 0 的文档化惯例一致。
func TestRoutingObservationRetentionDisabled(t *testing.T) {
	pm := &fakePartitionManager{}
	w := NewRetention(RetentionConfig{
		LogRetentionDays:                30,
		RoutingObservationRetentionDays: 0,
		TickerInterval:                  time.Hour,
	}, pm, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, w.Start(ctx))
	waitCounts(t, pm, 1, 1)
	cancel()
	require.NoError(t, w.Close(ctx))

	pm.mu.Lock()
	defer pm.mu.Unlock()
	require.Empty(t, pm.rdrops, "观测保留天数 0 → 不删除路由观测分区")
	require.Empty(t, pm.rstateDel, "观测保留天数 0 → 不删 snapshot_state")
	require.NotEmpty(t, pm.rnows, "分区预建与保留期无关（写入仍需当日/次日分区）")
}
