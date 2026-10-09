// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// converted 终止帧三情形（spec §2.4）：raw relay 下目标协议终止帧**恰一个**。
// 适配层先过滤源协议 [DONE]，目标 mapper 自产终止帧；流末仅当「目标终止帧尚未
// 产生」时补发。codex converted 两方向 = ChatToResp（client=Chat，终止 = data:
// [DONE]）与 MessToResp（client=Messages，终止 = message_delta ++ message_stop）。

// newCodexNoDoneUpstream codex responses mock 上游：逐事件发 `data: <ev>\n\n`
// 后正常结束（**不追加 [DONE]**）——「无 DONE 正常 EOF」补发用例。
func newCodexNoDoneUpstream(t *testing.T, events ...string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		for _, ev := range events {
			_, _ = io.WriteString(w, "data: "+ev+"\n\n")
			f.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func convertedCodexProxyForDir(t *testing.T, upURL string, dir domain.ProtocolConvert) *Proxy {
	t.Helper()
	p, _ := newConvertedCodexTestProxy(t, credential.TypeCodexOAuth,
		map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")},
		upURL, nil, []domain.ProtocolConvert{dir}, &captureLogStore{})
	return p
}

// TestConvertedCodexChatToRespTerminalFrames ChatToResp 三情形恰一个 data: [DONE]。
func TestConvertedCodexChatToRespTerminalFrames(t *testing.T) {
	post := func(p *Proxy) *httptest.ResponseRecorder {
		return postChatConv(t, p, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)
	}

	t.Run("completed_plus_source_done", func(t *testing.T) {
		up, _ := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{
			convCodexRespCreated, convCodexRespDelta, convCodexRespDone,
		}}) // 上游流末追加 [DONE]
		defer up.Close()
		rec := post(convertedCodexProxyForDir(t, up.URL, domain.ProtocolConvertChatToResp))
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		got := rec.Body.String()
		require.Equal(t, 1, strings.Count(got, "data: [DONE]"), "completed 已产终止帧 + 源 [DONE] 丢弃 → 恰一个")
		require.Contains(t, got, `"finish_reason":"stop"`)
	})

	t.Run("done_only", func(t *testing.T) {
		up, _ := newCodexHTTPUpstream(t, codexHTTPStep{status: 200}) // 仅 [DONE]
		defer up.Close()
		rec := post(convertedCodexProxyForDir(t, up.URL, domain.ProtocolConvertChatToResp))
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		require.Equal(t, "data: [DONE]\n\n", rec.Body.String(), "仅 DONE（无 completed）→ 流末补发恰一个")
	})

	t.Run("no_done_normal_eof", func(t *testing.T) {
		up := newCodexNoDoneUpstream(t, convCodexRespCreated, convCodexRespDelta)
		rec := post(convertedCodexProxyForDir(t, up.URL, domain.ProtocolConvertChatToResp))
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		got := rec.Body.String()
		require.Equal(t, 1, strings.Count(got, "data: [DONE]"), "无 DONE 正常 EOF（无 completed）→ 流末补发恰一个；body=%q", got)
		require.Contains(t, got, `"delta":{"content":"Hello"}`, "补发前的映射帧已写出；body=%q", got)
	})
}

// TestConvertedCodexMessToRespTerminalFrames MessToResp 三情形恰一个 message_stop。
func TestConvertedCodexMessToRespTerminalFrames(t *testing.T) {
	post := func(p *Proxy) *httptest.ResponseRecorder {
		return postAnthropicConv(t, p, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"max_tokens":100,"stream":true}`, nil)
	}

	t.Run("completed_plus_source_done", func(t *testing.T) {
		up, _ := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{
			convCodexRespCreated, convCodexRespDelta, convCodexRespDone,
		}})
		defer up.Close()
		rec := post(convertedCodexProxyForDir(t, up.URL, domain.ProtocolConvertMessToResp))
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		got := rec.Body.String()
		require.Equal(t, 1, strings.Count(got, "event: message_stop"), "completed 已产终止帧 + 源 [DONE] 丢弃 → 恰一个")
		require.Equal(t, 1, strings.Count(got, "event: message_delta"))
		require.NotContains(t, got, "[DONE]")
	})

	t.Run("done_only", func(t *testing.T) {
		up, _ := newCodexHTTPUpstream(t, codexHTTPStep{status: 200}) // 仅 [DONE]
		defer up.Close()
		rec := post(convertedCodexProxyForDir(t, up.URL, domain.ProtocolConvertMessToResp))
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		got := rec.Body.String()
		require.Equal(t, 1, strings.Count(got, "event: message_stop"), "仅 DONE（无 completed）→ 流末补发 message_stop")
		require.Equal(t, 1, strings.Count(got, "event: message_delta"))
		require.NotContains(t, got, "[DONE]")
	})

	t.Run("no_done_normal_eof", func(t *testing.T) {
		up := newCodexNoDoneUpstream(t, convCodexRespCreated, convCodexRespDelta)
		rec := post(convertedCodexProxyForDir(t, up.URL, domain.ProtocolConvertMessToResp))
		require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		got := rec.Body.String()
		require.Equal(t, 1, strings.Count(got, "event: message_stop"), "无 DONE 正常 EOF（无 completed）→ 流末补发 message_stop；body=%q", got)
		require.Contains(t, got, "event: message_start", "补发前的映射帧已写出；body=%q", got)
	})
}

// TestConvertedCodexTTFTPreWriteDropFrame spec §2.5：TTFT 在**唯一写出前 seam** 记录
// （与 native 同一回调实例）——首个上游帧被 mapper drop（response.in_progress）仍
// 计时（真值来自首个上游事件，而非首个写出帧）。
func TestConvertedCodexTTFTPreWriteDropFrame(t *testing.T) {
	up, _ := newCodexHTTPUpstream(t, codexHTTPStep{status: 200, events: []string{
		convCodexRespInProgress, convCodexRespCreated, convCodexRespDelta, convCodexRespDone,
	}})
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newConvertedCodexTestProxy(t, credential.TypeCodexOAuth,
		map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")},
		up.URL, nil, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp}, store)
	rec := postChatConv(t, p, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.NotNil(t, store.logs[0].TTFTMS, "首个 drop 帧也记录 TTFT（写出前 seam）")
}
