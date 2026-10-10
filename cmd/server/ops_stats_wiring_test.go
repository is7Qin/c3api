// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/handler"
	"github.com/is7qin/c3api/internal/proxy"
	"github.com/is7qin/c3api/internal/worker"
)

// TestContBindWorkerSatisfiesStatsProvider 钉死 M3：cont-bind worker 必须实现
// handler.StatsProvider，从而被 statsProviders 纳入 /api/admin/ops/workers
// （否则启动期 Warn 且运维面缺 cont-bind）。
func TestContBindWorkerSatisfiesStatsProvider(t *testing.T) {
	w := proxy.NewContBindWorker(nil, nil, proxy.ContBindConfig{})

	_, ok := interface{}(w).(handler.StatsProvider)
	require.True(t, ok, "cont-bind worker must implement handler.StatsProvider")

	providers := statsProviders([]worker.Worker{w}, nil)
	require.Len(t, providers, 1, "cont-bind must be admitted by statsProviders (no Warn branch)")
	require.Equal(t, "cont-bind", providers[0].Name())

	raw, err := json.Marshal(providers[0].Stats())
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	for _, k := range []string{"queued", "queue_cap", "bound", "dropped", "failed", "conflicts", "unattributed"} {
		_, present := m[k]
		require.True(t, present, "cont-bind stats must expose %q", k)
	}
}
