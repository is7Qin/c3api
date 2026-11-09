// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

// 账号 → codex 传输资源表（条目 + client 桶）的**唯一持有者**。
//
// 这张表被两个组件共享：调度器用它退役线程 / 移除账号，适配层用它取（并懒构造）
// client。它因此是**先构造的独立组件**（AGENTS.md「禁止事后回填：把能力抽成独立
// 组件按序构造」）：main 先建表，再把同一个实例分别交给 scheduler.Config 与
// NewCodex——构造环结构性消失，没有"被闭包捕获、装配完成后才回填"的指针。
//
// 锁纪律：摘表在锁内、terminate/Close 在锁外。SDK 的 clear() 会逐条 terminate，
// 可能在 socket 写闸上等待；锁内做会把整张表卡住（G4/N1）。

import (
	"sync"

	codexsdk "github.com/is7Qin/codex-sdk"
)

// ClientTable 是账号级 codex 传输资源表：
//
//	accountID → codexEntry（Auth / 凭据签名 / usage 快照）
//	accountID → map[clientKey]*HTTPClient（出口 × responses 传输）
//
// 两类条目**同生同灭**：条目重建（凭据替换）与账号移除都整账号摘除，所以不存在
// 「旧 auth 的 client 被新条目复用」这种半状态。
type ClientTable struct {
	mu      sync.Mutex
	entries map[int64]*codexEntry
	clients map[int64]map[clientKey]*codexsdk.HTTPClient
}

// NewClientTable 构造空表（装配点：main 在调度器之前，测试在 fixture 里）。
func NewClientTable() *ClientTable {
	return &ClientTable{
		entries: make(map[int64]*codexEntry),
		clients: make(map[int64]map[clientKey]*codexsdk.HTTPClient),
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
// 重建即整账号弃用旧资源：旧条目名下的 client 全部摘除并在**锁外**关闭——它们
// 载着旧 auth，留着就是"新凭据的请求被旧 client 送出去"（§3.2 凭据替换）。
// 构造失败时旧条目保留（不覆盖），错误由调用方锁外上报。
func (t *ClientTable) Entry(accountID int64, sig credKey, build func(*codexEntry) (codexsdk.Auth, error)) (*codexEntry, error) {
	t.mu.Lock()
	if old := t.entries[accountID]; old != nil && old.sig == sig {
		t.mu.Unlock()
		return old, nil
	}
	e := &codexEntry{accountID: accountID, sig: sig}
	auth, err := build(e)
	var dropped []*codexsdk.HTTPClient
	if err == nil {
		e.auth = auth
		t.entries[accountID] = e
		dropped = t.takeClientsLocked(accountID)
	}
	t.mu.Unlock()
	closeWSIdleConns(dropped)
	return e, err
}

// Client 取 (账号 × 出口 × 传输) 的 HTTPClient，缺失则用 build 懒构造。
//
// 构造后按**当前活跃组合集**回收（§3.2 出口 / transport 变更）：活跃集 = 本次请求
// 的出口摘要与直连（零值摘要——usage 快照等无 Selection 面走它）。其余摘要都是
// 历史规格（代理地址/密码轮换、改绑产生的旧 Spec.Key()），**必须摘除**：clientKey
// 的 proxy 维是出口规格摘要，键空间随规格版本无界增长，TTL 只管连接不管这张表。
//
// 摘表在锁内、Close 在锁外；并发持有旧 client 指针的在飞请求不受影响（它们只失去
// 共享的空闲连接池，下一条连接自己重拨）。
func (t *ClientTable) Client(accountID int64, key clientKey, build func() *codexsdk.HTTPClient) *codexsdk.HTTPClient {
	t.mu.Lock()
	m := t.clients[accountID]
	if m == nil {
		m = make(map[clientKey]*codexsdk.HTTPClient)
		t.clients[accountID] = m
	}
	if c := m[key]; c != nil {
		t.mu.Unlock()
		return c
	}
	dropped := pruneClientsLocked(m, key.proxy)
	c := build()
	m[key] = c
	t.mu.Unlock()
	closeWSIdleConns(dropped)
	return c
}

// pruneClientsLocked 摘除不在活跃集的 client 桶并返回它们（调用方持锁、锁外关闭）。
// 活跃集 = 本次出口摘要 + 直连摘要（零值）。
func pruneClientsLocked(m map[clientKey]*codexsdk.HTTPClient, active [16]byte) []*codexsdk.HTTPClient {
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

// takeClientsLocked 摘除并返回某账号全部 client（调用方持锁、锁外关闭）。
func (t *ClientTable) takeClientsLocked(accountID int64) []*codexsdk.HTTPClient {
	m := t.clients[accountID]
	if len(m) == 0 {
		delete(t.clients, accountID)
		return nil
	}
	out := make([]*codexsdk.HTTPClient, 0, len(m))
	for k, c := range m {
		out = append(out, c)
		delete(m, k)
	}
	delete(t.clients, accountID)
	return out
}

// RetireThread 退役该账号全部 client 上某线程的空闲 WS 连接（线程退休 / 重载丢槽
// 的唯一出口）。摘表在 SDK 内部完成、terminate 也在那儿——本方法只在**锁外**把桶
// 快照出来再逐条调用（scheduler.Config.RetireThread 的实现）。
//
// 前置条件由会话池保证：调用时该 threadKey 不可能有在飞 exchange（会话被租借独占，
// 退休发生在该请求 Close 归还之后），所以只需摘 idle。
func (t *ClientTable) RetireThread(accountID int64, threadKey string) {
	if t == nil || threadKey == "" {
		return
	}
	t.mu.Lock()
	m := t.clients[accountID]
	var clients []*codexsdk.HTTPClient
	if len(m) > 0 {
		clients = make([]*codexsdk.HTTPClient, 0, len(m))
		for _, c := range m {
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
	delete(t.entries, accountID)
	dropped := t.takeClientsLocked(accountID)
	t.mu.Unlock()
	closeWSIdleConns(dropped)
}
