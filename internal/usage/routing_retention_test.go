// SPDX-License-Identifier: AGPL-3.0-or-later
package usage

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRoutingPartitionRetentionViaWorker(t *testing.T) {
	pm := &fakePartitionManager{}
	w := NewRetention(RetentionConfig{LogRetentionDays: 30, ErrLogRetentionDays: 7, StatsRetentionDays: 180, TickerInterval: 20 * time.Millisecond}, pm, nil)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, w.Start(ctx))
	// wait for at least one tick
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		pm.mu.Lock()
		hasRouting := len(pm.rnows) > 0 && len(pm.rdrops) > 0
		pm.mu.Unlock()
		if hasRouting {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	require.NoError(t, w.Close(ctx))
	pm.mu.Lock()
	defer pm.mu.Unlock()
	require.NotEmpty(t, pm.rnows, "routing instance/rollup precreate must be called")
	require.NotEmpty(t, pm.rdrops, "routing drops must be called via retention")
}
