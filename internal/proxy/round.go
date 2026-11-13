// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

// 轮状态机与每轮一次结算（spec-commit-state-and-round-billing §6，零 schema 变更）。
//
// 事实边界：入站 WebSocket（`/v1/responses` 长连接）一次连接可承载**多轮**——每轮
// 一个 response.create → 上游一串事件 → 一个 response.completed。旧实现把整条连接
// 折叠成**一行**用量（最后一次 completed 的 usage 覆盖前轮，轮 1..K 的 token 只
// 剩下 K 的），轮级成本不可见。本文件把「轮」提升为一等对象：
//
//	RoundOpen ──(completed 且解析到 usage)──> RoundCompletedWithUsage   → 结算（含 tokens）
//	           └(completed 但无/坏 usage)───> RoundCompletedWithoutUsage → 结算（tokens=0）
//	           └(同一轮第二次 completed)────> RoundInvalidDuplicate       → 不重复结算，投递一次 Warn
//	连接收尾时仍为 RoundOpen ─────────────> RoundAborted                 → 失败行（§6.3）
//
// Completed* 是**吸收态**（§6.1「completed 是终态观测，优先级最高」）：一旦本轮
// 观测到完成帧，之后的 transport/upstream/下行写失败不得把它降级为失败，也不得
// 二次结算——它只能影响连接级分类与客户端体验，不能回头改已经结算的那一轮。
//
// 幂等（§6.2）：结算行以 (request_id, created_at) 为键复用 usage_logs **既有**
// 唯一索引（repository/partition.go 的 usagelog_request_id_created_at +
// usage_repo.go 的 OnConflictColumns(...).DoNothing()）——不新增表/列/索引/迁移。
// 因此 request_id 必须按轮派生、created_at 必须取**轮起点**（落库时刻会让同轮两
// 次结算得到两个不同的键，唯一索引就失效了）。

import (
	"context"
	"strconv"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/logx"
)

// roundState 轮状态（§6.1）。只有 RoundOpen 会离开；Completed* 吸收。
type roundState uint8

const (
	// RoundOpen 本轮已开始、尚未观测到协议终态帧（response.completed）。
	RoundOpen roundState = iota
	// RoundCompletedWithUsage 观测到终态帧且解析到 usage → 结算含 tokens。
	RoundCompletedWithUsage
	// RoundCompletedWithoutUsage 观测到终态帧但没有/坏了 usage → 结算 tokens=0
	// （不伪造用量、不记为 upstream_responded）。
	RoundCompletedWithoutUsage
	// RoundInvalidDuplicate 同一轮第二次终态帧 → 不重复结算，只 Warn。
	RoundInvalidDuplicate
	// RoundAborted 连接收尾时本轮仍 RoundOpen（§6.3 失败行）。允许携带已采
	// usage——「采到了部分用量但没看到完成帧」与「看到了完成帧但没有用量」
	// （CompletedWithoutUsage）是两件事，落库可区分（前者 error_type != none）。
	RoundAborted
)

func (s roundState) String() string {
	switch s {
	case RoundOpen:
		return "open"
	case RoundCompletedWithUsage:
		return "completed_with_usage"
	case RoundCompletedWithoutUsage:
		return "completed_without_usage"
	case RoundInvalidDuplicate:
		return "invalid_duplicate"
	case RoundAborted:
		return "aborted"
	default:
		return "unknown"
	}
}

// round 多轮车道的一轮（入站 WS 连接上的第 index 轮）。
//
// 单写者纪律：**只有 up-loop goroutine 读写轮字段**（终态帧在 up-loop 里观测 →
// 结算 → 翻轮），编排在 `<-upLoopDone` 之后（close 建 happens-before）才读快照
// ——与 businessN/roundCompleted 同款纪律，故 billingSettled 用普通 bool 即可
// （不需要 atomic：没有跨 goroutine 的结算者）。
type round struct {
	// base 连接级 request_id（= 入站 reqID）。派生 id 恒以它为前缀。
	base string
	// index 第几轮（1 起）。
	index int
	// StartedAt 轮起点：created_at 与 LatencyMS 的共同基准（§6.2/§6.3）。
	StartedAt time.Time
	// state FSM 状态（§6.1）。
	state roundState
	// billingSettled 本轮已结算（§6.2 第一层防线）。名字不用 settled：
	// pipeline_observer.go 的 foldOwner 已有同名字段，同名会让 grep 与 review 混淆。
	billingSettled bool
	// usage 本轮已采用量（终态帧覆盖语义；轮起点清零）。
	usage usageTuple
	// ttft 本轮首个下行业务帧的耗时（毫秒；本轮无帧 = nil）。
	ttft *int64
	// businessN 本轮真实下行业务帧计数（commit_state 的事实源）。
	businessN int
}

// newRound 连接的第 1 轮：轮起点 = 连接开始时刻（单轮 WS 车道因此与旧行为同值）。
func newRound(base string, start time.Time) *round {
	return &round{base: base, index: 1, StartedAt: start, state: RoundOpen}
}

// requestID 本轮派生的 request_id（§6.2）：index==1 → base；index>=2 → base#rK。
// 第 1 轮恒等于连接级原值（零行为变化）；K>=2 轮加后缀以免与同连接其它轮撞上
// (request_id, created_at) 唯一键，并让运营能按前缀关联同一连接的所有轮。
func (r *round) requestID() string {
	if r.index <= 1 {
		return r.base
	}
	return r.base + "#r" + strconv.Itoa(r.index)
}

// createdAt 本轮 created_at = **轮起点**，不是落库时刻：同轮两次结算必须得到
// 同一个键，否则既有唯一索引失效（§6.2）。
func (r *round) createdAt() time.Time { return r.StartedAt }

// completed 本轮是否已观测到终态帧（CompletedWith/WithoutUsage 与
// InvalidDuplicate 都算「已完成」——后者只是同一轮又收到一帧；只有 RoundOpen
// 与 RoundAborted 表示本轮未完成）。
func (r *round) completed() bool {
	return r.state == RoundCompletedWithUsage ||
		r.state == RoundCompletedWithoutUsage ||
		r.state == RoundInvalidDuplicate
}

// roll 翻到下一轮：index+1、轮起点 = now、本轮累计清零。
//
// 轮边界事实（§6.1）：一轮以 response.completed 收尾，下一轮以 **response.created**
// 起头——只有「当前轮已 completed 且收到 response.created」才翻轮（编排见
// ws_relay.go）。completed 之后的收尾杂帧（response.done / output_item.done /
// 心跳型事件…）仍属本轮，不构成起点：按任意帧翻轮会开出一个从未真正开始的空轮，
// 收尾时给它写一条 none/200 的假成功行（虚增 request_count）。翻轮只重置记账状态，
// 不改帧流——帧照旧 1:1 透传；开出的那一轮若始终没有终态帧，收尾时按 RoundAborted
// 落失败行（§6.3）。
func (r *round) roll(now time.Time) {
	r.index++
	r.StartedAt = now
	r.state = RoundOpen
	r.billingSettled = false
	r.usage = usageTuple{}
	r.ttft = nil
	r.businessN = 0
}

// observeCompleted 终态帧观测（§6.1 FSM 单向迁移）：本轮 Open → Completed*
// （带/不带 usage）；本轮已是 Completed* 的第二帧 → InvalidDuplicate（投递一次
// Warn，不写行）；已 InvalidDuplicate → 静默吞掉（不再刷 Warn）。
// 返回 true 表示本次观测**刚刚完成**本轮（调用方据此触发结算）。
func (r *round) observeCompleted(p *Proxy, f []byte, sel *scheduler.Selection) bool {
	if r.completed() {
		if r.state != RoundInvalidDuplicate && p.log != nil {
			p.log.Warn("responses round duplicate completed frame",
				logx.String("request_id", r.requestID()),
				logx.Int64("round_index", int64(r.index)),
				logx.Int64("account_id", sel.AccountID),
			)
		}
		r.state = RoundInvalidDuplicate
		return false
	}
	if u, ok := sniffResponsesCompletedUsage(f); ok {
		r.usage = u
		r.state = RoundCompletedWithUsage
		// codex 会话轮次推进（成功取到 usage 时 Step 一次；非 codex
		// Selection 无会话 → no-op）。多轮语义：每轮各 Step 一次。
		sel.StepIdentity()
		// 响应检测旁路（spec §6）：completed 帧恒在流末，最终计数覆盖前值。
		if respImageDetectOn(sel) {
			r.usage.calls = respImageCountCompleted(f)
		}
		return true
	}
	r.state = RoundCompletedWithoutUsage
	return true
}

// settle 结算一轮（§6.2）。
//
// 入口的 billingSettled 短路是**资金防线**（metis r2 P0-1）：DB 唯一索引只去重
// usage_logs 行，挡不住 DeductQuota/AddQuota——后者的 delta 会经 flushQuota
// 持久累加进 keys.quota_used，重复结算 = 配额被重复消耗（钱只扣一次，但限流
// 误伤）。成本 = 一次 bool 判断。
//
// 顺序与既有 finish 同序（applyBilling → Record → DeductQuota → AddQuota）：
// Record 只入内存队列，DeductQuota 紧随其后做内存准入扣减——不新增时序语义。
// **永不 Release**：Selection 租约是连接级的，release 只在连接收尾的 defer 调
// 一次（§6.4；release 本身已是 CAS 幂等，故不新增 guard）。
func (r *round) settle(p *Proxy, sel *scheduler.Selection, l *domain.UsageLog) {
	if r.billingSettled {
		return
	}
	r.billingSettled = true
	p.applyBilling(l)
	if p.cfg.UsageCapture {
		// 供应商收益出生定态：归属/分成/credited 三列按选中账号时捕获的财务
		// 上下文盖章（与 finish 同一落章点，必须在 routeLog 之前）。
		p.stampSupplier(sel, l)
		p.routeLog(l) // usage_logs（可计费）/ err_logs（全错误）唯一路由点
	}
	if p.cfg.BillingCapture {
		delta := p.auth.DeductQuota(l.KeyID, l.Cost)
		if delta > 0 && p.rec != nil {
			p.rec.AddQuota(l.KeyID, delta)
		}
	}
}

// buildRoundLog 一轮的写行点（§6.2「两表同 ID」的强制入口）：RequestID /
// CreatedAt / LatencyMS / TTFTMS 的唯一来源是 round 本身。
//
// 委托给 buildLog 以免字段映射分叉；三处要点：
//   - started 传 r.StartedAt ⇒ LatencyMS = 本轮到结算的耗时（不是连接总时长）；
//   - CreatedAt 覆盖为轮起点（buildLog 默认落库时刻——单轮车道保持该默认，
//     多轮车道的 created_at 必须是轮起点，见 createdAt 注释）；
//   - TTFTMS 取本轮首帧耗时（buildLog 不设该列；ctx 里那份是连接级的）。
func (p *Proxy) buildRoundLog(r *round, groupID, accountID int64, reqModel, mappedModel string, format domain.RequestFormat, status int, et domain.ErrorType, u usageTuple) *domain.UsageLog {
	l := p.buildLog(r.requestID(), groupID, accountID, reqModel, mappedModel, format, status, et, u, r.StartedAt)
	l.CreatedAt = r.createdAt()
	l.TTFTMS = r.ttft
	return l
}

// settleRound 结算一个**已完成**轮（CompletedWith/WithoutUsage）：完成轮恒为
// 成功行（error_type=none / 200 / commit_state=response_started）——与既有成功
// 出口（`withCommit(..., CommitResponseStarted)`）同口径；完成帧本身就是交付
// 事实（§6.1：观测到终态帧即本轮完成，之后的链接故障不得回头改已结算的轮）。
//
// 轮边界成本（§9 非功能约束）：一次 buildRoundLog（栈上组装 + 一次 ctx 查表）+
// 一次 routeLog 入队 + 一次内存扣减。无逐帧开销——本函数只在终态帧上跑。
func (p *Proxy) settleRound(ctx context.Context, r *round, sel *scheduler.Selection, reqModel string, groupID int64) {
	l := withCommit(
		logWithCtx(ctx, p.buildRoundLog(r, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), domain.FormatOpenAIResponsesWS, 200, domain.ErrNone, r.usage)),
		CommitResponseStarted,
	)
	r.settle(p, sel, l)
}
