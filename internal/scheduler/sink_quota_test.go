// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/latch"
	"github.com/is7qin/c3api/internal/rule"
)

// drainQuotaQueue 消费真实 QuotaQueue：先读统计（无消费时 enqueued == 积压、
// dropped 必须为 0），再排空 channel 取回任务快照。
func drainQuotaQueue(t *testing.T, q *QuotaQueue) []QuotaTask {
	t.Helper()
	enqueued, dropped, queued := q.Stats()
	require.Zero(t, dropped, "断言路径不得有丢弃")
	require.Equal(t, int(enqueued), queued, "入队累计与当前积压一致（未消费）")
	var tasks []QuotaTask
	for {
		select {
		case task := <-q.ch:
			tasks = append(tasks, task)
		default:
			require.Len(t, tasks, queued)
			return tasks
		}
	}
}

// newQuotaQueueForSink 构造 sink 侧的队列替身（容量 8 足够四类断言）。
func newQuotaQueueForSink() *QuotaQueue { return NewQuotaQueue(8) }

// quotaSinkEvent 一条带完整健康键分量的事件的基座（account_route scope 用）。
func quotaSinkEvent() rule.Event {
	return rule.Event{
		AccountID:                1,
		ExpectedIdentityRevision: 5,
		CandidateFingerprint:     testIdentity,
		RouteClassID:             "r1",
		QualityClassID:           "q1",
	}
}

// TestLatchSinkEnqueuesQuotaOnUseResetWithoutResetAt 门禁 6/22（S3b-R3）：冷却
// 动作声明 use_reset、但事件没带绝对截止 ⇒ 既有写入成功之后**恰好**投递一次
// 补拉任务，字段全部来自已算出的健康键与判决状态。
func TestLatchSinkEnqueuesQuotaOnUseResetWithoutResetAt(t *testing.T) {
	h, _, _ := newTestHealthWithLatch(t)
	q := newQuotaQueueForSink()
	sink := NewLatchSink(h, latch.NewLatchStore(), latch.NewHub(), q)
	th := domain.ThrottleAction{
		Scope: domain.ThrottleScopeAccountRoute, Mode: domain.ThrottleModeRetryAfter,
		UseReset: true, DurationMs: new(int64(5000)),
	}

	require.NoError(t, sink.Throttle(quotaSinkEvent(), th))
	require.NoError(t, h.Sync(context.Background()))
	// 顺序钉：入队发生在写入成功之后 ⇒ 记录已可见（若是"先入队后写"，这里仍
	// 可能为 READY，但下面的入队断言照样成立——所以两条一起断才有意义）。
	require.Equal(t, StateRetryAfter, h.EffectiveState(1, "q1", testIdentity, 5))

	tasks := drainQuotaQueue(t, q)
	require.Len(t, tasks, 1, "use_reset + ResetAt==nil 必须恰好入队一次")
	require.Equal(t, QuotaTask{
		AccountID:        1,
		IdentityRevision: 5,
		Fingerprint:      testIdentity,
		Quality:          "q1",
		State:            StateRetryAfter,
	}, tasks[0], "任务字段必须等于 sink 已算出的健康键 + 判决状态")
}

// TestLatchSinkQuotaAccountScopeUsesWildcardQuality：account scope ⇒ Quality 用
// 通配 "*"（与写入的键同值），不是事件里的 QualityClassID。
func TestLatchSinkQuotaAccountScopeUsesWildcardQuality(t *testing.T) {
	h, _, _ := newTestHealthWithLatch(t)
	q := newQuotaQueueForSink()
	sink := NewLatchSink(h, latch.NewLatchStore(), latch.NewHub(), q)
	ev := quotaSinkEvent()
	th := domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeRetryAfter, UseReset: true}

	require.NoError(t, sink.Throttle(ev, th))
	tasks := drainQuotaQueue(t, q)
	require.Len(t, tasks, 1)
	require.Equal(t, "*", tasks[0].Quality)
}

// TestLatchSinkSkipsQuotaWhenResetAtPresent：事件已带绝对截止 ⇒ 已在
// ThrottleUntil 落定，无需补拉（重复投递只会白烧一次上游查询）。
func TestLatchSinkSkipsQuotaWhenResetAtPresent(t *testing.T) {
	h, _, _ := newTestHealthWithLatch(t)
	q := newQuotaQueueForSink()
	sink := NewLatchSink(h, latch.NewLatchStore(), latch.NewHub(), q)
	until := time.Now().Add(24 * time.Minute).Truncate(time.Second)
	ev := quotaSinkEvent()
	ev.ResetAt = &until
	th := domain.ThrottleAction{Scope: domain.ThrottleScopeAccountRoute, Mode: domain.ThrottleModeRetryAfter, UseReset: true}

	require.NoError(t, sink.Throttle(ev, th))
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateRetryAfter, h.EffectiveState(1, "q1", testIdentity, 5))
	require.Empty(t, drainQuotaQueue(t, q), "ResetAt 已带 ⇒ 不得入队")
}

// TestLatchSinkSkipsQuotaOnOpenMode：Mode=Open 的判决与额度窗口无关 ⇒ 不入队。
func TestLatchSinkSkipsQuotaOnOpenMode(t *testing.T) {
	h, _, _ := newTestHealthWithLatch(t)
	q := newQuotaQueueForSink()
	sink := NewLatchSink(h, latch.NewLatchStore(), latch.NewHub(), q)
	th := domain.ThrottleAction{
		Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen,
		UseReset: true, DurationMs: new(int64(5000)),
	}

	require.NoError(t, sink.Throttle(quotaSinkEvent(), th))
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateOPEN, h.EffectiveState(1, "*", testIdentity, 5))
	require.Empty(t, drainQuotaQueue(t, q), "Mode=Open ⇒ 不得入队")
}

// TestLatchSinkSkipsQuotaWithoutUseReset：时长由规则声明（use_reset=false）⇒ 不
// 入队：补拉回来也没有"窗口 reset"这个语义可补。
func TestLatchSinkSkipsQuotaWithoutUseReset(t *testing.T) {
	h, _, _ := newTestHealthWithLatch(t)
	q := newQuotaQueueForSink()
	sink := NewLatchSink(h, latch.NewLatchStore(), latch.NewHub(), q)
	th := domain.ThrottleAction{
		Scope: domain.ThrottleScopeAccountRoute, Mode: domain.ThrottleModeRetryAfter,
		UseReset: false, DurationMs: new(int64(5000)),
	}

	require.NoError(t, sink.Throttle(quotaSinkEvent(), th))
	require.NoError(t, h.Sync(context.Background()))
	require.Equal(t, StateRetryAfter, h.EffectiveState(1, "q1", testIdentity, 5))
	require.Empty(t, drainQuotaQueue(t, q), "!UseReset ⇒ 不得入队")
}

// TestLatchSinkSkipsQuotaWhenWriteFails：「写入成功之后」是硬条件：健康写失败
// ⇒ 返回错误且不投递（没有可补全的判决，补拉会白烧一次上游查询）。
func TestLatchSinkSkipsQuotaWhenWriteFails(t *testing.T) {
	// nil client 的 RuntimeHealth：ThrottleUntil 恒返回 "health: no redis"。
	h := NewRuntimeHealth(nil, "self-a", nil, nil)
	q := newQuotaQueueForSink()
	sink := NewLatchSink(h, latch.NewLatchStore(), latch.NewHub(), q)
	th := domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeRetryAfter, UseReset: true}

	require.Error(t, sink.Throttle(quotaSinkEvent(), th))
	require.Empty(t, drainQuotaQueue(t, q), "健康写失败 ⇒ 不得入队")
}
