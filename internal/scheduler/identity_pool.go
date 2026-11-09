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
// 位次迁移会话（沿用线程/窗口计数/ts）；installation 变 ⇒ 该账号全部会话重建
// （旧线程空闲连接立即退役、ts 不迁）。缩容丢弃的位次：预留成功（=空闲）⇒ 立即
// 退役旧线程；预留失败（=在飞）⇒ **不做任何交接**，其残留 idle 由 SDK 的 idle TTL
// 时间有界回收（§5.2-5）——正是不引入「交接标志」的原因。旧池 GC。
func resizeIdentityPool(old *identityPool, spec poolSpec) *identityPool {
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
			retireThreadKey(spec, os.threadKey)
			os.busy.Store(false)
		}
		np.slots[i] = newCodexSession(spec.installationID, spec.policy)
	}
	if old != nil {
		for i := k; i < len(old.slots); i++ {
			os := old.slots[i]
			if os.busy.CompareAndSwap(false, true) {
				retireThreadKey(spec, os.threadKey)
				os.busy.Store(false)
			}
		}
	}
	return np
}

// retireThreadKey 退役一条线程的空闲连接（账号面出口）；未装配 = no-op。
func retireThreadKey(spec poolSpec, threadKey string) {
	if spec.retire != nil && threadKey != "" {
		spec.retire(spec.accountID, threadKey)
	}
}

// syncIdentityPool 取本次 reload 生效的池：容量/安装 ID/策略均未变则**原样复用**
// （保留在途 busy 标志与会话身份，杜绝无关重载重置会话）；任一变化才重建。
func syncIdentityPool(prev *identityPool, spec poolSpec) *identityPool {
	if prev != nil && len(prev.slots) == spec.k && prev.installationID == spec.installationID && prev.rotatePolicy() == spec.policy {
		return prev
	}
	return resizeIdentityPool(prev, spec)
}

// maxIdentityPoolSlots 是 codex 会话池容量 K 的防御性上界。K = 账号
// MaxConcurrency，而 reload 在持 publisher.mu 期间按 K **同步**分配 K 个 UUID
// （newCodexSession → NewIdentityState；tools/loadtest 曾配 100000）——无上界即
// 让一次 reload 的锁内分配量随配置线性放大。1024 覆盖任何合理并发会话数
// （账号默认 8，实测运维 ≤ 数十），同时把锁内 UUID 分配压在常数级。上界只在
// 装载期施加于 codex 账号（见 buildSnapshots），非 codex 账号的并发门禁不变；
// 门禁与池读同一份 acc，故二者仍恒同源。
const maxIdentityPoolSlots = 1024

// buildIdentityPools 按当前 byID 重建 codex 会话池注册表：仅 codex 凭据
// （oauth/pat）且 MaxConcurrency>0 建池；与静态叶**同发布点**（挂
// StaticView.identityPools），使门禁读的 MaxConcurrency 与池 K 恒同源。prevReg
// 供复用（未变的池原样保留在途 busy 与会话身份）；删除/非 codex 的账号不在新
// 表中 → 旧池随之 GC。K 的上界已在 buildSnapshots 装载期施加于 acc.MaxConcurrency。
func (s *Scheduler) buildIdentityPools(byID map[int64]*accountSnapshot, prevReg *identityRegistry) *identityRegistry {
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
		})
	}
	return &identityRegistry{pools: pools}
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
