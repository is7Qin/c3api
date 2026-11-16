// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestQuotaQueueDropsWhenFull：队列满时 Enqueue 立即返回（非阻塞）且计 dropped；
// Stats 的 queued 反映当前积压。
func TestQuotaQueueDropsWhenFull(t *testing.T) {
	q := NewQuotaQueue(1)
	task := QuotaTask{AccountID: 1, IdentityRevision: 1, Fingerprint: "fp", Quality: "*", State: StateRetryAfter}

	q.Enqueue(task) // 占满容量 1
	enqueued, dropped, queued := q.Stats()
	require.Equal(t, int64(1), enqueued)
	require.Zero(t, dropped)
	require.Equal(t, 1, queued, "积压必须反映已入队未消费的一条")

	start := time.Now()
	q.Enqueue(task) // 满 → 立即丢弃
	elapsed := time.Since(start)
	require.Less(t, elapsed, 100*time.Millisecond, "队列满时 Enqueue 必须立即返回（实测 %v）", elapsed)

	enqueued, dropped, queued = q.Stats()
	require.Equal(t, int64(1), enqueued, "丢弃不得计入入队")
	require.Equal(t, int64(1), dropped, "满 ⇒ dropped+1")
	require.Equal(t, 1, queued, "丢弃不改变积压")
}

// TestQuotaQueueCloseLeavesNoResidue（N4）关闭与并发投递交错的「幽灵任务」：
// 已通过 closed 判定、却在 worker 排空之后才落进 channel 的条目，必须由 Enqueue
// 的投递后复检收回并计 dropped。afterDeliver 把「投递成功、复检之前」钉成
// 确定时序：所有生产者都停在那里时 Close，再放行——复检必须看到 closed 并
// 各自收回一条（通道里条目等价）。没有复检窗口时这仍是一条正常的关闭路径。
func TestQuotaQueueCloseLeavesNoResidue(t *testing.T) {
	const producers = 8
	q := NewQuotaQueue(producers)
	task := QuotaTask{AccountID: 1, IdentityRevision: 1, Fingerprint: "fp", Quality: "*", State: StateRetryAfter}

	var delivered sync.WaitGroup
	delivered.Add(producers)
	release := make(chan struct{})
	q.afterDeliver = func() {
		delivered.Done()
		<-release
	}

	var wg sync.WaitGroup
	wg.Add(producers)
	for i := 0; i < producers; i++ {
		go func() {
			defer wg.Done()
			q.Enqueue(task)
		}()
	}
	delivered.Wait() // 全部投递成功，且全部停在复检之前
	q.Close()
	close(release)
	wg.Wait()

	enqueued, dropped, queued := q.Stats()
	require.Zero(t, queued, "关闭后不得残留幽灵任务")
	require.Equal(t, int64(producers), enqueued)
	require.Equal(t, int64(producers), dropped, "每条交错投递都必须被收回并计 dropped")
	require.Equal(t, enqueued, dropped, "成功投递最终都必须被收回（enqueued == dropped）")
}
