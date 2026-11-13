// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
//
// codex 会话池（账号级 K 会话）：一个会话 = 一条上游线程的完整生命周期——
// installation / thread / window 计数 / 连接身份。身份不再是客户端的
// 构造期配置，而是**请求数据**：会话池摊开成 `Identity` 交给一次调用（spec S4 §3.1）。
//
// 并发模型沿用原槽位池：认领 = 旋转游标 + 逐槽 CAS，归还 = Store(false)——无
// channel、无 mutex、请求路径零每请求锁。会话状态字段是**普通字段**：busy 独占
// 保证租借期内只有一个读者/写者，所以不再套 atomic（§3.1）。
//
// 因此**租借之外可读的字段必须不可变**：轮换策略属于池的构造参数，`policy` 只
// 是建会话时抄下的一份只读副本；resize 不写已发布的会话（容量变化只迁移会话
// 指针，策略变化开新会话并回收旧线程）。兜底临时身份也只读池字段，不读 slot。
package scheduler

import (
	"sync/atomic"

	"github.com/is7qin/c3api/pkg/egress"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// codexSession 一个会话：busy 独占标志 + 线程生命周期状态（§3.1 字段表）。
//
// 租借（busy.CAS false→true）成功的一方独占全部状态字段直到 release：读写都在
// 租借期内，没有第二读者。线程状态直接持有 SDK 的纯值类型 `IdentityState`（含
// `ContextWindowID`），windowID/threadKey 是它的派生值，在开线程与 step 时一次算好，
// 组装身份时只做字段拷贝（零分配）。
//
// 槽位路径不重放、不捕获 turn-state（Sink 恒 nil、Turn.State 恒空），所以会话上
// 没有 ts 字段，也不需要随凭据 owner 对齐。
type codexSession struct {
	busy atomic.Bool
	st   codexsdk.IdentityState
	// windowID = st.WindowID()、threadKey = threadKeyOf(st)：预计算，避免每请求拼接。
	windowID  string
	threadKey string
	// policy 建会话时抄下的轮换策略（只读：与池的构造参数恒同值）。step 用它推进
	// 窗口/退休；resize 永不改写它——策略真的变了就换新会话。
	policy codexsdk.RotatePolicy
}

// identity 本次请求的身份数据（§3.2 租借）：session == thread（根线程语义），
// 窗口号与 context_window_id 取会话当前值；first 由调用方按请求体判定。
//
// 直接组装 Identity 而不调 `IdentityState.Identity`——后者每次重算 WindowID 会分配。
// 全部是字段拷贝 ⇒ 零分配；Sink=nil、Turn.State=""（槽位路径不带 turn-state）。
func (s *codexSession) identity(first bool) codexsdk.Identity {
	return codexsdk.Identity{
		InstallationID: s.st.InstallationID,
		Session:        codexsdk.Session{SessionID: s.st.ThreadID, ThreadID: s.st.ThreadID, WindowID: s.windowID},
		Window:         codexsdk.Window{Number: s.st.WindowN, ContextID: s.st.ContextWindowID},
		Turn:           codexsdk.Turn{First: first},
	}
}

// step 推进一次完成轮（§3.2 推进）：委托 SDK 的 `Step`（窗口 span / 退休阈值是同一套
// 确定性派生，不存在第二个实现可以漂移）。窗口推进 ⇒ windowID 重算（ContextWindowID
// 由 Step 一并换新）；退休 ⇒ 返回**旧** threadKey（调用方在锁外用它退役旧线程的空闲
// 连接），并重算 windowID/threadKey。
func (s *codexSession) step() string {
	prev := s.st
	s.st = codexsdk.Step(prev, s.policy)
	if s.st.ThreadID != prev.ThreadID {
		old := s.threadKey
		s.windowID, s.threadKey = s.st.WindowID(), threadKeyOf(s.st.InstallationID, s.st.ThreadID)
		return old
	}
	if s.st.WindowN != prev.WindowN {
		s.windowID = s.st.WindowID()
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
		st:        st,
		windowID:  st.WindowID(),
		threadKey: threadKeyOf(installationID, st.ThreadID),
		policy:    policy,
	}
}

// identityPool 账号级会话池（无锁）：容量 K = 账号 MaxConcurrency；cursor 为认领
// 的旋转起点（round-robin 打散并发）。slots 切片发布后不再增删；逐会话状态经
// 租借独占。轮换策略是**池的不可变构造参数**（policy），会话只持有建会话时的
// 只读副本——这样"策略"没有第二个可写副本，resize 也不需要同步它。
type identityPool struct {
	slots          []*codexSession
	cursor         atomic.Uint64
	installationID string
	accountID      int64
	// policy 本池的轮换策略（构造后不变）。所有会话按它开线程、按它推进；resize
	// 比较它与新配置决定"迁移会话指针"还是"开新会话"。
	policy codexsdk.RotatePolicy
	// retireThread 退役某线程的空闲 WS 连接（构造期注入；nil = 未接——测试形态）。
	// 退休与重载丢槽都走它，装配点唯一。
	retireThread func(accountID int64, threadKey string)
	// reserveHook 是 test-only 屏障（生产 nil）：在「已判空闲、尚未写 busy」处留出
	// 确定性交错点，让「busy 用无 CAS 的 Store」这一变异（M7）能被稳定现红。
	reserveHook func()
}

// rotatePolicy 返回本池的轮换策略（**不可变构造参数**：读它不碰任何会话字段，
// 因此兜底路径/配置比较都可以在租借之外安全读）。
func (p *identityPool) rotatePolicy() codexsdk.RotatePolicy {
	if p == nil {
		return codexsdk.RotatePolicy{}
	}
	return p.policy
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
// 位次迁移会话（沿用线程/窗口计数）；installation 或轮换策略变 ⇒ 该账号
// 全部会话重建（旧线程空闲连接退役）。缩容丢弃的位次：预留成功（=空闲）
// ⇒ 收集退役旧线程；预留失败（=在飞）⇒ **不做任何交接**，其残留 idle 由 SDK 的
// idle TTL 时间有界回收（§5.2-5）。旧池 GC。
//
// 这里**只迁移会话指针**，绝不改写已发布会话的字段（尤其 policy：它是池的不可变
// 构造参数，会话上的副本也只在构造时写入）。策略真的变了就换新会话——持旧视图
// 的请求仍在旧池/旧会话上，它们读到的策略永远与建会话时一致。
//
// 退役**只收集**：terminate 可能在 socket 写闸上等，必须由持锁方解锁后执行
// （`poolReclaim.run`，§3.2「锁外 RetireThread」）。
func resizeIdentityPool(old *identityPool, spec poolSpec, reclaim *poolReclaim) *identityPool {
	k := max(spec.k, 1)
	np := &identityPool{
		installationID: spec.installationID,
		accountID:      spec.accountID,
		policy:         spec.policy,
		retireThread:   spec.retire,
		slots:          make([]*codexSession, k),
	}
	reusable := old != nil && old.installationID == spec.installationID && old.rotatePolicy() == spec.policy
	for i := range k {
		if old != nil && i < len(old.slots) {
			os := old.slots[i]
			if os.busy.CompareAndSwap(false, true) {
				if reusable {
					// 同一 installation + 同一策略：会话字段与池参数仍然自洽，迁移指针。
					//
					// 这里的 Store(false) **不是**"没人抢得到"：旧池仍是已发布池，持旧视图
					// 的请求随时可能认领同一会话指针。安全来自 `busy` 是**会话对象上的共享
					// CAS**——旧视图认领后把 busy 置回 true，本侧（或新池侧）下一次 CAS 必败，
					// 同一会话永远只会被一个请求独占。换句话说属性由 CAS 保证，不靠发布时机。
					os.busy.Store(false)
					np.slots[i] = os
					continue
				}
				// installation/策略变了：旧会话的线程不再属于新配置，退役后开新会话。
				retireThreadKey(spec, reclaim, os.threadKey)
				os.busy.Store(false)
			}
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

// poolReclaim 是重载路径的**锁外副作用批次**：线程退役与配置面 client 回收都只在
// 锁内收集，由持锁方的调用点在**解锁之后**执行（§3.2「锁外 RetireThread / 锁外
// Close」）。
//
// 为什么是批次而不是原地调用：terminate 逐条 client 调用 SDK 的 clear()，可能在
// socket 写闸上等待——放在 publisher.mu 里等于让一次 reload 被若干连接收尾拖住。
// 锁内只做内存摘表，锁外的部分按收集顺序串行执行，不需要额外 goroutine。
type poolReclaim struct {
	threads []threadRetire
	// config 本批次的账号配置快照（账号全集 → 当前出口 + 资源指令 + 批次代号）。
	// nil = 本批次不携带配置（单元用例直接建池）。应用在锁外、与线程退役同一个批次。
	config *accountConfig
	// egress 本批次被摘出注册表的出口 client（egress.Pool.Detach 的返回）：与配置
	// 指令共享"锁内只收集、锁外才动系统调用"的纪律——关闭空闲连接可能在连接收尾上
	// 等待，不能留在 publisher.mu 里。
	egress []*egress.Client
	retire func(accountID int64, threadKey string)
	apply  func(batch domain.ResourceBatch)
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
	// 出口 client 与线程退役同一批、同一纪律：锁内只摘，锁外关闭。
	egress.CloseClients(b.egress)
	// 配置面回收放最后：账号删除与出口可见性必须在**同一个** gen 准入之后应用——
	// 旧批次删掉的可能是新配置刚建出来的条目。
	if b.config != nil && b.apply != nil {
		b.apply(b.config.batch)
	}
}

// collectRetiredPools 池注册表差分的**唯一**语义：池消失 ⇒ 退役它名下会话的线程。
//
// 池消失 ≠ 账号移除（§3.3b-4）：K 变 0、凭据类型变更、账号从路由集合退出都会让池
// 消失，但账号资源（Auth/usage/client 桶）的生命周期由**账号全集差分**决定
// （accountConfig 的资源指令 → `ClientTable.ApplyBatch`）。这里只回收会话/线程，
// 不冒充账号移除。
//
// 这里只**收集**，执行在锁外（`poolReclaim.run`）。
func collectRetiredPools(prev, next *identityRegistry, reclaim *poolReclaim) {
	if prev == nil || reclaim == nil || reclaim.retire == nil {
		return
	}
	for id, pool := range prev.pools {
		if next != nil {
			if _, ok := next.pools[id]; ok {
				continue
			}
		}
		pool.collectThreads(reclaim)
	}
}

// collectThreads 收集本池**空闲**会话的线程退役动作（调用方持 publisher.mu）。
// 在飞会话不动：它的连接由该请求自己收尾，残留 idle 由 SDK 的 idle TTL 时间有界
// 回收（与缩容丢槽同一条边界）。
func (p *identityPool) collectThreads(reclaim *poolReclaim) {
	if p == nil || reclaim == nil {
		return
	}
	for _, s := range p.slots {
		if s.busy.CompareAndSwap(false, true) {
			if s.threadKey != "" {
				reclaim.threads = append(reclaim.threads, threadRetire{accountID: p.accountID, threadKey: s.threadKey})
			}
			s.busy.Store(false)
		}
	}
}

// buildIdentityPools 按当前 byID 重建 codex 会话池注册表：仅 codex 凭据
// （oauth/pat）且 MaxConcurrency>0 建池；与静态叶**同发布点**（挂
// StaticView.identityPools），使门禁读的 MaxConcurrency 与池 K 恒同源。prevReg
// 供复用（未变的池原样保留在途 busy 与会话身份）；删除/非 codex 的账号不在新
// 表中 → 旧池随之 GC。K 的上界已在 buildSnapshots 装载期施加于 acc.MaxConcurrency。
//
// 退役只写进 reclaim（锁外执行）；reclaim 为 nil 时（单元用例直接建池）退役不
// 执行——调用方自己决定要不要真退役。**账号移除不在这里判定**：池注册表只回答
// "这个账号此刻有没有会话池"，账号资源回收只由账号全集差分（accountConfig）驱动。
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
// （不落池、无代际/退役面，计一条 warn）——installation/policy 取自池的**不可变
// 字段**，不在租借之外读任何会话状态。
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
