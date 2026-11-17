// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"sync"
	"sync/atomic"
)

// quotaQueueDefault 有界队列的默认容量（不引入运维配置键：零值即取默认）。
const quotaQueueDefault = 256

// QuotaQueue 429 补拉任务的有界投递面：只做「有界 channel + 入队/丢弃计数」，
// 不认识 resolver / health / scheduler——正是这一层解耦把依赖图从
// 「worker ⟷ LatchSink」的构造环里拆出来（sink 只依赖本队列，worker 消费它）。
//
// 投递纪律：非阻塞，满即丢弃计数，绝不给请求热路径加等待。
//
// 判定与投递在同一把锁里（mu）：closed 判定 + 非阻塞投递/计数是一个原子单元，
// 于是「已关闭」是真正的终态——Close 置位之后不可能再有条目落进 channel，既不
// 需要概率性时序，也不需要"投递后复检 + 收回"。锁只在 Enqueue 进一次（429 失败
// 路径，一次请求至多一条任务；实测 8 并发仅 +7~11 ns/op）。
type QuotaQueue struct {
	ch chan QuotaTask

	// mu 护「closed 判定 + 投递/计数」（Enqueue）与置位（Close）。计数器本身是
	// atomic：Stats 读路径零锁。
	mu       sync.Mutex
	closed   bool
	enqueued atomic.Int64
	dropped  atomic.Int64
}

// NewQuotaQueue 构造有界队列；size<=0 取默认容量。
func NewQuotaQueue(size int) *QuotaQueue {
	if size <= 0 {
		size = quotaQueueDefault
	}
	return &QuotaQueue{ch: make(chan QuotaTask, size)}
}

// Enqueue 非阻塞投递：已关闭或队列满 → 丢弃 + 计数（绝不阻塞调用方）。
//
// closed 判定必须在投递之前、且与投递同锁：关闭后即便 channel 还有空位也不得
// 入队——此时已无消费者，入队只会留下一条永远不会被补拉、也永远不会被计入丢弃
// 的幽灵任务。同锁换来两条硬不变量：
//   - 每次调用恰计入 enqueued/dropped 之一 ⇒ enqueued + dropped == 调用次数；
//   - Close 返回后 queued 恒 0（置位后不可能再有投递，排空看到的就是终态）。
func (q *QuotaQueue) Enqueue(t QuotaTask) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		q.dropped.Add(1)
		return
	}
	select {
	case q.ch <- t:
		q.enqueued.Add(1)
	default:
		q.dropped.Add(1)
	}
}

// Close 置终态：此后 Enqueue 一律丢弃计数、不入队；并把剩余条目排空转入 dropped。
// 幂等（worker.Close 内调用，
// 也可能被装配期早退路径重复调用）。
//
// 置位在锁内，排空在锁外：置位之后不可能再有新投递（Enqueue 的判定与投递同锁），
// 故锁外排空看到的就是终态剩余——不会漏（剩余全在 channel 里）、也不会多（不会
// 再进来）。排空按条从 enqueued 转入 dropped：两条计数因此始终满足
// enqueued + dropped == Enqueue 调用次数（没有第三条记账路径），关闭瞬间的剩余
// 量也不会凭空从账上消失。
func (q *QuotaQueue) Close() {
	q.mu.Lock()
	q.closed = true
	q.mu.Unlock()
	for {
		select {
		case <-q.ch:
			q.enqueued.Add(-1)
			q.dropped.Add(1)
		default:
			return
		}
	}
}

// Stats 观测快照：成功入队累计 / 丢弃累计 / 当前积压。
//
// Close 返回后，enqueued + dropped 恒等于 Enqueue 调用次数（排空过程中每条转移有
// 瞬时 sum−1）：关闭排空只是把剩余条目从 enqueued 转进 dropped（两者之和不变），
// 故守恒是被同一把锁保证的强不变量，不是概率性质。
func (q *QuotaQueue) Stats() (enqueued, dropped int64, queued int) {
	return q.enqueued.Load(), q.dropped.Load(), len(q.ch)
}

var _ quotaEnqueuer = (*QuotaQueue)(nil)
