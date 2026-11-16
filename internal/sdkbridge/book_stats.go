// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import codexsdk "github.com/is7Qin/codex-sdk"

// BookStats 会话簿计数快照（宿主命名）。
//
// **逐字段显式映射**，不用反射、不用类型别名：SDK 新增一个计数器时，这里会静默
// 少一个键——所以 `book_stats_contract_test.go` 用 reflect 对照两侧字段集合并断言
// **相等**（多一个、少一个都红），迫使改动者显式决定"这个新计数要不要暴露"。
//
// json 键是 `/ops/workers` 的对外契约（openapi.yaml WorkerStatus.stats 的
// description 与 web locales 的 ops.stats.* 都按它渲染），改名即破坏前端。
type BookStats struct {
	ConvConversationCacheHits   int64 `json:"conv_conversation_cache_hits"`
	ConvConversationCacheMisses int64 `json:"conv_conversation_cache_misses"`
	ConvTurnCacheHits           int64 `json:"conv_turn_cache_hits"`
	ConvTurnCacheMisses         int64 `json:"conv_turn_cache_misses"`
	ConvConversationEntries     int64 `json:"conv_conversation_entries"`
	ConvTurnEntries             int64 `json:"conv_turn_entries"`
	ConvOpens                   int64 `json:"conv_opens"`
	ConvOpenFailures            int64 `json:"conv_open_failures"`
	ConvAdvances                int64 `json:"conv_advances"`
	ConvAdvanceFailures         int64 `json:"conv_advance_failures"`
	ConvLoads                   int64 `json:"conv_loads"`
	ConvLoadFailures            int64 `json:"conv_load_failures"`
	ConvBreakerOpens            int64 `json:"conv_breaker_opens"`
	ConvBreakerRejects          int64 `json:"conv_breaker_rejects"`
	ConvOneShotIdentities       int64 `json:"conv_one_shot_identities"`
	ConvTemporaryTurns          int64 `json:"conv_temporary_turns"`
	ConvQueueDrops              int64 `json:"conv_queue_drops"`
	ConvSaveTurnsCalls          int64 `json:"conv_save_turns_calls"`
	ConvSaveTurnsFailures       int64 `json:"conv_save_turns_failures"`
	ConvSaveItemFailures        int64 `json:"conv_save_item_failures"`
	ConvTurnStateIgnored        int64 `json:"conv_turn_state_ignored"`
	ConvWSUntrackedConnections  int64 `json:"conv_ws_untracked_connections"`
	ConvWSIncrementalInjects    int64 `json:"conv_ws_incremental_injects"`
	ConvWSLocalTemporaryTurns   int64 `json:"conv_ws_local_temporary_turns"`
	ConvWSLateTurnStates        int64 `json:"conv_ws_late_turn_states"`
	ConvSweeps                  int64 `json:"conv_sweeps"`
	ConvSweepDeleted            int64 `json:"conv_sweep_deleted"`
	ConvSweepNanos              int64 `json:"conv_sweep_nanos"`
}

// ShapeStats 请求整形的进程级计数快照（HTTP→HTTP、HTTP→WS、WS→WS 合计；自进程
// 启动起累计）。归属 SDK：整形规则由它定义，宿主只导出观测值。
type ShapeStats struct {
	ShapeAnonymous             int64 `json:"shape_anonymous"`
	ShapePartialIdentity       int64 `json:"shape_partial_identity"`
	ShapeDuplicateKeys         int64 `json:"shape_duplicate_keys"`
	ShapeNonStringInstructions int64 `json:"shape_non_string_instructions"`
	ShapeDualToolSources       int64 `json:"shape_dual_tool_sources"`
	ShapeUnknownModels         int64 `json:"shape_unknown_models"`
	ShapeIncrementalReinjects  int64 `json:"shape_incremental_reinjects"`
	ShapePrewarms              int64 `json:"shape_prewarms"`
	ShapePrewarmConnFailures   int64 `json:"shape_prewarm_conn_failures"`
	ShapePrewarmRejections     int64 `json:"shape_prewarm_rejections"`
	ShapePrewarmRedials        int64 `json:"shape_prewarm_redials"`
	ShapePrewarmChained        int64 `json:"shape_prewarm_chained"`
	ShapePrewarmUnchained      int64 `json:"shape_prewarm_unchained"`
}

// Stats 会话簿计数快照（原子读组装，零锁、零分配）。
func (b *Book) Stats() BookStats {
	s := b.b.Stats()
	return BookStats{
		ConvConversationCacheHits:   s.ConversationCacheHits,
		ConvConversationCacheMisses: s.ConversationCacheMisses,
		ConvTurnCacheHits:           s.TurnCacheHits,
		ConvTurnCacheMisses:         s.TurnCacheMisses,
		ConvConversationEntries:     s.ConversationEntries,
		ConvTurnEntries:             s.TurnEntries,
		ConvOpens:                   s.Opens,
		ConvOpenFailures:            s.OpenFailures,
		ConvAdvances:                s.Advances,
		ConvAdvanceFailures:         s.AdvanceFailures,
		ConvLoads:                   s.Loads,
		ConvLoadFailures:            s.LoadFailures,
		ConvBreakerOpens:            s.BreakerOpens,
		ConvBreakerRejects:          s.BreakerRejects,
		ConvOneShotIdentities:       s.OneShotIdentities,
		ConvTemporaryTurns:          s.TemporaryTurns,
		ConvQueueDrops:              s.QueueDrops,
		ConvSaveTurnsCalls:          s.SaveTurnsCalls,
		ConvSaveTurnsFailures:       s.SaveTurnsFailures,
		ConvSaveItemFailures:        s.SaveItemFailures,
		ConvTurnStateIgnored:        s.TurnStateIgnored,
		ConvWSUntrackedConnections:  s.WSUntrackedConnections,
		ConvWSIncrementalInjects:    s.WSIncrementalInjects,
		ConvWSLocalTemporaryTurns:   s.WSLocalTemporaryTurns,
		ConvWSLateTurnStates:        s.WSLateTurnStates,
		ConvSweeps:                  s.Sweeps,
		ConvSweepDeleted:            s.SweepDeleted,
		ConvSweepNanos:              s.SweepNanos,
	}
}

// ShapeStats 进程级整形计数快照。
func ShapeStatsSnapshot() ShapeStats {
	s := codexsdk.ShapeStatsSnapshot()
	return ShapeStats{
		ShapeAnonymous:             s.Anonymous,
		ShapePartialIdentity:       s.PartialIdentity,
		ShapeDuplicateKeys:         s.DuplicateKeys,
		ShapeNonStringInstructions: s.NonStringInstructions,
		ShapeDualToolSources:       s.DualToolSources,
		ShapeUnknownModels:         s.UnknownModels,
		ShapeIncrementalReinjects:  s.IncrementalReinjects,
		ShapePrewarms:              s.Prewarms,
		ShapePrewarmConnFailures:   s.PrewarmConnFailures,
		ShapePrewarmRejections:     s.PrewarmRejections,
		ShapePrewarmRedials:        s.PrewarmRedials,
		ShapePrewarmChained:        s.PrewarmChained,
		ShapePrewarmUnchained:      s.PrewarmUnchained,
	}
}
