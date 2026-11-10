// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/egress"
)

// TestReloadReconcilesClientEgress 配置面回收的**接线**：每次全量 reload / 组级批重载
// 都把「账号全集 → 当前 responses 出口」交给 ClientTable（§3.2 出口/transport 变更）。
//
// 没有这条接线，ApplyConfig 就是死代码：资源可见性永远停在某一版，代理 CRUD 之后旧
// 摘要不会被摘。
func TestReloadReconcilesClientEgress(t *testing.T) {
	pool := egress.NewPool(nil)
	px := &domain.Proxy{ID: 7, Scheme: domain.ProxySchemeSOCKS5, Host: "10.1.2.3", Port: 1080,
		Username: "u", Password: "p1", Enabled: true}
	tplA := tpl(1, domain.FormatOpenAIResponses, []string{"gpt-4o"})
	bound := proxyAcc(t, 1, tplA, px)
	direct := acc(2, tplA, 4)
	loader := newMemLoader(map[int64][]*domain.Account{10: {bound, direct}})

	var got []map[int64]*egress.Client
	var gens []uint64
	cfg := testCfg()
	cfg.Egress = pool
	cfg.ReconcileClients = func(gen uint64, active map[int64]*egress.Client) {
		gens = append(gens, gen)
		got = append(got, active)
	}
	s := newSchedWithCfg(t, cfg, loader)

	require.Len(t, got, 1, "全量 reload 必须触发一次配置面回收")
	require.Contains(t, got[0], int64(1), "账号全集：绑定代理的账号")
	require.Contains(t, got[0], int64(2), "账号全集：直连账号（活跃集恒含直连）")
	require.Same(t, pool.Direct(), got[0][2], "未绑定 = 直连出口（零值摘要）")
	require.Equal(t, egress.New("socks5", "10.1.2.3", 1080, "u", "p1").Key(), got[0][1].Spec.Key(),
		"绑定账号的活跃集 = 它当前配置的出口摘要")

	// 代理密码轮换（配置变更）：装载器像生产一样给出**新的**对象（新 Proxy 值 + 新账号），
	// 下一次 reload 传的就是新摘要——旧摘要不在集合里，该出口下的桶由 ApplyConfig 摘除。
	px2 := *px
	px2.Password = "p2"
	loader.set(10, []*domain.Account{proxyAcc(t, 1, tplA, &px2), direct})
	require.NoError(t, s.InvalidateAllSync())
	require.Len(t, got, 2, "重载必须再次交出活跃集")
	require.Equal(t, egress.New("socks5", "10.1.2.3", 1080, "u", "p2").Key(), got[1][1].Spec.Key(),
		"新配置的出口摘要（旧摘要不再出现 ⇒ 历史规格被摘除）")
	require.NotContains(t, [][16]byte{got[1][1].Spec.Key()}, egress.New("socks5", "10.1.2.3", 1080, "u", "p1").Key())

	// 账号消失 ⇒ 集合里不再有它（整账号摘除的判定输入，含没有会话池的账号）。
	loader.set(10, []*domain.Account{direct})
	require.NoError(t, s.InvalidateAllSync())
	require.Len(t, got, 3)
	require.NotContains(t, got[2], int64(1), "被删除的账号不再出现在活跃集里")
	require.Contains(t, got[2], int64(2))

	// 批次代号必须严格单调（锁内取号；ClientTable 只接受更新的号）。
	require.Equal(t, []uint64{1, 2, 3}, gens, "每次配置面回收取一个新号")
}

// universeLoader 在 memLoader 之上实现 AccountUniverseLoader：`ungrouped` 是**不在任何
// 组**的账号——生产里 GroupRepo.LoadGroupsAccountsFull 用同一次账号表扫描给出。
type universeLoader struct {
	*memLoader
	mu        sync.Mutex
	ungrouped []*domain.Account
}

func (l *universeLoader) LoadGroupsAccountsFull(ctx context.Context) (map[int64][]*domain.Account, []*domain.Account, error) {
	m, err := l.LoadGroupsAccounts(ctx)
	if err != nil {
		return nil, nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	seen := make(map[int64]struct{})
	all := make([]*domain.Account, 0, len(m)+len(l.ungrouped))
	for _, accs := range m {
		for _, a := range accs {
			if _, ok := seen[a.ID]; ok {
				continue
			}
			seen[a.ID] = struct{}{}
			all = append(all, a)
		}
	}
	for _, a := range l.ungrouped {
		if _, ok := seen[a.ID]; ok {
			continue
		}
		seen[a.ID] = struct{}{}
		all = append(all, a)
	}
	return m, all, nil
}

func (l *universeLoader) setUngrouped(accs []*domain.Account) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ungrouped = accs
}

// TestReconcileCoversUngroupedAccounts 配置面活跃集的输入是**账号全集**：未编组账号
// 不进路由根（无组即无 route），但仍是账号全集的一部分——不给它们登记资源可见性，
// 每次 reload 都会把管理面 usage 刚建的条目/桶摘掉。没有会话池的账号（K=0）同样在
// 集合里。
//
// 组级增量不重扫账号表：从组里移除**不是**账号移除（账号仍在全集/数据库里），资源
// 保留；真正的删除只由下一次全量装载的全集差分发现（§3.3b-4）。
func TestReconcileCoversUngroupedAccounts(t *testing.T) {
	pool := egress.NewPool(nil)
	tplCodex := tpl(1, domain.FormatOpenAIResponses, []string{"gpt-5"})
	tplCodex.CredentialType = credential.TypeCodexOAuth
	grouped := acc(1, tplCodex, 0)   // K=0 ⇒ 没有会话池
	ungrouped := acc(2, tplCodex, 0) // 不在任何组
	loader := &universeLoader{
		memLoader: newMemLoader(map[int64][]*domain.Account{10: {grouped}}),
		ungrouped: []*domain.Account{ungrouped},
	}

	var got []map[int64]*egress.Client
	cfg := testCfg()
	cfg.Egress = pool
	cfg.ReconcileClients = func(_ uint64, active map[int64]*egress.Client) { got = append(got, active) }
	s := newSchedWithCfg(t, cfg, loader)

	require.Len(t, got, 1)
	require.Contains(t, got[0], int64(1), "组内账号（K=0，没有池）在活跃集里")
	require.Contains(t, got[0], int64(2), "未编组账号也在账号全集里")

	// 组级增量路径不重扫账号表：账号 1 从组里消失，但仍在账号全集 ⇒ 资源保留。
	loader.set(10, nil)
	s.InvalidateGroups([]int64{10})
	require.Len(t, got, 2)
	require.Contains(t, got[1], int64(1), "离开组 ≠ 账号移除：资源仍按全集保留")
	require.Contains(t, got[1], int64(2), "组级路径仍保留未编组账号（基线）")

	// 未编组账号的删除同样只能由下一次全量装载发现（组级路径不重扫账号表）。
	loader.setUngrouped(nil)
	s.InvalidateGroups([]int64{10})
	require.Len(t, got, 3)
	require.Contains(t, got[2], int64(2), "组级路径不重扫账号表：基线保留（已披露边界）")
	require.NoError(t, s.InvalidateAllSync())
	require.Len(t, got, 4)
	require.NotContains(t, got[3], int64(1), "全量装载发现账号已从组与全集消失")
	require.NotContains(t, got[3], int64(2), "全量装载发现未编组账号已删除")
}

// TestFullReloadKeepsUngroupedEgressInBothLayers 两级出口资源消费**同一份**快照
// （§3.3b-1 的结构要求）：未编组账号独占的代理出口在全量 reload 后必须仍留在
// egress.Pool 里（不被按路由根口径误剪），并在同一批次里交给 ClientTable。
func TestFullReloadKeepsUngroupedEgressInBothLayers(t *testing.T) {
	pool := egress.NewPool(nil)
	px := &domain.Proxy{ID: 7, Scheme: domain.ProxySchemeSOCKS5, Host: "10.9.9.9", Port: 1080,
		Username: "u", Password: "p", Enabled: true}
	tplCodex := tpl(1, domain.FormatOpenAIResponses, []string{"gpt-5"})
	tplCodex.CredentialType = credential.TypeCodexOAuth
	ungrouped := proxyAcc(t, 3, tplCodex, px)
	loader := &universeLoader{
		memLoader: newMemLoader(map[int64][]*domain.Account{}),
		ungrouped: []*domain.Account{ungrouped},
	}
	spec := egress.New("socks5", "10.9.9.9", 1080, "u", "p")
	want := pool.For(spec)

	var got map[int64]*egress.Client
	cfg := testCfg()
	cfg.Egress = pool
	cfg.ReconcileClients = func(_ uint64, active map[int64]*egress.Client) { got = active }
	newSchedWithCfg(t, cfg, loader)

	require.Same(t, want, pool.For(spec), "未编组账号的出口 client 不得被按 byID 口径误剪")
	require.Equal(t, 1, pool.Size())
	require.Same(t, want, got[3], "同一批次交给 ClientTable 的是同一个出口指针")
}

// TestReconcileBatchesTakeSequenceInsidePublisherLock A/B 屏障：旧批次 A 在发布锁内
// 取号后、锁外 socket 收尾卡住；新批次 B 在同锁内取到更大的号并**先**完成应用；A
// 恢复后才轮到它的应用。代号必须仍按**锁内顺序**单调（[2,1]）——ClientTable 用这个
// 号拒绝旧批次回退资源可见性。
func TestReconcileBatchesTakeSequenceInsidePublisherLock(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	var mu sync.Mutex
	var applied []uint64
	s := &Scheduler{publisher: &routingPublisher{}, cfg: Config{
		RetireThread: func(int64, string) {
			close(entered)
			<-release
		},
		ReconcileClients: func(gen uint64, _ map[int64]*egress.Client) {
			mu.Lock()
			applied = append(applied, gen)
			mu.Unlock()
		},
	}}

	// 批次 A = 旧配置（账号 1）；线程退役把它卡在锁外。
	go func() {
		defer close(done)
		s.withPublisherLock(func(b *poolReclaim) {
			b.threads = []threadRetire{{accountID: 1, threadKey: "old-thread"}}
			cfg := s.accounts.fullLocked(nil, []*domain.Account{{ID: 1}}, nil)
			b.config = &cfg
		})
	}()
	<-entered // A 已取号（1）并进入锁外收尾

	// 批次 B = 新配置（删 1、加 2）：在 A 的锁外收尾期间取号（2）并先应用。
	s.withPublisherLock(func(b *poolReclaim) {
		cfg := s.accounts.fullLocked(nil, []*domain.Account{{ID: 2}}, nil)
		b.config = &cfg
	})

	close(release)
	<-done

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []uint64{2, 1}, applied, "应用顺序被交错（B 先），代号仍按锁内顺序单调")
	require.Equal(t, uint64(2), s.accounts.gen)
}
