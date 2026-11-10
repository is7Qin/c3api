// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

// 账号 → codex 传输资源表（条目，以及**条目私有**的 client 桶）的持有者。
//
// 这张表被两个组件共享：调度器问它"配置里还有哪些账号/出口"（ApplyConfig）并
// 退役线程，适配层向它取（并懒构造）client。它因此是**先构造的独立组件**
// （AGENTS.md「禁止事后回填：把能力抽成独立组件按序构造」）：main 先建表，再把
// 同一个实例分别交给 scheduler.Config 与 NewCodex——构造环结构性消失。
//
// **职责边界（配置权威与请求缓存分开）**：
//   - 配置只有一个权威（scheduler 的账号运行时），这里只接受它算好的**一个批次**：
//     `ApplyConfig(账号全集 → 当前 responses 出口, 批次代号)`。表不认识"删除事件"、
//     不认识"未编组基线"，也不判断"哪个出口算活跃"——只按给定集合回收。
//   - 请求路径**不写配置**：取 client 只按 (条目, 出口, 传输) 键取/建，从不登记
//     活跃集、也从不剪枝。以调用方意图布尔量定义全局配置的设计已删除。
//   - 凭据版本失效只作用于**来源条目**（evictIfCurrent）；整账号失效是另一条路径
//     （InvalidateAccount）。
//
// 锁纪律：摘表在锁内、terminate/Close 在锁外。SDK 的 clear() 会逐条 terminate，
// 可能在 socket 写闸上等待；锁内做会把整张表卡住。
//
// **桶的归属**：client 桶挂在 codexEntry 上，不在账号上；表只映射 accountID → 当前
// 条目。于是"条目被替换/删除"与"它的桶不可达"是同一件事：
//   - 请求 A 拿着旧条目句柄迟到的写入只落在**死条目自己的 map** 里，对任何后续请求都
//     不可见——不会出现「新凭据的请求被旧 auth 的 client 送出去」（PAT/账号 ID 轮换 =
//     旧 token 继续承载流量 / 跨上游账号串台）；
//   - 账号被移除后迟到的写入不可能重建出账号级可复用桶；账号重新加回时是全新条目。

import (
	"sync"

	codexsdk "github.com/is7Qin/codex-sdk"
	"github.com/is7qin/c3api/pkg/egress"
)

// ClientTable 是账号级 codex 传输资源表：
//
//	accountID → codexEntry（Auth / 凭据签名 / usage 快照 / 该条目的 client 桶）
//
// 条目与桶**同生同灭**：条目重建（凭据替换）、账号移除、配置面回收都整条目摘除，
// 所以不存在「旧 auth 的 client 被新条目复用」这种半状态。
type ClientTable struct {
	mu      sync.Mutex
	entries map[int64]*codexEntry
	// appliedGen 已应用的配置批次代号（ApplyConfig 的单调守卫）。批次编号由调度器
	// 在 publisher.mu 内取号、应用在锁外——两次重叠 reload 可能后取号者先跑到
	// 这里，所以只接受**更新**的号：旧批次不得把资源可见性回退到旧配置。
	// 0 = 还没有应用过任何批次。
	appliedGen uint64
}

// NewClientTable 构造空表（装配点：main 在调度器之前，测试在 fixture 里）。
func NewClientTable() *ClientTable {
	return &ClientTable{entries: make(map[int64]*codexEntry)}
}

// Get 取账号当前条目（不存在 = nil）。usage 快照面用它读缓存，不建条目。
func (t *ClientTable) Get(accountID int64) *codexEntry {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.entries[accountID]
}

// Entry 取账号条目；凭据签名变化时重建（**锁内**构造，与表操作原子——同账号并发
// 请求单飞构造）。build 为条目构造函数（适配层注入，保持表对 auth 语义零知识）。
//
// 重建即整条目弃用旧资源：旧条目的 client 桶全部摘除并在**锁外**关闭——它们载着旧
// auth，留着就是"新凭据的请求被旧 client 送出去"。旧桶随死条目一起变得不可达，
// 所以即使在飞请求随后往旧句柄里写，也进不了任何活路径。构造失败时旧条目保留
// （不覆盖），错误由调用方锁外上报。
func (t *ClientTable) Entry(accountID int64, sig credKey, sourceRevision int64, build func(*codexEntry) (codexsdk.Auth, error)) (*codexEntry, error) {
	t.mu.Lock()
	// 复用条件只有**凭据签名**（同一份凭据内容 ⇒ 同一个 Auth/连接池）；来源 K 随之
	// **单调抬升**而不是触发重建：
	//   - 同签名 + 更大的 K = 不动凭据内容的身份写入（base_url/template_id/installation）
	//     ⇒ 内容没变，围栏按新 K 才正确（否则该条目的事件会被永久判成陈旧）；
	//   - 无版本调用面（K=0，管理面额度快照）先建出条目时，首个请求把 K 抬上来即可
	//     ——不必为"补一个标量"重建 Auth/关连接池。
	if old := t.entries[accountID]; old != nil && old.sig == sig {
		if sourceRevision > old.srcRev.Load() {
			old.srcRev.Store(sourceRevision)
		}
		t.mu.Unlock()
		return old, nil
	}
	e := &codexEntry{accountID: accountID, sig: sig, clients: make(map[clientKey]*codexsdk.HTTPClient)}
	e.srcRev.Store(sourceRevision)
	auth, err := build(e)
	var dropped []*codexsdk.HTTPClient
	if err == nil {
		e.auth = auth
		dropped = takeBucketsLocked(t.entries[accountID])
		t.entries[accountID] = e
	}
	t.mu.Unlock()
	closeWSIdleConns(dropped)
	return e, err
}

// Client 取 (条目 × 出口 × responses 传输) 的 HTTPClient，缺失则用 build 懒构造。
//
// e 是调用方当前持有的条目句柄。桶挂在条目上，所以：
//   - 条目已被替换/移除时，本次写入只落在**死条目自己的 map**（对其他人不可见）：
//     迟到的旧 auth 不会被任何后续请求命中；
//   - 账号移除后不存在"账号级"可复用桶，重新加回时是全新条目。
//
// 请求路径**只取/建，不回收**：历史出口摘要由配置批次的 ApplyConfig 摘（键空间随
// 规格版本增长，TTL 只管连接不管这张表），单个请求无权按一次调用推断全局活跃集。
// 摘表在锁内、Close 在锁外；并发持有旧 client 指针的在飞请求不受影响（它们只失去
// 共享的空闲连接池，下一条连接自己重拨）。
func (t *ClientTable) Client(e *codexEntry, key clientKey, build func() *codexsdk.HTTPClient) *codexsdk.HTTPClient {
	if t == nil || e == nil {
		return build()
	}
	t.mu.Lock()
	if e.clients == nil {
		e.clients = make(map[clientKey]*codexsdk.HTTPClient)
	}
	if c := e.clients[key]; c != nil {
		t.mu.Unlock()
		return c
	}
	c := build()
	e.clients[key] = c
	t.mu.Unlock()
	return c
}

// ApplyConfig 应用**一个配置批次**：`active` 是账号全集 → 当前 responses 出口
// （nil 值 = 直连），`gen` 是批次的单调代号（调度器在 publisher.mu 内取号）。
// 这是资源可见性的**唯一写入口**。
//
//   - 不在 active 里的账号：条目与桶整账号摘除（账号全集差分 = 账号移除的唯一判据；
//     没有会话池的账号同样在集合里）；
//   - 在 active 里的账号：按 egress 维度摘除历史摘要（transport 维度不动——同出口
//     换传输是合法变体）。
//
// `active == nil` 表示"该能力未装配"（独立装配/单元用例）⇒ 整体 no-op；非 nil 的
// 空表表示"没有任何账号"⇒ 摘除全部资源。
//
// **同一个准入**：gen 由锁内取号、应用在锁外，两次重叠 reload 的应用顺序不保证。
// 只接受比 appliedGen **更新**的批次——旧批次（先取号、后被新批次插队）必须整体
// no-op，否则会把资源可见性回退到旧配置。摘表在锁内、Close 在锁外。
func (t *ClientTable) ApplyConfig(gen uint64, active map[int64]*egress.Client) {
	if t == nil || active == nil {
		return
	}
	var dropped []*codexsdk.HTTPClient
	t.mu.Lock()
	if gen <= t.appliedGen {
		t.mu.Unlock()
		return
	}
	t.appliedGen = gen
	for id, e := range t.entries {
		if _, ok := active[id]; ok {
			continue
		}
		delete(t.entries, id)
		dropped = append(dropped, takeBucketsLocked(e)...)
	}
	for id, eg := range active {
		if e := t.entries[id]; e != nil {
			dropped = append(dropped, pruneBucketsLocked(e.clients, egressDigest(eg))...)
		}
	}
	t.mu.Unlock()
	closeWSIdleConns(dropped)
}

// isCurrent 报告 e 是否仍是该账号的**当前条目**（表锁内比较指针）。版本句柄的
// 事件回流用它判定"这条事件是否仍属于当前凭据版本"。
func (t *ClientTable) isCurrent(e *codexEntry) bool {
	if t == nil || e == nil {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.entries[e.accountID] == e
}

// supersededBy 是**来源版本门**的判据（纯函数，不入表锁）。
//
// 两个维度都必须看：
//   - K 更大 ⇒ 表里的条目已属于更新的身份（旧快照的事件只能落到自己那一版）；
//   - 同 K 但凭据签名不同 ⇒ 同一代身份下只有一份权威凭据内容，两个不同签名的快照
//     不可能同时是当前版本；既然表里已有一个**构造成功**的条目，迟到的那份不能
//     反过来把它删掉（资源保留胜过误删）。
func supersededBy(e *codexEntry, sourceRevision int64, sig credKey) bool {
	if e == nil {
		return false
	}
	if cur := e.srcRev.Load(); cur != sourceRevision {
		return cur > sourceRevision
	}
	return e.sig != sig
}

// evictIfCurrent 仅当 e 仍是当前条目时摘除它（及其私有桶，锁外关闭）。返回是否
// 真的摘除。
//
// 这是**凭据版本资源失效**的唯一删除语义：fatal/evict 由某一版 Prepared 产生，
// 回流时不得按 accountID 删"此刻的表项"——那会杀掉重导入后新凭据的 Auth。比较
// 指针（而非签名）是因为条目身份就是版本身份：同签名复用同一条目。
//
// 刻意**不**动配置面可见性：凭据资源失效与配置生命周期不是同一件事，配置只由
// ApplyConfig 改写（请求/运行期路径不写配置）。
func (t *ClientTable) evictIfCurrent(e *codexEntry) bool {
	if t == nil || e == nil {
		return false
	}
	t.mu.Lock()
	if t.entries[e.accountID] != e {
		t.mu.Unlock()
		return false
	}
	delete(t.entries, e.accountID)
	dropped := takeBucketsLocked(e)
	t.mu.Unlock()
	closeWSIdleConns(dropped)
	return true
}

// InvalidateAccount **账号级**失效：摘除该账号此刻的条目与其全部 client，锁外
// 逐条 CloseWSIdleConnections——归还即失效（正在归还的连接被拒收并终止）。
//
// 这是"账号整体不可用"（健康剔除 / 配置里凭据不完整）的动作，不是单条凭据版本
// 的撤销：后者必须用 evictIfCurrent，否则旧请求的错误会杀掉新凭据的 Auth。
//
// **这不是配置批次入口**：失效是运行期事实（认证被撤销、回写失败），没有配置
// 代号；配置面的账号集合/出口只由 ApplyConfig 改写——两者不共用"按账号删当前
// 资源"的实现。
func (t *ClientTable) InvalidateAccount(accountID int64) {
	if t == nil {
		return
	}
	t.mu.Lock()
	e := t.entries[accountID]
	delete(t.entries, accountID)
	dropped := takeBucketsLocked(e)
	t.mu.Unlock()
	closeWSIdleConns(dropped)
}

// invalidateOlder 按**来源版本**失效：只有当表里的条目属于**更旧**的身份代际时
// 才整账号摘除它（及其桶，锁外关闭）。返回是否真的摘除。
//
// 这是"这一版凭据构造不出 Auth"的唯一资源动作：来源版本之后的条目（K 更大，或同 K
// 但不同凭据内容）不得被旧快照删掉。没有条目（当前无资源）⇒ 无事可做。
func (t *ClientTable) invalidateOlder(accountID int64, sourceRevision int64) bool {
	if t == nil {
		return false
	}
	t.mu.Lock()
	e := t.entries[accountID]
	if e == nil || e.srcRev.Load() >= sourceRevision {
		t.mu.Unlock()
		return false
	}
	delete(t.entries, accountID)
	dropped := takeBucketsLocked(e)
	t.mu.Unlock()
	closeWSIdleConns(dropped)
	return true
}

// pruneBucketsLocked 摘除不在活跃集的桶并返回它们（调用方持锁、锁外关闭）。活跃集 =
// 该账号当前配置出口 + 直连（零值摘要）。只看 proxy 维。
func pruneBucketsLocked(m map[clientKey]*codexsdk.HTTPClient, active [16]byte) []*codexsdk.HTTPClient {
	var dropped []*codexsdk.HTTPClient
	for k, c := range m {
		if k.proxy == active || k.proxy == ([16]byte{}) {
			continue
		}
		dropped = append(dropped, c)
		delete(m, k)
	}
	return dropped
}

// takeBucketsLocked 摘空一个条目的桶并返回（调用方持锁、锁外关闭）。map 本身保留
// （不置 nil）：死条目的迟到写入仍然安全地落在这张已经无人可达的表里。
func takeBucketsLocked(e *codexEntry) []*codexsdk.HTTPClient {
	if e == nil || len(e.clients) == 0 {
		return nil
	}
	out := make([]*codexsdk.HTTPClient, 0, len(e.clients))
	for k, c := range e.clients {
		out = append(out, c)
		delete(e.clients, k)
	}
	return out
}

// RetireThread 退役该账号**当前条目**全部 client 上某线程的空闲 WS 连接（线程退休 /
// 重载丢槽的唯一出口）。摘表在 SDK 内部完成、terminate 也在那儿——本方法只在**锁外**
// 把桶快照出来再逐条调用（scheduler.Config.RetireThread 的实现）。
//
// 前置条件由会话池保证：调用时该 threadKey 不可能有在飞 exchange（会话被租借独占，
// 退休发生在该请求 Close 归还之后），所以只需摘 idle。
//
// **边界**：只看**当前条目**的桶。条目被替换/移除时若有在飞请求仍持有死句柄，它随后
// 新开的连接不在任何 retire/close 的触达面里——只由 SDK 的 idle TTL 时间有界回收。
// 规模有界（≤ 死句柄数 × 1 桶），且死句柄的连接不会被任何后续请求复用（桶不可达）。
func (t *ClientTable) RetireThread(accountID int64, threadKey string) {
	if t == nil || threadKey == "" {
		return
	}
	t.mu.Lock()
	var clients []*codexsdk.HTTPClient
	if e := t.entries[accountID]; e != nil && len(e.clients) > 0 {
		clients = make([]*codexsdk.HTTPClient, 0, len(e.clients))
		for _, c := range e.clients {
			clients = append(clients, c)
		}
	}
	t.mu.Unlock()
	for _, c := range clients {
		c.RetireThread(threadKey)
	}
}
