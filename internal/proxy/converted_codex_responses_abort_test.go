// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// --- 转换路径 codex 输出 seam 回归（spec §3 第 3/4 项） ---
// 现有 codexHTTPStep 表达不了「200 流：先投帧、再中断报错」（200 步恒追加
// [DONE] → 正常结束；非 200 步无帧；SDK 对 EOF 亦返回 nil）。故另辟临时上游
// newCodexAbortUpstream 精确构造该形态，用于验证核心分类（raw relay 下经统一
// 出口判定 classifyStreamExit）：
//   - 首帧被 drop（未写出）后上游读错误 → Output 未提交 → 交 failover（502）；
//   - 首帧确写后上游读错误 → 已提交 → 客户端协议 SSE error + abort 收尾
//     （200 已定型，不折返 failover）。

// convCodexRespInProgress 上游 responses in_progress 事件：chat→resp 映射器无
// 对应帧（chat_resp.go 默认分支 → (nil, true)）→ 被 drop（未写任何字节）。
const convCodexRespInProgress = `{"type":"response.in_progress","response":{"id":"resp_cc","object":"response","status":"in_progress","model":"gpt-5.6"}}`

// newCodexAbortUpstream 构造「200 + 单个 SSE 帧后中断报错」的临时上游：读尽请求体
// （避免 Close 触发 RST 丢失已发字节）后 hijack 连接，手写 200 响应头 + 一帧
// `data: <frame>` 行后 Close。Content-Length 声明大于实际写出 → 客户端读满声明前
// 遇 EOF → http body 读取报错（非正常 EOF）→ SDK Stream 返回读取错误。
// frame == "" → 只发响应头，零帧中断。返回调用计数（failover 次数断言面）。
func newCodexAbortUpstream(t *testing.T, frame string) (*httptest.Server, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		hj, ok := w.(http.Hijacker)
		if !ok {
			return
		}
		conn, buf, err := hj.Hijack()
		if err != nil {
			return
		}
		payload := "data: " + frame + "\n\n"
		_, _ = fmt.Fprintf(buf, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: %d\r\n\r\n", len(payload)+64)
		_, _ = io.WriteString(buf, payload)
		_ = buf.Flush()
		_ = conn.Close()
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

// TestConvertedCodexDropFrameThenUpstreamError502 spec §3 第 3 项（I1）：chat_to_resp
// 转换路径，上游先投一帧 response.in_progress（被映射器 drop、未写任何字节）再
// 中断报错：framesWritten 仍为 false ⇒ core 折返 (code, body, false, err) ⇒
// failover 分类为 502。缺陷场景（修复前）：把「已 drop 一帧」误判为已写出 → 走
// abort/ResultClientCancel（错误地 200）。
func TestConvertedCodexDropFrameThenUpstreamError502(t *testing.T) {
	up, calls := newCodexAbortUpstream(t, convCodexRespInProgress)
	defer up.Close()
	p, _ := newConvertedCodexTestProxy(t, credential.TypeCodexOAuth,
		map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")},
		up.URL, nil, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp}, &captureLogStore{})

	rec := postChatConv(t, p, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)
	require.Equal(t, http.StatusBadGateway, rec.Code,
		"首帧 drop（未写出）后上游读取报错 → 头未提交 → failover 耗尽 502；body=%s", rec.Body.String())
	require.GreaterOrEqual(t, atomic.LoadInt32(calls), int32(1), "上游被触达（非前置拒绝）")
}

// TestConvertedCodexAbortAfterFrameWritten spec §3 第 4 项（I1 反向对照）：chat_to_resp
// 转换路径，上游先投一帧 response.output_text.delta（映射为可见 chat chunk、确写）
// 再中断报错：framesWritten true ⇒ 走「已写出后 abort」分支（ResultFailed +
// recordStreamAbort），200 已定型不折返 failover。
func TestConvertedCodexAbortAfterFrameWritten(t *testing.T) {
	up, calls := newCodexAbortUpstream(t, convCodexRespDelta)
	defer up.Close()
	store := &captureLogStore{}
	p, _ := newConvertedCodexTestProxy(t, credential.TypeCodexOAuth,
		map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")},
		up.URL, nil, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp}, store)

	rec := postChatConv(t, p, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, "首帧已写出 → 200 已定型（abort 分支，不折返 failover）；body=%s", rec.Body.String())
	require.Contains(t, rec.Body.String(), `"object":"chat.completion.chunk"`, "首帧确写（可见 chat chunk）")
	require.Equal(t, int32(1), atomic.LoadInt32(calls), "已写出 → 不 failover（仅一次上游调用）")

	require.NoError(t, p.rec.Close(context.Background()))
	store.mu.Lock()
	defer store.mu.Unlock()
	require.Len(t, store.logs, 1)
	require.Equal(t, domain.ErrAbort, store.logs[0].ErrorType, "已写出后上游中断 → recordStreamAbort")
}

// TestConvertedCodexZeroFrameDefense spec §3 第 4 项（新结构）：转换路径病态上游
// 正常结束但仅发 [DONE]（无 completed）——源协议 [DONE] 由适配层过滤；目标 mapper
// 未产终止帧 → 流末由适配层**补发目标终止帧**（chat: data: [DONE]）。故 body 恰为
// 一个 `data: [DONE]\\n\\n`，而非空。
func TestConvertedCodexZeroFrameDefense(t *testing.T) {
	up, upc := newCodexHTTPUpstream(t, codexHTTPStep{status: 200}) // 200 + 仅 [DONE]
	defer up.Close()
	p, _ := newConvertedCodexTestProxy(t, credential.TypeCodexOAuth,
		map[int64]*domain.AccountExt{10: codexOAuthExt(10, "at-10", "rt-10")},
		up.URL, nil, []domain.ProtocolConvert{domain.ProtocolConvertChatToResp}, &captureLogStore{})

	rec := postChatConv(t, p, `{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}],"stream":true}`, nil)
	require.Equal(t, http.StatusOK, rec.Code, "上游正常结束（仅 [DONE]）→ 200；body=%s", rec.Body.String())
	require.Equal(t, "text/event-stream", rec.Header().Get("Content-Type"), "提交 SSE 头")
	require.Equal(t, "data: [DONE]\n\n", rec.Body.String(), "目标终止帧尚未产生 → 流末恰补发一个 data: [DONE]")
	require.Equal(t, 1, upc.callsN())
}
