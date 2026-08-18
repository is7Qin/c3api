// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"math/rand/v2"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/domain"
)

// errRateScale 是错误率 EWMA 的定点缩放（0..1e6）。
const errRateScale = 1_000_000

type accState struct {
	status        domain.AccountStatus
	cooldownUntil *time.Time
	errCount      int
	lastError     *string
	lastUsedAt    *time.Time
}

// snapshotStatic 是账号静态字段视图（acc/tpl/gid/groupIDs）：**发布后不可变**。
// 重建复用实例 / 权重动作对静态字段的更新一律 copy-modify-Store 整体替换
// （原子指针发布），热路径经 atomic.Load() 一次取视图（与普通字段读同量级
// 开销），零锁读与低频写并发安全。评审 Critical 修复：复用分支此前对已发布
// 实例裸写 acc/tpl/gid/groupIDs，与热路径无锁读构成数据竞态（-race 复现）。
type snapshotStatic struct {
	acc      domain.Account
	tpl      *domain.Template
	gid      int64      // 所属组（权重动作重建路由用；多组账号 = 首个出现组）
	groupIDs []int64    // 账号所属全部分组（多组账号共享实例的跨组引用集；组级重载时其它组引用替换依据）
}

type accountSnapshot struct {
	// static 静态字段视图——不可变原子发布；重建/权重动作 copy-modify-Store，
	// 热路径 Load 一次取用（评审 Critical 修复：静态字段读全部经视图，杜绝
	// 与重建写并发的数据竞态）。
	static      atomic.Pointer[snapshotStatic]
	concurrency atomic.Int64
	errRate     atomic.Uint64 // 定点
	state       atomic.Pointer[accState]
}

func (a *accountSnapshot) statePtr() *accState {
	st := a.state.Load()
	if st == nil {
		st = &accState{status: domain.StatusActive}
		a.state.Store(st)
	}
	return st
}

// routeKey 是预生成调度路径的桶键；model == "" 表示默认回退桶
// （请求模型不在任何模板可服务集合内时，回落默认桶——仅含全模型账号的 tier2）。
type routeKey struct {
	format domain.RequestFormat
	model  string
}

// weightedSeq 是按权重预生成的循环段（GCD 归一化 + 一次 shuffle），
// 请求时以原子游标取模取用。重建时生成，Select 热路径零计算。
type weightedSeq struct {
	seq    []*accountSnapshot
	cursor atomic.Uint64 // 请求时原子游标取模取用（Select 热路径）
}

// route 是 (format, model) 桶：模型命中（Serves）走 tier1；未命中时白名单账号
// （有模型空间）跳过 → 404，全模型账号（无模型空间）走 tier2。
type route struct {
	tier1 *weightedSeq
	tier2 *weightedSeq
}

// maxSeqLen 是归一化序列长度上限：超出按比例缩放截断（防极端权重比）。
const maxSeqLen = 4096

func newWeightedSeq(pool []*accountSnapshot) *weightedSeq {
	g := 0
	for _, a := range pool {
		g = gcdInt(g, a.static.Load().acc.Weight)
	}
	if g <= 0 {
		g = 1
	}
	var total int
	for _, a := range pool {
		total += a.static.Load().acc.Weight / g
	}
	// 超长缩放：ceil 除法降权，每个账号至少保留 1 次。语义：序列长度 ≈
	// max(账号数, maxSeqLen) 量级——上限防极端权重比（9999:1 → 10000 长）的
	// 膨胀，账号数本身超过上限时不硬截（O(账号数) 可接受）。
	scale := 1
	if total > maxSeqLen {
		scale = (total + maxSeqLen - 1) / maxSeqLen // ceil
	}
	ws := &weightedSeq{}
	ws.seq = make([]*accountSnapshot, 0, total/scale+len(pool))
	for _, a := range pool {
		n := a.static.Load().acc.Weight / g / scale
		if n < 1 {
			n = 1
		}
		for i := 0; i < n; i++ {
			ws.seq = append(ws.seq, a)
		}
	}
	rand.Shuffle(len(ws.seq), func(i, j int) { ws.seq[i], ws.seq[j] = ws.seq[j], ws.seq[i] })
	return ws
}

func gcdInt(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

type groupSnapshot struct {
	accounts []*accountSnapshot
	routes   map[routeKey]*route
}

// snapshotStore 整体换入换出（atomic.Value），重建不阻塞请求路径。
type snapshotStore struct {
	groups atomic.Value // map[int64]*groupSnapshot
	byID   atomic.Value // map[int64]*accountSnapshot
}

func (s *snapshotStore) store(groups map[int64]*groupSnapshot, byID map[int64]*accountSnapshot) {
	s.groups.Store(groups)
	s.byID.Store(byID)
}
