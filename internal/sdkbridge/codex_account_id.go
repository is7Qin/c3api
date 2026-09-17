// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"strings"

	codexsdk "github.com/is7Qin/codex-sdk"
)

// —— codex account id 管理面派生薄包装（spec 2026-09-17 §7.0-6：派生点唯一在
// 落库——导入行 / 单账号 ext 保存；热路径只读 DB，零解析零出站） ——
//
// service/handler 经本文件调用 SDK account id 能力，不直引 codex-sdk（分层
// 纪律——codexsdk import 仅限本文件族，见 codex.go:23 注释）。

// DeriveCodexAccountID 从 ChatGPT 身份 JWT（access_token 或 id_token）离线提取
// account id（纯函数——codexsdk.AccountIDFromToken 薄包装：只解 payload claims，
// 不验签不校验 exp；过期 AT 亦可解）。任何解析失败 → ("", false)，不 panic。
func DeriveCodexAccountID(token string) (string, bool) {
	return codexsdk.AccountIDFromToken(token)
}

// FetchPATAccountID 经 PAT whoami 端点在线查询 account id（管理面唯一出站派生
// 点——热路径零调用；端点 base 默认 https://auth.openai.com/api/accounts，env
// CODEX_AUTHAPI_BASE_URL 可覆盖，与真客户端同名）。空 key → ("", nil) 零出站；
// whoami 失败 → error 原样（调用方 best-effort：失败不阻塞保存/导入）；
// 响应无 account id → ("", nil)（= 未派生，调用方按空处理）。
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
