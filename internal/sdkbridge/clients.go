// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

// 账号 → codex 传输资源表（条目，以及**条目私有**的 client 桶）的唯一持有者。
//
// 这张表被两个组件共享：调度器用它退役线程 / 移除账号 / 按配置回收桶，适配层用它取
// （并懒构造）client。它因此是**先构造的独立组件**（AGENTS.md「禁止事后回填：把能力抽成
// 独立组件按序构造」）：main 先建表，再把同一个实例分别交给 scheduler.Config 与
// NewCodex——构造环结构性消失，没有"被闭包捕获、装配完成后才回填"的指针。
//
// 锁纪律：摘表在锁内、terminate/Close 在锁外。SDK 的 clear() 会逐条 terminate，
// 可能在 socket 写闸上等待；锁内做会把整张表卡住（G4/N1）。
//
// **桶的归属**：client 桶挂在 codexEntry 上，不在账号上；表只映射 accountID → 当前
// 条目。于是"条目被替换/删除"与"它的桶不可达"是同一件事：
//   - 请求 A 拿着旧条目句柄迟到的写入只落在**死条目自己的 map** 里，对任何后续请求都
//     不可见——不会出现「新凭据的请求被旧 auth 的 client 送出去」（PAT/PAT-Key 轮换 =
//     旧 token 继续承载流量；改绑 CodexAccountID = 跨上游账号串台，§3.2 凭据替换）；
//   - 账号被移除后迟到的写入不可能重建出账号级可复用桶；账号重新加回时是全新条目
//     （§3.2 账号移除）。
//
// **活跃集（出口维度）**：由**账号当前配置**决定（`Reconcile`，reload 面的配置生命周期
// 更新点），不是由单个请求的入参决定——否则 usage 快照（恒 Direct）一次调用就会把在用
// 的代理桶删掉并永久 Close，直接打掉"跨窗口不重连"（§3.2 出口/transport 变更）。

import (
	"sync"

	codexsdk "github.com/is7Qin/codex-sdk"
	"github.com/is7qin/c3api/pkg/egress"
)

// ClientTable 是账号级 codex 传输资源表：
//
//	accountID → codexEntry（Auth / 凭据签名 / usage 快照 / 该条目的 client 桶）
//	accountID → 当前配置的 responses 出口摘要（活跃集，唯一来源是 Reconcile）
//
// 条目与桶**同生同灭**：条目重建（凭据替换）、账号移除、配置面回收都整条目摘除，
// 所以不存在「旧 auth 的 client 被新条目复用」这种半状态。
type ClientTable struct {
	mu      sync.Mutex
	entries map[int64]*codexEntry
	// active 账号当前**配置**的 responses 出口摘要（存在 = 配置已登记，值 `{}` =
	// 直连）。写入点只有 Reconcile；请求路径不得改写。缺失 = 尚未登记（独立装配/
	// 单元用例），此时允许请求侧按首次构造的出口兜底登记（见 Client）。
	active map[int64][16]byte
}

// NewClientTable 构造空表（装配点：main 在调度器之前，测试在 fixture 里）。
func NewClientTable() *ClientTable {
	return &ClientTable{
		entries: make(map[int64]*codexEntry),
		active:  make(map[int64][16]byte),
	}
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
// auth，留着就是"新凭据的请求被旧 client 送出去"（§3.2 凭据替换）。旧桶随死条目
// 一起变得不可达，所以即使在飞请求随后往旧句柄里写，也进不了任何活路径。
// 构造失败时旧条目保留（不覆盖），错误由调用方锁外上报。
func (t *ClientTable) Entry(accountID int64, sig credKey, build func(*codexEntry) (codexsdk.Auth, error)) (*codexEntry, error) {
	t.mu.Lock()
	if old := t.entries[accountID]; old != nil && old.sig == sig {
		t.mu.Unlock()
		return old, nil
	}
	e := &codexEntry{accountID: accountID, sig: sig, clients: make(map[clientKey]*codexsdk.HTTPClient)}
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
//     迟到的旧 auth 不会被任何后续请求命中（B4/B1）；
//   - 账号移除后不存在"账号级"可复用桶，重新加回时是全新条目（B3）。
//
// pinEgress 声明本次构造用的出口来自**账号当前配置**（responses/images/search 车道
// 拿的是 Selection 上的账号出口）；usage 快照面传 false。它只在该账号还没有配置登记的
// 情况下作为兜底登记（独立装配/单元用例）；生产装配下 Reconcile 已在 reload 面登记，
// 请求路径不改写活跃集——否则一条迟到旧视图的请求就能把整账号的活跃集拉回旧规格。
//
// 构造后按活跃集回收（§3.2 出口 / transport 变更）：保留 {该账号当前配置出口, 直连
// 零值}，其余出口摘要都是历史规格（代理地址/密码轮换、改绑产生的旧 Spec.Key()），
// **必须**摘除：clientKey 的 proxy 维是规格摘要，键空间随规格版本无界增长，TTL 只管
// 连接不管这张表。transport 维**不**参与剪枝——换传输是合法变体，同一出口最多两个桶，
// 剪掉它只会让旧传输面的请求重拨。直连（零值摘要）恒保留：它是 usage 快照的出口。
//
// 摘表在锁内、Close 在锁外；并发持有旧 client 指针的在飞请求不受影响（它们只失去
// 共享的空闲连接池，下一条连接自己重拨）。
func (t *ClientTable) Client(e *codexEntry, key clientKey, pinEgress bool, build func() *codexsdk.HTTPClient) *codexsdk.HTTPClient {
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
	// 只有"当前条目"的构造才能登记账号出口：死条目上的迟到构造不得改写活跃集。
	if pinEgress && t.entries[e.accountID] == e {
		if _, known := t.active[e.accountID]; !known {
			t.active[e.accountID] = key.proxy
		}
	}
	dropped := pruneBucketsLocked(e.clients, t.active[e.accountID])
	c := build()
	e.clients[key] = c
	t.mu.Unlock()
	closeWSIdleConns(dropped)
	return c
}

// Reconcile 按**账号当前配置**回收传输资源（§3.2 出口/transport 变更）。调用方是
// reload 面（配置生命周期），传**全量**账号集：accountID → 当前 responses 出口
// （nil = 直连）。nil 表示"未装配该能力"，no-op；非 nil 的空表表示"没有任何账号"，
// 会摘除全部资源。
//
//   - 不在集合里的账号：条目与桶整账号摘除（含**没有会话池**的账号——池注册表差分看
//     不到它们，N7；以及应用关闭/清空部署）；
//   - 在集合里的账号：登记活跃集，并按 egress 维度摘除历史摘要（transport 维度不动）。
//
// 这就是"活跃集不能由单个请求的入参定义"的落点：usage 面只会读这个集合，不会重定义它。
// 摘表在锁内、Close 在锁外。
func (t *ClientTable) Reconcile(active map[int64]*egress.Client) {
	if t == nil || active == nil {
		return
	}
	var dropped []*codexsdk.HTTPClient
	t.mu.Lock()
	for id := range t.active {
		if _, ok := active[id]; ok {
			continue
		}
		delete(t.active, id)
		if e := t.entries[id]; e != nil {
			delete(t.entries, id)
			dropped = append(dropped, takeBucketsLocked(e)...)
		}
	}
	for id, eg := range active {
		d := egressDigest(eg)
		t.active[id] = d
		if e := t.entries[id]; e != nil {
			dropped = append(dropped, pruneBucketsLocked(e.clients, d)...)
		}
	}
	// 从未登记过活跃集、也不在当前集合里的条目（装配前遗留/直接调用 Entry 的用例）。
	for id, e := range t.entries {
		if _, ok := active[id]; ok {
			continue
		}
		delete(t.entries, id)
		dropped = append(dropped, takeBucketsLocked(e)...)
	}
	t.mu.Unlock()
	closeWSIdleConns(dropped)
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

// RemoveAccount 账号从账号集合消失（§3.2 账号移除）：摘除条目与其全部 client，
// 锁外逐条 CloseWSIdleConnections——归还即失效（正在归还的连接被拒收并终止）。
//
// 判定点在上游：调度器重载时池注册表差分（池存在而账号/id 不在新表）。本方法
// 就是那个事件的实际执行面（scheduler.Config.RemoveAccount 的实现），不存在
// "无事件源"的挂起理由。
func (t *ClientTable) RemoveAccount(accountID int64) {
	if t == nil {
		return
	}
	t.mu.Lock()
	e := t.entries[accountID]
	delete(t.entries, accountID)
	delete(t.active, accountID)
	dropped := takeBucketsLocked(e)
	t.mu.Unlock()
	closeWSIdleConns(dropped)
}
