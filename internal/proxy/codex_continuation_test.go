// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/continuation"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/aiclient"
)

// --- codex 续接入队限定：仅直连（客户端 Responses）入队；converted 不入队 ---

// newCodexContProxy 构造装配了异步续接 store + worker 的 codex resp 代理（镜像
// newTestCodexRespProxy，额外下发 Deps{Continuation, ContBind}）。pcs = KeyMeta
// 组级 protocol_convert 方向集合（converted 用例用）。
func newCodexContProxy(t *testing.T, credType credential.Type, accounts map[int64]*domain.AccountExt, upstream string, pcs []domain.ProtocolConvert, s *continuation.Store, bind *ContBindWorker) *Proxy {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: "",
		CredentialType:   credType,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
		Models:           []string{"gpt-4o"},
	}
	accs := make(map[int64][]*domain.Account, 1)
	for id, ext := range accounts {
		accs[10] = append(accs[10], &domain.Account{
			ID: id, TemplateID: tpl.ID, Template: tpl, UpstreamKey: "",
			Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: 4, Ext: ext,
		})
	}
	rec := usage.New(usage.UsageConfig{BatchSize: 100, FlushInterval: time.Hour, QuotaFlushInterval: time.Hour}, &captureLogStore{}, nil)
	cfg := Config{MaxBodySize: 1 << 20, FailoverAttempts: 2, UpstreamTimeout: 5 * time.Second, UpstreamStreamTimeout: 30 * time.Second, UsageCapture: true}
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil, testHealthSink, nil)
	require.NoError(t, re.Reload(context.Background()))
	sched := scheduler.New(scheduler.Config{SyncInterval: time.Hour}, noopLoader{accs: accs}, re, nil, nil, nil, nil)
	require.NoError(t, sched.InvalidateAllSync())
	publishTestRoutes(t, sched)

	key := activeKey(1, 1, 10)
	key.ProtocolConverts = pcs
	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{"ck-1": key}}, noopUserLoader{}, nil, nil, true)
	require.NoError(t, auth.Reload(context.Background()))
	clients := aiclient.NewFactory(&http.Client{Transport: http.DefaultTransport}, aiclient.Config{UpstreamTimeout: 5 * time.Second, UpstreamStreamTimeout: 30 * time.Second})
	failure := sdkbridge.NewFailureHandler(sdkbridge.FailureDeps{Store: &fakeFailureStore{}, Failer: sched, Log: nil})
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{QueueSize: 4096, BatchSize: 100, FlushInterval: 20 * time.Millisecond}, &captureLogStore{}, nil)
	wctx, wcancel := context.WithCancel(context.Background())
	require.NoError(t, errlogW.Start(wctx))
	t.Cleanup(func() { wcancel(); _ = errlogW.Close(context.Background()) })
	codex := sdkbridge.NewCodex(failure, newProxyOfficialRewriteTransportWithAssert(t, upstream), sdkbridge.RotationDeps{})
	p := New(cfg, sched, credential.New(), rec, clients, auth, nil, nil, errlogW, Deps{Codex: codex, Continuation: s, ContBind: bind})
	t.Cleanup(func() { _ = p.rec.Close(context.Background()) })
	return p
}

// TestCodexDirectContinuationEnqueued 直连 codex 流式（客户端 Responses）：首个
// 有效响应 id 帧 → contProtocolREST 绑定已建（与 native 同一写出前 seam）。
func TestCodexDirectContinuationEnqueued(t *testing.T) {
	_, s, _ := contFixture(t)
	up, _ := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{t6RespCreated, t6RespDone}})
	defer up.Close()
	w := startTestContBind(t, s, ContBindConfig{})
	p := newCodexContProxy(t, credential.TypeCodexPAT,
		map[int64]*domain.AccountExt{10: codexPATExt(10, "pat-1")}, up.URL, nil, s, w)

	srv := httptest.NewServer(AIRouter(p))
	defer srv.Close()
	resp := postResponses(t, srv, `{"model":"gpt-4o","stream":true,"input":"hi"}`)
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "body=%s", b)
	require.Contains(t, string(b), "resp_t6", "上游 id 帧透传客户端")

	bind := waitBinding(t, s, contProtocolREST, "resp_t6")
	require.Equal(t, int64(10), bind.AccountID, "直连 codex 与 native 同一入队 seam")
}

// TestCodexConvertedContinuationNotEnqueued converted codex（客户端 Chat）：
// 不入队——客户端无法消费 Responses 续接 id；worker 零绑定、lookup miss。
func TestCodexConvertedContinuationNotEnqueued(t *testing.T) {
	_, s, _ := contFixture(t)
	up, _ := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{t6RespCreated, convCodexRespDelta, t6RespDone}})
	defer up.Close()
	w := startTestContBind(t, s, ContBindConfig{})
	p := newCodexContProxy(t, credential.TypeCodexOAuth,
		map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")},
		up.URL, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp}, s, w)

	rec := postChatConv(t, p, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"object":"chat.completion.chunk"`, "转换路径客户端收 chat chunk")

	require.Zero(t, w.Bound(), "converted 客户端非 Responses → 不入队")
	require.Zero(t, w.Unattributed(), "converted 不触发归属拒绝计数")
	_, ok := contLookupBinding(t, s, contProtocolREST, "resp_t6")
	require.False(t, ok, "converted 不应为客户端不可消费的 id 建绑定")
}
