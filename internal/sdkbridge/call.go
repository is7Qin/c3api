// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"net/http"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/pkg/egress"

	"github.com/is7qin/c3api/internal/domain"
)

// 网关面对 Codex 的**调用聚合**：一次上游调用只有两件事要说清——身份从哪来、
// 这一帧是什么。SDK 的 `Identity`、`Option`、`ConversationRequest` 与选项模式全部
// 停在本包内，宿主不再组装它们。

// BookCall 一次「带对话标识」的调用：绑到某本会话簿的一次请求。
//
// 只由 Book.Call 产出（三元组派生与 payload 形状判定都在那一步完成），宿主既造不出、
// 也读不到里面是什么。
type BookCall struct {
	book *Book
	req  codexsdk.ConversationRequest
}

// Call 组装会话路径的调用。
func (b *Book) Call(req BookRequest, payload []byte) *BookCall {
	return &BookCall{book: b, req: req.request(payload)}
}

// prepare 在**上游调用之前**一次性取身份（会话路径会读存储、推进缓存）。
func (c *BookCall) prepare(ctx context.Context) codexsdk.Identity {
	return c.book.b.Prepare(ctx, c.req)
}

// option WS 面的逐帧身份（等价 SDK 的 WithConversation）：turn-state 由会话簿按轮次
// 管理，`first` 由同一份首帧判定。
func (c *BookCall) option(firstFrame []byte) codexsdk.Option {
	return codexsdk.WithConversation(c.book.b, c.req, firstFrame)
}

// CallRequest 一次上游调用。两件事，各只有一个来源：
//
//   - 身份：`Book != nil` ⇒ 会话路径（由会话簿合成）；否则槽位路径，身份来自
//     `Thread`（零值 = 匿名身份，等价 SDK 的零值 Identity）
//   - 帧：`Frame`。WS 面是**将要写出的首帧**（`Turn.First` 与握手 routing-hint 的
//     判据）；HTTP 面是**实际发给上游的体**。两侧都与 SDK 内部用的是同一份字节——
//     调用方必须传真正出门的那一份，别传副本或改写前的版本。
type CallRequest struct {
	Thread ThreadHandle
	Book   *BookCall
	Frame  []byte
}

// wsIdentity WS 面的身份与选项（一次调用一次，不在帧循环里）。
//
// 槽位路径**必须**带 WithFirstFrame：握手头的 routing-hint 与 x-codex-turn-metadata
// 都由它按首帧算，SDK 也靠它在拨号阶段就整形并拒绝非法首帧（少了它，本地拒绝会从
// Dial 推迟到第一次 Send，客户端的 400 语义与"未发送"事实都会变）。会话路径由
// WithConversation 承担同一职责（两者不可同设，SDK 会拒绝）。
func (r CallRequest) wsIdentity() (codexsdk.Identity, []codexsdk.Option) {
	if r.Book != nil {
		return codexsdk.Identity{}, []codexsdk.Option{r.Book.option(r.Frame)}
	}
	return r.Thread.identity(r.Frame), []codexsdk.Option{codexsdk.WithFirstFrame(r.Frame)}
}

// httpIdentity HTTP 面的身份（会话路径急切 Prepare；槽位路径直接组装）。
func (r CallRequest) httpIdentity(ctx context.Context) codexsdk.Identity {
	if r.Book != nil {
		return r.Book.prepare(ctx)
	}
	return r.Thread.identity(r.Frame)
}

// DialCall 建立上游 Responses WS 连接。
//
// 心跳单源：SDK 内部心跳恒关（WithPingInterval(0)，且该选项已被 SDK 标记为无效果），
// 由编排层 30s Ping + 10s pong 超时单一负责。
func (a *Codex) DialCall(ctx context.Context, p Prepared, eg *egress.Client, r CallRequest) (*Stream, error) {
	id, opts := r.wsIdentity()
	c, err := a.DialPrepared(ctx, p, eg, id, append(opts, codexsdk.WithPingInterval(0))...)
	if err != nil {
		return nil, err
	}
	return &Stream{c: c}, nil
}

// ResponsesCall 非流式 responses（返回合成体原始字节：直连 = 原样转发、转换 = 反向映射）。
func (a *Codex) ResponsesCall(ctx context.Context, p Prepared, eg *egress.Client, r CallRequest, transport domain.CodexResponsesTransport) ([]byte, error) {
	resp, err := a.ResponsesPrepared(ctx, p, eg, r.Frame, r.httpIdentity(ctx), transport)
	if err != nil {
		return nil, err
	}
	return resp.Raw, nil
}

// StreamBodyCall 流式 responses：返回**未关闭**的 *http.Response（body 归调用方，
// proxy 经 sserelay 消费后恰好关闭一次）。返回值是标准库类型，不是 SDK 类型——调用方
// 要读 StatusCode 与 Body，这两件都在 http.Response 上。
func (a *Codex) StreamBodyCall(ctx context.Context, p Prepared, eg *egress.Client, r CallRequest, transport domain.CodexResponsesTransport) (*http.Response, error) {
	return a.StreamBodyPrepared(ctx, p, eg, r.Frame, r.httpIdentity(ctx), transport)
}
