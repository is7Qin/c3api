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
// 都把「账号全集 → 当前出口」翻译成**资源指令**交给传输资源表（§3.2 出口/transport
// 变更、§3.3b-1 职责边界）。
//
// 没有这条接线，ApplyBatch 就是死代码：资源可见性永远停在某一版，代理 CRUD 之后旧
// 摘要不会被摘。
func TestReloadReconcilesClientEgress(t *testing.T) {
	pool := egress.NewPool(nil)
	px := &domain.Proxy{ID: 7, Scheme: domain.ProxySchemeSOCKS5, Host: "10.1.2.3", Port: 1080,
		Username: "u", Password: "p1", Enabled: true}
	tplA := tpl(1, domain.FormatOpenAIResponses, []string{"gpt-4o"})
	bound := proxyAcc(t, 1, tplA, px)
	direct := acc(2, tplA, 4)
	loader := newMemLoader(map[int64][]*domain.Account{10: {bound, direct}})

	// 表替身预置"已持有 1 与 2 的资源"：账号删除的指令只能由控制器差分得出。
	table := newRecordingResources(1, 2)
	cfg := testCfg()
	cfg.Egress = pool
	cfg.ClientResources = table
	s := newSchedWithCfg(t, cfg, loader)

	require.Equal(t, 1, table.count(), "全量 reload 必须触发一次配置面回收")

	keep1, ok := table.retained(0, 1)
	require.True(t, ok, "绑定代理的账号必须被保留")
	require.Equal(t, []domain.EgressResourceKey{domain.EgressResourceKey(egress.New("socks5", "10.1.2.3", 1080, "u", "p1").Key()), {}},
		keep1, "保留键 = 当前出口摘要 + 直连（直连恒保留是控制器策略）")
	keep2, ok := table.retained(0, 2)
	require.True(t, ok)
	require.Equal(t, []domain.EgressResourceKey{{}}, keep2, "未绑定 = 只有直连键（零值摘要）")
	require.Empty(t, table.deleted(0), "全员在场的批次不点名删除")

	// 代理密码轮换（配置变更）：装载器像生产一样给出**新的**对象（新 Proxy 值 + 新账号），
	// 下一次 reload 的保留键就是新摘要——旧摘要不再被保留，该出口下的桶由表摘除。
	px2 := *px
	px2.Password = "p2"
	loader.set(10, []*domain.Account{proxyAcc(t, 1, tplA, &px2), direct})
	require.NoError(t, s.InvalidateAllSync())
	require.Equal(t, 2, table.count(), "重载必须再次下发指令")
	keep1, _ = table.retained(1, 1)
	require.Equal(t, domain.EgressResourceKey(egress.New("socks5", "10.1.2.3", 1080, "u", "p2").Key()), keep1[0],
		"新配置的出口摘要（旧摘要不再出现 ⇒ 历史规格被摘除）")
	require.NotEqual(t, domain.EgressResourceKey(egress.New("socks5", "10.1.2.3", 1080, "u", "p1").Key()), keep1[0])

	// 账号消失 ⇒ 控制器点名删除（含没有会话池的账号）。
	loader.set(10, []*domain.Account{direct})
	require.NoError(t, s.InvalidateAllSync())
	require.Equal(t, 3, table.count())
	require.Equal(t, []int64{1}, table.deleted(2), "被删除的账号由控制器点名 ResourceDelete")
	if _, ok := table.retained(2, 1); ok {
		require.FailNow(t, "已删除账号不得同时出现在保留指令里")
	}
	_, ok = table.retained(2, 2)
	require.True(t, ok, "仍存在的账号继续被保留")

	// 批次代号必须严格单调（锁内取号；传输资源表只接受更新的号）。
	require.Equal(t, []uint64{1, 2, 3}, table.gens(), "每次配置面回收取一个新号")
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

// TestReconcileCoversUngroupedAccounts 配置面的输入是**账号全集**：未编组账号不进路由
// 根（无组即无 route），但仍是账号全集的一部分——不给它们登记资源可见性，每次 reload
// 都会把管理面 usage 刚建的条目/桶摘掉。没有会话池的账号（K=0）同样在集合里。
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

	table := newRecordingResources(1, 2)
	cfg := testCfg()
	cfg.Egress = pool
	cfg.ClientResources = table
	s := newSchedWithCfg(t, cfg, loader)

	require.Equal(t, 1, table.count())
	_, ok := table.retained(0, 1)
	require.True(t, ok, "组内账号（K=0，没有池）在账号全集里")
	_, ok = table.retained(0, 2)
	require.True(t, ok, "未编组账号也在账号全集里")

	// 组级增量路径不重扫账号表：账号 1 从组里消失，但仍在账号全集 ⇒ 资源保留。
	loader.set(10, nil)
	s.InvalidateGroups([]int64{10})
	require.Equal(t, 2, table.count())
	_, ok = table.retained(1, 1)
	require.True(t, ok, "离开组 ≠ 账号移除：资源仍按全集保留")
	_, ok = table.retained(1, 2)
	require.True(t, ok, "组级路径仍保留未编组账号（基线）")
	require.Empty(t, table.deleted(1))

	// 未编组账号的删除同样只能由下一次全量装载发现（组级路径不重扫账号表）。
	loader.setUngrouped(nil)
	s.InvalidateGroups([]int64{10})
	require.Equal(t, 3, table.count())
	_, ok = table.retained(2, 2)
	require.True(t, ok, "组级路径不重扫账号表：基线保留（已披露边界）")
	require.NoError(t, s.InvalidateAllSync())
	require.Equal(t, 4, table.count())
	got := table.deleted(3)
	require.ElementsMatch(t, []int64{1, 2}, got,
		"全量装载发现账号已从组与全集消失 ⇒ 控制器点名删除两者")
}

// TestFullReloadKeepsUngroupedEgressInBothLayers 两级出口资源消费**同一份**快照
// （§3.3b-1 的结构要求）：未编组账号独占的代理出口在全量 reload 后必须仍留在
// egress.Pool 里（不被按路由根口径误剪），并在同一批次里作为保留键交给传输资源表。
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

	table := newRecordingResources()
	cfg := testCfg()
	cfg.Egress = pool
	cfg.ClientResources = table
	newSchedWithCfg(t, cfg, loader)

	require.Same(t, want, pool.For(spec), "未编组账号的出口 client 不得被按 byID 口径误剪")
	require.Equal(t, 1, pool.Size())
	keep, ok := table.retained(0, 3)
	require.True(t, ok)
	require.Equal(t, domain.EgressResourceKey(egress.ResourceKey(want)), keep[0],
		"同一批次交给传输资源表的是同一个出口的资源键")
}

// TestReconcileBatchesTakeSequenceInsidePublisherLock A/B 屏障：旧批次 A 在发布锁内
// 取号后、锁外 socket 收尾卡住；新批次 B 在同锁内取到更大的号并**先**完成应用；A
// 恢复后才轮到它的应用。代号必须仍按**锁内顺序**单调（[2,1]）——传输资源表用这个
// 号拒绝旧批次回退资源可见性。
func TestReconcileBatchesTakeSequenceInsidePublisherLock(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})
	var mu sync.Mutex
	var applied []uint64
	table := &applyRecorder{onApply: func(gen uint64) {
		mu.Lock()
		applied = append(applied, gen)
		mu.Unlock()
	}}
	s := &Scheduler{publisher: &routingPublisher{}, cfg: Config{
		RetireThread: func(int64, string) {
			close(entered)
			<-release
		},
		ClientResources: table,
	}}

	// 批次 A = 旧配置（账号 1）；线程退役把它卡在锁外。
	go func() {
		defer close(done)
		s.withPublisherLock(func(b *poolReclaim) {
			b.threads = []threadRetire{{accountID: 1, threadKey: "old-thread"}}
			cfg := s.accounts.fullLocked(nil, []*domain.Account{{ID: 1}}, nil, s.clientAccounts())
			b.config = &cfg
		})
	}()
	<-entered // A 已取号（1）并进入锁外收尾

	// 批次 B = 新配置（删 1、加 2）：在 A 的锁外收尾期间取号（2）并先应用。
	s.withPublisherLock(func(b *poolReclaim) {
		cfg := s.accounts.fullLocked(nil, []*domain.Account{{ID: 2}}, nil, s.clientAccounts())
		b.config = &cfg
	})

	close(release)
	<-done

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []uint64{2, 1}, applied, "应用顺序被交错（B 先），代号仍按锁内顺序单调")
	require.Equal(t, uint64(2), s.accounts.gen)
}

// applyRecorder 只记录批次代号的传输资源表替身。
type applyRecorder struct {
	onApply func(gen uint64)
}

func (r *applyRecorder) Accounts() []int64 { return nil }

func (r *applyRecorder) ApplyBatch(batch domain.ResourceBatch) {
	if r.onApply != nil {
		r.onApply(batch.Gen)
	}
}
