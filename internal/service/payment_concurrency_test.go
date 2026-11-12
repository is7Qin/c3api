// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// payment_concurrency_test.go P4（v1 §11，执行面 PG；本文件在 fake 面把**可判定的
// 部分**钉死）：两个 Service 实例共享同一 store，8 个并发回调同一张 pending 单
// → 只入账一次；`testAfterOrderLock` 钩子记录到的 `pending` 恰 1 次、其余都是
// `paid`。
//
// 为什么 fake 面也能判：fakeStore.WithTx 在整段事务期间持锁（= 单订单行锁的语义
// 等价物），提交才可见；因此"看到 pending 的并发者"只可能有一个。真实 PG 的
// `FOR UPDATE` 变异（M13）无法在 fake 上被观测——那一条必须 PG，见 §14。
//
// 禁 time.Sleep：并发靠 close(start) 齐发 + WaitGroup 收口；钩子只写切片，不碰
// store（否则会与 WithTx 的锁互等）。

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestCompleteOrderConcurrentSingleCredit P4：8 并发（2 实例共享 store）只入账一次。
func TestCompleteOrderConcurrentSingleCredit(t *testing.T) {
	fs := newFakeStore()
	svcA := &Service{store: fs, inv: &invRecorder{}}
	svcB := &Service{store: fs, inv: &invRecorder{}}

	var mu sync.Mutex
	seen := make([]domain.OrderStatus, 0, 8)
	hook := func(_ context.Context, o *domain.Order) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, o.Status)
	}
	svcA.testAfterOrderLock = hook
	svcB.testAfterOrderLock = hook

	ctx := context.Background()
	u := seedUser(t, fs, "concurrent@example.com", 0, 0)
	o := seedPendingOrder(t, fs, u.ID)

	const workers = 8
	start := make(chan struct{})
	errs := make([]error, workers)
	already := make([]bool, workers)
	var wg sync.WaitGroup
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			svc := svcA
			if i%2 == 1 {
				svc = svcB // 第二个实例共享同一 store（多副本部署的等价物）
			}
			<-start
			already[i], errs[i] = svc.CompleteOrder(ctx, paidInput(o.OutTradeNo))
		}(i)
	}
	close(start)
	wg.Wait()

	credited := 0
	for i, err := range errs {
		require.NoError(t, err, "并发回调不得报错（重复通知是常态）: worker %d", i)
		if !already[i] {
			credited++
		}
	}
	require.Equal(t, 1, credited, "8 个并发回调里只允许 1 个真正入账")

	pendingSeen := 0
	for _, st := range seen {
		if st == domain.OrderStatusPending {
			pendingSeen++
		}
	}
	require.Equal(t, 1, pendingSeen, "行锁语义：`pending` 只能被看见一次（其余都应是 paid）")
	require.Len(t, seen, workers)

	user, err := fs.GetUser(ctx, u.ID)
	require.NoError(t, err)
	require.Equal(t, int64(100_000), user.Balance, "只入账一次")
	require.Len(t, fs.codes, 1, "只应产生 1 张码（失败/幂等的那几次不得留下码）")
	require.Len(t, fs.uses, 1)
	require.Len(t, fs.balanceLogs, 1)
}
