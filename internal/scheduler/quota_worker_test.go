// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 429 额度补拉 worker 的门禁用例（设计 §8.3 门禁 6/14/21/28，R3 判定的新形状）。
// 全部替身：不建 scheduler、不连 Redis、不打网络——health writer / registry /
// resolver / lookup 四个窄接口就是为此存在的。队列独立为 QuotaQueue，测试按
// 「先建队列再建 worker」组装。
package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/latch"
)

// fakeQuotaResolver 可编程补拉替身：调用计数 + 最近一次入参 + 行为注入。
type fakeQuotaResolver struct {
	mu    sync.Mutex
	calls int
	last  *domain.Account
	fn    func(context.Context, *domain.Account) (time.Time, bool, error)
}

func (f *fakeQuotaResolver) ResolveThrottleDeadline(ctx context.Context, acct *domain.Account) (time.Time, bool, error) {
	f.mu.Lock()
	f.calls++
	f.last = acct
	fn := f.fn
	f.mu.Unlock()
	if fn == nil {
		return time.Time{}, false, nil
	}
	return fn(ctx, acct)
}

func (f *fakeQuotaResolver) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeQuotaRegistry 注册表替身（同一份 map + 读计数的互斥：worker 在 goroutine 里查）。
type fakeQuotaRegistry struct {
	mu           sync.Mutex
	byType       map[credential.Type]QuotaResolver
	resolveCalls int
}

func newFakeQuotaRegistry(byType map[credential.Type]QuotaResolver) *fakeQuotaRegistry {
	return &fakeQuotaRegistry{byType: byType}
}

func (r *fakeQuotaRegistry) Resolve(t credential.Type) (QuotaResolver, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolveCalls++
	res, ok := r.byType[t]
	return res, ok
}

func (r *fakeQuotaRegistry) Register(t credential.Type, res QuotaResolver) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byType[t] = res
}

func (r *fakeQuotaRegistry) resolveCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.resolveCalls
}

// quotaWrite 一次 ExtendCooldown 的可比较快照。
type quotaWrite struct {
	key   HealthKey
	state HealthState
	until time.Time
}

// fakeQuotaHealth 健康写入替身：记录每一次 ExtendCooldown 的全部实参。
type fakeQuotaHealth struct {
	mu     sync.Mutex
	writes []quotaWrite
	err    error
}

func (h *fakeQuotaHealth) ExtendCooldown(_ context.Context, key HealthKey, state HealthState, until time.Time) (int64, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.err != nil {
		return 0, h.err
	}
	h.writes = append(h.writes, quotaWrite{key: key, state: state, until: until})
	return 1, nil
}

func (h *fakeQuotaHealth) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.writes)
}

func (h *fakeQuotaHealth) snapshot() []quotaWrite {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]quotaWrite(nil), h.writes...)
}

// quotaLookupOf 固定账号的快照解析替身（生产装配点：sched.ProbeAccount）。
func quotaLookupOf(accts ...*domain.Account) func(int64) (*domain.Account, bool) {
	m := make(map[int64]*domain.Account, len(accts))
	for _, a := range accts {
		m[a.ID] = a
	}
	return func(id int64) (*domain.Account, bool) {
		a, ok := m[id]
		return a, ok
	}
}

// quotaCodexAccount 构造一个 codex 族账号快照（只填补拉需要的三个字段）。
func quotaCodexAccount(id, k int64) *domain.Account {
	return &domain.Account{
		ID:               id,
		IdentityRevision: k,
		Template:         &domain.Template{ID: 1, CredentialType: credential.TypeCodexOAuth},
	}
}

// startQuotaWorker 以可取消 ctx 启动 worker，并在用例结束时关停（closeOnce
// 使用例内显式 Close 之后的清理成为 no-op）。
func startQuotaWorker(t *testing.T, w *QuotaWorker) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, w.Start(ctx))
	t.Cleanup(func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer ccancel()
		require.NoError(t, w.Close(cctx))
		cancel()
	})
}

// waitQuotaWorker 轮询等待异步补拉的结果（worker 没有逐任务回调面，观测点就是
// 计数 + 替身快照）。超时直接判红，不静默放过。
func waitQuotaWorker(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

// TestQuotaWorkerExtendsDeadline 门禁 6/14：有可信截止 ⇒ ExtendCooldown 恰好
// 一次，key 的四个分量与 state 与任务逐字段一致，until 原样落写入。
//
// 这条同时钉死 R3 第 1 条：补拉**不产生第二条 rule 事件**——worker 手上压根没有
// MarkResult 这样的通道（结构上不可达，比断言"没调用"更强）。
func TestQuotaWorkerExtendsDeadline(t *testing.T) {
	until := time.Now().Add(7 * 24 * time.Hour).Truncate(time.Second)
	resolver := &fakeQuotaResolver{fn: func(context.Context, *domain.Account) (time.Time, bool, error) {
		return until, true, nil
	}}
	reg := newFakeQuotaRegistry(map[credential.Type]QuotaResolver{credential.TypeCodexOAuth: resolver})
	health := &fakeQuotaHealth{}
	q := NewQuotaQueue(0)
	w := NewQuotaWorker(q, health, reg, quotaLookupOf(quotaCodexAccount(7, 3)), nil, QuotaWorkerConfig{})
	startQuotaWorker(t, w)

	q.Enqueue(QuotaTask{
		AccountID: 7, IdentityRevision: 3,
		Fingerprint: "fp-abc", Quality: "q1", State: StateRetryAfter,
	})
	waitQuotaWorker(t, "ExtendCooldown 收到补全调用", func() bool { return health.count() == 1 })

	writes := health.snapshot()
	require.Equal(t, HealthKey{AccountID: 7, Quality: "q1", Identity: "fp-abc", IdentityRevision: 3}, writes[0].key,
		"健康键必须与任务的四个分量逐字段一致")
	require.Equal(t, StateRetryAfter, writes[0].state, "状态必须用判决时的 state（补拉不改判决）")
	require.True(t, writes[0].until.Equal(until), "写入的截止必须等于 resolver 给出的绝对 until：%v != %v", writes[0].until, until)
	require.Equal(t, 1, resolver.callCount())
	require.Equal(t, int64(1), w.Extended())
	require.Zero(t, w.Rejected(), "成功的补拉不得计入拒绝")
	require.Zero(t, w.Failed(), "成功的补拉不得计入失败")
	require.Zero(t, w.Empty())
}

// TestQuotaWorkerRejectsWhenRecoveryClearedRecord（B1 端到端，放行条件）：
// sink 入队一条 use_reset/ResetAt==nil 的 429 补拉 → 运维恢复（ClearAccount +
// SetProbing，等同 RecoverAccount）→ worker 才消费到这条迟到任务。补拉必须被
// ExtendCooldown 的闸门拒绝：PROBING 不被改写、记录不被重建、extended 不涨。
func TestQuotaWorkerRejectsWhenRecoveryClearedRecord(t *testing.T) {
	h, _, mr := newTestHealthWithLatch(t)
	ctx := context.Background()
	q := newQuotaQueueForSink()
	sink := NewLatchSink(h, latch.NewLatchStore(), latch.NewHub(), q)

	th := domain.ThrottleAction{
		Scope: domain.ThrottleScopeAccountRoute, Mode: domain.ThrottleModeRetryAfter, UseReset: true,
	}
	require.NoError(t, sink.Throttle(quotaSinkEvent(), th), "入队一条迟到补拉")
	require.NoError(t, h.Sync(ctx))
	require.Equal(t, StateRetryAfter, h.EffectiveState(1, "q1", testIdentity, 5))

	cleared, err := h.ClearAccount(ctx, 1)
	require.NoError(t, err)
	require.Equal(t, int64(1), cleared, "恢复必须清掉那条判决记录")
	require.NoError(t, h.SetProbing(ctx, 1, 5)) // 等同 RecoverAccount 的 PROBING 写

	until := time.Now().Add(7 * 24 * time.Hour).Truncate(time.Second)
	resolver := &fakeQuotaResolver{fn: func(context.Context, *domain.Account) (time.Time, bool, error) {
		return until, true, nil
	}}
	reg := newFakeQuotaRegistry(map[credential.Type]QuotaResolver{credential.TypeCodexOAuth: resolver})
	w := NewQuotaWorker(q, h, reg, quotaLookupOf(quotaCodexAccount(1, 5)), nil, QuotaWorkerConfig{})
	startQuotaWorker(t, w)

	waitQuotaWorker(t, "补拉被拒绝", func() bool { return w.Rejected() == 1 })
	require.Zero(t, w.Extended(), "被拒绝的补拉不得计入 extended")
	require.Zero(t, w.Failed(), "拒绝不是错误")
	require.Equal(t, 1, resolver.callCount())

	require.NoError(t, h.Sync(ctx))
	require.Equal(t, StateProbing, h.EffectiveState(1, "q1", testIdentity, 5),
		"运维恢复写的 PROBING 不得被迟到的补拉改写")
	taskField := HealthKey{AccountID: 1, Quality: "q1", Identity: testIdentity, IdentityRevision: 5}.String()
	require.False(t, mr.Exists(healthRecordPrefix+taskField), "迟到的补拉不得重建已被清除的记录")
	probingField := HealthKey{AccountID: 1, Quality: identityAny, Identity: identityAny, IdentityRevision: 5}.String()
	require.True(t, mr.Exists(healthRecordPrefix+probingField), "PROBING 记录必须仍在")
}

// TestQuotaWorkerSkipsWithoutWindow 门禁 6/21：补拉成功但无有效窗口 ⇒ 不写
// （健康 TTL 不得因此变长），只计 empty。
func TestQuotaWorkerSkipsWithoutWindow(t *testing.T) {
	resolver := &fakeQuotaResolver{fn: func(context.Context, *domain.Account) (time.Time, bool, error) {
		return time.Time{}, false, nil
	}}
	reg := newFakeQuotaRegistry(map[credential.Type]QuotaResolver{credential.TypeCodexOAuth: resolver})
	health := &fakeQuotaHealth{}
	q := NewQuotaQueue(0)
	w := NewQuotaWorker(q, health, reg, quotaLookupOf(quotaCodexAccount(9, 1)), nil, QuotaWorkerConfig{})
	startQuotaWorker(t, w)

	q.Enqueue(QuotaTask{AccountID: 9, IdentityRevision: 1, Fingerprint: "fp", Quality: "*", State: StateRetryAfter})
	waitQuotaWorker(t, "empty 计数", func() bool { return w.Empty() == 1 })

	require.Zero(t, health.count(), "无有效窗口不得触碰健康")
	require.Equal(t, 1, resolver.callCount())
	require.Zero(t, w.Extended())
}

// TestQuotaWorkerSkipsOnError 门禁 6：补拉失败 ⇒ 不写（冷却时长保持既有判决的
// 结论），只计 failed。
func TestQuotaWorkerSkipsOnError(t *testing.T) {
	resolver := &fakeQuotaResolver{fn: func(context.Context, *domain.Account) (time.Time, bool, error) {
		return time.Time{}, false, errors.New("upstream exploded")
	}}
	reg := newFakeQuotaRegistry(map[credential.Type]QuotaResolver{credential.TypeCodexOAuth: resolver})
	health := &fakeQuotaHealth{}
	q := NewQuotaQueue(0)
	w := NewQuotaWorker(q, health, reg, quotaLookupOf(quotaCodexAccount(9, 1)), nil, QuotaWorkerConfig{})
	startQuotaWorker(t, w)

	q.Enqueue(QuotaTask{AccountID: 9, IdentityRevision: 1, Fingerprint: "fp", Quality: "*", State: StateRetryAfter})
	waitQuotaWorker(t, "failed 计数", func() bool { return w.Failed() == 1 })

	require.Zero(t, health.count(), "失败的补拉不得改动健康")
	require.Zero(t, w.Empty())
}

// TestQuotaWorkerDropsStaleGeneration 门禁（代际绑定）：账号当前的 K 与任务的 K
// 不一致 ⇒ 既不调 resolver 也不写（旧代际的额度结论不得作用到新身份上）。
func TestQuotaWorkerDropsStaleGeneration(t *testing.T) {
	resolver := &fakeQuotaResolver{fn: func(context.Context, *domain.Account) (time.Time, bool, error) {
		return time.Now().Add(time.Hour), true, nil
	}}
	reg := newFakeQuotaRegistry(map[credential.Type]QuotaResolver{credential.TypeCodexOAuth: resolver})
	health := &fakeQuotaHealth{}
	// 账号快照已经是 K=5，任务来自 K=4。
	q := NewQuotaQueue(0)
	w := NewQuotaWorker(q, health, reg, quotaLookupOf(quotaCodexAccount(9, 5)), nil, QuotaWorkerConfig{})
	startQuotaWorker(t, w)

	q.Enqueue(QuotaTask{AccountID: 9, IdentityRevision: 4, Fingerprint: "fp", Quality: "*", State: StateRetryAfter})
	waitQuotaWorker(t, "stale 计数", func() bool { return w.Stale() == 1 })

	// 账号整行不可解析（已删/未加载）走同一条 stale 出口。
	q.Enqueue(QuotaTask{AccountID: 42, IdentityRevision: 1, Fingerprint: "fp", Quality: "*", State: StateRetryAfter})
	waitQuotaWorker(t, "账号缺失计 stale", func() bool { return w.Stale() == 2 })

	require.Zero(t, health.count(), "代际不符不得写健康")
	require.Zero(t, resolver.callCount(), "代际不符不得发起补拉")
	require.Zero(t, reg.resolveCount(), "代际校验先于 resolver 查找")
}

// TestQuotaWorkerSkipsUnsupportedCredential：凭据类型没有注册 resolver ⇒ 不写、
// 不 panic，只计 unsupported（api_key 族没有额度面）。
func TestQuotaWorkerSkipsUnsupportedCredential(t *testing.T) {
	resolver := &fakeQuotaResolver{}
	reg := newFakeQuotaRegistry(map[credential.Type]QuotaResolver{credential.TypeCodexOAuth: resolver})
	health := &fakeQuotaHealth{}
	acct := quotaCodexAccount(9, 1)
	acct.Template = &domain.Template{ID: 2, CredentialType: credential.TypeAPIKey}
	q := NewQuotaQueue(0)
	w := NewQuotaWorker(q, health, reg, quotaLookupOf(acct), nil, QuotaWorkerConfig{})
	startQuotaWorker(t, w)

	q.Enqueue(QuotaTask{AccountID: 9, IdentityRevision: 1, Fingerprint: "fp", Quality: "*", State: StateRetryAfter})
	waitQuotaWorker(t, "unsupported 计数", func() bool { return w.Unsupported() == 1 })

	require.Zero(t, health.count(), "未注册凭据族不得写健康")
	require.Zero(t, resolver.callCount())
}

// TestQuotaWorkerDropsWhenFull 门禁 28：队列满 ⇒ Enqueue 在硬时限内返回且计
// dropped（绝不阻塞请求热路径）。不 Start：队列不会被消费，语义确定。
func TestQuotaWorkerDropsWhenFull(t *testing.T) {
	health := &fakeQuotaHealth{}
	q := NewQuotaQueue(1)
	w := NewQuotaWorker(q, health, newFakeQuotaRegistry(nil), quotaLookupOf(quotaCodexAccount(1, 1)), nil,
		QuotaWorkerConfig{})
	task := QuotaTask{AccountID: 1, IdentityRevision: 1, Fingerprint: "fp", Quality: "*", State: StateRetryAfter}
	q.Enqueue(task) // 占满容量 1

	start := time.Now()
	q.Enqueue(task) // 满 → 丢弃
	elapsed := time.Since(start)
	require.Less(t, elapsed, 100*time.Millisecond, "队列满时 Enqueue 必须立即返回（实测 %v）", elapsed)
	require.Equal(t, int64(1), w.Dropped())
	require.Equal(t, int64(1), w.EnqueuedCount())
	require.Equal(t, 1, w.Queued())

	require.NoError(t, w.Close(context.Background()))
	require.Equal(t, int64(2), w.Dropped(), "关闭后队列剩余计入 dropped")
}

// TestQuotaWorkerStartAfterCloseFails：Close 先于 Start 之后不允许再启动
// （终态语义：关过的 worker 不允许重新启动）。
func TestQuotaWorkerStartAfterCloseFails(t *testing.T) {
	q := NewQuotaQueue(0)
	w := NewQuotaWorker(q, &fakeQuotaHealth{}, newFakeQuotaRegistry(nil), quotaLookupOf(quotaCodexAccount(1, 1)), nil,
		QuotaWorkerConfig{})
	require.NoError(t, w.Close(context.Background()))
	require.Error(t, w.Start(context.Background()))
	// 关过的 worker 二次 Start 仍是错误（不是"已启动"）。
	require.Error(t, w.Start(context.Background()))
}

// TestQuotaWorkerCloseCancelsInflight 门禁 28：Close 取消在途任务（其 ctx 立即
// 到期），并在有界预算内返回——不死等在途上游。
func TestQuotaWorkerCloseCancelsInflight(t *testing.T) {
	entered := make(chan struct{}, 1)
	resolver := &fakeQuotaResolver{fn: func(ctx context.Context, _ *domain.Account) (time.Time, bool, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-ctx.Done() // 在途任务只能靠 Close 的取消退出
		return time.Time{}, false, ctx.Err()
	}}
	reg := newFakeQuotaRegistry(map[credential.Type]QuotaResolver{credential.TypeCodexOAuth: resolver})
	health := &fakeQuotaHealth{}
	q := NewQuotaQueue(0)
	w := NewQuotaWorker(q, health, reg, quotaLookupOf(quotaCodexAccount(1, 1)), nil,
		QuotaWorkerConfig{Timeout: time.Minute})
	startQuotaWorker(t, w)

	q.Enqueue(QuotaTask{AccountID: 1, IdentityRevision: 1, Fingerprint: "fp", Quality: "*", State: StateRetryAfter})
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("在途任务未开始")
	}

	cctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	require.NoError(t, w.Close(cctx))
	require.NoError(t, cctx.Err(), "Close 必须在预算内返回（不死等在途上游）")
	require.Equal(t, int64(1), w.Failed(), "被取消的在途任务按 failed 记账")
	require.Zero(t, health.count(), "取消的在途任务不得写健康")
}

// TestQuotaWorkerCloseDrainsQueue 门禁 28：停止取新任务后，队列里没被消费的
// 任务计入 dropped；关闭后队列必须排空。
//
// 不 Start：worker 没有消费者，排空计数可判定（与"Close 取消在途"分开测——
// 那条用例里循环可能在被取消后再消费一条，dropped/failed 的分配不确定）。
func TestQuotaWorkerCloseDrainsQueue(t *testing.T) {
	health := &fakeQuotaHealth{}
	q := NewQuotaQueue(0)
	w := NewQuotaWorker(q, health, newFakeQuotaRegistry(nil), quotaLookupOf(quotaCodexAccount(2, 1)), nil,
		QuotaWorkerConfig{})
	task := QuotaTask{AccountID: 2, IdentityRevision: 1, Fingerprint: "fp", Quality: "*", State: StateRetryAfter}
	q.Enqueue(task)
	q.Enqueue(task)
	require.Equal(t, 2, w.Queued())

	require.NoError(t, w.Close(context.Background()))
	require.Zero(t, w.Queued(), "关闭后队列必须排空")
	require.Equal(t, int64(2), w.Dropped(), "队列剩余 = 两条未消费任务")
	require.Zero(t, health.count())
}

// TestQuotaWorkerCloseRejectsEnqueue 终态语义（原挂在 worker 上的那条断言，
// 迁到队列）：worker 关闭后队列随之关闭，外部 queue.Enqueue 一律计 dropped
// 且不入队——没有消费者的 channel 不该再积压"永远不会被补拉"的任务。
func TestQuotaWorkerCloseRejectsEnqueue(t *testing.T) {
	health := &fakeQuotaHealth{}
	q := NewQuotaQueue(0)
	w := NewQuotaWorker(q, health, newFakeQuotaRegistry(nil), quotaLookupOf(quotaCodexAccount(3, 1)), nil,
		QuotaWorkerConfig{})
	require.NoError(t, w.Close(context.Background()))

	_, droppedBefore, _ := q.Stats()
	require.NoError(t, w.Close(context.Background()), "Close 必须幂等")
	_, droppedAfterDouble, _ := q.Stats()
	require.Equal(t, droppedBefore, droppedAfterDouble, "二次 Close 不得重复计数")

	q.Enqueue(QuotaTask{AccountID: 3, IdentityRevision: 1, Fingerprint: "fp", Quality: "*", State: StateRetryAfter})
	_, dropped, queued := q.Stats()
	require.Equal(t, droppedBefore+1, dropped, "关闭后 Enqueue ⇒ dropped+1")
	require.Zero(t, queued, "关闭后 Enqueue 不得入队")
	require.Equal(t, dropped, w.Dropped(), "观测面（worker.Dropped）取自队列，口径一致")
	require.Zero(t, health.count())
}

// TestQuotaWorkerTimeoutKeepsLoopAlive 门禁 28：单任务超时只影响该任务（计
// failed），循环继续处理下一个任务并正常写健康。
func TestQuotaWorkerTimeoutKeepsLoopAlive(t *testing.T) {
	resolver := &fakeQuotaResolver{fn: func(ctx context.Context, acct *domain.Account) (time.Time, bool, error) {
		if acct.ID == 1 {
			<-ctx.Done() // 永不返回的补拉：只能被单任务超时打断
			return time.Time{}, false, ctx.Err()
		}
		return time.Now().Add(time.Hour), true, nil
	}}
	reg := newFakeQuotaRegistry(map[credential.Type]QuotaResolver{credential.TypeCodexOAuth: resolver})
	health := &fakeQuotaHealth{}
	q := NewQuotaQueue(0)
	w := NewQuotaWorker(q, health, reg, quotaLookupOf(quotaCodexAccount(1, 1), quotaCodexAccount(2, 1)), nil,
		QuotaWorkerConfig{Timeout: 30 * time.Millisecond})
	startQuotaWorker(t, w)

	q.Enqueue(QuotaTask{AccountID: 1, IdentityRevision: 1, Fingerprint: "fp", Quality: "*", State: StateRetryAfter})
	waitQuotaWorker(t, "超时计 failed", func() bool { return w.Failed() == 1 })

	q.Enqueue(QuotaTask{AccountID: 2, IdentityRevision: 1, Fingerprint: "fp", Quality: "*", State: StateRetryAfter})
	waitQuotaWorker(t, "下一个任务被处理", func() bool { return w.Extended() == 1 })

	writes := health.snapshot()
	require.Len(t, writes, 1)
	require.Equal(t, int64(2), writes[0].key.AccountID, "超时的任务不得写健康")
}

// TestQuotaResolversRegistry 注册表实现的两条语义：未注册 ⇒ false；重复注册
// 覆盖（装配期确定性）；nil resolver 视同未注册。
func TestQuotaResolversRegistry(t *testing.T) {
	reg := NewQuotaResolvers()
	_, ok := reg.Resolve(credential.TypeCodexOAuth)
	require.False(t, ok, "空注册表必须返回 false")

	first := &fakeQuotaResolver{}
	second := &fakeQuotaResolver{}
	reg.Register(credential.TypeCodexOAuth, first)
	got, ok := reg.Resolve(credential.TypeCodexOAuth)
	require.True(t, ok)
	require.Same(t, first, got)

	reg.Register(credential.TypeCodexOAuth, second)
	got, ok = reg.Resolve(credential.TypeCodexOAuth)
	require.True(t, ok)
	require.Same(t, second, got, "重复注册覆盖前值（装配期确定性）")

	reg.Register(credential.TypeCodexPAT, nil)
	_, ok = reg.Resolve(credential.TypeCodexPAT)
	require.False(t, ok, "nil resolver 等价未注册")
}

// TestQuotaWorkerNilCollaboratorsFailClosed：协作方缺失（装配缺陷）不 panic、
// 不写健康，按 failed 计数（循环必须活着，让 /ops/workers 看得见）。
func TestQuotaWorkerNilCollaboratorsFailClosed(t *testing.T) {
	q := NewQuotaQueue(0)
	w := NewQuotaWorker(q, nil, newFakeQuotaRegistry(nil), quotaLookupOf(quotaCodexAccount(1, 1)), nil, QuotaWorkerConfig{})
	startQuotaWorker(t, w)

	q.Enqueue(QuotaTask{AccountID: 1, IdentityRevision: 1, Fingerprint: "fp", Quality: "*", State: StateRetryAfter})
	waitQuotaWorker(t, "缺协作方计 failed", func() bool { return w.Failed() == 1 })
	require.Zero(t, w.Extended())
}
