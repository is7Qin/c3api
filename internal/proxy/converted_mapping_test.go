// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

func newConvertedMappingProxy(t *testing.T, upstream string, pcs []domain.ProtocolConvert, mapping map[string]domain.ModelMappingEntry) *Proxy {
	t.Helper()
	var tplFormats []domain.RequestFormat
	switch pcs[0] {
	case domain.ProtocolConvertChatToResp:
		tplFormats = []domain.RequestFormat{domain.FormatOpenAIResponses}
	case domain.ProtocolConvertMessToResp:
		tplFormats = []domain.RequestFormat{domain.FormatOpenAIResponses}
	case domain.ProtocolConvertRespToMess:
		tplFormats = []domain.RequestFormat{domain.FormatAnthropic}
	case domain.ProtocolConvertChatToMess:
		tplFormats = []domain.RequestFormat{domain.FormatAnthropic}
	}
	tpl := &domain.Template{
		ID: 1, Name: "t", BaseURL: upstream,
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: tplFormats,
		ModelMapping:     mapping,
	}
	accs := map[int64][]*domain.Account{10: {{
		ID: 1, TemplateID: 1, Template: tpl, UpstreamKey: "sk-upstream",
		Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4,
	}}}
	return newConvertedTestProxyAccs(t, accs, pcs)
}

func TestConvertedMappingREST(t *testing.T) {
	dirs := []struct {
		name       string
		dir        domain.ProtocolConvert
		clientPath string
		clientBody string
	}{
		{"chat_to_resp", domain.ProtocolConvertChatToResp, "/v1/chat/completions", `{"model":"client-model","messages":[{"role":"user","content":"hi"}]}`},
		{"mess_to_resp", domain.ProtocolConvertMessToResp, "/v1/messages", `{"model":"client-model","messages":[{"role":"user","content":"hi"}],"max_tokens":100}`},
		{"resp_to_mess", domain.ProtocolConvertRespToMess, "/v1/responses", `{"model":"client-model","input":"hi"}`},
		{"chat_to_mess", domain.ProtocolConvertChatToMess, "/v1/chat/completions", `{"model":"client-model","messages":[{"role":"user","content":"hi"}]}`},
	}
	for _, tc := range dirs {
		t.Run(tc.name+"/explicit_keeps_upstream", func(t *testing.T) {
			up := &capturedUpstream{}
			srv := up.srv(t)
			defer srv.Close()
			m := map[string]domain.ModelMappingEntry{"client-model": {MappedModel: "upstream-model", Mode: domain.ModelMappingModeExplicit}}
			p := newConvertedMappingProxy(t, srv.URL, []domain.ProtocolConvert{tc.dir}, m)
			req := httptest.NewRequest(http.MethodPost, tc.clientPath, strings.NewReader(tc.clientBody))
			req.Header.Set("Authorization", "Bearer ck-1")
			rec := httptest.NewRecorder()
			switch tc.clientPath {
			case "/v1/chat/completions":
				p.HandleChat(rec, req)
			case "/v1/messages":
				p.HandleAnthropic(rec, req)
			case "/v1/responses":
				p.HandleResponses(rec, req)
			}
			require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
			_, body, _ := up.last(t)
			require.Equal(t, "upstream-model", body["model"], "upstream sees target")
			require.Contains(t, rec.Body.String(), `"model":"upstream-model"`, "explicit keeps upstream model in client JSON")
			require.NotContains(t, rec.Body.String(), `"model":"client-model"`)
		})
		t.Run(tc.name+"/implicit_rewrites_to_client", func(t *testing.T) {
			m := map[string]domain.ModelMappingEntry{"client-model": {MappedModel: "upstream-model", Mode: domain.ModelMappingModeImplicit}}
			store := &captureLogStore{}
			var mu sync.Mutex
			var gotUpstreamModel string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				_ = json.NewDecoder(r.Body).Decode(&body)
				mu.Lock()
				gotUpstreamModel, _ = body["model"].(string)
				mu.Unlock()
				switch r.URL.Path {
				case "/v1/responses":
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{
						"id": "rsp_1", "object": "response", "created_at": 1750000000,
						"status": "completed", "model": body["model"],
						"output": []any{map[string]any{
							"id": "msg_1", "type": "message", "status": "completed", "role": "assistant",
							"content": []any{map[string]any{"type": "output_text", "text": "hi", "annotations": []any{}}},
						}},
						"usage": map[string]any{"input_tokens": 3, "output_tokens": 5, "total_tokens": 8},
					})
				case "/v1/messages":
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{
						"id": "msg_1", "type": "message", "role": "assistant",
						"model": body["model"], "content": []any{map[string]any{"type": "text", "text": "hi"}},
						"stop_reason": "end_turn", "stop_sequence": nil,
						"usage": map[string]any{"input_tokens": 3, "output_tokens": 5},
					})
				case "/v1/chat/completions":
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(map[string]any{
						"id": "c_1", "object": "chat.completion", "model": body["model"],
						"choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "hi"}, "finish_reason": "stop"}},
						"usage":   map[string]any{"prompt_tokens": 3, "completion_tokens": 5, "total_tokens": 8},
					})
				}
			}))
			defer srv.Close()
			tpl := &domain.Template{ID: 1, Name: "t", BaseURL: srv.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{func() domain.RequestFormat {
				switch tc.dir {
				case domain.ProtocolConvertChatToResp, domain.ProtocolConvertMessToResp:
					return domain.FormatOpenAIResponses
				default:
					return domain.FormatAnthropic
				}
			}()}, ModelMapping: m}
			accs := map[int64][]*domain.Account{10: {{ID: 1, TemplateID: 1, Template: tpl, UpstreamKey: "sk-upstream", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4}}}
			p2 := newConvertedTestProxyAccsLogs(t, accs, []domain.ProtocolConvert{tc.dir}, store, 30*time.Second)
			req := httptest.NewRequest(http.MethodPost, tc.clientPath, strings.NewReader(tc.clientBody))
			req.Header.Set("Authorization", "Bearer ck-1")
			rec := httptest.NewRecorder()
			switch tc.clientPath {
			case "/v1/chat/completions":
				p2.HandleChat(rec, req)
			case "/v1/messages":
				p2.HandleAnthropic(rec, req)
			case "/v1/responses":
				p2.HandleResponses(rec, req)
			}
			require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
			mu.Lock()
			require.Equal(t, "upstream-model", gotUpstreamModel, "upstream sees target even when implicit")
			mu.Unlock()
			require.Contains(t, rec.Body.String(), `"model":"client-model"`, "implicit rewrites to client model")
			require.NotContains(t, rec.Body.String(), `"model":"upstream-model"`)
			require.NoError(t, p2.rec.Close(context.Background()))
			store.mu.Lock()
			defer store.mu.Unlock()
			require.Len(t, store.logs, 1)
			require.Equal(t, "", store.logs[0].MappedModel, "implicit mapping does not record MappedModel")
			require.Equal(t, int64(8), store.logs[0].TotalTokens)
		})
	}
}

func TestConvertedMappingSSE(t *testing.T) {
	dirs := []struct {
		name       string
		dir        domain.ProtocolConvert
		clientPath string
		clientBody string
		wantDone   bool
	}{
		{"chat_to_resp", domain.ProtocolConvertChatToResp, "/v1/chat/completions", `{"model":"client-model","messages":[{"role":"user","content":"hi"}],"stream":true}`, true},
		{"mess_to_resp", domain.ProtocolConvertMessToResp, "/v1/messages", `{"model":"client-model","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"stream":true}`, false},
		{"resp_to_mess", domain.ProtocolConvertRespToMess, "/v1/responses", `{"model":"client-model","input":"hi","stream":true}`, false},
		{"chat_to_mess", domain.ProtocolConvertChatToMess, "/v1/chat/completions", `{"model":"client-model","messages":[{"role":"user","content":"hi"}],"stream":true}`, true},
	}
	for _, tc := range dirs {
		t.Run(tc.name+"/implicit_each_frame_rewritten", func(t *testing.T) {
			up := &capturedUpstream{}
			srv := up.srv(t)
			defer srv.Close()
			m := map[string]domain.ModelMappingEntry{"client-model": {MappedModel: "upstream-model", Mode: domain.ModelMappingModeImplicit}}
			p := newConvertedMappingProxy(t, srv.URL, []domain.ProtocolConvert{tc.dir}, m)
			req := httptest.NewRequest(http.MethodPost, tc.clientPath, strings.NewReader(tc.clientBody))
			req.Header.Set("Authorization", "Bearer ck-1")
			rec := httptest.NewRecorder()
			switch tc.clientPath {
			case "/v1/chat/completions":
				p.HandleChat(rec, req)
			case "/v1/messages":
				p.HandleAnthropic(rec, req)
			case "/v1/responses":
				p.HandleResponses(rec, req)
			}
			require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
			body := rec.Body.String()
			require.Contains(t, body, "client-model")
			require.NotContains(t, body, "upstream-model")
			require.Contains(t, body, "data:")
			frames := strings.Split(strings.TrimSpace(body), "\n\n")
			for _, f := range frames {
				if strings.TrimSpace(f) == "" {
					continue
				}
				require.True(t, strings.Contains(f, "data:"), "each frame has data: line, got %q", f)
				require.True(t, strings.HasSuffix(f, "\n") || strings.Contains(f, "data:"), "frame boundary preserved")
			}
			if tc.wantDone {
				require.Contains(t, body, "data: [DONE]", "chat direction must emit [DONE]")
			} else {
				if tc.name == "mess_to_resp" {
					require.Contains(t, body, "event: message_stop")
				} else if tc.name == "resp_to_mess" {
					require.Contains(t, body, "event: response.completed")
				}
			}
		})
		t.Run(tc.name+"/explicit_no_rewrite", func(t *testing.T) {
			up := &capturedUpstream{}
			srv := up.srv(t)
			defer srv.Close()
			m := map[string]domain.ModelMappingEntry{"client-model": {MappedModel: "upstream-model", Mode: domain.ModelMappingModeExplicit}}
			p := newConvertedMappingProxy(t, srv.URL, []domain.ProtocolConvert{tc.dir}, m)
			req := httptest.NewRequest(http.MethodPost, tc.clientPath, strings.NewReader(tc.clientBody))
			req.Header.Set("Authorization", "Bearer ck-1")
			rec := httptest.NewRecorder()
			switch tc.clientPath {
			case "/v1/chat/completions":
				p.HandleChat(rec, req)
			case "/v1/messages":
				p.HandleAnthropic(rec, req)
			case "/v1/responses":
				p.HandleResponses(rec, req)
			}
			require.Equal(t, 200, rec.Code)
			body := rec.Body.String()
			require.NotContains(t, body, `"model":"client-model"`, "explicit must not inject client-model, got %s", body)
		})
	}
}

func TestConvertedMappingStreamFramesViaProxy(t *testing.T) {
	t.Run("zero_frame_dropped_via_caller", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fl := w.(http.Flusher)
			fmt.Fprint(w, `event: response.in_progress`+"\n"+`data: {"type":"response.in_progress","response":{"id":"rsp_1","status":"in_progress"}}`+"\n\n")
			fl.Flush()
			fmt.Fprint(w, `event: response.output_text.delta`+"\n"+`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"hi"}`+"\n\n")
			fl.Flush()
			fmt.Fprint(w, `event: response.completed`+"\n"+`data: {"type":"response.completed","response":{"id":"rsp_1","object":"response","created_at":1750000000,"status":"completed","model":"upstream-model","output":[],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}}`+"\n\n")
			fl.Flush()
		}))
		defer srv.Close()
		m := map[string]domain.ModelMappingEntry{"client-model": {MappedModel: "upstream-model", Mode: domain.ModelMappingModeImplicit}}
		p := newConvertedMappingProxy(t, srv.URL, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp}, m)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"client-model","messages":[{"role":"user","content":"hi"}],"stream":true}`))
		req.Header.Set("Authorization", "Bearer ck-1")
		rec := httptest.NewRecorder()
		p.HandleChat(rec, req)
		require.Equal(t, 200, rec.Code)
		body := rec.Body.String()
		require.NotContains(t, body, "response.in_progress", "zero frame must be dropped via StreamMapper.Map")
		require.Contains(t, body, `"delta":{"content":"hi"}`)
		require.Contains(t, body, "client-model")
		require.NotContains(t, body, "upstream-model")
	})
	t.Run("single_frame_mapped_and_rewritten", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, `event: response.output_text.delta`+"\n"+`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"single"}`+"\n\n")
			fmt.Fprint(w, `event: response.completed`+"\n"+`data: {"type":"response.completed","response":{"id":"rsp_1","object":"response","created_at":1750000000,"status":"completed","model":"upstream-model","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
		}))
		defer srv.Close()
		m := map[string]domain.ModelMappingEntry{"client-model": {MappedModel: "upstream-model", Mode: domain.ModelMappingModeImplicit}}
		p := newConvertedMappingProxy(t, srv.URL, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp}, m)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"client-model","messages":[{"role":"user","content":"hi"}],"stream":true}`))
		req.Header.Set("Authorization", "Bearer ck-1")
		rec := httptest.NewRecorder()
		p.HandleChat(rec, req)
		require.Equal(t, 200, rec.Code)
		body := rec.Body.String()
		count := strings.Count(body, "data:")
		require.GreaterOrEqual(t, count, 2, "single delta plus completed chunk plus DONE")
		require.Contains(t, body, "client-model")
		require.Contains(t, body, `"delta":{"content":"single"}`)
		require.NotContains(t, body, "upstream-model")
	})
	t.Run("multiple_frames_from_single_upstream_event", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, `event: response.created`+"\n"+`data: {"type":"response.created","response":{"id":"rsp_1","object":"response","created_at":1750000000,"status":"in_progress","model":"upstream-model","output":[],"usage":null}}`+"\n\n")
			fmt.Fprint(w, `event: response.completed`+"\n"+`data: {"type":"response.completed","response":{"id":"rsp_1","object":"response","created_at":1750000000,"status":"completed","model":"upstream-model","output":[],"usage":{"input_tokens":4,"output_tokens":6,"total_tokens":10}}}`+"\n\n")
		}))
		defer srv.Close()
		m := map[string]domain.ModelMappingEntry{"client-model": {MappedModel: "upstream-model", Mode: domain.ModelMappingModeImplicit}}
		store := &captureLogStore{}
		tpl := &domain.Template{ID: 1, Name: "t", BaseURL: srv.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}, ModelMapping: m}
		accs := map[int64][]*domain.Account{10: {{ID: 1, TemplateID: 1, Template: tpl, UpstreamKey: "sk-upstream", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4}}}
		p := newConvertedTestProxyAccsLogs(t, accs, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp}, store, 30*time.Second)
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"client-model","messages":[{"role":"user","content":"hi"}],"stream":true}`))
		req.Header.Set("Authorization", "Bearer ck-1")
		rec := httptest.NewRecorder()
		p.HandleChat(rec, req)
		require.Equal(t, 200, rec.Code)
		body := rec.Body.String()
		require.Contains(t, body, "client-model")
		require.NotContains(t, body, "upstream-model")
		require.Contains(t, body, "data: [DONE]", "multiple output frames must include terminal [DONE] as separate frame")
		dataCount := strings.Count(body, "data:")
		require.GreaterOrEqual(t, dataCount, 3, "created chunk + completed chunk + DONE = at least 3 data lines")
		require.Equal(t, 2, strings.Count(body, `"model":"client-model"`), "both model-bearing frames rewritten")
		require.NoError(t, p.rec.Close(context.Background()))
		store.mu.Lock()
		defer store.mu.Unlock()
		require.Len(t, store.logs, 1)
		require.Equal(t, int64(10), store.logs[0].TotalTokens, "raw observer extracts usage before rewrite, tokens still correct")
		require.Equal(t, "", store.logs[0].MappedModel)
	})
}

func TestConvertedMappingFrameBoundariesMetadataAndRawOrdering(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `event: response.completed`+"\n"+`data: {"type":"response.completed","response":{"id":"rsp_1","object":"response","created_at":1750000000,"status":"completed","model":"upstream-model","output":[],"usage":{"input_tokens":7,"output_tokens":8,"total_tokens":15}}}`+"\n\n")
		fmt.Fprint(w, `event: response.output_text.delta`+"\n"+`data: {"type":"response.output_text.delta","delta":"hi","response":{"model":"upstream-model"}}`+"\n\n")
	}))
	defer srv.Close()
	m := map[string]domain.ModelMappingEntry{"client-model": {MappedModel: "upstream-model", Mode: domain.ModelMappingModeImplicit}}
	store := &captureLogStore{}
	tpl := &domain.Template{ID: 1, Name: "t", BaseURL: srv.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}, ModelMapping: m}
	accs := map[int64][]*domain.Account{10: {{ID: 1, TemplateID: 1, Template: tpl, UpstreamKey: "sk-upstream", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4}}}
	p := newConvertedTestProxyAccsLogs(t, accs, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp}, store, 30*time.Second)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"client-model","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, 200, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, "client-model")
	require.NotContains(t, body, "upstream-model")
	require.Contains(t, body, "\n\n", "frame boundaries \\n\\n preserved")
	parts := strings.Split(body, "\n\n")
	nonEmpty := 0
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			nonEmpty++
			require.Contains(t, part, "data:", "each frame must have data: line")
		}
	}
	require.GreaterOrEqual(t, nonEmpty, 2, "at least two frames preserved with boundaries")
	require.Contains(t, body, "data: [DONE]", "[DONE] terminal preserved as separate frame after completed mapping")
	require.Contains(t, body, "data: [DONE]\n\n", "[DONE] frame ends with \\n\\n boundary")
	require.Equal(t, 1, strings.Count(body, "data: [DONE]"), "[DONE] appears exactly once")
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, int64(7), store.logs[0].InputTokens, "input tokens extracted from raw ev.Data before rewrite")
	require.Equal(t, int64(8), store.logs[0].OutputTokens, "output tokens extracted from raw before rewrite")
	require.Equal(t, int64(15), store.logs[0].TotalTokens)
	require.Equal(t, "", store.logs[0].MappedModel)
	src := []byte(": comment keep\nid: 1\nevent: response.completed\ndata: {\"response\":{\"model\":\"upstream-model\"}}\n\n")
	rewritten := rewriteConvertedFrames(src, "client-model")
	require.Contains(t, string(rewritten), ": comment keep", "rewrite helper preserves comment metadata")
	require.Contains(t, string(rewritten), "id: 1", "rewrite helper preserves id metadata")
	require.Contains(t, string(rewritten), "client-model")
	require.NotContains(t, string(rewritten), "upstream-model")
}

func TestConvertedMappingImagesSearchExclusions(t *testing.T) {
	t.Run("images_json_request_target_multipart_identical_response_unfabricated", func(t *testing.T) {
		var mu sync.Mutex
		var gotBody []byte
		var gotCT string
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			gotBody = b
			gotCT = r.Header.Get("Content-Type")
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"created": 1, "data": []any{map[string]any{"b64_json": "AAA"}}})
		}))
		defer up.Close()
		tpl := &domain.Template{ID: 1, Name: "t", BaseURL: up.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIImages}, Models: []string{"gpt-image-1"}, ModelMapping: map[string]domain.ModelMappingEntry{"client-img": {MappedModel: "gpt-image-1", Mode: domain.ModelMappingModeImplicit}}}
		store := &captureLogStore{}
		p := newTestProxyTplTimeoutLogs(t, tpl, 1, true, 30*time.Second, store, nil)
		req := httptest.NewRequest(http.MethodPost, "/v1/images/generations", strings.NewReader(`{"model":"client-img","prompt":"hi"}`))
		req.Header.Set("Authorization", "Bearer ck-1")
		rec := httptest.NewRecorder()
		p.HandleImagesGenerations(rec, req)
		require.Equal(t, 200, rec.Code)
		mu.Lock()
		require.Equal(t, "gpt-image-1", gjson.GetBytes(gotBody, "model").String(), "JSON images request rewritten to target")
		mu.Unlock()
		require.NotContains(t, rec.Body.String(), `"model"`, "images response must not fabricate model field")
		var buf bytes.Buffer
		mw := multipart.NewWriter(&buf)
		require.NoError(t, mw.WriteField("model", "client-img"))
		require.NoError(t, mw.WriteField("prompt", "hi"))
		fw, err := mw.CreateFormFile("image", "a.png")
		require.NoError(t, err)
		_, err = fw.Write([]byte("bin"))
		require.NoError(t, err)
		require.NoError(t, mw.Close())
		body := buf.Bytes()
		ct := mw.FormDataContentType()
		req2 := httptest.NewRequest(http.MethodPost, "/v1/images/generations", bytes.NewReader(body))
		req2.Header.Set("Authorization", "Bearer ck-1")
		req2.Header.Set("Content-Type", ct)
		rec2 := httptest.NewRecorder()
		p.HandleImagesGenerations(rec2, req2)
		require.Equal(t, 200, rec2.Code)
		mu.Lock()
		require.Equal(t, body, gotBody, "multipart body byte-identical")
		require.Equal(t, ct, gotCT, "multipart Content-Type preserved")
		mu.Unlock()
	})
	t.Run("search_opaque_fixed_billing_never_rewrite", func(t *testing.T) {
		var mu sync.Mutex
		var gotBody []byte
		var gotPath string
		up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			mu.Lock()
			gotBody = b
			gotPath = r.URL.Path
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"results":[{"type":"web_search","output":"hi"}],"id":"search_1"}`))
		}))
		defer up.Close()
		tpl := &domain.Template{ID: 1, Name: "t", BaseURL: up.URL, CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}, ModelMapping: map[string]domain.ModelMappingEntry{"client-model": {MappedModel: "upstream-model", Mode: domain.ModelMappingModeImplicit}}}
		accs := map[int64][]*domain.Account{10: {{ID: 1, TemplateID: 1, Template: tpl, UpstreamKey: "sk-upstream", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4}}}
		store := &captureLogStore{}
		p := newConvertedTestProxyAccsLogs(t, accs, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp}, store, 30*time.Second)
		req := httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(`{"model":"client-model","query":"hi"}`))
		req.Header.Set("Authorization", "Bearer ck-1")
		rec := httptest.NewRecorder()
		p.HandleSearch(rec, req)
		require.Equal(t, 200, rec.Code, "body=%s", rec.Body.String())
		mu.Lock()
		require.Equal(t, "/v1/alpha/search", gotPath, "search hits static SearchRaw endpoint")
		require.Equal(t, `{"model":"client-model","query":"hi"}`, string(gotBody), "search request body opaque not mapped")
		mu.Unlock()
		require.Equal(t, `{"results":[{"type":"web_search","output":"hi"}],"id":"search_1"}`, rec.Body.String(), "search response bytes unchanged never rewritten to client-model")
		require.NotContains(t, rec.Body.String(), "client-model")
		require.NoError(t, p.rec.Close(context.Background()))
		store.mu.Lock()
		defer store.mu.Unlock()
		require.Len(t, store.logs, 1)
		require.Equal(t, domain.FormatOpenAISearch, store.logs[0].Format)
		require.Equal(t, int64(1), store.logs[0].CallCount)
	})
}
