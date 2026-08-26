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
			return
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
