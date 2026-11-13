// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	codexsdk "github.com/is7Qin/codex-sdk"
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
	book := codexsdk.NewConversationBook(nopConvStore{})
	w := proxy.NewConversationWorker(book, nil, proxy.ConversationWorkerConfig{})

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

// nopConvStore 会话簿存储空实现（仅装配断言用，不发生存储调用）。
type nopConvStore struct{}

func (nopConvStore) Open(context.Context, codexsdk.ConversationKey, codexsdk.ConversationRecord) (codexsdk.ConversationRecord, bool, error) {
	return codexsdk.ConversationRecord{}, false, nil
}

func (nopConvStore) Advance(context.Context, codexsdk.ConversationKey, uint64, uint64, string) (codexsdk.ConversationRecord, error) {
	return codexsdk.ConversationRecord{}, nil
}

func (nopConvStore) Load(context.Context, codexsdk.ConversationKey, uint64) (codexsdk.ConversationRecord, bool, codexsdk.TurnRecord, bool, error) {
	return codexsdk.ConversationRecord{}, false, codexsdk.TurnRecord{}, false, nil
}

func (nopConvStore) SaveTurns(context.Context, []codexsdk.TurnWrite) ([]codexsdk.TurnResult, error) {
	return nil, nil
}
