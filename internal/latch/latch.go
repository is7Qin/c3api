// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package latch 是规则失效锁存的一等组件（根因重开）：单实例内存
// 锁存（account+fingerprint+revision）+ 规则失败事件的同步扇出中介。
// 叶子包：只依赖 internal/rule 的事件类型（无环：rule 永不回指本包），
// 不依赖 scheduler（scheduler 反向持有本包指针——反向持有即 import 环）。
// 所有权：main 构造 LatchStore + Hub 各恰好一次，经构造参出借（scheduler
// 只借指针，reload 清理/TryLatch/IsLatched/Snapshot 逻辑不变）；生命周期
// 与 redis 无关（latch 是单实例内存，RuntimeHealth 的 Redis 面保持独立）。
package latch

import (
	"sync"

	"github.com/is7qin/c3api/internal/rule"
)

// LatchKey 是锁存身份的对外形态：账号 + 候选指纹 + **身份代际 K**（不是配置代际
// C——C 变化不作废锁存，K 变化才作废）。
type LatchKey struct {
	AccountID        int64
	Fingerprint      string
	IdentityRevision int64
}

type latchKey struct {
	AccountID        int64
	Fingerprint      string
	IdentityRevision int64
}

// LatchStore 单实例内存锁存：FailAccount 先锁存（fail-closed）再异步持久化；
// 成功恢复/新 revision/指纹变更/账号移除时清理。并发安全，可多 goroutine 共用。
type LatchStore struct {
	mu sync.RWMutex
	m  map[int64]latchKey
}

// NewLatchStore 构造空锁存（main 恰好调用一次；测试自建传参）。
func NewLatchStore() *LatchStore {
	return &LatchStore{m: make(map[int64]latchKey)}
}

// TryAcquire 尝试锁存：同指纹同 revision 已锁存返回 false（幂等吸收），
// 否则写入并返回 true。
func (l *LatchStore) TryAcquire(accountID int64, fingerprint string, identityRevision int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur, ok := l.m[accountID]; ok {
		if cur.Fingerprint == fingerprint && cur.IdentityRevision == identityRevision {
			return false
		}
	}
	l.m[accountID] = latchKey{AccountID: accountID, Fingerprint: fingerprint, IdentityRevision: identityRevision}
	return true
}

// IsLatched 查询锁存：未锁存返回 false。指纹与身份代际 K 是两个**独立**的围栏
// 维度——任一不符即视为陈旧（陈旧 latch 天然不生效，正确性不依赖显式释放动作
// 是否可靠）。空 fingerprint / 零 revision = 该维度不参与比较（调用方缺该信息
// 时的通配，例如按账号查询）。
func (l *LatchStore) IsLatched(accountID int64, fingerprint string, identityRevision int64) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	cur, ok := l.m[accountID]
	if !ok {
		return false
	}
	if fingerprint != "" && cur.Fingerprint != "" && cur.Fingerprint != fingerprint {
		return false
	}
	if identityRevision != 0 && cur.IdentityRevision != identityRevision {
		return false
	}
	return true
}

// Clear 清除指定账号锁存。
func (l *LatchStore) Clear(accountID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, accountID)
}

// Snapshot returns the live latches in compiler input form (all present keys
// are latched=true). Owned copy; safe for concurrent readers/writers.
func (l *LatchStore) Snapshot() map[LatchKey]bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make(map[LatchKey]bool, len(l.m))
	for _, k := range l.m {
		out[LatchKey{AccountID: k.AccountID, Fingerprint: k.Fingerprint, IdentityRevision: k.IdentityRevision}] = true
	}
	return out
}

// ClearIfFingerprintChanged 指纹变更时清旧锁存（候选身份变化 = 新候选人，
// 旧锁存不得株连）。
func (l *LatchStore) ClearIfFingerprintChanged(accountID int64, currentFingerprint string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur, ok := l.m[accountID]; ok && cur.Fingerprint != currentFingerprint {
		delete(l.m, accountID)
	}
}

// Hub 是规则失败事件的同步扇出中介（C1 选中）：scheduler 在 New 内
// Subscribe(s.onRuleFailure)；rule 工作协程经 LatchSink.FailAccount 内
// Dispatch——同协程执行内存摘除（零异步窗口；若未来改异步，需先给
// Select 加 latch 门）。仅构造期 Subscribe，运行时只读订阅表。
type Hub struct {
	mu   sync.RWMutex
	subs []func(rule.Event)
}

// NewHub 构造空扇出中介（main 恰好调用一次；测试自建传参）。
func NewHub() *Hub {
	return &Hub{}
}

// Subscribe 注册失败事件订阅（仅构造期调用；运行时并发 Subscribe 未定义）。
func (h *Hub) Subscribe(fn func(rule.Event)) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subs = append(h.subs, fn)
}

// Dispatch 同步扇出事件到全部订阅（调用者为 rule 工作协程，与今日
// sched.FailAccount 同 timeliness；订阅回调不得阻塞）。
func (h *Hub) Dispatch(ev rule.Event) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, fn := range h.subs {
		fn(ev)
	}
}
