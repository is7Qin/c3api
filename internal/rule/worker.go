// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package rule

import (
	"context"
	"fmt"
	"time"

	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/logx"
)

// cleanupInterval 过期账号清理周期（窗口计数防 map 泄漏）。
const cleanupInterval = time.Minute

// Name 满足 worker.Worker 契约（Global Constraints #5）。
func (e *RuleEngine) Name() string { return "rule-engine" }

// Start 启动事件消费循环（含周期性窗口清理）；重复 Start 返回错误（幂等）。
func (e *RuleEngine) Start(ctx context.Context) error {
	if !e.startOnce.CompareAndSwap(false, true) {
		return fmt.Errorf("rule-engine: already started")
	}
	persistCtx, cancel := context.WithCancel(ctx)
	e.persistMu.Lock()
	e.persistCtx = persistCtx
	e.persistCancel = cancel
	e.persistDone = worker.GoLoop(persistCtx, "rule-engine-persist", e.log, e.persistLoop)
	e.persistMu.Unlock()
	worker.GoLoop(ctx, "rule-engine", e.log, e.loop)
	return nil
}

// loop 消费循环：ctx 取消退出；scheduler 投递不依赖启动态（事件先入队）。
// 复位点（热点修复 B）：HandleEvent 处理完毕后检查队列排空 → 丢弃告警边沿
// 回落（rule engine 无周期 flush，复位只能挂在消费循环——见
// resetDropWarnIfDrained）。
func (e *RuleEngine) loop(ctx context.Context) {
	t := time.NewTicker(cleanupInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-e.ch:
			e.HandleEvent(ctx, ev)
			e.resetDropWarnIfDrained()
		case <-t.C:
			e.wm.cleanup(e.timeNow())
		}
	}
}

// Flush 同步排空队列：处理完当前队列中的全部事件后返回（测试与优雅关闭用）。
// 幂等，未 Start 时也可安全排空。
func (e *RuleEngine) Flush(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-e.ch:
			e.HandleEvent(ctx, ev)
			e.resetDropWarnIfDrained()
		default:
			e.flushPersist(ctx)
			return
		}
	}
}

func (e *RuleEngine) flushPersist(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-e.persistCh:
			func() {
				defer func() {
					if r := recover(); r != nil {
						if e.log != nil {
							e.log.Warn("persist callback panicked", logx.Any("panic", r))
						}
					}
					if e.persistPending.Add(-1) < 0 {
						e.persistPending.Store(0)
					}
				}()
				e.persistFnMu.RLock()
				fn := e.persistFn
				e.persistFnMu.RUnlock()
				if fn != nil {
					if err := fn(ctx, item); err != nil && ctx.Err() == nil {
						e.persistFailures.Add(1)
					}
				}
			}()
		default:
			return
		}
	}
}

func (e *RuleEngine) persistLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case item := <-e.persistCh:
			func() {
				defer func() {
					// Release pending exactly once, even if callback panics.
					if e.persistPending.Add(-1) < 0 {
						e.persistPending.Store(0)
					}
					// Do not recover here fully; let panic propagate to worker.Loop for restart.
					// But we already need to ensure pending released before propagate.
					// Use recover to log then re-panic.
					if r := recover(); r != nil {
						if e.log != nil {
							e.log.Warn("persist callback panicked", logx.Any("panic", r))
						}
						panic(r)
					}
				}()
				e.persistFnMu.RLock()
				fn := e.persistFn
				e.persistFnMu.RUnlock()
				if fn != nil {
					if err := fn(ctx, item); err != nil && ctx.Err() == nil {
						e.persistFailures.Add(1)
					}
				}
			}()
		}
	}
}

// resetDropWarnIfDrained 丢弃告警边沿回落（热点修复 B，errlog 同构）：队列
// 已排空且告警已置位 → 复位。连续风暴期队列恒满不回落（与 errlog 风暴恒满
// 同构），风暴平息排空后下次风暴再告警——每风暴恰好一次。复位点 pin 在消费
// 循环 HandleEvent 之后（rule engine 无周期 flush，loop/Flush/Close 三个消费
// 路径共用同一落点语义；原子 Load/Store，与 Enqueue 无锁竞争）。
func (e *RuleEngine) resetDropWarnIfDrained() {
	if e.warnDropped.Load() && len(e.ch) == 0 {
		e.warnDropped.Store(false)
	}
}

// Close 排空剩余事件（限时，复用 scheduler.Close 模式）；幂等，
// 未 Start 时也可安全排空。循环本身随 Start 的 ctx 取消而退出。
// Fix: cancel persist loop and join in-flight callback; no callback may remain after Close.
func (e *RuleEngine) Close(ctx context.Context) error {
	done := make(chan struct{})
	worker.GoRecover("rule-engine-close", e.log, func() {
		for {
			select {
			case ev := <-e.ch:
				e.HandleEvent(ctx, ev)
				e.resetDropWarnIfDrained()
			default:
				close(done)
				return
			}
		}
	})
	select {
	case <-done:
	case <-ctx.Done():
		if e.log != nil {
			e.log.Warn("rule-engine close timeout, dropping queued events")
		}
	}
	// Cancel persist loop and wait for in-flight callback to finish (worker pattern).
	e.persistMu.Lock()
	cancel := e.persistCancel
	doneCh := e.persistDone
	e.persistMu.Unlock()
	if cancel != nil {
		cancel()
		if doneCh != nil {
			select {
			case <-doneCh:
			case <-ctx.Done():
				if e.log != nil {
					e.log.Warn("rule-engine persist close timeout, waiting for callback")
				}
				<-doneCh
			}
		}
	}
	// Drain any remaining persist items that were queued but not yet processed
	// (pending already includes them; flush will decrement per item).
	// If Start was never called, background loop never ran, so synchronously drain.
	e.flushPersist(ctx)
	return nil
}

// Enqueue 投递事件：有界 channel，满则丢弃（dropped 原子计数）。热点修复 B：
// 逐条 Warn → 阈值告警（errlog 同构）——丢弃累计 ≥ ruleDropWarnThreshold 且
// 边沿未告警 → Warn 恰好一次（带累计数），不再刷屏。热路径纪律：丢弃路径
// 仅两个原子操作（Add + CompareAndSwap），零分配；日志只在阈值跨越时产生。
func (e *RuleEngine) Enqueue(ev Event) {
	select {
	case e.ch <- ev:
	default:
		n := e.dropped.Add(1)
		if n >= uint64(ruleDropWarnThreshold) && e.warnDropped.CompareAndSwap(false, true) {
			if e.log != nil {
				e.log.Warn("rule-engine event queue full, dropping events",
					logx.Int64("dropped", int64(n)),
					logx.Int64("threshold", ruleDropWarnThreshold),
					logx.Int("queue_cap", cap(e.ch)),
				)
			}
		}
	}
}
