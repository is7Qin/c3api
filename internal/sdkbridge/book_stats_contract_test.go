// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"encoding/json"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"

	codexsdk "github.com/is7Qin/codex-sdk"
	"github.com/stretchr/testify/require"
)

// 统计口径漂移门禁。/ops/workers 的 conv_*/shape_* 键是对外契约（openapi.yaml 的
// WorkerStatus.stats description 与 web locales 的 ops.stats.* 都按它渲染）。SDK 增删
// 一个计数器和本包漏改一个字段，在运行时表现为"前端少一格/多一格"，没有别的地方会红。
//
// 所以断言是**集合相等**（双向）：多一个键、少一个键都失败，迫使改动者显式决定。
func TestBookStatsKeysMatchSDK(t *testing.T) {
	book := newTestBook(newChanConvStore())
	var n atomic.Int64
	w := NewConversationWorker(book, nil, ConversationWorkerConfig{LocalRejects: n.Load})

	raw, err := json.Marshal(w.Stats())
	require.NoError(t, err)
	var got map[string]int64
	require.NoError(t, json.Unmarshal(raw, &got))

	want := map[string]bool{"conv_local_rejects": true}
	for _, f := range reflect.VisibleFields(reflect.TypeFor[codexsdk.ConversationStats]()) {
		want["conv_"+snakeCase(f.Name)] = true
	}
	for _, f := range reflect.VisibleFields(reflect.TypeFor[codexsdk.ShapeStats]()) {
		want["shape_"+snakeCase(f.Name)] = true
	}
	keys := map[string]bool{}
	for k := range got {
		keys[k] = true
	}
	require.Equal(t, want, keys)

	// 本地拒绝计数来自注入的读口（计数所有权仍在 proxy 的转发面）。
	before := got["conv_local_rejects"]
	n.Add(1)
	raw, err = json.Marshal(w.Stats())
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &got))
	require.Equal(t, before+1, got["conv_local_rejects"])
}

var snakeBoundary = regexp.MustCompile(`([a-z0-9])([A-Z])|([A-Z]+)([A-Z][a-z])`)

// snakeCase Go 字段名 → json 蛇形键（WSUntrackedConnections → ws_untracked_connections）。
func snakeCase(s string) string {
	s = snakeBoundary.ReplaceAllString(s, "${1}${3}_${2}${4}")
	return strings.ToLower(s)
}

// TestOpsWorkerStatsExactKeys 把 `/ops/workers` 的**键清单写成字面量**（42 条）。
//
// TestBookStatsKeysMatchSDK 用 reflect 从 SDK 结构体推期望集，能抓"SDK 加了字段而
// 我们没跟"；但它抓不到"json tag 被改名"这类漂移——那两边会一起变。这一条是**冻结
// 快照**：key 名就是对外契约（openapi.yaml + web locales + 运维看板），改动必须是有
// 意识的破坏性变更，而不是重构的副作用。
func TestOpsWorkerStatsExactKeys(t *testing.T) {
	book := newTestBook(newChanConvStore())
	w := NewConversationWorker(book, nil, ConversationWorkerConfig{})
	raw, err := json.Marshal(w.Stats())
	require.NoError(t, err)
	var got map[string]int64
	require.NoError(t, json.Unmarshal(raw, &got))

	want := []string{
	"conv_advance_failures",
	"conv_advances",
	"conv_breaker_opens",
	"conv_breaker_rejects",
	"conv_conversation_cache_hits",
	"conv_conversation_cache_misses",
	"conv_conversation_entries",
	"conv_load_failures",
	"conv_loads",
	"conv_local_rejects",
	"conv_one_shot_identities",
	"conv_open_failures",
	"conv_opens",
	"conv_queue_drops",
	"conv_save_item_failures",
	"conv_save_turns_calls",
	"conv_save_turns_failures",
	"conv_sweep_deleted",
	"conv_sweep_nanos",
	"conv_sweeps",
	"conv_temporary_turns",
	"conv_turn_cache_hits",
	"conv_turn_cache_misses",
	"conv_turn_entries",
	"conv_turn_state_ignored",
	"conv_ws_incremental_injects",
	"conv_ws_late_turn_states",
	"conv_ws_local_temporary_turns",
	"conv_ws_untracked_connections",
	"shape_anonymous",
	"shape_dual_tool_sources",
	"shape_duplicate_keys",
	"shape_incremental_reinjects",
	"shape_non_string_instructions",
	"shape_partial_identity",
	"shape_prewarm_chained",
	"shape_prewarm_conn_failures",
	"shape_prewarm_redials",
	"shape_prewarm_rejections",
	"shape_prewarm_unchained",
	"shape_prewarms",
	"shape_unknown_models",
	}
	require.Len(t, want, 42)
	require.Equal(t, len(want), len(got), "键数变了：新增/删除统计必须同步 openapi 与 web locales")
	for _, k := range want {
		_, ok := got[k]
		require.True(t, ok, "缺少契约键 %q", k)
	}
}
