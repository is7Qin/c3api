// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/handler"
	"github.com/is7qin/c3api/internal/proxy"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/internal/worker"
)

// TestContBindWorkerSatisfiesStatsProvider 钉死 M3：cont-bind worker 必须实现
// handler.StatsProvider，从而被 statsProviders 纳入 /api/admin/ops/workers
// （否则启动期 Warn 且运维面缺 cont-bind）。
func TestContBindWorkerSatisfiesStatsProvider(t *testing.T) {
	w := proxy.NewContBindWorker(nil, nil, proxy.ContBindConfig{})

	_, ok := any(w).(handler.StatsProvider)
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

// TestConversationWorkerSatisfiesStatsProvider codex-conversation worker 必须实现
// handler.StatsProvider 并被 statsProviders 纳入 /api/admin/ops/workers；stats 为
// 扁平键（conv_* 会话簿计数 + conv_local_rejects，shape_* 整形计数——清单见
// openapi.yaml WorkerStatus.stats）。
func TestConversationWorkerSatisfiesStatsProvider(t *testing.T) {
	// 真会话簿（miniredis）——装配断言不需要存储调用，但构造路径必须与生产同一个
	// （sdkbridge.NewBook），否则这个用例无法证明 main 那条线装得上。
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr(), ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = rdb.Close() })
	book := sdkbridge.NewBook(rdb)
	w := sdkbridge.NewConversationWorker(book, nil, sdkbridge.ConversationWorkerConfig{})

	providers := statsProviders([]worker.Worker{w}, nil)
	require.Len(t, providers, 1, "codex-conversation must be admitted by statsProviders (no Warn branch)")
	require.Equal(t, "codex-conversation", providers[0].Name())

	raw, err := json.Marshal(providers[0].Stats())
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	for _, k := range []string{"conv_opens", "conv_conversation_cache_hits", "conv_queue_drops", "conv_save_turns_failures", "conv_local_rejects", "shape_anonymous", "shape_unknown_models", "shape_incremental_reinjects", "shape_prewarms", "shape_prewarm_rejections"} {
		_, present := m[k]
		require.True(t, present, "codex-conversation stats must expose %q", k)
	}
	for k, v := range m {
		require.True(t, strings.HasPrefix(k, "conv_") || strings.HasPrefix(k, "shape_"), "unexpected key %q", k)
		_, isNum := v.(float64)
		require.True(t, isNum, "stats must be flat numbers, %q = %v", k, v)
	}
}
