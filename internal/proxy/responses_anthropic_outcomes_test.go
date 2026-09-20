// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/quality"
)

func fakeResponsesOutcome(t *testing.T, mode string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			w.WriteHeader(404)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		stream, _ := body["stream"].(bool)
		if mode == "429" && !stream {
			w.WriteHeader(429)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "rate limited"}})
			return
		}
		if mode == "500" && !stream {
			w.WriteHeader(500)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "boom"}})
			return
		}
		if mode == "400" && !stream {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "bad request"}})
			return
		}
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			fl := w.(http.Flusher)
			_, _ = w.Write([]byte("event: response.completed\n"))
			_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"usage\":{\"input_tokens\":3,\"output_tokens\":5,\"input_tokens_details\":{\"cached_tokens\":1}}}}\n\n"))
			fl.Flush()
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "r1", "object": "response", "model": body["model"],
			"usage": map[string]any{"input_tokens": 3, "output_tokens": 5, "input_tokens_details": map[string]any{"cached_tokens": 1}},
		})
	}))
}

func fakeAnthropicOutcome(t *testing.T, mode string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(404)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		stream, _ := body["stream"].(bool)
		if mode == "429" && !stream {
			w.WriteHeader(429)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "rate limited"}})
			return
		}
		if mode == "400" && !stream {
			w.WriteHeader(400)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "bad request"}})
			return
		}
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			fl := w.(http.Flusher)
			_, _ = w.Write([]byte("event: message_start\n"))
			_, _ = w.Write([]byte("data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":4,\"cache_read_input_tokens\":1,\"cache_creation_input_tokens\":2}}}\n\n"))
			fl.Flush()
			_, _ = w.Write([]byte("event: message_delta\n"))
			_, _ = w.Write([]byte("data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":6}}\n\n"))
			fl.Flush()
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			fl.Flush()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "m1", "type": "message", "model": body["model"],
			"usage": map[string]any{"input_tokens": 4, "output_tokens": 6, "cache_read_input_tokens": 1, "cache_creation_input_tokens": 2},
		})
	}))
}

func newTestProxyResponses(t *testing.T, upstream string, accountID int64, logs *captureLogStore) *Proxy {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}, Models: []string{"gpt-4o"},
	}
	return newTestProxyTplTimeoutLogs(t, tpl, accountID, true, 30*time.Second, logs, nil)
}

func newTestProxyAnthropicCapture(t *testing.T, upstream string, accountID int64, logs *captureLogStore) *Proxy {
	t.Helper()
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatAnthropic}, Models: []string{"gpt-4o"},
	}
	return newTestProxyTplTimeoutLogs(t, tpl, accountID, true, 30*time.Second, logs, nil)
}

func TestResponsesAndAnthropic_Outcomes_ExactlyOneObservation(t *testing.T) {
	upR := fakeResponsesOutcome(t, "")
	defer upR.Close()
	storeR := &captureLogStore{}
	pR := newTestProxyResponses(t, upR.URL, 1, storeR)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4o","input":"hi"}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	pR.HandleResponses(rec, req)
	require.Equal(t, 200, rec.Code)
	require.NoError(t, pR.rec.Close(context.Background()))
	storeR.mu.Lock()
	require.Len(t, storeR.logs, 1)
	require.Equal(t, int64(2), storeR.logs[0].InputTokens)
	require.Equal(t, int64(5), storeR.logs[0].OutputTokens)
	require.Equal(t, int64(1), storeR.logs[0].CacheReadTokens)
	storeR.mu.Unlock()

	upA := fakeAnthropicOutcome(t, "")
	defer upA.Close()
	storeA := &captureLogStore{}
	pA := newTestProxyAnthropicCapture(t, upA.URL, 1, storeA)
	reqA := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-4o","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
	reqA.Header.Set("Authorization", "Bearer ck-1")
	recA := httptest.NewRecorder()
	pA.HandleAnthropic(recA, reqA)
	require.Equal(t, 200, recA.Code)
	require.NoError(t, pA.rec.Close(context.Background()))
	storeA.mu.Lock()
	require.Len(t, storeA.logs, 1)
	require.Equal(t, int64(4), storeA.logs[0].InputTokens)
	require.Equal(t, int64(6), storeA.logs[0].OutputTokens)
	require.Equal(t, int64(1), storeA.logs[0].CacheReadTokens)
	require.Equal(t, int64(2), storeA.logs[0].CacheCreationTokens)
	storeA.mu.Unlock()
}

func TestResponsesAnthropic_Outcomes_StreamTTFTAndUsage(t *testing.T) {
	upR := fakeResponsesOutcome(t, "")
	defer upR.Close()
	storeR := &captureLogStore{}
	pR := newTestProxyResponses(t, upR.URL, 1, storeR)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4o","input":"hi","stream":true}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	pR.HandleResponses(rec, req)
	require.Equal(t, 200, rec.Code)
	require.Contains(t, rec.Body.String(), "response.completed")
	require.NoError(t, pR.rec.Close(context.Background()))
	storeR.mu.Lock()
	require.Len(t, storeR.logs, 1)
	require.Equal(t, int64(2), storeR.logs[0].InputTokens)
	require.NotNil(t, storeR.logs[0].TTFTMS)
	storeR.mu.Unlock()

	upA := fakeAnthropicOutcome(t, "")
	defer upA.Close()
	storeA := &captureLogStore{}
	pA := newTestProxyAnthropicCapture(t, upA.URL, 1, storeA)
	reqA := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-4o","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"stream":true}`))
	reqA.Header.Set("Authorization", "Bearer ck-1")
	recA := httptest.NewRecorder()
	pA.HandleAnthropic(recA, reqA)
	require.Equal(t, 200, recA.Code)
	require.Contains(t, recA.Body.String(), "message_start")
	require.NoError(t, pA.rec.Close(context.Background()))
	storeA.mu.Lock()
	require.Len(t, storeA.logs, 1)
	require.Equal(t, int64(4), storeA.logs[0].InputTokens)
	require.Equal(t, int64(6), storeA.logs[0].OutputTokens)
	require.NotNil(t, storeA.logs[0].TTFTMS)
	storeA.mu.Unlock()
}

func TestResponsesAnthropic_Outcomes_ConcurrentExactlyOnce(t *testing.T) {
	upR := fakeResponsesOutcome(t, "")
	defer upR.Close()
	store := &captureLogStore{}
	p := newTestProxyResponses(t, upR.URL, 1, store)
	var wg sync.WaitGroup
	var done atomic.Int32
	barrier := make(chan struct{})
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-barrier
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4o","input":"hi"}`))
			req.Header.Set("Authorization", "Bearer ck-1")
			rec := httptest.NewRecorder()
			p.HandleResponses(rec, req)
			if rec.Code == 200 || rec.Code == 429 {
				done.Add(1)
			}
		}()
	}
	close(barrier)
	wg.Wait()
	require.Equal(t, int32(16), done.Load())
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, int64(0), ri.Concurrency)
}

func TestResponsesAnthropic_Outcomes_ClientCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	upR := fakeResponsesOutcome(t, "")
	defer upR.Close()
	store := &captureLogStore{}
	p := newTestProxyResponses(t, upR.URL, 1, store)
	// Use context that is already cancelled to simulate client disconnect before upstream response
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4o","input":"hi","stream":true}`)).WithContext(ctx)
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleResponses(rec, req)
	// Stream with cancelled context should not panic and should release slot
	ri, ok := p.sched.Runtime(1)
	require.True(t, ok)
	require.Equal(t, int64(0), ri.Concurrency)
}

func TestResponsesAnthropic_Outcomes_StatusPreservation(t *testing.T) {
	for _, mode := range []string{"429", "500", "400"} {
		upR := fakeResponsesOutcome(t, mode)
		store := &captureLogStore{}
		pR := newTestProxyResponses(t, upR.URL, 1, store)
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4o","input":"hi"}`))
		req.Header.Set("Authorization", "Bearer ck-1")
		rec := httptest.NewRecorder()
		pR.HandleResponses(rec, req)
		expected := map[string]int{"429": 429, "500": 502, "400": 400}[mode]
		require.Equal(t, expected, rec.Code, "responses mode %s", mode)
		upR.Close()
		require.NoError(t, pR.rec.Close(context.Background()))
		require.NoError(t, pR.errlog.Close(context.Background()))
		upA := fakeAnthropicOutcome(t, mode)
		storeA := &captureLogStore{}
		pA := newTestProxyAnthropicCapture(t, upA.URL, 1, storeA)
		if mode == "429" || mode == "400" {
			reqA := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"gpt-4o","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`))
			reqA.Header.Set("Authorization", "Bearer ck-1")
			recA := httptest.NewRecorder()
			pA.HandleAnthropic(recA, reqA)
			require.Equal(t, expected, recA.Code, "anthropic mode %s", mode)
		}
		upA.Close()
		require.NoError(t, pA.rec.Close(context.Background()))
		require.NoError(t, pA.errlog.Close(context.Background()))
		// v3-F1: the FlowChain compile pin is deleted with the box; the
		// quality import below still pins the lane for the counters above.
		_ = quality.FlowChainIncompleteObserved
	}
}
