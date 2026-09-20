// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/domain"
)

// errRateScale 是错误率 EWMA 的定点缩放（0..1e6）。
const errRateScale = 1_000_000

// accState 运行时账号状态。status = 运行时调度状态（active/disabled——
// disabled 来自 FailAccount 或 failed_at 装载；临时健康细分在 RuntimeHealth）；
// errCount/lastUsedAt 为运行时观测投影（管理端展示面，不参与选号判定）。
type accState struct {
	status     domain.AccountStatus
	errCount   int
	lastUsedAt *time.Time
}

// snapshotStatic 是账号静态字段视图（acc/tpl/gid/groupIDs）：**发布后不可变**。
// 重建对静态字段的更新一律 copy-modify-Store 整体替换
// （原子指针发布），热路径经 atomic.Load() 一次取视图（与普通字段读同量级
// 开销），零锁读与低频写并发安全。评审 Critical 修复：复用分支此前对已发布
// 实例裸写 acc/tpl/gid/groupIDs，与热路径无锁读构成数据竞态（-race 复现）。
type snapshotStatic struct {
	acc      domain.Account
	tpl      *domain.Template
	groupIDs []int64 // 账号所属全部分组（多组账号共享实例的跨组引用集；组级重载时其它组引用替换依据）
}

// eventGID 是事件投递归组用的单组代表值：**由 groupIDs 现算**，不是存储字段。
//
// 为什么不存字段：此前 gid 是一个填在 struct 里的派生值，取「首个出现组」——
// 即 map 迭代序首元素。同一份 DB 数据在不同进程/不同重载下可得不同 gid，而组
// 归属是静态事实，不得有这种自由度。改成 min(groupIDs) 修掉了不确定性，但仍留下
// 「派生值需与来源保持同步」的隐患：每个构造点都得记得算一次，漏一处就产生
// gid ∉ groupIDs 的坏状态，且只有运行时才暴露。
//
// 方法化让不一致**在结构上不可表达**：没有可写字段，就不存在忘记同步这回事。
// min 与迭代序无关，且与组集合一一对应。
func (s *snapshotStatic) eventGID() int64 {
	return minGID(s.groupIDs)
}

type sharedRuntime struct {
	concurrency atomic.Int64
	errRate     atomic.Uint64 // 定点
	state       atomic.Pointer[accState]
}

type accountSnapshot struct {
	accountID int64
	// static 静态字段视图——不可变原子发布；重建 copy-modify-Store，
	// 热路径 Load 一次取用（评审 Critical 修复：静态字段读全部经视图，杜绝
	// 与重建写并发的数据竞态）。发布后永不原地突变；变更账号分配全新 leaf，
	// 旧 leaf 保持稳定（immutable leaf discipline）。
	static  atomic.Pointer[snapshotStatic]
	runtime *sharedRuntime
}

func newAccountSnapshot(av *snapshotStatic, st *accState) *accountSnapshot {
	rt := &sharedRuntime{}
	rt.state.Store(st)
	as := &accountSnapshot{accountID: av.acc.ID, runtime: rt}
	as.static.Store(av)
	return as
}

func (a *accountSnapshot) statePtr() *accState {
	if a == nil || a.runtime == nil {
		return &accState{status: domain.StatusActive}
	}
	st := a.runtime.state.Load()
	if st == nil {
		st = &accState{status: domain.StatusActive}
		a.runtime.state.Store(st)
	}
	return st
}

// routeKey 是调度路径的桶键；model == "" 表示默认回退桶
// （请求模型不在任何模板可服务集合内时，回落默认桶——仅含全模型账号的 tier2）。
type routeKey struct {
	format domain.RequestFormat
	model  string
}

// route 是 (format, model) 桶标记：桶键集由 buildRoutes 按白名单/tier 语义
// 生成，编译车道据此枚举并重算候选（legacy 加权预生成序列已随 cutover 删除）。
type route struct{}

type groupSnapshot struct {
	accounts []*accountSnapshot
	routes   map[routeKey]*route
}
