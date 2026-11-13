// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Codex 会话簿 worker（spec §5.6）：会话簿（codexsdk.ConversationBook）的轮次写
// 经有界队列异步落 Redis，本 worker 按固定节拍调 book.Flush 排空；停机时在预算内
// 最后 Flush 一次（尽量不丢已入队的轮次记录）。Stats() 暴露会话簿计数（conv_*）、
// 本地拒绝计数（conv_local_rejects）与进程级整形计数（shape_*）。
//
// 纪律：Flush 的批与预算由 SDK 管（每批 256、2s）；worker 只负责节拍与停机。同一
// 时刻只有一个 Flush 在跑（节拍 goroutine 与 Close/FlushNow 由 flushMu 串行）。
// 热路径（Prepare 命中）与本 worker 无交互，不新增锁与分配。
package proxy

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/logx"
)

// conversationFlushIntervalDefault 会话簿 Flush 节拍默认值（SDK 约定 50ms）。
const conversationFlushIntervalDefault = 50 * time.Millisecond

// ConversationWorkerConfig 会话簿 worker 构造参数（零值字段取内部默认，不引入
// 运维配置键）。
type ConversationWorkerConfig struct {
	Interval time.Duration // Flush 节拍（默认 50ms；测试可注入）
}

// ConversationWorker 会话簿 Flush worker（worker.Worker + handler.StatsProvider
// 契约，Name="codex-conversation"）。
type ConversationWorker struct {
	book     *codexsdk.ConversationBook
	log      *logx.Logger
	interval time.Duration

	// flushMu 串行化 Flush（SDK 要求同一时刻只有一个 Flush）：节拍、FlushNow 与
	// Close 的最后一次 Flush 互斥。
	flushMu sync.Mutex

	started   atomic.Bool
	cancel    context.CancelFunc
	loopDone  chan struct{}
	closeOnce sync.Once
}

// NewConversationWorker 构造会话簿 worker；book 须非 nil，log 可 nil（静默）。
func NewConversationWorker(book *codexsdk.ConversationBook, log *logx.Logger, cfg ConversationWorkerConfig) *ConversationWorker {
	if cfg.Interval <= 0 {
		cfg.Interval = conversationFlushIntervalDefault
	}
	return &ConversationWorker{
		book:     book,
		log:      log,
		interval: cfg.Interval,
		loopDone: make(chan struct{}),
	}
}

// Name worker.Worker 契约。
func (w *ConversationWorker) Name() string { return "codex-conversation" }

// Start 非阻塞启动节拍 goroutine（worker.Loop 托管，panic 不崩进程）。二次
// Start 返回错误。
func (w *ConversationWorker) Start(ctx context.Context) error {
	if !w.started.CompareAndSwap(false, true) {
		return fmt.Errorf("codex-conversation worker: already started")
	}
	lctx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	go func() {
		defer close(w.loopDone)
		worker.Loop(lctx, "codex-conversation", w.log, w.loop)
	}()
	return nil
}

func (w *ConversationWorker) loop(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.flush(ctx)
		}
	}
}

// flush 一次 Flush（串行）。错误只 Warn：失败批次 SDK 已丢弃并计入
// conv_save_turns_failures / conv_save_item_failures，不重试。
func (w *ConversationWorker) flush(ctx context.Context) {
	w.flushMu.Lock()
	err := w.book.Flush(ctx)
	w.flushMu.Unlock()
	if err != nil && w.log != nil && ctx.Err() == nil {
		w.log.Warn("codex-conversation flush failed", logx.Error(err))
	}
}

// FlushNow 同步 Flush 一次（测试与跨副本用例的确定性屏障）。
func (w *ConversationWorker) FlushNow(ctx context.Context) {
	w.flush(ctx)
}

// Close 幂等：停节拍 → 等 loop 退出（受 ctx 预算约束）→ 在剩余预算内最后
// Flush 一次。未 Start 也安全（直接最后一次 Flush）。
func (w *ConversationWorker) Close(ctx context.Context) error {
	w.closeOnce.Do(func() {
		if w.started.Load() {
			w.cancel()
			select {
			case <-w.loopDone:
			case <-ctx.Done():
				if w.log != nil {
					w.log.Warn("codex-conversation close: loop did not exit in time")
				}
				return
			}
		}
		if ctx.Err() != nil {
			return
		}
		w.flush(ctx)
	})
	return nil
}

// conversationWorkerStats /ops/workers 观测快照（handler.StatsProvider 形态）。
// json 键清单同步义务：openapi.yaml WorkerStatus.stats description + web locales
// ops.stats.*（前端缺 key 兜底显示原始字段名）。conv_* = 会话簿计数
// （codexsdk.ConversationStats）+ 本地拒绝；shape_* = 进程级整形计数
// （codexsdk.ShapeStatsSnapshot）。
type conversationWorkerStats struct {
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
	ConvLocalRejects            int64 `json:"conv_local_rejects"`

	ShapeAnonymous             int64 `json:"shape_anonymous"`
	ShapePartialIdentity       int64 `json:"shape_partial_identity"`
	ShapeDuplicateKeys         int64 `json:"shape_duplicate_keys"`
	ShapeNonStringInstructions int64 `json:"shape_non_string_instructions"`
	ShapeDualToolSources       int64 `json:"shape_dual_tool_sources"`
	ShapeUnknownModels         int64 `json:"shape_unknown_models"`
	ShapeIncrementalReinjects  int64 `json:"shape_incremental_reinjects"`
}

// Stats handler.StatsProvider 契约：原子读组装（零锁）。
func (w *ConversationWorker) Stats() any {
	b := w.book.Stats()
	s := codexsdk.ShapeStatsSnapshot()
	return conversationWorkerStats{
		ConvConversationCacheHits:   b.ConversationCacheHits,
		ConvConversationCacheMisses: b.ConversationCacheMisses,
		ConvTurnCacheHits:           b.TurnCacheHits,
		ConvTurnCacheMisses:         b.TurnCacheMisses,
		ConvConversationEntries:     b.ConversationEntries,
		ConvTurnEntries:             b.TurnEntries,
		ConvOpens:                   b.Opens,
		ConvOpenFailures:            b.OpenFailures,
		ConvAdvances:                b.Advances,
		ConvAdvanceFailures:         b.AdvanceFailures,
		ConvLoads:                   b.Loads,
		ConvLoadFailures:            b.LoadFailures,
		ConvBreakerOpens:            b.BreakerOpens,
		ConvBreakerRejects:          b.BreakerRejects,
		ConvOneShotIdentities:       b.OneShotIdentities,
		ConvTemporaryTurns:          b.TemporaryTurns,
		ConvQueueDrops:              b.QueueDrops,
		ConvSaveTurnsCalls:          b.SaveTurnsCalls,
		ConvSaveTurnsFailures:       b.SaveTurnsFailures,
		ConvSaveItemFailures:        b.SaveItemFailures,
		ConvTurnStateIgnored:        b.TurnStateIgnored,
		ConvWSUntrackedConnections:  b.WSUntrackedConnections,
		ConvWSIncrementalInjects:    b.WSIncrementalInjects,
		ConvWSLocalTemporaryTurns:   b.WSLocalTemporaryTurns,
		ConvWSLateTurnStates:        b.WSLateTurnStates,
		ConvSweeps:                  b.Sweeps,
		ConvSweepDeleted:            b.SweepDeleted,
		ConvSweepNanos:              b.SweepNanos,
		ConvLocalRejects:            convLocalRejects.Load(),

		ShapeAnonymous:             s.Anonymous,
		ShapePartialIdentity:       s.PartialIdentity,
		ShapeDuplicateKeys:         s.DuplicateKeys,
		ShapeNonStringInstructions: s.NonStringInstructions,
		ShapeDualToolSources:       s.DualToolSources,
		ShapeUnknownModels:         s.UnknownModels,
		ShapeIncrementalReinjects:  s.IncrementalReinjects,
	}
}
