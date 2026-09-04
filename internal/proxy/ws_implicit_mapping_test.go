package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/aiclient"
)

func TestRelayWSImplicitMappingRewritesTextFrame(t *testing.T) {
	ft := &fakeTransport{readQueue: []fakeRead{
		{typ: websocket.MessageText, frame: []byte(`{"type":"response.created","response":{"model":"upstream-b"}}`)},
		{typ: websocket.MessageText, frame: []byte(`{"model":"upstream-b","response":{"model":"upstream-b"}}`)},
		{typ: websocket.MessageText, frame: []byte(responsesWSCompletedFrame)},
		{typ: 0, err: websocket.CloseError{Code: websocket.StatusNormalClosure}},
	}}
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: "http://127.0.0.1:1",
		CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS},
		Models: []string{"gpt-4o"}, ModelMapping: map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "upstream-b", Mode: domain.ModelMappingModeImplicit}},
	}
	store := &captureLogStore{}
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, nil)
	sel, err := p.sched.Select(10, domain.FormatOpenAIResponsesWS, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, "upstream-b", sel.Model)
	require.Equal(t, "gpt-4o", sel.ClientResponseModel("gpt-4o"))
	env := relayWSWithSel(t, p, sel, ft, nil, `{"type":"response.create","model":"gpt-4o","input":"hi"}`)
	c := env.client
	require.Equal(t, `{"type":"response.created","response":{"model":"gpt-4o"}}`, string(readResponsesWSFrame(t, c)))
	require.Equal(t, `{"model":"gpt-4o","response":{"model":"gpt-4o"}}`, string(readResponsesWSFrame(t, c)))
	f3 := string(readResponsesWSFrame(t, c))
	require.Contains(t, f3, `"model":"gpt-4o"`)
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)
	r := <-env.out
	require.True(t, r.handled)
}

func TestRelayWSExplicitMappingLeavesFrameByteIdentical(t *testing.T) {
	ft := &fakeTransport{readQueue: []fakeRead{
		{typ: websocket.MessageText, frame: []byte(`{"model":"upstream-b"}`)},
		{typ: 0, err: websocket.CloseError{Code: websocket.StatusNormalClosure}},
	}}
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: "http://127.0.0.1:1",
		CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS},
		Models: []string{"gpt-4o"}, ModelMapping: map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "upstream-b", Mode: domain.ModelMappingModeExplicit}},
	}
	store := &captureLogStore{}
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, nil)
	sel, err := p.sched.Select(10, domain.FormatOpenAIResponsesWS, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, "", sel.ClientResponseModel("gpt-4o"))
	env := relayWSWithSel(t, p, sel, ft, nil, `{"type":"response.create","model":"gpt-4o"}`)
	c := env.client
	require.Equal(t, `{"model":"upstream-b"}`, string(readResponsesWSFrame(t, c)))
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)
	<-env.out
}

func TestRelayWSImplicitMappingHookSeesOriginal(t *testing.T) {
	var hooked [][]byte
	var mu sync.Mutex
	ft := &fakeTransport{readQueue: []fakeRead{
		{typ: websocket.MessageText, frame: []byte(`{"model":"upstream-b","response":{"model":"upstream-b"}}`)},
		{typ: 0, err: websocket.CloseError{Code: websocket.StatusNormalClosure}},
	}}
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: "http://127.0.0.1:1",
		CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS},
		Models: []string{"gpt-4o"}, ModelMapping: map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "upstream-b", Mode: domain.ModelMappingModeImplicit}},
	}
	store := &captureLogStore{}
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, nil)
	sel, _ := p.sched.Select(10, domain.FormatOpenAIResponsesWS, "gpt-4o")
	hook := func(b []byte) {
		mu.Lock()
		hooked = append(hooked, append([]byte(nil), b...))
		mu.Unlock()
	}
	env := relayWSWithSel(t, p, sel, ft, hook, `{"type":"response.create","model":"gpt-4o"}`)
	c := env.client
	got := string(readResponsesWSFrame(t, c))
	require.Equal(t, `{"model":"gpt-4o","response":{"model":"gpt-4o"}}`, got, "client must see rewritten")
	mu.Lock()
	require.Len(t, hooked, 1)
	require.Equal(t, `{"model":"upstream-b","response":{"model":"upstream-b"}}`, string(hooked[0]), "hook must see original target model")
	mu.Unlock()
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)
	<-env.out
}

func TestRelayWSImplicitMappingBinaryAndMalformedUnchanged(t *testing.T) {
	ft := &fakeTransport{readQueue: []fakeRead{
		{typ: websocket.MessageBinary, frame: []byte(`{"model":"upstream-b"}`)},
		{typ: websocket.MessageText, frame: []byte(`not-json{{{`)},
		{typ: websocket.MessageText, frame: []byte(`{"model":123}`)},
		{typ: 0, err: websocket.CloseError{Code: websocket.StatusNormalClosure}},
	}}
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: "http://127.0.0.1:1",
		CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS},
		Models: []string{"gpt-4o"}, ModelMapping: map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "upstream-b", Mode: domain.ModelMappingModeImplicit}},
	}
	store := &captureLogStore{}
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, nil)
	sel, _ := p.sched.Select(10, domain.FormatOpenAIResponsesWS, "gpt-4o")
	env := relayWSWithSel(t, p, sel, ft, nil, `{"type":"response.create","model":"gpt-4o"}`)
	c := env.client
	typ, b, err := c.Read(context.Background())
	require.NoError(t, err)
	require.Equal(t, websocket.MessageBinary, typ)
	require.Equal(t, `{"model":"upstream-b"}`, string(b))
	require.Equal(t, `not-json{{{`, string(readResponsesWSFrame(t, c)))
	require.Equal(t, `{"model":123}`, string(readResponsesWSFrame(t, c)))
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)
	<-env.out
}

func TestRelayWSImplicitMappingNoAllocNoOp(t *testing.T) {
	body := []byte(`{"model":"upstream-b"}`)
	got := rewriteResponseModelJSON(body, "")
	require.Same(t, &body[0], &got[0])
	allocs := testing.AllocsPerRun(1000, func() {
		modelRewriteSink = rewriteResponseModelJSON(body, "")
	})
	require.Zero(t, allocs)

	body2 := []byte(`{"model":"gpt-4o"}`)
	got2 := rewriteResponseModelJSON(body2, "gpt-4o")
	require.Same(t, &body2[0], &got2[0])
	allocs2 := testing.AllocsPerRun(1000, func() {
		modelRewriteSink = rewriteResponseModelJSON(body2, "gpt-4o")
	})
	require.Zero(t, allocs2)
}

func TestResponsesWSImplicitMapping(t *testing.T) {
	hooks := &fakeWSHooks{frameLimit: 1}
	up := fakeResponsesWSWithModel(t, hooks, "o3")
	defer up.Close()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS},
		Models: []string{"gpt-4o"}, ModelMapping: map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "o3", Mode: domain.ModelMappingModeImplicit}},
	}
	store := &captureLogStore{}
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, nil)
	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer srv.Close()
	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	for i := 0; i < 4; i++ {
		f := readResponsesWSFrame(t, c)
		if bytes.Contains(f, []byte(`"model"`)) {
			require.Contains(t, string(f), `"model":"gpt-4o"`)
			require.NotContains(t, string(f), `"model":"o3"`)
		}
	}
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)
	hooks.mu.Lock()
	require.Len(t, hooks.frames, 1)
	require.Contains(t, hooks.frames[0], `"model":"o3"`, "upstream must receive target model")
	hooks.mu.Unlock()
}

func TestResponsesWSExplicitMapping(t *testing.T) {
	hooks := &fakeWSHooks{frameLimit: 1}
	up := fakeResponsesWS(t, hooks)
	defer up.Close()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: up.URL,
		CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS},
		Models: []string{"gpt-4o"}, ModelMapping: map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "o3", Mode: domain.ModelMappingModeExplicit}},
	}
	store := &captureLogStore{}
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, nil)
	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer srv.Close()
	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	for i := 0; i < 4; i++ {
		_ = readResponsesWSFrame(t, c)
	}
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)
}

func TestCodexWSImplicitMapping(t *testing.T) {
	store := &captureLogStore{}
	up, upstreamFrames := newCodexWSMappingUpstream(t, "upstream-b", true)
	defer up.Close()
	p, recorder := newCodexWSProxyWithMapping(t, up.URL, domain.ModelMappingModeImplicit, store)
	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer srv.Close()
	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	f1 := string(readResponsesWSFrame(t, c))
	require.Contains(t, f1, `"token_invalidated"`, "death frame must be forwarded")
	f2 := string(readResponsesWSFrame(t, c))
	require.Contains(t, f2, `"model":"gpt-4o"`, "client must see rewritten client model")
	require.NotContains(t, f2, `"model":"upstream-b"`)
	require.Equal(t, 1, recorderCalls(recorder), "fatal hook must observe death frame")
	_, acc, reason := recorder.snapshot()
	require.Equal(t, int64(10), acc)
	require.Contains(t, reason, "token_invalidated")
	upstreamFrames.mu.Lock()
	require.Len(t, upstreamFrames.frames, 1)
	require.Contains(t, upstreamFrames.frames[0], `"model":"upstream-b"`, "hook must have seen original before rewrite")
	upstreamFrames.mu.Unlock()
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)
}

func TestCodexWSExplicitMappingNoRewrite(t *testing.T) {
	store := &captureLogStore{}
	up, _ := newCodexWSMappingUpstream(t, "upstream-b", false)
	defer up.Close()
	p, recorder := newCodexWSProxyWithMapping(t, up.URL, domain.ModelMappingModeExplicit, store)
	srv := httptest.NewServer(http.HandlerFunc(p.HandleResponsesWS))
	defer srv.Close()
	c := dialResponsesWS(t, srv)
	defer c.CloseNow()
	require.NoError(t, c.Write(context.Background(), websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-4o","input":"hi"}`)))
	f1 := string(readResponsesWSFrame(t, c))
	require.Contains(t, f1, `"model":"upstream-b"`, "explicit mapping must leave upstream model byte-identical")
	require.NotContains(t, f1, `"model":"gpt-4o"`)
	require.Equal(t, 0, recorderCalls(recorder), "explicit mapping death-free upstream must not trigger fatal")
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)
}

func TestRelayWSImplicitWriteFailureOrdering(t *testing.T) {
	deathFrame := []byte(`{"type":"error","error":{"code":"token_invalidated","message":"x"}}`)
	hookCalled := make(chan []byte, 1)
	ft := &fakeTransport{readQueue: []fakeRead{{typ: websocket.MessageText, frame: deathFrame}}}
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: "http://127.0.0.1:1",
		CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS},
		Models: []string{"gpt-4o"}, ModelMapping: map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "upstream-b", Mode: domain.ModelMappingModeImplicit}},
	}
	store := &captureLogStore{}
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, nil)
	sel, _ := p.sched.Select(10, domain.FormatOpenAIResponsesWS, "gpt-4o")
	env := relayWSWithSel(t, p, sel, ft, func(f []byte) { hookCalled <- append([]byte(nil), f...) }, `{"type":"response.create","model":"gpt-4o"}`)
	c := env.client
	c.CloseNow()
	select {
	case f := <-hookCalled:
		require.Equal(t, string(deathFrame), string(f))
	case <-time.After(3 * time.Second):
		t.Fatal("hook not called before client write failure")
	}
	<-env.out
}

func fakeResponsesWSWithModel(t *testing.T, hooks *fakeWSHooks, model string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(404)
			return
		}
		hooks.mu.Lock()
		hooks.headers = append(hooks.headers, r.Header.Clone())
		hooks.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer sk-upstream" {
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("Responses-Websockets") != aiclient.ResponsesWSBetaHeader {
			w.WriteHeader(400)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{
			CompressionMode: websocket.CompressionContextTakeover,
		})
		if err != nil {
			return
		}
		defer c.CloseNow()
		streamed := false
		n := 0
		for {
			typ, msg, err := c.Read(context.Background())
			if err != nil {
				return
			}
			hooks.mu.Lock()
			hooks.frames = append(hooks.frames, string(msg))
			hooks.mu.Unlock()
			if !streamed {
				streamed = true
				completed := `{"type":"response.completed","response":{"id":"rsp_ws_1","status":"completed","model":"` + model + `","output":[],"usage":{"input_tokens":3,"output_tokens":5,"total_tokens":8,"input_tokens_details":{"cached_tokens":1,"text_tokens":2,"audio_tokens":0},"output_tokens_details":{"reasoning_tokens":2,"text_tokens":3,"audio_tokens":0},"cache_creation":{"ephemeral_5m_input_tokens":2,"ephemeral_1h_input_tokens":1}}}}`
				for _, f := range []string{
					`{"type":"response.created","response":{"id":"rsp_ws_1","model":"` + model + `"}}`,
					`{"type":"response.output_text.delta","delta":"hi"}`,
					completed,
				} {
					if err := c.Write(context.Background(), typ, []byte(f)); err != nil {
						return
					}
				}
			}
			payload, err := json.Marshal(map[string]any{"type": "echo", "payload": string(msg)})
			if err != nil {
				return
			}
			if err := c.Write(context.Background(), typ, payload); err != nil {
				return
			}
			n++
			if hooks.frameLimit > 0 && n >= hooks.frameLimit {
				_ = c.Close(websocket.StatusNormalClosure, "")
				return
			}
		}
	}))
	return srv
}

type codexMappingUpstream struct {
	mu     sync.Mutex
	frames []string
}

func newCodexWSMappingUpstream(t *testing.T, upstreamModel string, withDeath bool) (*httptest.Server, *codexMappingUpstream) {
	t.Helper()
	mu := &codexMappingUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" && r.URL.Path != "/backend-api/codex/responses" {
			w.WriteHeader(404)
			return
		}
		c, err := websocket.Accept(w, r, &websocket.AcceptOptions{})
		if err != nil {
			return
		}
		defer c.CloseNow()
		streamed := false
		n := 0
		for {
			typ, msg, err := c.Read(context.Background())
			if err != nil {
				return
			}
			mu.mu.Lock()
			mu.frames = append(mu.frames, string(msg))
			mu.mu.Unlock()
			if !streamed {
				streamed = true
				var frames []string
				if withDeath {
					frames = []string{
						`{"type":"error","error":{"code":"token_invalidated","message":"access token invalidated","param":null,"type":"token_invalidated"}}`,
						`{"model":"` + upstreamModel + `","response":{"model":"` + upstreamModel + `"}}`,
					}
				} else {
					frames = []string{
						`{"model":"` + upstreamModel + `","response":{"model":"` + upstreamModel + `"}}`,
					}
				}
				for _, f := range frames {
					if err := c.Write(context.Background(), typ, []byte(f)); err != nil {
						return
					}
				}
			} else {
				payload, err := json.Marshal(map[string]any{"type": "echo", "payload": string(msg)})
				if err != nil {
					return
				}
				if err := c.Write(context.Background(), typ, payload); err != nil {
					return
				}
			}
			n++
			if n >= 1 {
				_ = c.Close(websocket.StatusNormalClosure, "")
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, mu
}

func newCodexWSProxyWithMapping(t *testing.T, upstream string, mode domain.ModelMappingMode, logs *captureLogStore) (*Proxy, *fakeFailureStore) {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: "",
		CredentialType:   credential.TypeCodexOAuth,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS},
		Models:           []string{"gpt-4o"},
		ModelMapping:     map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "upstream-b", Mode: mode}},
	}
	ext := codexWSExt(10, "at-10", "rt-10")
	accs := map[int64][]*domain.Account{10: {{
		ID: 10, TemplateID: tpl.ID, Template: tpl, UpstreamKey: "",
		Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4, Ext: ext,
	}}}
	rec := usage.New(usage.UsageConfig{
		BatchSize: 100, FlushInterval: time.Hour,
		QuotaFlushInterval: time.Hour,
	}, logs, nil)
	cfg := Config{
		MaxBodySize: 1 << 20, FailoverAttempts: 2,
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
		UsageCapture:          true,
	}
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background()))
	sched := scheduler.New(scheduler.Config{DefaultMaxConcurrency: 4, SyncInterval: time.Hour}, noopLoader{accs: accs}, re, nil)
	require.NoError(t, sched.InvalidateAllSync())
	auth := NewAuth(noopKeyLoader{keys: map[string]domain.KeyMeta{
		"ck-1": activeKey(1, 1, 10),
	}}, noopUserLoader{}, nil, true)
	require.NoError(t, auth.Reload(context.Background()))
	hc := &http.Client{Transport: http.DefaultTransport}
	clients := aiclient.NewFactory(hc, aiclient.Config{
		UpstreamTimeout:       5 * time.Second,
		UpstreamStreamTimeout: 30 * time.Second,
	})
	store := &fakeFailureStore{}
	failure := sdkbridge.NewFailureHandler(sdkbridge.FailureDeps{Store: store, Failer: sched, Log: nil})
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{
		QueueSize: 4096, BatchSize: 100,
		FlushInterval: 20 * time.Millisecond,
	}, logs, nil)
	wctx, wcancel := context.WithCancel(context.Background())
	require.NoError(t, errlogW.Start(wctx))
	t.Cleanup(func() { wcancel(); _ = errlogW.Close(context.Background()) })
	codex := sdkbridge.NewCodex(failure)
	codex.SetTransport(newProxyOfficialRewriteTransportWithAssert(t, upstream))
	p := New(cfg, sched, credential.New(), rec, clients, auth, nil, nil, errlogW)
	p.SetCodex(codex)
	return p, store
}

type relayWSEnv2 struct {
	p      *Proxy
	client *websocket.Conn
	out    chan relayOutcome
}

func relayWSWithSel(t *testing.T, p *Proxy, sel *scheduler.Selection, ft *fakeTransport, hook func([]byte), first string) *relayWSEnv2 {
	t.Helper()
	reqID := newReqID()
	start := time.Now()
	out := make(chan relayOutcome, 2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		client, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionContextTakeover})
		if err != nil {
			out <- relayOutcome{}
			return
		}
		defer client.CloseNow()
		handled, fwMsg := p.relayWS(client, ft, hook, r, reqID, 10, start, sel, "gpt-4o", websocket.MessageText, []byte(first))
		out <- relayOutcome{handled: handled, fwMsg: fwMsg}
		close(out)
	}))
	t.Cleanup(srv.Close)
	c := dialResponsesWS(t, srv)
	t.Cleanup(func() { c.CloseNow() })
	return &relayWSEnv2{p: p, client: c, out: out}
}

var _ = json.Marshal
var _ = errors.New
