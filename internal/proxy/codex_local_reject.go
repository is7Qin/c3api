// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// SDK 本地拒绝（spec §5.4）：codexsdk.ErrInvalidRequestBody 表示请求体不能整形成
// Codex responses 请求（不是 JSON 对象 / 缺字符串 model / service_tier 含控制字节
// 等），SDK 在出网前拒绝。它是**客户端错误**：账号没坏、上游没收到任何字节，
// 换号也必然同结果。因此四个落点（HTTP 两个 core 的 SDK 错误分支、WS 拨号、WS
// 首帧、WS 后续帧）一律本地终止：记录 400 / Err4xx，不 Classify、不 MarkResult、
// 不投递健康事件、不故障转移。识别只在错误分支做（errors.Is），热路径零开销。
package proxy

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

// codexInvalidBodyMsg 本地拒绝的固定文案（落盘与客户端同一句，不带 SDK 原文）。
const codexInvalidBodyMsg = "invalid request body"

// codexInvalidBodyJSON HTTP 本地拒绝体（OpenAI 错误形态；直连与转换车道同一份——
// 网关本地错误不按客户端协议改写，与 writeErr 的约定一致）。
var codexInvalidBodyJSON = []byte(`{"error":{"message":"invalid request body","type":"invalid_request_error"}}` + "\n")

// codexInvalidBodyFrame WS 本地拒绝错误帧（拨号、首帧、后续帧三处同一帧）。
var codexInvalidBodyFrame = []byte(`{"type":"error","status":400,"error":{"type":"invalid_request_error","message":"invalid request body"}}`)

// convLocalRejects 本地拒绝累计（进程级；四个落点共用），经 codex-conversation
// worker 的 Stats() 以 conv_local_rejects 暴露。
var convLocalRejects atomic.Int64

// isCodexInvalidBody 判定 SDK 本地拒绝（穿透 sdkbridge.EnvelopeError 与
// codexsdk.DialError 的 Unwrap 链）。
func isCodexInvalidBody(err error) bool {
	return err != nil && errors.Is(err, codexsdk.ErrInvalidRequestBody)
}

// finishCodexInvalidBody 本地拒绝收尾记录（四个落点共用）：计数 + 400/Err4xx
// 记录。写法同 caller.go 的 "streaming is required" 本地终止——不 Classify、不
// MarkResult、不 emitCodexOutcome（dispatch 观测由循环的 abandon 收口）。
func (p *Proxy) finishCodexInvalidBody(ctx context.Context, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, format domain.RequestFormat) {
	convLocalRejects.Add(1)
	l := logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), format, http.StatusBadRequest, domain.Err4xx, usageTuple{}, start))
	em := codexInvalidBodyMsg
	l.ErrorMessage = &em
	p.finish(sel, l)
}

// writeCodexInvalidBody 写 HTTP 本地拒绝体（头未提交时调用）。
func writeCodexInvalidBody(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write(codexInvalidBodyJSON)
}

// writeCodexInvalidBodyFrame 向客户端写本地拒绝错误帧（不关闭连接：后续帧落点
// 会话继续；拨号与首帧落点由调用方随后关闭）。
func writeCodexInvalidBodyFrame(ctx context.Context, client *websocket.Conn) error {
	wctx, cancel := context.WithTimeout(ctx, responsesWSCloseTimeout)
	defer cancel()
	return client.Write(wctx, websocket.MessageText, codexInvalidBodyFrame)
}

// rejectCodexWSInvalidBody 拨号/首帧落点的本地拒绝：写错误帧并正常关闭客户端，
// 记录 400。
func (p *Proxy) rejectCodexWSInvalidBody(r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, client *websocket.Conn) {
	p.finishCodexInvalidBody(r.Context(), reqID, groupID, start, sel, reqModel, sel.Format)
	_ = writeCodexInvalidBodyFrame(context.Background(), client)
	_ = client.Close(websocket.StatusNormalClosure, "")
}
