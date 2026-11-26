// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"strings"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/codexproto"
)

// —— codex account id 管理面派生（spec 2026-09-17 §7.0-6：派生点唯一在落库——
// 导入行 / 单账号 ext 保存；热路径只读 DB，零解析零出站） ——
//
// P6-2 起分两半，住两个地方：
//
//   - **离线解析**（JWT claims）的实现住在零内部依赖叶子包
//     `internal/codexproto.DeriveAccountID`。handler 要解 account id，但**不该**
//     为此拖进本包的一整套传输栈（websocket / redis / bps-sdk / codex-sdk，实测
//     305 个传递依赖）。「两份实现必须一致」不是口头约定：
//     `internal/codexproto/codexproto_test.go` 的对照用例把它与
//     codexsdk.AccountIDFromToken 逐用例比对。`DeriveCodexAccountID` **保留**为
//     本包对外的既有面，实现改为委托——一份逻辑、两个出入口，不会漂移。
//
//   - **在线查询**（PAT whoami）留在这里：真出站 I/O，实现是 codexsdk 的公开面。
//     消费方（handler）经自己声明的 `AccountIDResolver` 端口注入 `PATWhoami`
//     的方法值，因此仍然不必 import 本包。
//
// codexsdk import 仅限本文件族（见 codex.go:23 注释）。

// DeriveCodexAccountID 从 ChatGPT 身份 JWT（access_token 或 id_token）离线提取
// account id（纯函数——委托 `codexproto.DeriveAccountID`：只解 payload claims，
// 不验签不校验 exp；过期 AT 亦可解）。任何解析失败 → ("", false)，不 panic。
//
// 新代码请直接调 `codexproto.DeriveAccountID`（无中间商的中立实现，也是 handler
// 侧的调用点）；本函数只为不破坏既有调用方而保留。
func DeriveCodexAccountID(token string) (string, bool) {
	return codexproto.DeriveAccountID(token)
}

// PATWhoami 是 PAT 在线查询面的**方法集载体**：组合根把它装给
// handler.AccountIDResolver 时写 `sdkbridge.PATWhoami{}` 即可，不必在 cmd/server
// 里造一个只做转发的闭包——闭包会让「谁实现了这个端口」不可 grep，也让「装配
// 成立」失去编译期证明。
//
// 零值可用（无状态、不分配）。
type PATWhoami struct{}

// ResolveAccountID 实现 handler.AccountIDResolver：PAT whoami 查 account id。
// 方法集匹配 ⇒ 组合根赋值即编译期证明装配成立。等价于 FetchPATAccountID。
func (PATWhoami) ResolveAccountID(ctx context.Context, patKey string) (string, error) {
	return FetchPATAccountID(ctx, patKey)
}

// FetchPATAccountID 经 PAT whoami 端点在线查询 account id（管理面唯一出站派生
// 点——热路径零调用；端点 base 默认 https://auth.openai.com/api/accounts，env
// CODEX_AUTHAPI_BASE_URL 可覆盖，与真客户端同名）。空 key → ("", nil) 零出站；
// whoami 失败 → error 原样（调用方 best-effort：失败不阻塞保存/导入）；
// 响应无 account id → ("", nil)（= 未派生，调用方按空处理）。
//
// 请求实现仍是 codexsdk.FetchPATMetadata：那是 SDK 的**公开面**，再实现一遍只会
// 得到两份会漂移的出站契约（默认头 / 10s 超时 / 宽松 JSON 映射）。这与
// internal/codexproto 里那份「约 30 行纯 stdlib、值得重复」的 JWT 解析不同。
func FetchPATAccountID(ctx context.Context, patKey string) (string, error) {
	if strings.TrimSpace(patKey) == "" {
		return "", nil
	}
	md, err := codexsdk.FetchPATMetadata(ctx, patKey)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(md.AccountID), nil
}
