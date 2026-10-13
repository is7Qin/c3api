// SPDX-License-Identifier: AGPL-3.0-or-later
package usage

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRoutingPartitionRetentionViaWorker(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		pm := &fakePartitionManager{}
		w := NewRetention(RetentionConfig{LogRetentionDays: 30, ErrLogRetentionDays: 7, StatsRetentionDays: 180, RoutingObservationRetentionDays: 7, TickerInterval: 20 * time.Millisecond}, pm, nil)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		require.NoError(t, w.Start(ctx))
		// 启动即 runOnce（bootstrap），再等一个 tick 使巡检稳定发生（虚拟时钟）。
		synctest.Sleep(20 * time.Millisecond)
		cancel()
		require.NoError(t, w.Close(ctx))
		pm.mu.Lock()
		defer pm.mu.Unlock()
		require.NotEmpty(t, pm.rnows, "routing fact precreate must be called")
		require.NotEmpty(t, pm.rdrops, "routing drops must be called via retention")
	})
}
