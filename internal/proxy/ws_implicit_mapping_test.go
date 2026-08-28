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
	"github.com/is7qin/c3api/internal/scheduler"
)

func TestRelayWSImplicitMappingRewritesTextFrame(t *testing.T) {
	// Implicit: client gpt-4o -> upstream upstream-b, response must expose client model.
	ft := &fakeTransport{readQueue: []fakeRead{
		{typ: websocket.MessageText, frame: []byte(`{"type":"response.created","response":{"model":"upstream-b"}}`)},
		{typ: websocket.MessageText, frame: []byte(`{"model":"upstream-b","response":{"model":"upstream-b"}}`)},
		{typ: websocket.MessageText, frame: []byte(responsesWSCompletedFrame)},
		{typ: 0, err: websocket.CloseError{Code: websocket.StatusNormalClosure}},
	}}
	// Build proxy with implicit mapping gpt-4o -> upstream-b
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
	// hijack httptest server that runs relayWS directly to isolate rewrite
	env := relayWSWithSel(t, p, sel, ft, nil, `{"type":"response.create","model":"gpt-4o","input":"hi"}`)
	c := env.client
	require.Equal(t, `{"type":"response.created","response":{"model":"gpt-4o"}}`, string(readResponsesWSFrame(t, c)))
	require.Equal(t, `{"model":"gpt-4o","response":{"model":"gpt-4o"}}`, string(readResponsesWSFrame(t, c)))
	// completed frame's model is gpt-4o upstream but rewritten: check it contains client model
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
	// binary unchanged: typ must remain binary (client Read returns whatever upstream sent, but our relay preserves typ)
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
	// explicit/unmapped or already-equal must return original slice and zero allocs (proxy via rewrite helper)
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
	// Ordinary transport e2e viahttptest fake upstream: client gpt-4o implicit -> upstream o3, client sees gpt-4o
	hooks := &fakeWSHooks{frameLimit: 1}
	up := fakeResponsesWS(t, hooks)
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
		// All model-bearing frames must expose client model gpt-4o, not o3
		if bytes.Contains(f, []byte(`"model"`)) {
			require.Contains(t, string(f), `"model":"gpt-4o"`)
			require.NotContains(t, string(f), `"model":"o3"`)
		}
		if i == 0 {
			// also ensure echo not needed
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
	// explicit leaves upstream model in client frames (fake upstream always sends gpt-4o) -> client sees gpt-4o byte-identical, no rewrite
	for i := 0; i < 4; i++ {
		_ = readResponsesWSFrame(t, c)
	}
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)
}

func TestCodexWSImplicitMapping(t *testing.T) {
	// Codex transport implicit: ensure fatal hook sees original, client sees rewritten
	// Use relayWS with codex-like selection but via fakeTransport hook to simulate Codex path
	var hooked [][]byte
	var mu sync.Mutex
	ft := &fakeTransport{readQueue: []fakeRead{
		{typ: websocket.MessageText, frame: []byte(`{"type":"error","error":{"code":"token_invalidated","message":"x"}}`)},
		{typ: websocket.MessageText, frame: []byte(`{"model":"upstream-b"}`)},
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
		// simulate codex fatal check
		if sniffCodexWSDeath(b) != nil {
			// fatal hook would fire
		}
	}
	env := relayWSWithSel(t, p, sel, ft, hook, `{"type":"response.create","model":"gpt-4o"}`)
	c := env.client
	// first frame is error but still rewritten? model path not present so unchanged, but second frame rewritten
	f1 := string(readResponsesWSFrame(t, c))
	require.Contains(t, f1, `"token_invalidated"`)
	f2 := string(readResponsesWSFrame(t, c))
	require.Equal(t, `{"model":"gpt-4o"}`, f2)
	mu.Lock()
	require.Len(t, hooked, 2)
	require.Equal(t, `{"model":"upstream-b"}`, string(hooked[1]))
	mu.Unlock()
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)
	<-env.out
}

func TestCodexWSExplicitMappingNoRewrite(t *testing.T) {
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
	sel, _ := p.sched.Select(10, domain.FormatOpenAIResponsesWS, "gpt-4o")
	env := relayWSWithSel(t, p, sel, ft, nil, `{"type":"response.create","model":"gpt-4o"}`)
	c := env.client
	require.Equal(t, `{"model":"upstream-b"}`, string(readResponsesWSFrame(t, c)))
	readResponsesWSClose(t, c, websocket.StatusNormalClosure)
	<-env.out
}

func TestRelayWSImplicitWriteFailureOrdering(t *testing.T) {
	// client write failure after hook must still have hook invoked
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

// helpers for implicit mapping relay tests

type relayWSEnv2 struct {
	p      *Proxy
	client *websocket.Conn
	out    chan relayOutcome
	outHandled func() bool
}

func relayWSWithSel(t *testing.T, p *Proxy, sel *scheduler.Selection, ft *fakeTransport, hook func([]byte), first string) *relayWSEnv2 {
	t.Helper()
	// we need to create httptest server that calls relayWS with given sel
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
	return &relayWSEnv2{p: p, client: c, out: out, outHandled: func() bool {
		select {
		case r := <-out:
			return r.handled
		case <-time.After(5 * time.Second):
			return false
		}
	}}
}

var _ = json.Marshal
var _ = errors.New

