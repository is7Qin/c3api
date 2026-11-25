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
//
// 本包与调度器之间只有三处缝：`Spec`（构造参数）、`Pool.Claim`（领取）、
// `Reclaimer`（锁内只收集退役）。池**不知道**账号配置、出口 client、资源批次，
// 那些仍属调度器的 `poolReclaim`；池只知道"某账号某线程该退役了"。
//
// 导出面纪律（P5b 修正 4）：**不导出任何可写状态**。`Session` 的所有字段不导出，
// 对外只有生命周期方法（Step/Release）与只读派生值（Key/Thread/Policy/Busy）；
// 没有任何 `SetBusy`/`Slots()` 之类的写入口——独占协议只能经 Claim/Release，
// 调用方无法在 API 层绕过它。
package identitypool

import (
	"sync/atomic"

	"github.com/is7qin/c3api/internal/sdkbridge"
)

// Session 一个会话：busy 独占标志 + 线程生命周期状态（§3.1 字段表）。
//
// 租借（busy.CAS false→true）成功的一方独占全部状态字段直到 release：读写都在
// 租借期内，没有第二读者。线程状态直接持有 SDK 的纯值类型 `IdentityState`（含
// `ContextWindowID`），windowID/threadKey 是它的派生值，在开线程与 step 时一次算好，
// 组装身份时只做字段拷贝（零分配）。
//
// 槽位路径不重放、不捕获 turn-state（Sink 恒 nil、Turn.State 恒空），所以会话上
// 没有 ts 字段，也不需要随凭据 owner 对齐。
type Session struct {
	busy atomic.Bool
	// h 线程句柄（**不透明**）：协议状态由 sdkbridge.Threads 持有，本包只搬运。
	h sdkbridge.ThreadHandle
	// threadKey 连接复用键（不透明字符串）：退休时交给 retire，不解析、不拼装。
	threadKey string
	// threads 建会话时绑定的工厂（无状态，仅承载策略）。step 走它的唯一变更入口；
	// resize 永不改写它——策略真的变了就换新会话。
	threads *sdkbridge.Threads
}

// Step 推进一次完成轮（§3.2 推进）：委托 SDK 的 `Step`（窗口 span / 退休阈值是同一套
// 确定性派生，不存在第二个实现可以漂移）。窗口推进 ⇒ windowID 重算（ContextWindowID
// 由 Step 一并换新）；退休 ⇒ 返回**旧** threadKey（调用方在锁外用它退役旧线程的空闲
// 连接），并重算 windowID/threadKey。
func (s *Session) Step() string {
	retired := s.threads.Step(s.h) // 唯一状态转移入口（窗口 span / 退休阈值都在 bridge 内）
	if retired != "" {
		s.threadKey = s.threads.Key(s.h)
	}
	return retired
}

// Release 归还会话（非阻塞 Store——无 channel 锁、无容量死锁）；nil 安全。
func (s *Session) Release() {
	if s != nil {
		s.busy.Store(false)
	}
}

// Key 池缓存的连接复用键（= 建会话/退休时由句柄派生并抄下的那一份）。与
// `threads.Key(h)` 恒等，但不穿句柄读——出借期外解引用别名是并发读协议状态。
func (s *Session) Key() string { return s.threadKey }

// Thread 本会话的不透明线程句柄（值语义、可拷贝；零值 = 匿名身份）。只搬运：
// 任何状态转移都必须经 Step，否则会与持有者并发写同一份 threadState。
func (s *Session) Thread() sdkbridge.ThreadHandle { return s.h }

// Policy 建会话时抄下的轮换策略**只读副本**（策略变化 = 换新会话，所以它恒等于
// 建会话那一刻池的策略）。抄的是本包归一后的部署策略（见 Policy），不是工厂的
// SDK 策略——本包的导出面不出现 SDK 类型。
func (s *Session) Policy() Policy { return policyOf(s.threads.Policy()) }

// Busy 会话此刻是否被租借（**只读**：独占协议唯一入口是 Claim/Release，没有 setter）。
// 观测面用：迁移/收集断言"空闲才动、在飞不碰"。
func (s *Session) Busy() bool { return s.busy.Load() }

// NewSession 开一个新会话：身份状态用 installationID 开新线程（线程 UUIDv7、
// WMax 按 policy 抽样、turns/window 归零），windowID/threadKey 一次算好。
//
// 调用点白名单：建池（`buildIdentityPools` 路径上的 Resize/Sync）与全忙兜底的
// 临时身份（`claimCodexSession`）——请求热路径不得在此之外新开会话（每会话 7 allocs）。
func NewSession(installationID string, policy Policy) *Session {
	th := sdkbridge.NewThreads(policy.sdk())
	h := th.Open(installationID)
	return &Session{h: h, threadKey: th.Key(h), threads: th}
}

// Pool 账号级会话池（无锁）：容量 K = 账号 MaxConcurrency；cursor 为认领
// 的旋转起点（round-robin 打散并发）。slots 切片发布后不再增删；逐会话状态经
// 租借独占。轮换策略是**池的不可变构造参数**（policy），会话只持有建会话时的
// 只读副本——这样"策略"没有第二个可写副本，resize 也不需要同步它。
type Pool struct {
	slots          []*Session
	cursor         atomic.Uint64
	installationID string
	accountID      int64
	// policy 本池的轮换策略（构造后不变）。所有会话按它开线程、按它推进；resize
	// 比较它与新配置决定"迁移会话指针"还是"开新会话"。存的是本包的 Policy，与
	// 会话上的只读副本**同一个类型**（比较仍是数值比较）；SDK 形态只在 NewSession
	// 的构造边界上出现一次（Policy.sdk）。
	policy Policy
	// retireThread 退役某线程的空闲 WS 连接（构造期注入；nil = 未接——测试形态）。
	// 退休与重载丢槽都走它，装配点唯一。
	retireThread func(accountID int64, threadKey string)
	// hooks 是 **test-only** 的确定性交错点与只读旁表（生产恒 nil）。
	//
	// 为什么是一个指针而不是五个平铺字段：`Pool` 的布局（88B，字段偏移见
	// testdata/layout.golden）是热路径的伪共享面，测试面不该改它——指针恰好占住
	// 原 `reserveHook` 那 8 字节，于是"零行为改动"连布局都保住了。生产路径每个注入
	// 点只剩一次 nil 判断（claim 里短路在原子读之前），claim+release 仍 0 alloc。
	hooks *hooks
}

// hooks 五个确定性注入点（位置写在各自字段注释里；生产 nil）。
type hooks struct {
	// reserve claim 内「已判空闲、尚未写 busy」处（既有 M7 屏障）。
	reserve func()
	// migrate resize 的迁移分支「CAS 成功后、Store(false) 之前」处：把 resizer 停在
	// 持有态，让旧视图请求同时 claim 同一会话，断言「仍只有一个持有者＋
	// np.slots[i]==os」——这是那段"安全性来自共享 CAS，不靠发布时机"论证唯一
	// 可执行的形式。
	migrate func(i int)
	// shrink 缩容丢槽循环「CAS 成功后、收集退役前」处：证明收集期该会话已独占、
	// 且 retire 尚未发生（只收集不执行）。
	shrink func(i int)
	// collect collectThreads「CAS 成功后、Store(false) 之前」处：证明收集期并发
	// claim 必失败（半成品不对外可见）。
	collect func(i int)
	// registry collectRetiredPools 的循环体（phase = "enter"/"exit"）：让读者在
	// 收集进行中并发 Lookup，把"发布后不可变"变成可执行判据。
	registry func(phase string)
}

// RotatePolicy 返回本池的轮换策略（**不可变构造参数**：读它不碰任何会话字段，
// 因此兜底路径/配置比较都可以在租借之外安全读）。
func (p *Pool) RotatePolicy() Policy {
	if p == nil {
		return Policy{}
	}
	return p.policy
}

// InstallationID 本池的账号级安装 ID（不可变构造参数）。兜底临时身份按它开线程。
func (p *Pool) InstallationID() string {
	if p == nil {
		return ""
	}
	return p.installationID
}

// Len 池容量 K（发布后不变；只读）。
func (p *Pool) Len() int {
	if p == nil {
		return 0
	}
	return len(p.slots)
}

// Slot 第 i 个槽位的会话（**只读**：切片本身不导出，调用方拿不到写入口；
// 槽位指针发布后不变，只有会话内部的 busy 会变）。
func (p *Pool) Slot(i int) *Session { return p.slots[i] }

// Retire 退役某条线程的空闲连接（锁外调用；nil 池/未装配 = no-op）。
func (p *Pool) Retire(threadKey string) {
	if p != nil && p.retireThread != nil && threadKey != "" {
		p.retireThread(p.accountID, threadKey)
	}
}

// Registry 账号 ID → 池 的只读注册表：**发布后不可变**（map 不增删），
// reload 用 copy-modify-store 换入（与 scheduler.go 的 view atomic.Pointer 同款
// 纪律）。请求路径 Load() 读、无每请求锁、无可变 map 竞态。
type Registry struct {
	Pools map[int64]*Pool
}

// Lookup 取账号的池（nil 注册表/账号无池 → nil）。发布后不可变 ⇒ 无锁读。
func (r *Registry) Lookup(accountID int64) *Pool {
	if r == nil {
		return nil
	}
	return r.Pools[accountID]
}

// Claim 认领一个空闲会话（无锁：旋转游标 + 逐槽 CAS）。全忙（理论不可达——gate
// 保证 ≤K 在途）返回 nil，由调用方兜底临时身份（不落池）。
func (p *Pool) Claim() *Session {
	if p == nil || len(p.slots) == 0 {
		return nil
	}
	h := p.hooks
	k := uint64(len(p.slots))
	base := p.cursor.Add(1)
	for i := range k {
		s := p.slots[(base+i)%k]
		if h != nil && h.reserve != nil && !s.busy.Load() {
			h.reserve()
		}
		if s.busy.CompareAndSwap(false, true) {
			return s
		}
	}
	return nil
}

// Spec 是建池的构造期参数（账号级）：容量、installation、轮换策略、线程退役
// 出口。全部来自本次 reload 生效的静态根。
type Spec struct {
	AccountID      int64
	K              int
	InstallationID string
	Policy         Policy
	Retire         func(accountID int64, threadKey string)
}

// Reclaimer 是"锁内只收集、锁外才执行"的退役汇：池侧只调用 RetireThread 收集，
// 真正 terminate 由调用方在解锁后统一执行（可能阻塞在 socket 写闸上）。
//
// 这是池与调度器之间**唯一**的回调接口：它不承担 Run、不承担配置/出口回收——
// 那些仍属调度器的 `poolReclaim`。调度器把它实现成"追加到本批次的线程清单"。
type Reclaimer interface {
	RetireThread(accountID int64, threadKey string)
}

// Resize 按 spec.K 重建池：按位次迁移旧会话至 min(old,new)。
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
func Resize(old *Pool, spec Spec, r Reclaimer) *Pool {
	k := max(spec.K, 1)
	np := &Pool{
		installationID: spec.InstallationID,
		accountID:      spec.AccountID,
		policy:         spec.Policy,
		retireThread:   spec.Retire,
		slots:          make([]*Session, k),
	}
	var h *hooks // 迁移/丢槽的注入点取自**旧池**（resize 由它驱动）
	if old != nil {
		h = old.hooks
	}
	reusable := old != nil && old.installationID == spec.InstallationID && old.RotatePolicy() == spec.Policy
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
					if h != nil && h.migrate != nil {
						h.migrate(i)
					}
					os.busy.Store(false)
					np.slots[i] = os
					continue
				}
				// installation/策略变了：旧会话的线程不再属于新配置，退役后开新会话。
				retireThreadKey(spec, r, os.threadKey)
				os.busy.Store(false)
			}
		}
		np.slots[i] = NewSession(spec.InstallationID, spec.Policy)
	}
	if old != nil {
		for i := k; i < len(old.slots); i++ {
			os := old.slots[i]
			if os.busy.CompareAndSwap(false, true) {
				if h != nil && h.shrink != nil {
					h.shrink(i)
				}
				retireThreadKey(spec, r, os.threadKey)
				os.busy.Store(false)
			}
		}
	}
	return np
}

// retireThreadKey 收集一条线程的退役动作：锁内只入汇，terminate 由调用方在
// 解锁后执行（`poolReclaim.run`）。未装配（测试形态）⇒ 丢弃。
//
// 门槛与改前逐字一致：`spec.Retire != nil`（本池有没有退役出口）× threadKey 非空
// × 汇非 nil。前一条是**池**的事实，后一条是**批次**的事实，二者不可合并。
func retireThreadKey(spec Spec, r Reclaimer, threadKey string) {
	if spec.Retire != nil && threadKey != "" && r != nil {
		r.RetireThread(spec.AccountID, threadKey)
	}
}

// Sync 取本次 reload 生效的池：容量/安装 ID/策略均未变则**原样复用**
// （保留在途 busy 标志与会话身份，杜绝无关重载重置会话）；任一变化才重建。
func Sync(prev *Pool, spec Spec, r Reclaimer) *Pool {
	if prev != nil && len(prev.slots) == spec.K && prev.installationID == spec.InstallationID && prev.RotatePolicy() == spec.Policy {
		return prev
	}
	return Resize(prev, spec, r)
}

// MaxSlots 是 codex 会话池容量 K 的防御性上界。K = 账号
// MaxConcurrency，而 reload 在持 publisher.mu 期间按 K **同步**分配 K 个 UUID
// （NewSession → NewIdentityState；tools/loadtest 曾配 100000）——无上界即
// 让一次 reload 的锁内分配量随配置线性放大。1024 覆盖任何合理并发会话数
// （账号默认 8，实测运维 ≤ 数十），同时把锁内 UUID 分配压在常数级。上界只在
// 装载期施加于 codex 账号（见 buildSnapshots），非 codex 账号的并发门禁不变；
// 门禁与池读同一份 acc，故二者仍恒同源。
const MaxSlots = 1024

// ThreadRetire 一条线程退役动作（账号面）。
type ThreadRetire struct {
	AccountID int64
	ThreadKey string
}

// Reclaim 是 Reclaimer 的参考实现：**锁内只收集、锁外才执行**。
//
// 为什么是批次而不是原地调用：terminate 逐条 client 调用 SDK 的 clear()，可能在
// socket 写闸上等待——放在 publisher.mu 里等于让一次 reload 被若干连接收尾拖住。
// 锁内只做内存摘表（本结构），锁外由持锁方按 `Run` 串行执行。
//
// 调度器的 `poolReclaim` 直接**嵌入**本类型（线程部分）、自己只留配置/出口那两段，
// 因此"收集"只有一份实现、一条路径。
type Reclaim struct {
	Threads []ThreadRetire
}

// RetireThread 收集一条线程的退役动作（锁内；纯内存追加，不触任何系统调用）。
func (b *Reclaim) RetireThread(accountID int64, threadKey string) {
	b.Threads = append(b.Threads, ThreadRetire{AccountID: accountID, ThreadKey: threadKey})
}

// Run 执行收集到的退役动作（必须在解锁之后调用；nil 安全）。
// retire 为 nil（本批次未装配退役出口）⇒ no-op。
func (b *Reclaim) Run(retire func(accountID int64, threadKey string)) {
	if b == nil || retire == nil {
		return
	}
	for _, t := range b.Threads {
		retire(t.AccountID, t.ThreadKey)
	}
}

// CollectRetiredPools 池注册表差分的**唯一**语义：池消失 ⇒ 退役它名下会话的线程。
//
// 池消失 ≠ 账号移除（§3.3b-4）：K 变 0、凭据类型变更、账号从路由集合退出都会让池
// 消失，但账号资源（Auth/usage/client 桶）的生命周期由**账号全集差分**决定
// （调度器的 accountConfig 资源指令 → ClientTable.ApplyBatch）。这里只回收会话/线程，
// 不冒充账号移除。
//
// 这里只**收集**，执行在锁外（`poolReclaim.run`）。r == nil（本批次无从执行）⇒
// 一条都不收集——与改前"`reclaim.retire == nil` 即返回"等价。
func CollectRetiredPools(prev, next *Registry, r Reclaimer) {
	if prev == nil || r == nil {
		return
	}
	for id, pool := range prev.Pools {
		if next != nil {
			if _, ok := next.Pools[id]; ok {
				continue
			}
		}
		pool.registryHook("enter")
		pool.CollectThreads(r)
		pool.registryHook("exit")
	}
}

// registryHook 调本池的 test-only 注册表注入点（生产 nil ⇒ 一次判断）。
func (p *Pool) registryHook(phase string) {
	if p != nil && p.hooks != nil && p.hooks.registry != nil {
		p.hooks.registry(phase)
	}
}

// CollectThreads 收集本池**空闲**会话的线程退役动作（调用方持 publisher.mu）。
// 在飞会话不动：它的连接由该请求自己收尾，残留 idle 由 SDK 的 idle TTL 时间有界
// 回收（与缩容丢槽同一条边界）。
func (p *Pool) CollectThreads(r Reclaimer) {
	if p == nil || r == nil {
		return
	}
	for i, s := range p.slots {
		if s.busy.CompareAndSwap(false, true) {
			if p.hooks != nil && p.hooks.collect != nil {
				p.hooks.collect(i)
			}
			if s.threadKey != "" {
				r.RetireThread(p.accountID, s.threadKey)
			}
			s.busy.Store(false)
		}
	}
}
