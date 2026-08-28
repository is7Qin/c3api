// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

func newImplicitTpl(upstream string, format domain.RequestFormat, mode domain.ModelMappingMode) *domain.Template {
	return &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{format}, Models: []string{"client-model"},
		ModelMapping: map[string]domain.ModelMappingEntry{
			"client-model": {MappedModel: "upstream-model", Mode: mode},
		},
	}
}

func TestChatImplicitRESTRewritesModel(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := json.Marshal(map[string]any{"id": "c1", "object": "chat.completion", "model": "upstream-model", "usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	defer up.Close()
	store := &captureLogStore{}
	tpl := newImplicitTpl(up.URL, domain.FormatOpenAIChat, domain.ModelMappingModeImplicit)
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"client-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code)
	require.Equal(t, "client-model", gjson.Get(rec.Body.String(), "model").String(), "implicit REST must rewrite model to client-model")
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, "client-model", func() string {
		for _, l := range store.logs {
			return l.MappedModel
		}
		return ""
	}(), "implicit MappedModel = client-model per matrix")
}

func TestChatExplicitRESTPreservesUpstreamModel(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := json.Marshal(map[string]any{"id": "c1", "object": "chat.completion", "model": "upstream-model", "usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	defer up.Close()
	store := &captureLogStore{}
	tpl := newImplicitTpl(up.URL, domain.FormatOpenAIChat, domain.ModelMappingModeExplicit)
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"client-model","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code)
	require.Equal(t, "upstream-model", gjson.Get(rec.Body.String(), "model").String(), "explicit must preserve upstream model")
}

func TestChatImplicitSSERewritesEveryFrame(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		for _, payload := range []string{
			`{"id":"c1","object":"chat.completion.chunk","model":"upstream-model","choices":[{"delta":{"content":"hi"}}]}`,
			`{"id":"c1","object":"chat.completion.chunk","model":"upstream-model","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		} {
			_, _ = w.Write([]byte("data: " + payload + "\n\n"))
			fl.Flush()
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		fl.Flush()
	}))
	defer up.Close()
	store := &captureLogStore{}
	tpl := newImplicitTpl(up.URL, domain.FormatOpenAIChat, domain.ModelMappingModeImplicit)
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"client-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, `"model":"client-model"`, "implicit SSE must rewrite model")
	require.NotContains(t, body, `"model":"upstream-model"`, "upstream model must not leak")
	require.Contains(t, body, "data: [DONE]")
}

func TestChatExplicitSSEPreservesUpstream(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		_, _ = w.Write([]byte(`data: {"model":"upstream-model","choices":[]}` + "\n\n"))
		fl.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		fl.Flush()
	}))
	defer up.Close()
	tpl := newImplicitTpl(up.URL, domain.FormatOpenAIChat, domain.ModelMappingModeExplicit)
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, &captureLogStore{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"client-model","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), `"model":"upstream-model"`)
}

func TestAnthropicImplicitRESTRewritesMessageModel(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := json.Marshal(map[string]any{"id": "msg_1", "type": "message", "role": "assistant", "model": "upstream-model", "content": []any{}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1}})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	defer up.Close()
	tpl := newImplicitTpl(up.URL, domain.FormatAnthropic, domain.ModelMappingModeImplicit)
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, &captureLogStore{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"client-model","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "ck-1")
	p.HandleAnthropic(rec, req)
	require.Equal(t, 200, rec.Code)
	require.Equal(t, "client-model", gjson.Get(rec.Body.String(), "model").String())
}

func TestAnthropicImplicitSSERewritesMessageModel(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		_, _ = w.Write([]byte(`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"upstream-model","usage":{"input_tokens":1}}}` + "\n\n"))
		fl.Flush()
		_, _ = w.Write([]byte(`event: message_delta` + "\n" + `data: {"type":"message_delta","usage":{"output_tokens":1}}` + "\n\n"))
		fl.Flush()
		_, _ = w.Write([]byte(`event: message_stop` + "\n" + `data: {"type":"message_stop"}` + "\n\n"))
		fl.Flush()
	}))
	defer up.Close()
	tpl := newImplicitTpl(up.URL, domain.FormatAnthropic, domain.ModelMappingModeImplicit)
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, &captureLogStore{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"client-model","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("x-api-key", "ck-1")
	p.HandleAnthropic(rec, req)
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), `"model":"client-model"`)
	require.NotContains(t, rec.Body.String(), `"model":"upstream-model"`)
}

func TestResponsesImplicitRESTRewritesResponseModel(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := json.Marshal(map[string]any{"id": "rsp_1", "object": "response", "model": "upstream-model", "status": "completed", "output": []any{}, "usage": map[string]any{"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}))
	defer up.Close()
	tpl := newImplicitTpl(up.URL, domain.FormatOpenAIResponses, domain.ModelMappingModeImplicit)
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, &captureLogStore{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"client-model","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	p.HandleResponses(rec, req)
	require.Equal(t, 200, rec.Code)
	body := rec.Body.String()
	require.Equal(t, "client-model", gjson.Get(body, "model").String(), "implicit must rewrite top-level model")
}

func TestResponsesImplicitSSERewritesResponseModel(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fl := w.(http.Flusher)
		_, _ = w.Write([]byte(`event: response.completed` + "\n" + `data: {"type":"response.completed","response":{"id":"rsp_1","model":"upstream-model","status":"completed"}}` + "\n\n"))
		fl.Flush()
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
		fl.Flush()
	}))
	defer up.Close()
	tpl := newImplicitTpl(up.URL, domain.FormatOpenAIResponses, domain.ModelMappingModeImplicit)
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, &captureLogStore{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"client-model","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	p.HandleResponses(rec, req)
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), `"model":"client-model"`)
	require.NotContains(t, rec.Body.String(), `"model":"upstream-model"`)
}

func TestImplicitHTTPErrorBodyUnchanged(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"bad","type":"upstream_error"}}`))
	}))
	defer up.Close()
	tpl := newImplicitTpl(up.URL, domain.FormatOpenAIChat, domain.ModelMappingModeImplicit)
	p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, &captureLogStore{}, nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"client-model","messages":[]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	p.HandleChat(rec, req)
	require.Equal(t, 400, rec.Code)
	require.Contains(t, rec.Body.String(), "bad")
	require.NotContains(t, rec.Body.String(), "client-model")
}

func TestImplicitExplicitNoAllocDirectPath(t *testing.T) {
	tplExplicit := newImplicitTpl("http://unused", domain.FormatOpenAIChat, domain.ModelMappingModeExplicit)
	_ = tplExplicit
	body := []byte(`{"model":"upstream-model","response":{"model":"upstream-model"}}`)
	allocs := testing.AllocsPerRun(1000, func() {
		_ = newResponseModelRewriter("")
		_ = newResponseModelSSEMapper("")
		_ = rewriteResponseModelJSON(body, "")
	})
	require.Zero(t, allocs)
}
