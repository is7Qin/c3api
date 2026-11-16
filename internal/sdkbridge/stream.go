// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"

	"github.com/coder/websocket"
	codexsdk "github.com/is7Qin/codex-sdk"
)

// Stream 上游 Responses WS 连接在网关侧的具体薄壳。
//
// 边界与代价（这两条是同一个决定）：
//   - **形态**：字段是具体类型 `*codexsdk.Client`，五个方法各是一次纯转发。编译器
//     对这种方法体内联（`can inline (*Stream).Write`），展开后与直接调 SDK 同一条
//     指令流——这是"不产生性能损失"的构造性证明，不是基准推断。
//   - **替换而非叠加**：本类型**取代** proxy 原先的 `codexTransport`。relay 每帧的
//     那一次接口跳（`wsRelayTransport`）在改造前就存在，改造后仍是同一次；在外层再
//     套一层 facade 才会变成两跳，那是禁止的。
//
// typ 语义沿用 relay 传输面约定：Send 忽略 typ 恒文本（SDK Send 内部恒以 MessageText
// 写连接）；Recv 丢弃帧类型 → Read 恒返回 MessageText（responses WS 协议全 text 帧，
// binary 帧降级文本转发——既有语义不变）。错误原样透传（SDK Recv 透传 coder/websocket
// 错误 ⇒ errors.As *CloseError 成立，proxy 的 relayClassify/recordClose 直接复用）。
type Stream struct{ c *codexsdk.Client }

// Write 写一帧（一次 SDK Send）。
func (s *Stream) Write(ctx context.Context, _ websocket.MessageType, frame []byte) error {
	return s.c.Send(ctx, frame)
}

// Read 读一帧（一次 SDK Recv）。
func (s *Stream) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	f, err := s.c.Recv(ctx)
	return websocket.MessageText, f, err
}

// Ping 发心跳（编排层 30s+10s 心跳单一所有者）。
func (s *Stream) Ping(ctx context.Context) error { return s.c.Ping(ctx) }

// Close 幂等关闭（SDK closeOnce）。
func (s *Stream) Close(code websocket.StatusCode, reason string) error {
	return s.c.Close(code, reason)
}

// CloseNow 立即断开（不等待关闭握手）。
func (s *Stream) CloseNow() { s.c.CloseNow() }
