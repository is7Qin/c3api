// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/coder/websocket"
	codexsdk "github.com/is7Qin/codex-sdk"
	"github.com/is7Qin/codex-sdk/redisstore"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/aiclient"
)

// --- Codex 会话路径（spec §5.1–§5.3）验收夹具：miniredis 驱动会话簿 + 可观测全部
// 出网字节的假上游（HTTP SSE 与 WS 同一个 server）。---

// newMiniredisBook 在 mr 上构造一份会话簿（每次调用一个独立的 redis client，模拟
// 一个副本）。
func newMiniredisBook(t *testing.T, mr *miniredis.Miniredis) *codexsdk.ConversationBook {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	return codexsdk.NewConversationBook(redisstore.New(rdb))
}

// flushBook 同步落地会话簿写入队列（第三步的 worker 之前，测试手动 Flush）。
func flushBook(t *testing.T, b *codexsdk.ConversationBook) {
	t.Helper()
	require.NoError(t, b.Flush(context.Background()))
}

// convTestAcc 会话夹具的账号描述：每个账号一个模板（模型映射按账号区分）。
type convTestAcc struct {
	id           int64
	installation string
	domain       string // "" = 未配置（私有域 private:<id>）
	mapped       string // 非空：gpt-4o → mapped（显式映射）
	maxConc      int    // 0 → 4；负数 → 0（无槽位池）
}

type convTestOpts struct {
	accs      []convTestAcc
	order     []int64 // 非空：两种格式都发布这个确定的候选序
	transport domain.CodexResponsesTransport
	pcs       []domain.ProtocolConvert // ck-1 的协议转换方向
	book      *codexsdk.ConversationBook
}

func convInstallation(id int64) string {
	return fmt.Sprintf("00000000-0000-4000-8000-%012d", id)
}

func convPAT(id int64) string { return fmt.Sprintf("pat-%d", id) }

// newConvTestProxy 构造会话路径测试代理：PAT 凭据（Authorization = Bearer pat-<id>
// 可辨识账号）、Responses + Responses-WS 两种格式、同组 10；API key ck-1（keyID 1，
// 带 pcs）与 ck-2（keyID 2）。
func newConvTestProxy(t *testing.T, upstream string, o convTestOpts) *Proxy {
	t.Helper()
	accs := map[int64][]*domain.Account{}
	for _, a := range o.accs {
		tpl := &domain.Template{
			ID: a.id, Name: fmt.Sprintf("t%d", a.id), BaseURL: "",
			CredentialType:          credential.TypeCodexPAT,
			SupportedFormats:        []domain.RequestFormat{domain.FormatOpenAIResponses, domain.FormatOpenAIResponsesWS},
			Models:                  []string{"gpt-4o"},
			CodexResponsesTransport: o.transport,
		}
		if a.mapped != "" {
			tpl.ModelMapping = map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: a.mapped, Mode: domain.ModelMappingModeExplicit}}
		}
		ext := codexPATExt(a.id, convPAT(a.id))
		inst := a.installation
		if inst == "" {
			inst = convInstallation(a.id)
		}
		ext.CodexIdentity = &domain.CodexIdentity{InstallationID: inst}
		maxConc := a.maxConc
		switch {
		case maxConc == 0:
			maxConc = 4
		case maxConc < 0:
			maxConc = 0
		}
		acc := &domain.Account{
			ID: a.id, TemplateID: tpl.ID, Template: tpl, UpstreamKey: "",
			Enabled: true, LifecycleRevision: 1, IdentityRevision: 1, MaxConcurrency: maxConc, Ext: ext,
		}
		if a.domain != "" {
			d := a.domain
			acc.CacheDomain = &d
		}
		accs[10] = append(accs[10], acc)
	}
	logs := &captureLogStore{}
	rec := usage.New(usage.UsageConfig{BatchSize: 100, FlushInterval: time.Hour, QuotaFlushInterval: time.Hour}, logs, nil)
	cfg := Config{
		MaxBodySize: 1 << 20, FailoverAttempts: 2,
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
		UsageCapture:          true,
	}
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil, testHealthSink, nil)
	require.NoError(t, re.Reload(context.Background()))
	egPool := testEgressPool(newProxyOfficialRewriteTransportWithAssert(t, upstream))
	codexClients := sdkbridge.NewClientTable()
	sched := scheduler.New(scheduler.Config{
		SyncInterval: time.Hour, Egress: egPool,
		RetireThread: codexClients.RetireThread, ClientResources: codexClients,
	}, noopLoader{accs: accs}, re, nil, nil, nil, nil)
	require.NoError(t, sched.InvalidateAllSync())
	publishTestRoutes(t, sched)
	if len(o.order) > 0 {
		cands := make([]scheduler.CompiledCandidate, 0, len(o.order))
		for _, id := range o.order {
			cands = append(cands, scheduler.CompiledCandidate{AccountID: id})
		}
		for _, f := range []domain.RequestFormat{domain.FormatOpenAIResponses, domain.FormatOpenAIResponsesWS} {
			sched.PublishDecisionForTest(scheduler.RouteRefFor(10, string(f), "gpt-4o"), &scheduler.RouteDecision{Primary: cands})
		}
	}
	k1 := activeKey(1, 1, 10)
	k1.ProtocolConverts = o.pcs
	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{
		"ck-1": k1,
		"ck-2": activeKey(2, 1, 10),
	}}, noopUserLoader{}, nil, nil, true)
	require.NoError(t, auth.Reload(context.Background()))
	clients := aiclient.NewFactory(&http.Client{Transport: http.DefaultTransport}, aiclient.Config{
		UpstreamTimeout: 5 * time.Second, UpstreamStreamTimeout: 30 * time.Second,
	})
	failure := sdkbridge.NewFailureHandler(sdkbridge.FailureDeps{Store: &fakeFailureStore{}, Failer: sched, Log: nil})
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{QueueSize: 4096, BatchSize: 100, FlushInterval: 20 * time.Millisecond}, logs, nil)
	wctx, wcancel := context.WithCancel(context.Background())
	require.NoError(t, errlogW.Start(wctx))
	t.Cleanup(func() { wcancel(); _ = errlogW.Close(context.Background()) })
	codexAdapter := sdkbridge.NewCodex(failure, egPool, codexClients, sdkbridge.RotationDeps{})
	return New(cfg, sched, credential.New(), rec, clients, auth, nil, nil, errlogW, Deps{Codex: codexAdapter, Conversations: o.book})
}

// convWire 是假上游收到的一次请求（HTTP 一次 POST；WS 一次握手 + 其后所有帧）。
type convWire struct {
	ws     bool
	header http.Header
	body   []byte   // HTTP：zstd 解压后的体
	frames [][]byte // WS：客户端帧（已按 permessage-deflate 解压）
}

// convUpstream 记录全部出网字节的假 Codex 上游。HTTP：200 → SSE（created / item /
// completed），响应头签发 x-codex-turn-state = "ts-<序号>"；WS：每个客户端帧回
// created + response.metadata（ts-<序号>）+ completed。statuses 按序弹出（HTTP
// 每次 POST / WS 每次握手各弹一个；耗尽 = 200）。
type convUpstream struct {
	mu       sync.Mutex
	wires    []*convWire
	statuses []int
	seq      int
}

func newConvUpstream(t *testing.T, statuses ...int) (*httptest.Server, *convUpstream) {
	t.Helper()
	u := &convUpstream{statuses: statuses}
	srv := httptest.NewServer(http.HandlerFunc(u.serve))
	t.Cleanup(srv.Close)
	return srv, u
}

func (u *convUpstream) next() (int, int) {
	u.mu.Lock()
	defer u.mu.Unlock()
	status := http.StatusOK
	if len(u.statuses) > 0 {
		status = u.statuses[0]
		u.statuses = u.statuses[1:]
	}
	u.seq++
	return status, u.seq - 1
}

func (u *convUpstream) serve(w http.ResponseWriter, r *http.Request) {
	wire := &convWire{header: r.Header.Clone()}
	isWS := strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
	wire.ws = isWS
	if !isWS {
		wire.body = readFakeUpstreamBody(r)
	}
	u.mu.Lock()
	u.wires = append(u.wires, wire)
	u.mu.Unlock()
	status, seq := u.next()
	if status != http.StatusOK {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"message":"upstream rejected"}}`))
		return
	}
	if !isWS {
		w.Header().Set("x-codex-turn-state", fmt.Sprintf("ts-%d", seq))
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for _, ev := range convEvents(fmt.Sprintf("resp_%d", seq)) {
			_, _ = io.WriteString(w, "data: "+ev+"\n\n")
			f.Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
		return
	}
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionContextTakeover})
	if err != nil {
		return
	}
	defer c.CloseNow()
	for n := 0; ; n++ {
		typ, msg, err := c.Read(context.Background())
		if err != nil {
			return
		}
		u.mu.Lock()
		wire.frames = append(wire.frames, msg)
		u.mu.Unlock()
		id := fmt.Sprintf("resp_%d_%d", seq, n)
		evs := convEvents(id)
		meta := `{"type":"response.metadata","response_id":"` + id + `","headers":{"x-codex-turn-state":"` + fmt.Sprintf("ts-%d", seq) + `"}}`
		evs = append(evs[:1], append([]string{meta}, evs[1:]...)...)
		for _, ev := range evs {
			if err := c.Write(context.Background(), typ, []byte(ev)); err != nil {
				return
			}
		}
	}
}

// convEvents created / output_item.done / completed（completed 带 usage）。
func convEvents(id string) []string {
	return []string{
		`{"type":"response.created","response":{"id":"` + id + `","object":"response","status":"in_progress","model":"gpt-4o"}}`,
		t6RespItemEv,
		`{"type":"response.completed","response":{"id":"` + id + `","object":"response","status":"completed","usage":` + t6RespUsage + `}}`,
	}
}

func (u *convUpstream) wire(t *testing.T, i int) *convWire {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	require.Greater(t, len(u.wires), i, "上游请求数不足")
	return u.wires[i]
}

func (u *convUpstream) n() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.wires)
}

// allBytes 全部出网字节（头名与值 + 解压后的体 / 帧），哨兵扫描面。
func (u *convUpstream) allBytes() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	var b strings.Builder
	for _, w := range u.wires {
		for k, vs := range w.header {
			for _, v := range vs {
				b.WriteString(k + ": " + v + "\n")
			}
		}
		b.Write(w.body)
		for _, f := range w.frames {
			b.Write(f)
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// convIdentity 一次上游请求可观测的身份面（HTTP：头 + 体；WS：握手头 + 第 i 帧）。
type convIdentity struct {
	auth, sessionHdr, threadHdr, windowHdr, turnState, routingHint string
	turnMeta                                                        string
	metaSession, metaThread, metaWindow, metaInstallation, metaTurn string
	metaTurnState                                                   string
	promptCacheKey, model                                           string
}

func (w *convWire) identity(frame int) convIdentity {
	payload := w.body
	if w.ws && frame < len(w.frames) {
		payload = w.frames[frame]
	}
	cm := gjson.GetBytes(payload, "client_metadata")
	return convIdentity{
		auth:             w.header.Get("Authorization"),
		sessionHdr:       w.header.Get("Session-Id"),
		threadHdr:        w.header.Get("Thread-Id"),
		windowHdr:        w.header.Get("X-Codex-Window-Id"),
		turnState:        w.header.Get("X-Codex-Turn-State"),
		routingHint:      w.header.Get("X-Codex-Routing-Hint"),
		turnMeta:         w.header.Get("X-Codex-Turn-Metadata"),
		metaSession:      cm.Get("session_id").String(),
		metaThread:       cm.Get("thread_id").String(),
		metaWindow:       cm.Get(`x-codex-window-id`).String(),
		metaInstallation: cm.Get(`x-codex-installation-id`).String(),
		metaTurn:         cm.Get("turn_id").String(),
		metaTurnState:    cm.Get(`x-codex-turn-state`).String(),
		promptCacheKey:   gjson.GetBytes(payload, "prompt_cache_key").String(),
		model:            gjson.GetBytes(payload, "model").String(),
	}
}

// postConv 向网关发 /v1/responses（key = ck-1/ck-2，hdr 附加头），读完并返回状态码。
func postConv(t *testing.T, p *Proxy, key, body string, hdr map[string]string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	AIRouter(p).ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// wsConvSession 经网关开一条 WS 会话：写 frames，每帧读到 response.completed 为止，
// 然后正常关闭。
func wsConvSession(t *testing.T, p *Proxy, hdr http.Header, frames ...string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer srv.Close()
	h := http.Header{"Authorization": {"Bearer ck-1"}}
	for k, vs := range hdr {
		h[k] = vs
	}
	c := dialResponsesWSHeaders(t, srv, h)
	defer c.CloseNow()
	for _, f := range frames {
		require.NoError(t, c.Write(context.Background(), websocket.MessageText, []byte(f)))
		for {
			b := readResponsesWSFrame(t, c)
			typ := gjson.GetBytes(b, "type").String()
			require.NotEqual(t, "error", typ, "frame=%s", b)
			if typ == "response.completed" {
				break
			}
		}
	}
	require.NoError(t, c.Close(websocket.StatusNormalClosure, ""))
}
