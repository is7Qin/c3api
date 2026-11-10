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

// TestReconcileStaleBatchPreservesNewEntry 旧批次不得删掉新配置的资源（永久用例）：
// 旧批次 A（账号全集里没有账号 2）在锁内取号后、锁外收尾被卡住；新批次 B（账号 2
// 加回）取到更大的号、先应用，随后真实 Prepare 建出账号 2 的新条目；A 恢复。
//
// 账号删除与出口可见性共享**同一个** gen 准入 ⇒ A 整体 no-op，新条目连指针都不变。
//
// 交错只用真实组件：真 ClientTable、真 Prepare、真 withPublisherLock 与账号运行时；
// 判据是资源指针，不是回调调用序，也没有任何 sleep。
func TestReconcileStaleBatchPreservesNewEntry(t *testing.T) {
	table := sdkbridge.NewClientTable()
	adapter := sdkbridge.NewCodex(nil, nil, table, sdkbridge.RotationDeps{})
	cred := &domain.AccountCredential{AccountID: 2, PATKey: "pat"}

	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	s := &Scheduler{publisher: &routingPublisher{}, cfg: Config{
		// A 的锁外收尾被卡住：批次已经拿到旧代号，但还没轮到应用。
		RetireThread:     func(int64, string) { close(entered); <-release },
		ReconcileClients: table.ApplyConfig,
	}}

	go func() {
		defer close(done)
		s.withPublisherLock(func(b *poolReclaim) {
			b.threads = []threadRetire{{accountID: 1, threadKey: "old-thread"}}
			// A = 旧配置：账号全集为空（账号 2 尚未出现）。
			cfg := s.accounts.fullLocked(nil, []*domain.Account{}, nil)
			b.config = &cfg
		})
	}()
	<-entered // A 已取号，并卡在锁外收尾

	// B = 新配置（账号 2 加回）：取更大的号并先完成应用。
	s.withPublisherLock(func(b *poolReclaim) {
		cfg := s.accounts.fullLocked(nil, []*domain.Account{{ID: 2}}, nil)
		b.config = &cfg
	})
	// 真实准备：账号 2 按新配置建出的条目就是"新配置的资源"。
	_, err := adapter.Prepare(cred)
	require.NoError(t, err)
	current := table.Get(2)
	require.NotNil(t, current)

	close(release)
	<-done

	require.Same(t, current, table.Get(2),
		"旧批次的账号缺失必须与新批次共享 gen 准入（不得删掉新配置的条目）")
}

// TestPoolDisappearanceOnlyRetiresThreads 池消失 ≠ 账号移除（§3.3b-4）：K 变 0 让池
// 从注册表消失时，只退役会话线程；账号的传输资源（Auth/usage/client 桶）不动——
// 它仍在账号全集里，资源回收只由全集差分决定。
func TestPoolDisappearanceOnlyRetiresThreads(t *testing.T) {
	spy := &retireSpy{}
	table := sdkbridge.NewClientTable()
	adapter := sdkbridge.NewCodex(nil, nil, table, sdkbridge.RotationDeps{})
	cred := &domain.AccountCredential{AccountID: 7, PATKey: "pat"}

	// 账号资源先建立（配置批次登记它）。
	table.ApplyConfig(1, map[int64]*egress.Client{7: nil})
	_, err := adapter.Prepare(cred)
	require.NoError(t, err)
	before := table.Get(7)
	require.NotNil(t, before)

	pool := newTestPoolWith(2, testPolicy(), spy.fn)
	reclaim := &poolReclaim{retire: spy.fn, apply: table.ApplyConfig}
	collectRetiredPools(
		&identityRegistry{pools: map[int64]*identityPool{7: pool}},
		&identityRegistry{pools: map[int64]*identityPool{}}, // K=0 / 类型变更 / 退出路由集合
		reclaim,
	)
	require.Len(t, reclaim.threads, 2, "池消失只收集会话线程退役")
	require.Nil(t, reclaim.config, "池差分不产生账号配置批次（账号删除只由全集差分判定）")

	reclaim.run()
	require.Len(t, spy.snapshot(), 2)
	require.Same(t, before, table.Get(7),
		"账号资源仍按账号全集保留（池消失不冒充账号移除）")
}
