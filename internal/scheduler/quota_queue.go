// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import "sync/atomic"

// quotaQueueDefault 有界队列的默认容量（不引入运维配置键：零值即取默认）。
const quotaQueueDefault = 256

// QuotaQueue 429 补拉任务的有界投递面：只做「有界 channel + 入队/丢弃计数」，
// 不认识 resolver / health / scheduler——正是这一层解耦把依赖图从
// 「worker ⟷ LatchSink」的构造环里拆出来（sink 只依赖本队列，worker 消费它）。
//
// 投递纪律：非阻塞，满即丢弃计数，绝不给请求热路径加等待。
type QuotaQueue struct {
	ch       chan QuotaTask
	closed   atomic.Bool // 终态：关闭后一律丢弃计数，绝不再入队
	enqueued atomic.Int64
	dropped  atomic.Int64

	// afterDeliver 仅测试注入（生产 nil）：投递成功之后、复检 closed 之前调用。
	// 它把「关闭与投递交错」的窗口钉成确定时序（否则只能靠调度概率）；热路径
	// 只多一次 nil 判断，不改变生产语义。与 RuntimeHealth.now 同类 seam。
	// （只许在启动任何生产者 goroutine 之前赋值；运行期写入是数据竞争）
	afterDeliver func()
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
// closed 判定必须在 select 之前：关闭后即便 channel 还有空位也不得入队——
// 此时已无消费者，入队只会留下一条永远不会被补拉、也永远不会被计入丢弃的
// 幽灵任务。closed 判定与投递不原子（无锁热路径）：投递成功后必须复检一次，
// 把「已通过判定、却在 worker 排空之后才落进 channel」的那条收回并计 dropped。
// 通道里条目等价，收回谁都一样；enqueued 含被收回的那次投递，故 backlog =
// enqueued - dropped - 已被消费数（queue 本身无消费者时即 enqueued - dropped）。
// 「通道里条目等价」以**终态**语义为前提：将来若把 closed 复用于暂停/弃单这类
// 非终态开关，收回会丢掉别人的任务。
func (q *QuotaQueue) Enqueue(t QuotaTask) {
	if q.closed.Load() {
		q.dropped.Add(1)
		return
	}
	select {
	case q.ch <- t:
		q.enqueued.Add(1)
		if q.afterDeliver != nil {
			q.afterDeliver()
		}
		if q.closed.Load() {
			// 关闭与投递交错：把刚投进去的那条收回（通道里条目等价），否则它
			// 既不被消费也不计 dropped。非阻塞：并发消费者可能已经取走一条，
			// 那时本次投递已有了归属，无需再补。
			select {
			case <-q.ch:
				q.dropped.Add(1)
			default:
			}
		}
	default:
		q.dropped.Add(1)
	}
}

// Close 置终态：此后 Enqueue 一律丢弃计数、不入队。幂等（worker.Close 内调用，
// 也可能被装配期早退路径重复调用）。
func (q *QuotaQueue) Close() { q.closed.Store(true) }

// Stats 观测快照：成功入队累计 / 丢弃累计 / 当前积压。
func (q *QuotaQueue) Stats() (enqueued, dropped int64, queued int) {
	return q.enqueued.Load(), q.dropped.Load(), len(q.ch)
}

var _ quotaEnqueuer = (*QuotaQueue)(nil)
