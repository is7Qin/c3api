// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/aiclient"
	"github.com/is7qin/c3api/pkg/egress"
)

// captureErrLogStore 捕获 errlog 行：失败语义（spec-proxy-pool §5.1/§5.2）的唯一
// 观测面——`error_type`/`error_message` 的落库结果，而不是内存里的中间变量。
type captureErrLogStore struct {
	mu   sync.Mutex
	rows []*domain.UsageLog
}

func (c *captureErrLogStore) InsertBatch(_ context.Context, l []*domain.UsageLog) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.rows = append(c.rows, l...)
	return nil
}

func (c *captureErrLogStore) snapshot() []*domain.UsageLog {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]*domain.UsageLog, len(c.rows))
	copy(out, c.rows)
	return out
}

// closedAddr 返回一个**已经释放**的本机端口地址（"127.0.0.1:port"）：拨它必然
// ECONNREFUSED，用来模拟"代理进程挂了"而不是"代理返回了错误"。
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

// newEgressFailingProxy 与 newEgressPathProxy 同形，但 errlog 写者是可观测的
// 捕获器，并返回 worker 以便测试显式 Close（flush）后断言行内容。
func newEgressFailingProxy(t *testing.T, upstream, proxyAddr string, captured *captureErrLogStore) (*Proxy, *usage.ErrLogWorker) {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, Models: []string{"gpt-4o"},
	}
	proxyID := int64(7)
	acc := &domain.Account{
		ID: 1, TemplateID: 1, Template: tpl, UpstreamKey: "sk-upstream",
		Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 4,
		ProxyID: &proxyID,
		Proxy: &domain.Proxy{
			ID: proxyID, Name: "dead-px", Scheme: domain.ProxySchemeHTTP,
			Host: mustSplitHost(t, proxyAddr), Port: mustSplitPort(t, proxyAddr),
			Username: "pxuser", Password: "pxpass", Enabled: true,
		},
	}
	accs := map[int64][]*domain.Account{10: {acc}}
	pool := egress.NewPool(&http.Client{Transport: http.DefaultTransport})
	cfg := Config{
		MaxBodySize: 1 << 20, FailoverAttempts: 2,
		UpstreamTimeout:       2 * time.Second,
		UpstreamStreamTimeout: 2 * time.Second,
		UsageCapture:          true,
	}
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil, testHealthSink, nil)
	require.NoError(t, re.Reload(context.Background()))
	sched := scheduler.New(scheduler.Config{SyncInterval: time.Hour, Egress: pool}, noopLoader{accs: accs}, re, nil, nil, nil, nil)
	require.NoError(t, sched.InvalidateAllSync())
	publishTestRoutes(t, sched)

	rec := usage.New(usage.UsageConfig{BatchSize: 100, FlushInterval: time.Hour, QuotaFlushInterval: time.Hour}, noopLogStore{}, nil)
	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{"ck-1": activeKey(1, 1, 10)}}, noopUserLoader{}, nil, nil, true)
	require.NoError(t, auth.Reload(context.Background()))
	hc := &http.Client{Transport: http.DefaultTransport}
	clients := aiclient.NewFactory(hc, aiclient.Config{UpstreamTimeout: 2 * time.Second, UpstreamStreamTimeout: 2 * time.Second})
	worker := usage.NewErrLogWorker(usage.ErrLogConfig{QueueSize: 4096, FlushInterval: time.Hour}, captured, nil)
	return New(cfg, sched, credential.New(), rec, clients, auth, nil, nil, worker, Deps{}), worker
}

func mustSplitHost(t *testing.T, addr string) string {
	t.Helper()
	h, _, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	return h
}

func mustSplitPort(t *testing.T, addr string) int {
	t.Helper()
	_, p, err := net.SplitHostPort(addr)
	require.NoError(t, err)
	port, err := net.LookupPort("tcp", p)
	require.NoError(t, err)
	return port
}

// TestEgressProxyDialFailureIsNetworkAndAnnotated 钉住 spec §5.1/§5.2 的失败语义：
//
//   - 代理链路失败（拒连）不得新增枚举：`error_type` 仍是 `network`；
//   - `error_message` 必须带 `proxy_id=<本行 id>` 与脱敏摘要（scheme://host:port），
//     且**不含**用户名/密码；
//   - 用户侧仍是 502（网关不把上游/代理的内部错误当自己的 4xx）。
//
// 反例（M3 之前/漏接线时的形态）：请求静默直连成功、或失败后 message 里没有任何
// 出口标识——运维看到"网络错误"却不知道是哪条出口挂了。
func TestEgressProxyDialFailureIsNetworkAndAnnotated(t *testing.T) {
	up := newEgressFaceUpstream(t)
	dead := closedAddr(t)
	captured := &captureErrLogStore{}
	p, worker := newEgressFailingProxy(t, up.URL, dead, captured)

	rec := postEgressFace(t, p.HandleChat, "/v1/chat/completions", "application/json",
		[]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	require.Equal(t, http.StatusBadGateway, rec.Code, "代理拒连 ⇒ 502；body=%s", rec.Body.String())

	require.NoError(t, worker.Close(context.Background()))
	rows := captured.snapshot()
	require.NotEmpty(t, rows, "失败必须落 enrl 日志")
	row := rows[len(rows)-1]
	require.Equal(t, domain.ErrNetwork, row.ErrorType, "代理链路失败按既有 network 分类（不新增枚举）")
	require.NotNil(t, row.ErrorMessage)
	msg := *row.ErrorMessage
	require.Contains(t, msg, "proxy_id=7", "失败文本必须指明是哪条出口：message=%q", msg)
	require.Contains(t, msg, "http://"+dead, "摘要出 scheme://host:port")
	require.NotContains(t, msg, "pxpass", "密码恒不出（§5.2）")
	require.NotContains(t, msg, "pxuser", "用户名恒不出（§5.2）")

	// 上游一次都没被触达：失败发生在建立出口隧道时（not_sent 语义，可安全迁移）。
	hits, marked := up.counts()
	require.Zero(t, hits, "代理拒连时上游必须零触达")
	require.Zero(t, marked)
}

// TestEgressNoteDirectIsNoop 钉住零代理零回归：未绑定时注解恒空，err_logs.message
// 与 M3 之前逐字节一致（不因引入出口概念而多出任何后缀）。
func TestEgressNoteDirectIsNoop(t *testing.T) {
	require.Empty(t, egressNote(nil), "nil Selection ⇒ 无注解")
	sel := &scheduler.Selection{}
	require.Empty(t, egressNote(sel), "未绑定（ProxyID nil）⇒ 无注解")
	require.Equal(t, "boom", withEgressNote("boom", sel), "直连：原文原样返回")
	require.Empty(t, withEgressNote("", sel), "直连：原文为空仍为空")
}

// TestEgressNoteIsRedactedAndBounded 钉住注解的脱敏与上限：只出 id + 脱敏摘要，
// 且整段仍受 message 列长度约束（不因拼接而溢出）。
func TestEgressNoteIsRedactedAndBounded(t *testing.T) {
	pxID := int64(42)
	spec := egress.New("socks5", "10.0.0.9", 1080, "secretuser", "secretpass")
	sel := &scheduler.Selection{ProxyID: &pxID, Egress: &egress.Client{Spec: spec}}

	note := egressNote(sel)
	require.Equal(t, "proxy_id=42 socks5://10.0.0.9:1080", note)
	require.NotContains(t, note, "secretuser")
	require.NotContains(t, note, "secretpass")

	long := ""
	for range 600 {
		long += "x"
	}
	got := withEgressNote(long, sel)
	require.LessOrEqual(t, len([]rune(got)), domain.ErrMsgMaxLen, "拼接后仍受 message 列上限约束")
	require.Contains(t, got, "proxy_id=42", "截断不得把出口标识截掉（它拼在尾部……所以这里只要求存在）")
}

var _ = httptest.NewRequest
