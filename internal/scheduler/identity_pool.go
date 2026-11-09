// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
//
// codex 会话池（账号级 K 会话）：一个会话 = 一条上游线程的完整生命周期——
// installation / thread / window 计数 / turn-state / 连接身份。身份不再是客户端的
// 构造期配置，而是**请求数据**：会话池摊开成 `Identity` 交给一次调用（spec S4 §3.1）。
//
// 并发模型沿用原槽位池：认领 = 旋转游标 + 逐槽 CAS，归还 = Store(false)——无
// channel、无 mutex、请求路径零每请求锁。会话状态字段是**普通字段**：busy 独占
// 保证租借期内只有一个读者/写者，所以不再套 atomic（§3.1）。
package scheduler

import (
	"sync/atomic"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/egress"
	"github.com/is7qin/c3api/pkg/logx"
)

// codexSession 一个会话：busy 独占标志 + 线程生命周期状态（§3.1 字段表）。
//
// 租借（busy.CAS false→true）成功的一方独占全部状态字段直到 release：读写都在
// 租借期内，没有第二读者。credOwner 记录本会话已对齐的凭据 owner——它跟着**会话
// 对象**走（含跨池迁移），不复制到池上：池会被整体替换，复制即丢失效（§3.2）。
type codexSession struct {
	busy         atomic.Bool
	installation string
	thread       string
	windowN      uint64
	windowID     string
	threadKey    string
	turns        uint64
	nextAt       uint64
	wmax         uint64
	turnState    string
	policy       codexsdk.RotatePolicy
	// credOwner 已对齐的凭据 owner 签名（sdkbridge.CredOwner）。零值 = 尚未对齐
	// （新会话没有 ts 可清，首次对齐只是把它写上）。
	credOwner [16]byte
}

// SetTurnState 是 SDK 的 TurnStateSink：一次普通赋值（§3.2）。
//
// 契约保证只在租借期内被调用（`Body.Close`/聚合返回之后不再回调，§2.2），所以
// 这里既不加锁也不做代际判定。
func (s *codexSession) SetTurnState(ts string) { s.turnState = ts }

// identity 本次请求的身份数据（§3.2 租借）：Session 与 Meta 同源（session ==
// thread，根线程语义），ts 取会话当前值，Sink 指向本会话。
//
// windowID/threadKey 预计算在会话上 ⇒ 本函数零分配（§4）；Sink 是指针接口，
// 装进接口不产生分配。
func (s *codexSession) identity() codexsdk.Identity {
	return codexsdk.Identity{
		Session: codexsdk.Session{SessionID: s.thread, ThreadID: s.thread, WindowID: s.windowID},
		Meta: codexsdk.CodexMeta{
			InstallationID: s.installation,
			SessionID:      s.thread,
			ThreadID:       s.thread,
			WindowID:       s.windowID,
		},
		TurnState: s.turnState,
		Sink:      s,
	}
}

// step 推进一次完成轮（§3.2 推进）：turns++；跨过 nextAt ⇒ 窗口 +1、windowID 重算、
// nextAt 叠加下一个 span。窗口数达 wmax（>0）⇒ 退休，返回**旧** threadKey（调用方
// 在锁外用它退役旧线程的空闲连接）。
//
// 窗口 span / 退休阈值沿用 SDK 的纯值类型 `IdentityState`/`Step`：那是同一套
// （thread, windowN）确定性派生，网关只是把状态摊在自己的普通字段上，不再有第二个
// 实现可以漂移。
func (s *codexSession) step() string {
	cur := codexsdk.IdentityState{
		InstallationID: s.installation,
		ThreadID:       s.thread,
		Turns:          s.turns,
		WindowN:        s.windowN,
		NextWindowAt:   s.nextAt,
		WMax:           s.wmax,
	}
	next := codexsdk.Step(cur, s.policy)
	s.turns, s.nextAt = next.Turns, next.NextWindowAt
	if next.ThreadID != s.thread {
		// 退休（§3.2）：① 记旧 threadKey ② ts 清空 ③ 新线程 + 计数/阈值/上限重算。
		old := s.threadKey
		s.turnState = ""
		s.thread, s.windowN, s.windowID = next.ThreadID, next.WindowN, next.WindowID()
		s.threadKey, s.wmax = threadKeyOf(s.installation, next.ThreadID), next.WMax
		return old
	}
	if next.WindowN != s.windowN {
		s.windowN, s.windowID = next.WindowN, next.WindowID()
	}
	return ""
}

// release 归还会话（非阻塞 Store——无 channel 锁、无容量死锁）；nil 安全。
func (s *codexSession) release() {
	if s != nil {
		s.busy.Store(false)
	}
}

// threadKeyOf 连接复用键，与 SDK 同构：installation|session|thread。本池的
// session == thread（根线程语义），所以是 installation|thread|thread。
func threadKeyOf(installationID, threadID string) string {
	return installationID + "|" + threadID + "|" + threadID
}

// newCodexSession 开一个新会话：身份状态用 installationID 开新线程（线程 UUIDv7、
// WMax 按 policy 抽样、turns/window 归零），windowID/threadKey 一次算好。
func newCodexSession(installationID string, policy codexsdk.RotatePolicy) *codexSession {
	st := codexsdk.NewIdentityState(installationID, policy)
	return &codexSession{
		installation: installationID,
		thread:       st.ThreadID,
		windowN:      st.WindowN,
		windowID:     st.WindowID(),
		threadKey:    threadKeyOf(installationID, st.ThreadID),
		turns:        st.Turns,
		nextAt:       st.NextWindowAt,
		wmax:         st.WMax,
		policy:       policy,
	}
}

// identityPool 账号级会话池（无锁）：容量 K = 账号 MaxConcurrency；cursor 为认领
// 的旋转起点（round-robin 打散并发）。slots 切片发布后不再增删；逐会话状态经
// 租借独占。轮换策略单一来源在**会话**（policy），池经 rotatePolicy 从会话派生。
//
// 池上**没有**任何凭据失效状态：owner 对齐读的是请求自己的凭据、落在会话字段上
// （§3.2），所以重载换池、pending 池交错都不会丢失效。
type identityPool struct {
	slots          []*codexSession
	cursor         atomic.Uint64
	installationID string
	accountID      int64
	// retireThread 退役某线程的空闲 WS 连接（构造期注入；nil = 未接——测试形态）。
	// 退休与重载丢槽都走它，装配点唯一。
	retireThread func(accountID int64, threadKey string)
	// reserveHook 是 test-only 屏障（生产 nil）：在「已判空闲、尚未写 busy」处留出
	// 确定性交错点，让「busy 用无 CAS 的 Store」这一变异（M7）能被稳定现红。
	reserveHook func()
}

// rotatePolicy 返回本池的轮换策略（单一来源 = 会话：池内各会话构造期写入同一策略）。
func (p *identityPool) rotatePolicy() codexsdk.RotatePolicy {
	if p == nil || len(p.slots) == 0 {
		return codexsdk.RotatePolicy{}
	}
	return p.slots[0].policy
}

// retire 退役某条线程的空闲连接（锁外调用；nil 池/未装配 = no-op）。
func (p *identityPool) retire(threadKey string) {
	if p != nil && p.retireThread != nil && threadKey != "" {
		p.retireThread(p.accountID, threadKey)
	}
}

// identityRegistry 账号 ID → 池 的只读注册表：**发布后不可变**（map 不增删），
// reload 用 copy-modify-store 换入（与 scheduler.go 的 view atomic.Pointer 同款
// 纪律）。请求路径 Load() 读、无每请求锁、无可变 map 竞态。
type identityRegistry struct {
	pools map[int64]*identityPool
}

// claim 认领一个空闲会话（无锁：旋转游标 + 逐槽 CAS）。全忙（理论不可达——gate
// 保证 ≤K 在途）返回 nil，由调用方兜底临时身份（不落池）。
func (p *identityPool) claim() *codexSession {
	if p == nil || len(p.slots) == 0 {
		return nil
	}
	k := uint64(len(p.slots))
	base := p.cursor.Add(1)
	for i := range k {
		s := p.slots[(base+i)%k]
		if p.reserveHook != nil && !s.busy.Load() {
			p.reserveHook()
		}
		if s.busy.CompareAndSwap(false, true) {
			return s
		}
	}
	return nil
}

// poolSpec 是建池的构造期参数（账号级）：容量、installation、轮换策略、线程退役
// 出口。全部来自本次 reload 生效的静态根。
type poolSpec struct {
	accountID      int64
	k              int
	installationID string
	policy         codexsdk.RotatePolicy
	retire         func(accountID int64, threadKey string)
}

// resizeIdentityPool 按 spec.k 重建池：按位次迁移旧会话至 min(old,new)。
//
// 迁移判据是**原子预留**（旧会话 busy CAS false→true）：只有取得所有权的旧会话才
// 被读写——重载不得碰在飞会话的状态（读它就是在和持有者竞争，§3.2）。预留成功的
// 位次迁移会话（沿用线程/窗口计数/ts/owner）；installation 变 ⇒ 该账号全部会话
// 重建（旧线程空闲连接退役、ts 不迁）。缩容丢弃的位次：预留成功（=空闲）⇒ 收集
// 退役旧线程；预留失败（=在飞）⇒ **不做任何交接**，其残留 idle 由 SDK 的 idle TTL
// 时间有界回收（§5.2-5）——正是不引入「交接标志」的原因。旧池 GC。
//
// 退役**只收集**：terminate 可能在 socket 写闸上等，必须由持锁方解锁后执行
// （`poolReclaim.run`，§3.2「锁外 RetireThread」）。
func resizeIdentityPool(old *identityPool, spec poolSpec, reclaim *poolReclaim) *identityPool {
	k := max(spec.k, 1)
	np := &identityPool{
		installationID: spec.installationID,
		accountID:      spec.accountID,
		retireThread:   spec.retire,
		slots:          make([]*codexSession, k),
	}
	for i := range k {
		if old != nil && i < len(old.slots) && old.slots[i].busy.CompareAndSwap(false, true) {
			os := old.slots[i]
			if old.installationID == spec.installationID {
				os.policy = spec.policy
				os.busy.Store(false) // 新池尚未发布：此刻释放不会被别人抢到
				np.slots[i] = os
				continue
			}
			retireThreadKey(spec, reclaim, os.threadKey)
			os.busy.Store(false)
		}
		np.slots[i] = newCodexSession(spec.installationID, spec.policy)
	}
	if old != nil {
		for i := k; i < len(old.slots); i++ {
			os := old.slots[i]
			if os.busy.CompareAndSwap(false, true) {
				retireThreadKey(spec, reclaim, os.threadKey)
				os.busy.Store(false)
			}
		}
	}
	return np
}

// retireThreadKey 收集一条线程的退役动作：锁内只入清单，terminate 由调用方在
// 解锁后执行（`poolReclaim.run`）。未装配（测试形态）⇒ 丢弃。
func retireThreadKey(spec poolSpec, reclaim *poolReclaim, threadKey string) {
	if spec.retire != nil && threadKey != "" && reclaim != nil {
		reclaim.threads = append(reclaim.threads, threadRetire{accountID: spec.accountID, threadKey: threadKey})
	}
}

// syncIdentityPool 取本次 reload 生效的池：容量/安装 ID/策略均未变则**原样复用**
// （保留在途 busy 标志与会话身份，杜绝无关重载重置会话）；任一变化才重建。
func syncIdentityPool(prev *identityPool, spec poolSpec, reclaim *poolReclaim) *identityPool {
	if prev != nil && len(prev.slots) == spec.k && prev.installationID == spec.installationID && prev.rotatePolicy() == spec.policy {
		return prev
	}
	return resizeIdentityPool(prev, spec, reclaim)
}

// maxIdentityPoolSlots 是 codex 会话池容量 K 的防御性上界。K = 账号
// MaxConcurrency，而 reload 在持 publisher.mu 期间按 K **同步**分配 K 个 UUID
// （newCodexSession → NewIdentityState；tools/loadtest 曾配 100000）——无上界即
// 让一次 reload 的锁内分配量随配置线性放大。1024 覆盖任何合理并发会话数
// （账号默认 8，实测运维 ≤ 数十），同时把锁内 UUID 分配压在常数级。上界只在
// 装载期施加于 codex 账号（见 buildSnapshots），非 codex 账号的并发门禁不变；
// 门禁与池读同一份 acc，故二者仍恒同源。
const maxIdentityPoolSlots = 1024

// threadRetire 一条线程退役动作（账号面）。
type threadRetire struct {
	accountID int64
	threadKey string
}

// poolReclaim 是重载路径的**锁外副作用批次**：线程退役、账号移除与配置面 client 回收
// 都只在锁内收集，由持锁方的调用点在**解锁之后**执行（§3.2「锁外 RetireThread /
// 锁外 Close」）。
//
// 为什么是批次而不是原地调用：terminate 逐条 client 调用 SDK 的 clear()，可能在
// socket 写闸上等待——放在 publisher.mu 里等于让一次 reload 被若干连接收尾拖住
// （N1）。锁内只做内存摘表，锁外的部分按收集顺序串行执行，不需要额外 goroutine。
type poolReclaim struct {
	threads  []threadRetire
	accounts []int64
	// egress 本批次的账号全集 → 当前 responses 出口（nil 值 = 直连）；只由
	// collectActiveEgress 填充，reconcile 为 nil 时保持 nil。
	egress map[int64]*egress.Client
	// gen 本批次的配置代号（publisher.mu 内取号）。应用在锁外，ClientTable 只
	// 接受更新的号——旧批次不能把活跃集回退到旧配置（B1）。
	gen       uint64
	retire    func(accountID int64, threadKey string)
	remove    func(accountID int64)
	reconcile func(gen uint64, active map[int64]*egress.Client)
}

// run 执行收集到的副作用（必须在 publisher.mu 之外调用；nil 安全）。
func (b *poolReclaim) run() {
	if b == nil {
		return
	}
	if b.retire != nil {
		for _, t := range b.threads {
			b.retire(t.accountID, t.threadKey)
		}
	}
	if b.remove != nil {
		for _, id := range b.accounts {
			b.remove(id)
		}
	}
	// 配置面回收放最后：它按「账号当前配置」重设活跃集并摘除历史出口摘要，同时把不在
	// 账号集合里的资源整账号摘除（与上面的 remove 幂等，多走一遍只是 no-op）。
	// gen 由锁内取号，ClientTable 只应用更新的批次（旧批次的 socket 收尾已经在上面
	// 完成，且幂等；只有"设状态"的活跃集应用需要顺序）。
	if b.reconcile != nil && b.egress != nil {
		b.reconcile(b.gen, b.egress)
	}
}

// collectRemovedAccounts 收集「旧注册表有池、新注册表没有」的账号（§3.2 账号移除的
// 判定点：池存在而账号/id 不在新表就是移除信号——工作 byID 恒为全量克隆，不存在
// 部分编译导致的误判）。调用方持 publisher.mu；执行在锁外（poolReclaim.run）。
func collectRemovedAccounts(prev, next *identityRegistry, reclaim *poolReclaim) {
	if prev == nil || reclaim == nil || reclaim.remove == nil {
		return
	}
	for id := range prev.pools {
		if next != nil {
			if _, ok := next.pools[id]; ok {
				continue
			}
		}
		reclaim.accounts = append(reclaim.accounts, id)
	}
}

// collectActiveEgress 收集「账号全集 → 当前 responses 出口」交给 ClientTable 做配置面
// 回收（§3.2 出口/transport 变更）：活跃集 = 该账号当前配置的出口 + usage 的直连，且
// **只按 egress 维度**剪枝。
//
// 输入必须是**账号全集**：byID 是组→账号折叠出的账号（含 `MaxConcurrency=0`、没有
// 会话池的账号——buildIdentityPools 只给 codex 且 K>0 的账号建池，池注册表差分看不到
// 其余账号，momus N7）；extra 是本轮**未编组**账号的出口（全量 reload 现算，组级增量
// 路径沿用上次全量装载的基线——组级路径不重扫账号表）。账号从全集里消失 ⇒ 该账号的
// 传输资源整账号摘除。同一 id 以 byID 为准（工作根是最新事实）。
//
// 批次序号在**锁内**取（publisher.mu 由调用方持有）：应用在锁外、顺序不保证，序号是
// "旧批次不得覆盖新配置"的唯一判据（B1）。terminate 仍由 reclaim.run 在锁外执行。
func (s *Scheduler) collectActiveEgress(byID map[int64]*accountSnapshot, extra map[int64]*egress.Client, reclaim *poolReclaim) {
	if reclaim == nil || reclaim.reconcile == nil {
		return
	}
	active := make(map[int64]*egress.Client, len(byID)+len(extra))
	for id, eg := range extra {
		active[id] = eg
	}
	for id, as := range byID {
		av := as.static.Load()
		if av == nil {
			continue
		}
		active[id] = av.egress
	}
	reclaim.egress = active
	reclaim.gen = s.publisher.reconcileGen + 1
	s.publisher.reconcileGen = reclaim.gen
}

// ungroupedEgress 全量装载里**不在任何组**的账号 → 当前 responses 出口（未装配出口
// 池的测试形态 ⇒ nil 值 = 直连）。它们不进路由根（无组即无 route），但仍是账号全集的
// 一部分：不给它们登记活跃集，每次 reload 都会把管理面 usage 刚建的条目/桶摘掉
// （momus N-e）。未编组账号的删除只能由下一次全量装载发现——残留有界（无路由，只有
// 管理面 usage 触达）。
func (s *Scheduler) ungroupedEgress(byID map[int64]*accountSnapshot, all []*domain.Account) map[int64]*egress.Client {
	if len(all) == 0 {
		return nil
	}
	var out map[int64]*egress.Client
	pool := s.cfg.Egress
	for _, acc := range all {
		if acc == nil {
			continue
		}
		if _, grouped := byID[acc.ID]; grouped {
			continue
		}
		if out == nil {
			out = make(map[int64]*egress.Client, len(all)-len(byID))
		}
		var eg *egress.Client
		if pool != nil {
			eg = pool.Direct()
			if acc.Proxy != nil {
				eg = pool.For(specOfDomain(acc.Proxy))
			}
		}
		out[acc.ID] = eg
	}
	return out
}

// buildIdentityPools 按当前 byID 重建 codex 会话池注册表：仅 codex 凭据
// （oauth/pat）且 MaxConcurrency>0 建池；与静态叶**同发布点**（挂
// StaticView.identityPools），使门禁读的 MaxConcurrency 与池 K 恒同源。prevReg
// 供复用（未变的池原样保留在途 busy 与会话身份）；删除/非 codex 的账号不在新
// 表中 → 旧池随之 GC。K 的上界已在 buildSnapshots 装载期施加于 acc.MaxConcurrency。
//
// 退役与移除只写进 reclaim（锁外执行）；reclaim 为 nil 时（单元用例直接建池）
// 退役不执行——调用方自己决定要不要真退役。账号移除差分不在这里做：组级批路径
// 逐步重建池，只有**整批结束后**才知道哪个账号真的从集合里消失了。
func (s *Scheduler) buildIdentityPools(byID map[int64]*accountSnapshot, prevReg *identityRegistry, reclaim *poolReclaim) *identityRegistry {
	pools := make(map[int64]*identityPool)
	for id, as := range byID {
		av := as.static.Load()
		if av == nil || av.tpl == nil || av.acc.Ext == nil {
			continue
		}
		if !av.tpl.CredentialType.IsCodex() {
			continue
		}
		k := av.acc.MaxConcurrency
		if k <= 0 {
			continue
		}
		var prev *identityPool
		if prevReg != nil {
			prev = prevReg.pools[id]
		}
		pools[id] = syncIdentityPool(prev, poolSpec{
			accountID:      id,
			k:              k,
			installationID: installationIDOf(av.acc.Ext),
			policy:         s.cfg.RotatePolicy,
			retire:         s.cfg.RetireThread,
		}, reclaim)
	}
	reg := &identityRegistry{pools: pools}
	return reg
}

// prevIdentityPoolsLocked 取当前「最新」静态根的池注册表作为复用源：优先
// staged（pending）根，否则已发布根——与 reload 取 oldByID 同纪律（持
// publisher.mu 读取安全）。
func (s *Scheduler) prevIdentityPoolsLocked() *identityRegistry {
	if p := s.publisher.pending; p != nil {
		return p.identityPools
	}
	if cur := s.view.Load(); cur != nil && cur.static != nil {
		return cur.static.identityPools
	}
	return nil
}

// claimCodexSession 为一次**已成功预留**的 codex 调用认领会话：仅 codex 凭据
// （oauth/pat）认领；非 codex / 无池 → nil（不认领、不注入会话身份）。pools 取自
// 本次预留所用视图的静态根（与门禁 limit 同源）；全忙兜底 = 临时一次性身份
// （不落池、无代际/退役面，计一条 warn）——installation/policy 取自池。
func (s *Scheduler) claimCodexSession(pools *identityRegistry, accountID int64, credType credential.Type) (*codexSession, *identityPool) {
	if !credType.IsCodex() || pools == nil {
		return nil, nil
	}
	pool := pools.pools[accountID]
	if pool == nil {
		return nil, nil
	}
	if session := pool.claim(); session != nil {
		return session, pool
	}
	if s.log != nil {
		s.log.Warn("codex session pool exhausted; using ephemeral identity", logx.Int64("account_id", accountID))
	}
	return newCodexSession(pool.installationID, pool.rotatePolicy()), nil
}

// installationIDOf 取账号 ext 的安装 ID（账号级稳定项；缺列 → 空串）。
func installationIDOf(ext *domain.AccountExt) string {
	if ext == nil || ext.CodexIdentity == nil {
		return ""
	}
	return ext.CodexIdentity.InstallationID
}
