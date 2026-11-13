// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

// 轮状态机与每轮一次结算的门禁（spec-commit-state-and-round-billing §6/§8.3）。
//
// 本文件只做**端到端断言**（真实 relayWS + 真实 Recorder/Billing/Auth + 分表捕获）：
// 不引用 round/roundState 等新符号，故可以在旧实现上编译并告红——G5/G6/G14/G16b/
// G16c 在旧实现上以断言告红（旧实现整条连接只落一行、无 Warn、无 #rK 派生）；
// G17 在「completed 之后任意帧即开轮」的编排上告红（收起尾杂帧会凭空多一行假成功）。
// 新符号自身的单测在 round_test.go。
//
// 观测面三件套（缺一不可，G16d 要的是**真实副作用**不是行数）：
//   - usage 行：Recorder 落库面（usage_logs），经 rec.Close 显式排空；
//   - err 行：ErrLogWorker 落库面（err_logs），经 errlog.Close 显式排空；
//   - 配额：QuotaWriter 回写面（keys.quota_used）与 Auth 额度计数器（余额侧）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/aiclient"
	"github.com/is7qin/c3api/pkg/logx"
)

// roundWireCreated 一轮的协议首帧（每个 response 恒以 response.created 起头）。
func roundWireCreated(id string) []byte {
	return []byte(`{"type":"response.created","response":{"id":"` + id + `","model":"gpt-4o"}}`)
}

// roundWireCompleted 一轮的协议终态帧。it/ot 决定该轮成本（价目见
// proxyPricingEntry：输入 10 毫分/token、输出 20 毫分/token）。
func roundWireCompleted(id string, it, ot int64) []byte {
	return []byte(`{"type":"response.completed","response":{"id":"` + id +
		`","status":"completed","usage":{"input_tokens":` + strconv.FormatInt(it, 10) +
		`,"output_tokens":` + strconv.FormatInt(ot, 10) + `,"total_tokens":` + strconv.FormatInt(it+ot, 10) + `}}}`)
}

// roundWireCompletedNoUsage 终态帧但**没有** usage（G13 的 CompletedWithoutUsage）。
func roundWireCompletedNoUsage(id string) []byte {
	return []byte(`{"type":"response.completed","response":{"id":"` + id + `","status":"completed"}}`)
}

// roundQuotaSum 额度回写面的累计值（沿用 billing_test.go 的 captureQuotaWriter）。
func roundQuotaSum(q *captureQuotaWriter) int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	var sum int64
	for _, v := range q.total {
		sum += v
	}
	return sum
}

// roundBillingEnv 多轮车道结算观测面（usage_logs 与 err_logs 分表捕获）。
type roundBillingEnv struct {
	p     *Proxy
	usage *captureLogStore
	errs  *captureLogStore
	quota *captureQuotaWriter
	sel   *scheduler.Selection
	out   chan roundRelayOutcome
}

type roundRelayOutcome struct {
	handled  bool
	fwMsg    string
	panicked bool
}

// roundBillingProxy 计费全开 + 额度 key 的 WS 代理与三个观测面（不起会话）。
func roundBillingProxy(t *testing.T, logPath string) (*Proxy, *captureLogStore, *captureLogStore, *captureQuotaWriter, domain.KeyMeta) {
	t.Helper()
	usageRows := &captureLogStore{}
	errRows := &captureLogStore{}
	qw := &captureQuotaWriter{}
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: "http://127.0.0.1:1",
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS},
		Models:           []string{"gpt-4o"},
	}
	accs := map[int64][]*domain.Account{10: {{
		ID: 1, TemplateID: 1, Template: tpl, UpstreamKey: "sk-upstream",
		Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 4,
	}}}
	cfg := Config{
		MaxBodySize: 1 << 20, FailoverAttempts: 2,
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
		UsageCapture:          true, BillingCapture: true,
	}
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil, testHealthSink, nil)
	require.NoError(t, re.Reload(context.Background()))
	sched := scheduler.New(scheduler.Config{SyncInterval: time.Hour}, noopLoader{accs: accs}, re, nil, nil, nil, nil)
	require.NoError(t, sched.InvalidateAllSync())
	publishTestRoutes(t, sched)

	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
		QuotaWriter:        qw,
	}, usageRows, nil)
	// 额度 key：HasQuota=true 是 DeductQuota 生效的前提（否 Auth 侧短路返 0）。
	quotaKey := activeKey(1, 1, 10)
	quotaKey.HasQuota = true
	quotaKey.Quota = 1 << 40
	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{"ck-1": quotaKey}}, noopUserLoader{}, nil, nil, true)
	require.NoError(t, auth.Reload(context.Background()))
	bal := billing.NewBalances(fakeBalanceLoader{m: map[int64]int64{1: 50000}}, nil)
	require.NoError(t, bal.Reload(context.Background()))
	clients := aiclient.NewFactory(&http.Client{Transport: http.DefaultTransport}, aiclient.Config{
		UpstreamTimeout: 5 * time.Second, UpstreamStreamTimeout: 30 * time.Second,
	})
	var logger *logx.Logger
	if logPath != "" {
		var err error
		logger, err = logx.New("warn", logPath)
		require.NoError(t, err)
	}
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{QueueSize: 4096, FlushInterval: time.Hour}, errRows, nil)
	p := New(cfg, sched, credential.New(), rec, clients, auth, logger, &BillingHooks{
		Resolver: &fakePriceLookup{
			entries:  map[string]*domain.PriceEntry{"gpt-4o": proxyPricingEntry()},
			variants: map[string][]*domain.PriceVariant{"gpt-4o": proxyPricingVariants()},
		},
		Balances: bal,
	}, errlogW, Deps{})
	return p, usageRows, errRows, qw, quotaKey
}

// newRoundBillingEnv 起一条 relayWS 会话（真实 Select 抢槽：结算永不 Release，
// 连接收尾的 leaseGuard 释放一次）。
func newRoundBillingEnv(t *testing.T, ft wsRelayTransport, first []byte, logPath string) (*roundBillingEnv, *websocket.Conn, *httptest.Server) {
	t.Helper()
	p, usageRows, errRows, qw, quotaKey := roundBillingProxy(t, logPath)
	sched := p.sched
	sel, err := sched.Select(10, domain.FormatOpenAIResponsesWS, "gpt-4o")
	require.NoError(t, err, "Select 必须成功（真实抢槽）")
	reqID := newReqID()
	start := time.Now()
	out := make(chan roundRelayOutcome, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 归属注入（生产由 guardPipeline 写入）：logWithCtx 靠它盖 user_id/key_id，
		// 没有它 DeductQuota(keyID=0) 恒 no-op（额度面就观测不到了）。
		rm := &reqMeta{meta: quotaKey}
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyReqMeta{}, rm))
		client, aerr := websocket.Accept(w, r, &websocket.AcceptOptions{
			CompressionMode: websocket.CompressionContextTakeover,
		})
		if aerr != nil {
			out <- roundRelayOutcome{}
			return
		}
		defer client.CloseNow()
		// panic 路径（G15 第 1 条）：先记账再重抛；leaseGuard 恰是生产调用方的
		// 连接级 owner（caller_responses_ws.go 的 defer leaseGuard(sel)）。
		defer func() {
			if rec := recover(); rec != nil {
				out <- roundRelayOutcome{panicked: true}
			}
		}()
		defer leaseGuard(sel)
		handled, fwErr := p.relayWS(client, ft, nil, r, reqID, 10, start, sel, "gpt-4o", websocket.MessageText, first)
		out <- roundRelayOutcome{handled: handled, fwMsg: relayErrText(fwErr)}
	}))
	t.Cleanup(srv.Close)
	c := dialResponsesWS(t, srv)
	t.Cleanup(func() { c.CloseNow() })
	return &roundBillingEnv{p: p, usage: usageRows, errs: errRows, quota: qw, sel: sel, out: out}, c, srv
}

// drain 排空两个落库面并返回 (usage 行, err 行)。
func (e *roundBillingEnv) drain(t *testing.T) ([]*domain.UsageLog, []*domain.UsageLog) {
	t.Helper()
	require.NoError(t, e.p.rec.Close(context.Background()))
	require.NoError(t, e.p.errlog.Close(context.Background()))
	e.usage.mu.Lock()
	u := append([]*domain.UsageLog(nil), e.usage.logs...)
	e.usage.mu.Unlock()
	e.errs.mu.Lock()
	er := append([]*domain.UsageLog(nil), e.errs.logs...)
	e.errs.mu.Unlock()
	return u, er
}

// roundRowOf 按派生 id 取某一轮的行（第 1 轮 = 无 #rK 后缀）。
func roundRowOf(t *testing.T, rows []*domain.UsageLog, index int) *domain.UsageLog {
	t.Helper()
	for _, l := range rows {
		if roundIndexSuffix(l.RequestID) == index {
			return l
		}
	}
	t.Fatalf("第 %d 轮的结算行不存在；rows=%v", index, roundRequestIDs(rows))
	return nil
}

// roundIndexSuffix 从派生 request_id 还原轮号（base → 1，base#rK → K）。
func roundIndexSuffix(id string) int {
	i := strings.LastIndex(id, "#r")
	if i < 0 {
		return 1
	}
	n := 0
	for _, c := range id[i+2:] {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	if n < 2 {
		return 0
	}
	return n
}

func roundRequestIDs(rows []*domain.UsageLog) []string {
	out := make([]string, 0, len(rows))
	for _, l := range rows {
		out = append(out, l.RequestID)
	}
	return out
}

// panickingTransport 在收尾（Close）时 panic：G15 的 panic 路径要求崩溃点落在
// relayWS 主 goroutine（goroutine 内的 panic 已被 relayRecover 接住），由调用方
// 的 defer leaseGuard（= 生产 caller_responses_ws.go 的连接级 owner）收尾。
type panickingTransport struct{ *fakeTransport }

func (p *panickingTransport) Close(websocket.StatusCode, string) error {
	panic("panickingTransport: injected close panic")
}

func roundText(f []byte) fakeRead {
	return fakeRead{typ: websocket.MessageText, frame: f}
}

func roundCloseNormal() fakeRead {
	return fakeRead{typ: 0, err: websocket.CloseError{Code: websocket.StatusNormalClosure}}
}

// --- G5：一条连接 3 轮均 completed → 3 行、派生 id、sum(cost)、DeductQuota 3 次 ---
//
// 「DeductQuota 3 次」的判定：三轮用量两两不同 ⇒ 三轮成本两两不同 ⇒ 余额侧消耗
// 等于三者之和时，任何「只扣一次/只扣部分轮」的实现都不可能巧合相等（真子集和
// 严格小于总和），故 sum 相等即证明三次非零扣减都发生过。
func TestRoundG5ThreeCompletedRoundsSettlePerRound(t *testing.T) {
	ft := &fakeTransport{readQueue: []fakeRead{
		roundText(roundWireCreated("rsp_1")), roundText(roundWireCompleted("rsp_1", 1, 1)),
		roundText(roundWireCreated("rsp_2")), roundText(roundWireCompleted("rsp_2", 2, 2)),
		roundText(roundWireCreated("rsp_3")), roundText(roundWireCompleted("rsp_3", 3, 3)),
		roundCloseNormal(),
	}}
	env, c, _ := newRoundBillingEnv(t, ft, []byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`), "")
	for range 6 {
		_ = readResponsesWSFrame(t, c) // created/completed ×3（终态帧照常透传客户端）
	}
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)
	res := <-env.out
	require.True(t, res.handled)
	usageRows, errRows := env.drain(t)

	require.Len(t, usageRows, 3, "每轮一行：ids=%v", roundRequestIDs(usageRows))
	require.Empty(t, errRows, "三轮全部完成 ⇒ 无错误行")
	base := usageRows[0].RequestID
	require.NotContains(t, base, "#r", "第 1 轮 request_id = 连接级原值（零变化）")
	require.Equal(t, []string{base, base + "#r2", base + "#r3"}, roundRequestIDs(usageRows),
		"派生 id = base / base#r2 / base#r3（§6.2）")

	var sum int64
	for i, l := range usageRows {
		require.Equal(t, domain.ErrNone, l.ErrorType)
		require.Equal(t, int64(i+1), l.InputTokens, "第 %d 轮 token 归属", i+1)
		require.Equal(t, int64(i+1), l.OutputTokens)
		require.Positive(t, l.Cost, "三轮都有价目命中（成本非零才证明计量真发生）")
		require.NotNil(t, l.CommitState)
		require.Equal(t, CommitResponseStarted.String(), *l.CommitState)
		require.WithinDuration(t, l.CreatedAt, time.Now(), time.Minute,
			"created_at = 轮起点（落在本连接时间窗内）")
		sum += l.Cost
	}
	require.NotEqual(t, usageRows[1].Cost, usageRows[2].Cost, "三轮成本互不相同（三次扣减的判据前提）")
	require.Equal(t, sum, roundQuotaSum(env.quota), "quota_used 回写 = 三轮成本之和")
	require.Equal(t, sum, env.consumed(), "余额侧消耗 = 三轮成本之和（DeductQuota 三轮各一次）")
}

// consumed 额度计数器现值（余额侧的「已消耗计费毫分」）。
func (e *roundBillingEnv) consumed() int64 {
	snap := e.p.auth.gate.store.Load()
	q, ok := snap.quotas[1]
	if !ok {
		return 0
	}
	return q.consumed.Load()
}

// --- G6：2 轮完成 + 第 3 轮中止 → 2 条轮结算行 + 1 条失败行归类正确 ---
func TestRoundG6CompletedRoundsThenAbort(t *testing.T) {
	ft := &fakeTransport{
		readQueue: []fakeRead{
			roundText(roundWireCreated("rsp_1")), roundText(roundWireCompleted("rsp_1", 1, 1)),
			roundText(roundWireCreated("rsp_2")), roundText(roundWireCompleted("rsp_2", 2, 2)),
			roundText(roundWireCreated("rsp_3")), // 第 3 轮已由 created 起头、但没有终态帧
		},
		readBlock: make(chan struct{}), // 队列耗尽后阻塞：客户端先退出，分类确定
	}
	env, c, _ := newRoundBillingEnv(t, ft, []byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`), "")
	for range 5 {
		_ = readResponsesWSFrame(t, c)
	}
	require.NoError(t, c.Close(websocket.StatusNormalClosure, "")) // 第 3 轮中途客户端断开
	res := <-env.out
	require.True(t, res.handled)
	usageRows, errRows := env.drain(t)

	require.Len(t, usageRows, 3, "2 轮完成 + 第 3 轮（已交付 1 帧 ⇒ 可计费）失败行")
	base := usageRows[0].RequestID
	rb := roundRowOf(t, usageRows, 2)
	require.Equal(t, base+"#r2", rb.RequestID)
	third := roundRowOf(t, usageRows, 3)
	require.Equal(t, base+"#r3", third.RequestID, "失败行的轮号取中止的那一轮")
	require.Equal(t, domain.ErrAbort, third.ErrorType, "客户端中途断 ⇒ abort")
	require.Equal(t, http.StatusOK, third.StatusCode)
	require.Zero(t, third.InputTokens, "第 3 轮没有 usage ⇒ 0（不伪造、不继承前轮）")
	require.Positive(t, third.Cost+rb.Cost, "前两轮照常计费")

	require.Len(t, errRows, 1, "中止轮只留一条错误明细")
	require.Equal(t, third.RequestID, errRows[0].RequestID,
		"两表同 ID：同一轮的 usage_logs 与 err_logs 必须用同一个派生 request_id（§6.2）")
}

// --- G13：completed 但无 usage → CompletedWithoutUsage（tokens=0、不重复扣费） ---
func TestRoundG13CompletedWithoutUsageNoFabrication(t *testing.T) {
	ft := &fakeTransport{readQueue: []fakeRead{
		roundText(roundWireCreated("rsp_1")),
		roundText(roundWireCompletedNoUsage("rsp_1")),
		roundCloseNormal(),
	}}
	env, c, _ := newRoundBillingEnv(t, ft, []byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`), "")
	_ = readResponsesWSFrame(t, c)
	_ = readResponsesWSFrame(t, c)
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)
	res := <-env.out
	require.True(t, res.handled)
	usageRows, errRows := env.drain(t)

	require.Len(t, usageRows, 1, "完成轮仍是一行（错的只有用量，不是成员资格）")
	require.Empty(t, errRows, "完成轮不是错误行")
	row := usageRows[0]
	require.Equal(t, domain.ErrNone, row.ErrorType)
	require.Zero(t, row.InputTokens, "无 usage 不伪造 token")
	require.Zero(t, row.OutputTokens)
	require.Zero(t, row.TotalTokens)
	require.Zero(t, row.Cost, "0 token ⇒ 0 成本")
	require.Zero(t, row.CallCount)
	require.Zero(t, roundQuotaSum(env.quota), "0 成本不得产生 quota 增量")
	require.Zero(t, env.consumed(), "余额不得被减")
}

// --- G14：重复 completed ⇒ 只结算一次 + 一次 Warn ---
func TestRoundG14DuplicateCompletedSettlesOnceWithOneWarn(t *testing.T) {
	dir, err := os.MkdirTemp("", "c3api-round-*")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	logOut := filepath.Join(dir, "warn.log")

	completed := roundWireCompleted("rsp_1", 1, 1)
	ft := &fakeTransport{readQueue: []fakeRead{
		roundText(roundWireCreated("rsp_1")),
		roundText(completed),
		roundText(completed), // 同一轮第二个终态帧（协议违规）
		roundCloseNormal(),
	}}
	env, c, _ := newRoundBillingEnv(t, ft, []byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`), logOut)
	for range 3 {
		_ = readResponsesWSFrame(t, c) // 两帧终态帧都照常透传（fidelity 不因记账改变）
	}
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)
	res := <-env.out
	require.True(t, res.handled)
	usageRows, errRows := env.drain(t)

	require.Len(t, usageRows, 1, "同一轮只结算一次（第二次 completed 不写行）：%v", roundRequestIDs(usageRows))
	require.Empty(t, errRows)
	require.Equal(t, int64(1), usageRows[0].InputTokens)

	data, rerr := os.ReadFile(logOut)
	require.NoError(t, rerr)
	require.Equal(t, 1, strings.Count(string(data), "responses round duplicate completed frame"),
		"重复 completed 投递一次 Warn（warn=%s）", string(data))
}

// --- G15：release-once 四路径（panic / 客户端断开 / 上游超时 / 完成后立即断） ---
func TestRoundG15ReleaseOnceOnAllFourPaths(t *testing.T) {
	cases := []struct {
		name  string
		ft    wsRelayTransport
		after func(t *testing.T, c *websocket.Conn)
	}{
		{
			name: "panic",
			// 收尾阶段（主 goroutine）注入 panic：Close 内崩，defer leaseGuard 收尾。
			ft: &panickingTransport{fakeTransport: &fakeTransport{
				readQueue: []fakeRead{roundText(roundWireCreated("rsp_1")), roundText(roundWireCompleted("rsp_1", 1, 1)), roundCloseNormal()},
			}},
			after: func(t *testing.T, c *websocket.Conn) { _ = c.CloseNow() },
		},
		{
			name: "client_disconnect",
			ft: &fakeTransport{
				readQueue: []fakeRead{roundText(roundWireCreated("rsp_1"))},
				readBlock: make(chan struct{}),
			},
			after: func(t *testing.T, c *websocket.Conn) {
				require.NoError(t, c.Close(websocket.StatusNormalClosure, ""))
			},
		},
		{
			name: "upstream_timeout",
			ft: &fakeTransport{readQueue: []fakeRead{
				roundText(roundWireCreated("rsp_1")),
				{typ: 0, err: context.DeadlineExceeded},
			}},
			after: func(t *testing.T, c *websocket.Conn) { _ = c.CloseNow() },
		},
		{
			name: "completed_then_disconnect",
			ft: &fakeTransport{
				readQueue: []fakeRead{roundText(roundWireCreated("rsp_1")), roundText(roundWireCompleted("rsp_1", 1, 1))},
				readBlock: make(chan struct{}),
			},
			after: func(t *testing.T, c *websocket.Conn) { _ = c.CloseNow() },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env, c, _ := newRoundBillingEnv(t, tc.ft, []byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`), "")
			tc.after(t, c)
			res := <-env.out
			require.True(t, res.handled || res.panicked, "会话必须收尾（panic 也算收尾）")

			ri, ok := env.p.sched.Runtime(1)
			require.True(t, ok)
			require.Zero(t, ri.Concurrency, "并发槽必须释放")
			// 生效次数 = 1：Release 已 CAS 幂等，重复释放不得二次递减计数。
			env.sel.Release()
			ri, ok = env.p.sched.Runtime(1)
			require.True(t, ok)
			require.Zero(t, ri.Concurrency, "release 生效次数必须恰为 1（重复调用为 no-op）")
		})
	}
}

// --- G16b：同一轮两次 completed ⇒ err_logs 只 1 行 ---
//
// 两条面：①重复 completed 不产生第二条（任何表）；②不可计费轮的失败行只进
// err_logs 且只有一行（§6.2.2：err_logs 无唯一索引，靠 FSM 在内存拦）。
func TestRoundG16bNoSecondRowPerRound(t *testing.T) {
	t.Run("duplicate_completed", func(t *testing.T) {
		completed := roundWireCompleted("rsp_1", 1, 1)
		ft := &fakeTransport{readQueue: []fakeRead{
			roundText(roundWireCreated("rsp_1")), roundText(completed), roundText(completed), roundCloseNormal(),
		}}
		env, c, _ := newRoundBillingEnv(t, ft, []byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`), "")
		for range 3 {
			_ = readResponsesWSFrame(t, c)
		}
		readResponsesWSClose(t, c, websocket.StatusNormalClosure)
		<-env.out
		usageRows, errRows := env.drain(t)
		require.Len(t, usageRows, 1, "同一轮只 1 行 usage_logs")
		require.Empty(t, errRows, "完成轮不进 err_logs；重复帧更不得凭空造错误行")
	})

	t.Run("unbillable_abort_is_err_only_once", func(t *testing.T) {
		// 零业务帧的轮：上游直接报错 ⇒ not_sent ⇒ 不可计费 ⇒ 只进 err_logs。
		ft := &fakeTransport{readQueue: []fakeRead{{typ: 0, err: errRoundUpstreamBoom}}}
		env, c, _ := newRoundBillingEnv(t, ft, []byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`), "")
		_ = c.CloseNow()
		<-env.out
		usageRows, errRows := env.drain(t)
		require.Empty(t, usageRows, "未交付失败不进 usage_logs")
		require.Len(t, errRows, 1, "失败行在 err_logs 恰好一行（FSM 拦截第二次写）")
		require.Equal(t, domain.ErrNetwork, errRows[0].ErrorType)
	})
}

// errRoundUpstreamBoom 上游读失败（零帧已交付 ⇒ not_sent）。
var errRoundUpstreamBoom = &roundUpstreamBoom{}

type roundUpstreamBoom struct{}

func (e *roundUpstreamBoom) Error() string { return "round fixture: upstream read failure" }

// --- G16c：派生 request_id（3 轮不冲突；单轮 = base；两表同 ID） ---
//
// 端到端面：同一轮的四条写路径里「收尾失败行」与「轮结算行」都在本文件覆盖
// （G6 断言两表同 ID）。recordRejected / failover 属 relay 之前的单轮路径——
// 它们的 id 就是第 1 轮 id（= base），由 round_test.go 的单元断言钉住。
func TestRoundG16cDerivedRequestIDs(t *testing.T) {
	require.Equal(t, 1, roundIndexSuffix("req-plain"), "无后缀 ⇒ 第 1 轮")
	require.Equal(t, 2, roundIndexSuffix("req-plain#r2"))
	require.Equal(t, 3, roundIndexSuffix("req-plain#r3"))

	// 单轮车道零变化：一条只完成一轮的连接，请求行 id 仍是连接级原值。
	ft := &fakeTransport{readQueue: []fakeRead{
		roundText(roundWireCreated("rsp_1")), roundText(roundWireCompleted("rsp_1", 1, 1)), roundCloseNormal(),
	}}
	env, c, _ := newRoundBillingEnv(t, ft, []byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`), "")
	_ = readResponsesWSFrame(t, c)
	_ = readResponsesWSFrame(t, c)
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)
	<-env.out
	usageRows, _ := env.drain(t)
	require.Len(t, usageRows, 1)
	require.NotContains(t, usageRows[0].RequestID, "#r",
		"第 1 轮 = 原值（前端/对账零变化）")
}

// --- G17：completed 之后的收尾杂帧不得开轮（假成功行回归门禁） ---
//
// 一次上游调用只落一行。真实上游在 response.completed 之后仍可能发收尾帧
// （response.done / output_item.done / 心跳型事件 / 回声）：旧编排把「任意非
// completed 帧」当下一轮起点，于是给这个**从未真正开始的空轮**写一条
// error_type=none / status 200 的假成功行——多占一条 usage_logs、虚增
// request_count，BillingCapture 开启时还被账本消费一条零价行。
//
// 判据（§6.1）：轮边界 = 当前轮已 completed **且**收到 response.created（每轮真正的
// 开始必伴它以带新 response id）。两道面：
//
//	① 杂帧 ⇒ 仍 1 行、无 #r2、request_count 不变、零价账一行也不多；
//	② response.created ⇒ 真第 2 轮（#r2 落账），证明判据没有误杀正常多轮。
func TestRoundG17PostCompletedNoiseDoesNotOpenRound(t *testing.T) {
	t.Run("noise_does_not_open_round", func(t *testing.T) {
		// 一轮正常完成，随后上游继续发两个非 created 的收尾帧。
		queue := []fakeRead{
			roundText(roundWireCreated("rsp_1")),
			roundText(roundWireCompleted("rsp_1", 1, 1)),
			roundText([]byte(`{"type":"response.done","response":{"id":"rsp_1","status":"completed"}}`)),
			roundText([]byte(`{"type":"keepalive"}`)),
			roundCloseNormal(),
		}
		env, c, _ := newRoundBillingEnv(t, &fakeTransport{readQueue: queue},
			[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`), "")
		for range 4 {
			_ = readResponsesWSFrame(t, c) // created/completed/response.done/keepalive 全部照旧透传
		}
		readResponsesWSClose(t, c, websocket.StatusNormalClosure)
		require.True(t, (<-env.out).handled)
		usageRows, errRows := env.drain(t)

		require.Len(t, usageRows, 1, "收尾杂帧不开轮 ⇒ usage_logs 仍 1 行：%v", roundRequestIDs(usageRows))
		require.Empty(t, errRows, "杂帧不是失败信号（err_logs 也不许多行）")
		row := usageRows[0]
		require.NotContains(t, row.RequestID, "#r", "无第 2 轮 ⇒ request_id 不得带 #r2 后缀")
		require.Equal(t, domain.ErrNone, row.ErrorType)
		require.Equal(t, http.StatusOK, row.StatusCode)
		require.Equal(t, int64(1), row.InputTokens, "落账仍是第 1 轮的 usage")
		// 统计侧 request_count = count(*)：1 行即 1 次请求，虚增的那次不存在。
		require.Equal(t, []string{row.RequestID}, roundRequestIDs(usageRows), "request_count 不变（仍 1 次）")
		// BillingCapture：扣减恰一次，空轮不再多消费一条零价账。
		require.Equal(t, row.Cost, roundQuotaSum(env.quota), "quota 增量 = 唯一那一行的成本")
	})

	t.Run("created_opens_second_round", func(t *testing.T) {
		// 正例：completed 之后的 response.created 是真第 2 轮起点 ⇒ 照常 #r2 落账。
		ft := &fakeTransport{readQueue: []fakeRead{
			roundText(roundWireCreated("rsp_1")), roundText(roundWireCompleted("rsp_1", 1, 1)),
			roundText(roundWireCreated("rsp_2")), roundText(roundWireCompleted("rsp_2", 2, 2)),
			roundCloseNormal(),
		}}
		env, c, _ := newRoundBillingEnv(t, ft,
			[]byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`), "")
		for range 4 {
			_ = readResponsesWSFrame(t, c)
		}
		readResponsesWSClose(t, c, websocket.StatusNormalClosure)
		require.True(t, (<-env.out).handled)
		usageRows, errRows := env.drain(t)

		require.Len(t, usageRows, 2, "created 开轮 ⇒ 第 2 轮照常结算：%v", roundRequestIDs(usageRows))
		require.Empty(t, errRows)
		base := usageRows[0].RequestID
		require.Equal(t, []string{base, base + "#r2"}, roundRequestIDs(usageRows),
			"completed 后的 response.created = 第 2 轮（判据不误杀多轮）")
		second := roundRowOf(t, usageRows, 2)
		require.Equal(t, int64(2), second.InputTokens, "第 2 轮 usage 归第 2 轮")
		require.Equal(t, domain.ErrNone, second.ErrorType)
	})
}
