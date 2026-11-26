// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package codexproto codex 协议的纯数据与纯解析（零内部依赖，只 import stdlib）。
//
// 为什么单独一个包：管理面要在保存/批量导入账号时补全 codex account id（离线解
// JWT claims），并在 /api/admin/accounts/usage 查询失败时把错误分类成
// 「凭据失效 / 上游错误」两档。这两件事都只需要「协议形状」——一个纯函数、一类
// 纯值——但都不该为此 import internal/sdkbridge：那个包实测传递依赖 305 个包
// （websocket / redis / bps-sdk / codex-sdk / pkg/egress），等于为了解一段 JWT
// 把整套传输栈拖进控制器层的类型依赖。
//
// 为什么是「再实现一遍」而不是薄包 SDK：
//
//   - codexsdk.AccountIDFromToken 本身就是约 30 行 stdlib（strings.Split +
//     base64.RawURLEncoding + json.Unmarshal + TrimSpace），薄包它换不来任何
//     简化，只会带进 218 个传递依赖（websocket / zstd / gjson / net/http）——
//     本包就不再是"只依赖 stdlib 的中立叶子"，矩阵里的 `to: []string{}` 也就
//     不再成立，本包存在的意义（切断 handler 的传输栈依赖）随之消失；
//   - 这份逻辑是**稳定协议**（JWT payload 的固定 claims 路径），不是易变业务；
//   - SDK 那份对 c3api 是第三方实现，不该成为"控制器层能否解 JWT"的前提。
//
// 代价是同一段逻辑存在两份。「两份必须语义一致」由**对照测试**钉成机械约束，
// 而不是口头约定：codexproto_test.go 的 TestDeriveAccountIDMatchesSDK 把本包实现
// 与 codexsdk.AccountIDFromToken 逐用例对照——改一边而不改另一边，那条测试就红。
//
// ⚠ 依赖方向硬约束：本包**禁止** import 任何 internal/* 或 pkg/*。矩阵里本包是
// 显式 `to: []string{}`（不靠 leafRule 兜底），加一条内部 import 立刻变红。
package codexproto

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// DeriveAccountID 从 ChatGPT 身份 JWT（access_token 或 id_token）的 payload 提取
// chatgpt_account_id。
//
// 语义（与 codexsdk.AccountIDFromToken 逐条一致）：只解**第二段**并读
// `https://api.openai.com/auth.chatgpt_account_id`；**不验签、不校验 exp**
// （过期 access token 的 claims 照样可读——补全 account id 不需要它还有效）。
//
// 任何失败都返回 ("", false)，**不 panic、不返回 error**：调用方（管理面保存/
// 导入）把"派生不出来"当成合法的留空语义（交给 service 层行级必填校验兜底），
// 而不是一次需要区分原因的失败。失败面：不是三段式 JWT、payload 不是 base64url、
// payload 不是 JSON、claims 路径缺失、account id 为空串或纯空白。
func DeriveAccountID(token string) (string, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", false
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	var payload struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", false
	}
	id := strings.TrimSpace(payload.Auth.AccountID)
	if id == "" {
		return "", false
	}
	return id, true
}
