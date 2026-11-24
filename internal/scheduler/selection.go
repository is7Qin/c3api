// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"github.com/is7qin/c3api/internal/api"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/identitypool"
	"github.com/is7qin/c3api/internal/sdkbridge"
)

// Selection 是数据面契约（api.Selection）的控制面拼写：别名，不是新类型。
//
// 形状 A（spec 2026-11-23 P3b §2.1）：19 个只读事实是 api.Selection 的**字段**，
// 控制面私有的租约/会话/池在 selection 包装里、只经 Handle 暴露。别名让 P3b 前
// 的 238 处字段直读与 88 个类型位点零改写地继续编译，同时把所有权交给中立包。
type Selection = api.Selection

var (
	_ api.SelectionHandle = (*selection)(nil)
	_ api.AttemptPlan     = (*selectSession)(nil)
)

// selection 是 Selection 在控制面的实体：契约字段 + 三个私有生命周期指针。
//
// 为什么私有指针放在**包装**而不是 Selection 里：api 是中立包，不能持有
// identitypool.Session/identitypool.Pool/leaseToken 这些控制面类型。包装与嵌入的 Selection
// 在同一个堆对象上（对外返回的是 &selected.Selection 内指针），所以这次拆分
// **不多分配**：总尺寸 288B 仍落在同一个 288B 分配档（3 allocs / 328 B 基准）。
type selection struct {
	Selection
	lease *leaseToken
	// session 本次预留认领的 codex 会话（仅 codex 凭据；nil = 无会话——非 codex
	// 面不认领）。Release 归还；Thread/StepIdentity 经它读写。
	session *identitypool.Session
	// pool 会话所属池（线程退役出口）。全忙兜底的临时会话无池。
	pool *identitypool.Pool
}

// Release 归还租约（幂等）。nil / 无租约 ⇒ no-op——数据面把「没有预留」的
// selection 也丢进同一条收尾路径（leaseGuard / defer）。
func (s *selection) Release() {
	if s == nil || s.lease == nil {
		return
	}
	if s.lease.released.CompareAndSwap(false, true) {
		// 先归还会话，再减并发计数：门禁按 concurrency 放行，若先减计数则释放
		// 窗口内可出现「K 会话全忙而 concurrency=K-1」——新预留放行后
		// claim()==nil（兜底临时身份 + Warn，破坏「K 条稳定会话」）。归还会话
		// 先于计数，使门禁观测到的空闲额度对应的会话已真正可用。
		s.session.Release()
		s.lease.acc.runtime.concurrency.Add(-1)
	}
}

// Thread 本次请求的槽位身份数据（identity 是**请求数据**，§3.1）。
//
// first = 本请求是一轮的第一个请求（调用方取 `!AnalyzeConversationInput(payload).FollowUp()`）。
// 槽位路径不重放、不捕获 turn-state：Sink 恒 nil、Turn.State 恒空——与此前
// `codexTurnStateReplay=false` 的线上行为等价。全部取自会话（installation 是账号级
// 稳定项、与 session/thread/window 同源），字段拷贝、零分配。
//
// 无会话（非 codex / 池缺席 / 会话路径不认领槽位）→ 零值（SDK 视为匿名）。
func (s *selection) Thread() sdkbridge.ThreadHandle {
	if s == nil || s.session == nil {
		return sdkbridge.ThreadHandle{}
	}
	return s.session.Thread()
}

// StepIdentity 推进本会话一次完成轮（§3.2 推进）——**仅成功取到 usage、且上游
// 连接已归还（`Body.Close`）之后**调用一次；无会话 no-op。
//
// 退休（窗口数达 wmax）在此发生：会话独占租借，本函数不持任何锁，退役旧线程的
// 空闲连接是锁外动作（§3.2 ④）。
func (s *selection) StepIdentity() {
	if s == nil || s.session == nil {
		return
	}
	if old := s.session.Step(); old != "" {
		s.pool.Retire(old)
	}
}

// Credential 本次请求的凭据投影：`CredentialFromExt` 的各列 + **来源 K**。
//
// 调用方一律用它而不是 `domain.CredentialFromExt(sel.Ext)`：凭据内容与 K 必须
// 同一次快照读，否则"内容属于某一版、K 属于另一版"会让 SDK 侧的事件围栏失准。
// K 冻结在租借上（不是再读快照）——请求准备路径因此没有第二次静态读。
//
// nil Ext ⇒ 零值 Credential（早退语义与 P3b 前逐字一致：CredentialFromExt(nil)
// 返回零值，identityRevision 也为 0）。
func (s *selection) Credential() domain.AccountCredential {
	cred := domain.CredentialFromExt(s.Ext)
	cred.IdentityRevision = s.identityRevision()
	return cred
}

// identityRevision 本次请求所用凭据的**来源身份代际 K**（认领租借时冻结的那一版）。
// 0 = 无租借（非选号调用面）。
func (s *selection) identityRevision() int64 {
	if s == nil || s.lease == nil {
		return 0
	}
	return s.lease.identityRevision
}

// leaseAccountForTest 暴露租借账号（测试断言租借归属用）。
func (s *selection) leaseAccountForTest() *accountSnapshot {
	if s == nil || s.lease == nil {
		return nil
	}
	return s.lease.acc
}

// Select 选号并占用并发槽：执行预编译 DecisionView 计划
// （NewAttemptPlan + ReserveAttempt：lane 顺序、完整唯一 overflow 尾、
// reservation reject 不耗 attempt）。未编译路由（编译车道尚未产出该桶）
// 直接 ErrFormatUnavailable。
// 调用方完成请求后必须 Release + MarkResult。
//
// the session is a stack value; the scheduler call takes a stack
// pointer that never escapes (nothing retained across calls).
func (s *Scheduler) Select(groupID int64, format domain.RequestFormat, model string) (*Selection, error) {
	return s.selectAttempt(true, groupID, format, model)
}

// SelectOpaque selects from a compiled route without applying model mapping.
func (s *Scheduler) SelectOpaque(groupID int64, format domain.RequestFormat, model string) (*Selection, error) {
	return s.selectAttempt(false, groupID, format, model)
}

// selectAttempt is the single reservation body behind Select/SelectOpaque; the
// only difference is whether the compiled plan applies model mapping.
func (s *Scheduler) selectAttempt(applyMapping bool, groupID int64, format domain.RequestFormat, model string) (*Selection, error) {
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{ApplyModelMapping: applyMapping}, RouteRefFor(groupID, string(format), model))
	if err != nil {
		return nil, err
	}
	sel, _, err := s.ReserveAttempt(&plan)
	if err != nil {
		return nil, err
	}
	return sel, nil
}
