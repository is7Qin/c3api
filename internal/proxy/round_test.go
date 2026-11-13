// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

// round 本体的单测（spec-commit-state-and-round-billing §6）：派生 id / 轮起点 /
// FSM 单向迁移 / settle 的资金幂等（G16d）/ 四条写路径同 ID 的结构证据（G16c）。

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

// TestRoundRequestIDAndCreatedAt 派生 id 与轮起点（§6.2）：k==1 → base（零变化）；
// k>=2 → base#rK；created_at 恒 = 轮起点（不是落库时刻）。
func TestRoundRequestIDAndCreatedAt(t *testing.T) {
	start := time.Now().Add(-90 * time.Second)
	rd := newRound("req-abc", start)

	require.Equal(t, "req-abc", rd.requestID(), "第 1 轮 = 连接级原值")
	require.Equal(t, start, rd.createdAt(), "created_at = 轮起点")

	rd.roll(start.Add(time.Second))
	require.Equal(t, "req-abc#r2", rd.requestID(), "第 2 轮加 #r2（同连接轮间不撞唯一键）")
	require.Equal(t, start.Add(time.Second), rd.createdAt(), "翻轮后起点前移")

	rd.roll(start.Add(2 * time.Second))
	require.Equal(t, "req-abc#r3", rd.requestID())
	require.Equal(t, start.Add(2*time.Second), rd.createdAt())
}

// TestRoundFSMTransitions §6.1：Open → Completed*（带/不带 usage）；Completed* 是
// 吸收态（第二次 completed 只进 InvalidDuplicate，不返回「刚完成」）。
func TestRoundFSMTransitions(t *testing.T) {
	p := &Proxy{} // observeCompleted 只在有 logger 时才打 Warn
	sel := &scheduler.Selection{}
	rd := newRound("req-fsm", time.Now())
	completed := roundWireCompleted("rsp_1", 3, 4)

	require.True(t, rd.observeCompleted(p, completed, sel), "首次终态帧 ⇒ 刚完成")
	require.Equal(t, RoundCompletedWithUsage, rd.state)
	require.Equal(t, int64(3), rd.usage.it)
	require.Equal(t, int64(4), rd.usage.ot)
	require.Equal(t, int64(7), rd.usage.tt)

	require.False(t, rd.observeCompleted(p, completed, sel), "同一轮第二次 ⇒ 不结算")
	require.Equal(t, RoundInvalidDuplicate, rd.state)
	require.False(t, rd.observeCompleted(p, completed, sel), "重复之后仍不结算（不再 Warn）")
	require.Equal(t, RoundInvalidDuplicate, rd.state)

	rd.roll(time.Now())
	require.True(t, rd.observeCompleted(p, roundWireCompletedNoUsage("rsp_2"), sel), "新一轮的无 usage 终态帧同样「刚完成」")
	require.Equal(t, RoundCompletedWithoutUsage, rd.state)
}

// TestRoundObserveCompletedWithoutUsageIsCompletion G13：completed 无 usage 仍是
// 完成（tokens=0，不伪造），且返回「刚完成」以触发结算。
func TestRoundObserveCompletedWithoutUsageIsCompletion(t *testing.T) {
	p := &Proxy{}
	sel := &scheduler.Selection{}
	rd := newRound("req-nu", time.Now())
	require.True(t, rd.observeCompleted(p, roundWireCompletedNoUsage("rsp_1"), sel))
	require.Equal(t, RoundCompletedWithoutUsage, rd.state)
	require.True(t, rd.completed())
	require.Zero(t, rd.usage.it)
	require.Zero(t, rd.usage.ot)
}

// TestRoundG16cSameIDOnAllWritePaths G16c 的结构面：四条写路径（轮结算 /
// recordRejected / failover 耗尽 / 连接收尾）都不许自建 request_id——
// 前两条在 relay 之前，恒属第 1 轮（round.requestID() == reqID、round.createdAt()
// == start，逐字相同）；后两条走 buildRoundLog，id 只能来自当前轮。
func TestRoundG16cSameIDOnAllWritePaths(t *testing.T) {
	reqID := newReqID()
	start := time.Now()
	rd := newRound(reqID, start)

	// relay 之前的两条路径（recordRejected / failover 耗尽）收的是 reqID+start，
	// 它们与第 1 轮的轮身份必须逐字一致——否则两表立刻分叉。
	require.Equal(t, reqID, rd.requestID(), "recordRejected/failover 用的 reqID = 第 1 轮 id")
	require.Equal(t, start, rd.createdAt(), "created_at 同为轮起点")

	p, _, _, _, _ := roundBillingProxy(t, "")
	// 后两条路径（轮结算 / 连接收尾）一律经 buildRoundLog：RequestID/CreatedAt
	// 的唯一来源是 round 本身。
	l := p.buildRoundLog(rd, 10, 1, "gpt-4o", "", domain.FormatOpenAIResponsesWS, 200, domain.ErrNone, rd.usage)
	require.Equal(t, rd.requestID(), l.RequestID, "轮结算行的 id = 轮派生 id")
	require.Equal(t, rd.createdAt(), l.CreatedAt, "轮结算行的 created_at = 轮起点")
	require.Less(t, l.LatencyMS, int64(5_000), "LatencyMS = settle_at − 轮起点（不是落库时刻差）")

	rd.roll(start.Add(time.Minute))
	l2 := p.buildRoundLog(rd, 10, 1, "gpt-4o", "", domain.FormatOpenAIResponsesWS, 200, domain.ErrNone, rd.usage)
	require.Equal(t, reqID+"#r2", l2.RequestID, "第 2 轮的写行点自动跟着轮号走")
	require.Equal(t, rd.createdAt(), l2.CreatedAt)
}

// TestRoundG16dSettleTwiceIsIdempotent G16d：同一轮 settle 两次 ⇒ usage_logs 1 行、
// DeductQuota 的非零 delta 只发生一次、Recorder.quotaUsed 只累加一次、余额只减一次。
// 观测对象是**真实副作用**（回写面 + 额度计数器），不是只数 DB 行。
func TestRoundG16dSettleTwiceIsIdempotent(t *testing.T) {
	p, usageRows, errRows, qw, quotaKey := roundBillingProxy(t, "")
	sel, err := p.sched.Select(10, domain.FormatOpenAIResponsesWS, "gpt-4o")
	require.NoError(t, err)
	t.Cleanup(sel.Release)

	rm := &reqMeta{meta: quotaKey}
	ctx := context.WithValue(context.Background(), ctxKeyReqMeta{}, rm)

	rd := newRound(newReqID(), time.Now())
	l := withCommit(logWithCtx(ctx, p.buildRoundLog(rd, 10, sel.AccountID, "gpt-4o", "", domain.FormatOpenAIResponsesWS, 200, domain.ErrNone, usageTuple{it: 2, ot: 2, tt: 4})), CommitResponseStarted)

	rd.settle(p, sel, l)
	require.True(t, rd.billingSettled, "首次结算置位")
	cost := l.Cost
	require.Positive(t, cost, "价目命中（成本非零才可能观测到重复扣费）")
	rd.settle(p, sel, l) // 第二次：必须整段短路
	require.True(t, rd.billingSettled)

	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))
	usageRows.mu.Lock()
	used := append([]*domain.UsageLog(nil), usageRows.logs...)
	usageRows.mu.Unlock()
	errRows.mu.Lock()
	failed := append([]*domain.UsageLog(nil), errRows.logs...)
	errRows.mu.Unlock()

	require.Len(t, used, 1, "同一轮只一行 usage_logs")
	require.Empty(t, failed)
	require.Equal(t, cost, roundQuotaSum(qw), "quota_used 只累加一次（delta 只非零一次）")

	// 余额只减一次：额度计数器的消耗额恒为单次成本。
	require.Equal(t, cost, quotaConsumedOf(t, p, quotaKey.KeyID))
}

// quotaConsumedOf 读取额度计数器的已消耗计费毫分（余额侧的本地计数）。
func quotaConsumedOf(t *testing.T, p *Proxy, keyID int64) int64 {
	t.Helper()
	snap := p.auth.gate.store.Load()
	q, ok := snap.quotas[keyID]
	require.True(t, ok, "额度 key 必须有本地计数器（HasQuota=true）")
	return q.consumed.Load()
}
