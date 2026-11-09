// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/pkg/egress"
)

// TestReconcileStaleRemovalPreservesNewEntry oracle R4 B1 的确定性交错回归（永久用例）：
// 旧批次 A（删账号 2）在锁内取号后、锁外收尾被卡住；新批次 B（账号 2 加回）取到更大的
// 号、先应用，随后真实 Prepare 建出账号 2 的新条目；A 恢复。
//
//   - reconcile-only（对照）：A 不携带 remove，只证明 gen 守卫拒绝旧活跃集；
//   - stale-removal（本轮的 blocking）：A 携带 remove=[2]。删除与活跃集共享**同一个**
//     批次准入 ⇒ A 整体 no-op，新条目连指针都不变。旧实现里 RemoveAccount 在
//     Reconcile 之前执行，这里会拿到 nil（oracle 探针的红）。
//
// 交错只用真实组件：真 ClientTable、真 Prepare、真 withPublisherLock /
// collectActiveEgress / collectRemovedAccounts；判据是资源指针，不是回调调用序，
// 也没有任何 sleep。
func TestReconcileStaleRemovalPreservesNewEntry(t *testing.T) {
	for _, remove := range []bool{false, true} {
		name := map[bool]string{false: "reconcile-only", true: "stale-removal"}[remove]
		t.Run(name, func(t *testing.T) {
			table := sdkbridge.NewClientTable()
			adapter := sdkbridge.NewCodex(nil, nil, table, sdkbridge.RotationDeps{})
			cred := &domain.AccountCredential{AccountID: 2, PATKey: "pat"}

			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			s := &Scheduler{publisher: &routingPublisher{}, cfg: Config{
				// A 的锁外收尾被卡住：批次已经拿到旧代号，但还没轮到应用。
				RetireThread:     func(int64, string) { close(entered); <-release },
				ReconcileClients: table.Reconcile,
			}}

			go func() {
				defer close(done)
				s.withPublisherLock(func(b *poolReclaim) {
					b.threads = []threadRetire{{accountID: 1, threadKey: "old-thread"}}
					// A = 旧配置：账号全集只有 1。
					s.collectActiveEgress(nil, map[int64]*egress.Client{1: nil}, b)
					if remove {
						// 池注册表差分：账号 2 在旧表里、不在新表里。
						collectRemovedAccounts(
							&identityRegistry{pools: map[int64]*identityPool{2: nil}},
							&identityRegistry{pools: map[int64]*identityPool{}},
							b,
						)
					}
				})
			}()
			<-entered // A 已取号，并卡在锁外收尾

			// B = 新配置（账号 2 加回）：取更大的号并先完成应用。
			s.withPublisherLock(func(b *poolReclaim) {
				s.collectActiveEgress(nil, map[int64]*egress.Client{2: nil}, b)
			})
			// 真实准备：账号 2 按新配置建出的条目就是"新配置的资源"。
			_, err := adapter.Prepare(cred)
			require.NoError(t, err)
			current := table.Get(2)
			require.NotNil(t, current)

			close(release)
			<-done

			require.Same(t, current, table.Get(2),
				"旧批次的删除必须与新批次共享 gen 准入（不得删掉新配置的条目）")
		})
	}
}
