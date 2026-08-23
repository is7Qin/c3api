// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

// 分表设计并行隔离（用户裁决 2026-08-11）：计费游标消费者（F2 后无内存队列——
// 消费 DB 游标扣费）与 errlog worker（错误审计明细 → err_logs）在代理内并行
// 运行，两路无共享可变状态（Flusher 游标周期 vs ErrLogWorker 队列）。并发喂入
// 两路 → 各自排空计数精确，互不串路、互不阻塞。

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/usage"
)

// captureErrInserter errlog 写者捕获（InsertBatch 追加）。
type captureErrInserter struct {
	mu  sync.Mutex
	ids []string
}

func (c *captureErrInserter) InsertBatch(ctx context.Context, logs []*domain.UsageLog) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, l := range logs {
		c.ids = append(c.ids, l.RequestID)
	}
	return nil
}

func (c *captureErrInserter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.ids)
}

// TestFlusherErrlogWorkerParallelIsolated 并行隔离：游标消费者 Close 排空
//（DB 游标真值）与 errlog worker 并发喂入/排空同时进行 → 各自完整收敛
//（flusher 全部行 billed 翻转；errlog 捕获 = 拒绝行数），互不串路。
func TestFlusherErrlogWorkerParallelIsolated(t *testing.T) {
	store := newFakeLedgerStore()
	const billed, rejected = 2000, 2000
	for i := 1; i <= billed; i++ {
		store.seedRow(int64(i), int64(i%4+1), 100, time.Now())
		store.setBalance(int64(i%4+1), 1_000_000)
	}
	f := newFlusherWith(store, 4, map[int64]int64{1: 1e9, 2: 1e9, 3: 1e9, 4: 1e9})

	errs := &captureErrInserter{}
	ew := usage.NewErrLogWorker(usage.ErrLogConfig{QueueSize: 4096, FlushInterval: time.Hour}, errs, nil)

	var wg sync.WaitGroup
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < rejected/4; i++ {
				ew.EnqueueRejected(&domain.UsageLog{
					RequestID: "rej", Model: "m",
					StatusCode: 429, ErrorType: domain.Err429, CreatedAt: time.Now(),
				})
			}
		}(g)
	}

	closeDone := make(chan struct{})
	go func() { defer close(closeDone); _ = f.Close(context.Background()) }() // 排空至游标清空

	wg.Wait()
	select {
	case <-closeDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Close 未在预算内排空游标")
	}
	require.NoError(t, ew.Close(context.Background()))

	require.Equal(t, 0, store.unbilledCount(), "计费游标独立排空全部 billed 行（billed 翻转）")
	var consumed int64
	for _, c := range store.deductSnapshot() {
		consumed += int64(len(c.ids))
	}
	require.Equal(t, int64(billed), consumed, "扣费事务逐组推进合计（消费不受 errlog 并行影响）")
	require.Equal(t, rejected, errs.count(), "errlog worker 独立排空全部拒绝行（与 flusher 互不串路）")
	require.Zero(t, ew.Queued(), "errlog 队列排空")
}
