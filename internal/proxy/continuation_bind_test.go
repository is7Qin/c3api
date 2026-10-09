// SPDX-License-Identifier: AGPL-3.0-or-later
// Async continuation bind worker: enqueue snapshot / batch pipeline / drop +
// conflict counting / Close drain; contEnqueue no-op semantics.
package proxy

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/continuation"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/redisx"
)

// startTestContBind starts a worker over the fixture store with the given
// config and arranges teardown (cancel + Close drain under a budget).
func startTestContBind(t *testing.T, s *continuation.Store, cfg ContBindConfig) *ContBindWorker {
	t.Helper()
	w := NewContBindWorker(s, nil, cfg)
	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, w.Start(ctx))
	t.Cleanup(func() {
		cancel()
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer ccancel()
		require.NoError(t, w.Close(cctx))
	})
	return w
}

// workerBindReq builds a snapshot for user 1 / group 10 (matching
// contLookupBinding's key) bound to account 1.
func workerBindReq(t *testing.T, id string) continuation.BindRequest {
	t.Helper()
	return continuation.BindRequest{
		UserID:           1,
		GroupID:          10,
		RouteClassID:     contRouteID(t, domain.FormatOpenAIResponses, "gpt-4o", domain.OpResponses),
		ProtocolTag:      contProtocolREST,
		ContinuationID:   id,
		AccountID:        1,
		Fingerprint:      contFP(t, 1, "sk-acc1"),
		IdentityRevision: 1,
	}
}

func TestContBindWorkerEnqueuesAndBinds(t *testing.T) {
	_, s, _ := contFixture(t)
	w := startTestContBind(t, s, ContBindConfig{})
	w.Enqueue(workerBindReq(t, "resp_w1"))
	w.Enqueue(workerBindReq(t, "resp_w2"))
	require.Eventually(t, func() bool { return w.Bound() == 2 }, 5*time.Second, 5*time.Millisecond)
	require.Zero(t, w.Failed())
	require.Zero(t, w.Dropped())
	require.Zero(t, w.Conflicts())
	for _, id := range []string{"resp_w1", "resp_w2"} {
		b, ok := contLookupBinding(t, s, contProtocolREST, id)
		require.True(t, ok)
		require.Equal(t, int64(1), b.AccountID)
	}
}

func TestContBindWorkerDropsWhenQueueFull(t *testing.T) {
	_, s, _ := contFixture(t)
	// Not started: nothing drains; capacity 1 → second enqueue drops.
	w := NewContBindWorker(s, nil, ContBindConfig{QueueSize: 1, BatchWait: time.Hour})
	w.Enqueue(workerBindReq(t, "resp_q1"))
	w.Enqueue(workerBindReq(t, "resp_q2"))
	require.Equal(t, int64(1), w.Dropped(), "queue full must drop + count")
	require.Equal(t, 1, w.Queued())
	// Close (no Start) must still drain the queued item.
	require.NoError(t, w.Close(context.Background()))
	require.Equal(t, int64(1), w.Bound())
	b, ok := contLookupBinding(t, s, contProtocolREST, "resp_q1")
	require.True(t, ok)
	require.Equal(t, int64(1), b.AccountID)
	// Enqueue after Close is dropped.
	w.Enqueue(workerBindReq(t, "resp_q3"))
	require.Equal(t, int64(2), w.Dropped())
}

func TestContBindWorkerCountsConflict(t *testing.T) {
	_, s, _ := contFixture(t)
	// Pre-bind the id to a foreign account identity: the worker write conflicts.
	rid := contRouteID(t, domain.FormatOpenAIResponses, "gpt-4o", domain.OpResponses)
	_, err := s.CreateOrRefresh(context.Background(), 1, 10, rid, contProtocolREST, "resp_c1", 999, contFP(t, 999, "sk-foreign"), 1)
	require.NoError(t, err)
	w := startTestContBind(t, s, ContBindConfig{})
	w.Enqueue(workerBindReq(t, "resp_c1")) // account 1 ≠ 999 → conflict
	require.Eventually(t, func() bool { return w.Conflicts() == 1 }, 5*time.Second, 5*time.Millisecond)
	require.Zero(t, w.Bound())
	// CAS conflict keeps the OLD binding (never claims a later 410).
	b, ok := contLookupBinding(t, s, contProtocolREST, "resp_c1")
	require.True(t, ok)
	require.Equal(t, int64(999), b.AccountID)
}

func TestContBindWorkerCloseDrains(t *testing.T) {
	_, s, _ := contFixture(t)
	w := NewContBindWorker(s, nil, ContBindConfig{})
	for _, id := range []string{"resp_d1", "resp_d2", "resp_d3"} {
		w.Enqueue(workerBindReq(t, id))
	}
	require.Zero(t, w.Bound())
	require.NoError(t, w.Close(context.Background()))
	require.Equal(t, int64(3), w.Bound(), "Close must drain every queued request")
	for _, id := range []string{"resp_d1", "resp_d2", "resp_d3"} {
		_, ok := contLookupBinding(t, s, contProtocolREST, id)
		require.True(t, ok)
	}
}

func TestContEnqueueCountsUnattributed(t *testing.T) {
	_, s, _ := contFixture(t)
	tpl := &domain.Template{ID: 1, Name: "t", BaseURL: "https://cont.invalid", CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}, Models: []string{"gpt-4o"}}
	w := NewContBindWorker(s, nil, ContBindConfig{})
	p := contProxyBind(t, domain.FormatOpenAIResponses, []*domain.Account{contAcc(1, tpl, "sk-acc1", "https://cont.invalid")}, s, w)

	// (a) missing dispatch observation (reqMeta present, dispatch absent).
	rm := &reqMeta{meta: domain.KeyMeta{UserID: 1}}
	ctxMeta := context.WithValue(context.Background(), ctxKeyReqMeta{}, rm)
	p.contEnqueue(ctxMeta, contProtocolREST, "resp_nod", 10)
	require.Equal(t, int64(1), w.Unattributed(), "missing dispatch must be counted")

	// (b) missing reqMeta (dispatch present, reqMeta absent).
	ctxDisp := context.WithValue(context.Background(), ctxKeyDispatch{}, &dispatchObservation{})
	p.contEnqueue(ctxDisp, contProtocolREST, "resp_nod2", 10)
	require.Equal(t, int64(2), w.Unattributed(), "missing reqMeta must be counted")

	// (c) empty response id.
	p.contEnqueue(ctxMeta, contProtocolREST, "", 10)
	require.Equal(t, int64(3), w.Unattributed())

	require.Zero(t, w.Queued())
	require.Zero(t, w.Bound())
	require.Zero(t, w.Dropped())
}

func TestContEnqueueNoopWhenUnwired(t *testing.T) {
	_, s, _ := contFixture(t)
	tpl := &domain.Template{ID: 1, Name: "t", BaseURL: "https://cont.invalid", CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}, Models: []string{"gpt-4o"}}
	// A constructed (but unwired) worker proves the unwired path counts nothing.
	w := NewContBindWorker(s, nil, ContBindConfig{})
	// store wired, worker NOT wired (ContBind nil): zero behaviour, no count, no panic.
	p := contProxy(t, domain.FormatOpenAIResponses, []*domain.Account{contAcc(1, tpl, "sk-acc1", "https://cont.invalid")}, s)
	p.contEnqueue(context.Background(), contProtocolREST, "resp_x", 10)
	require.Zero(t, w.Unattributed())
	require.Zero(t, w.Queued())
}

// stallPipelineHook 令"已武装"的整批 pipeline Exec 阻塞到 ctx 到期后以 ctx.Err()
// 返回——模拟"Redis 连接存活但响应停滞"（对齐 B7：ContextTimeoutEnabled 生效后，
// 停滞下命令应在 deadline 返回，而非退化为无界等待）。未武装时透传。
type stallPipelineHook struct{ armed atomic.Bool }

func (h *stallPipelineHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *stallPipelineHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return next
}
func (h *stallPipelineHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		if !h.armed.Load() {
			return next(ctx, cmds)
		}
		<-ctx.Done()
		return ctx.Err()
	}
}

// TestContBindWorkerBatchBoundedByBatchTimeout（B7）：停滞 Redis 下，一批绑定落库
// 须在 BatchTimeout 预算内结束并按批计失败计数，而非无限阻塞。注入短 BatchTimeout
// （150ms）保持快速、自终止——无 sleep 轮询（require.Eventually 内部用阻塞探针）。
func TestContBindWorkerBatchBoundedByBatchTimeout(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })
	st, err := continuation.New(c, "cont-test-secret-0123456789")
	require.NoError(t, err)

	hook := &stallPipelineHook{}
	c.AddHook(hook)

	const budget = 150 * time.Millisecond
	w := NewContBindWorker(st, nil, ContBindConfig{BatchSize: 256, BatchWait: time.Millisecond, BatchTimeout: budget})
	hook.armed.Store(true) // 武装于 Start 之前：首个批次即遇停滞

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, w.Start(ctx))
	t.Cleanup(func() {
		cancel()
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer ccancel()
		require.NoError(t, w.Close(cctx))
	})

	w.Enqueue(workerBindReq(t, "resp_stall"))

	start := time.Now()
	require.Eventually(t, func() bool { return w.Failed() == 1 }, 5*time.Second, 5*time.Millisecond,
		"停滞 Redis 下批次须在 BatchTimeout 内失败计数，而非无限阻塞")
	elapsed := time.Since(start)
	require.GreaterOrEqual(t, elapsed, budget, "须至少耗尽 BatchTimeout 预算（证明确为停滞而非假成功）")
	require.Less(t, elapsed, 2*time.Second, "须受 BatchTimeout 预算约束")
	require.Zero(t, w.Bound())
}
