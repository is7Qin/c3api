// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"

	"github.com/is7qin/c3api/internal/codexproto"
	"github.com/is7qin/c3api/internal/credential"
)

// AccountIDResolver codex account id 的**在线查询**面（PAT 的 whoami 端点）——
// 本包声明的消费方接口。
//
// 与 CodexUsageProber 刻意**分开**，不要合并：读一次额度快照与查一次 account id
// 是两个关注点（前者是展示面数据源，后者只在保存/导入时补全一行），把它们绑进
// 同一个接口只是让注入项少一个，却迫使任何只想要一边的装配方实现另一边。
//
// 实现 = `internal/sdkbridge` 的 `FetchPATAccountID`（组合根注入薄适配器，
// 见 cmd/server）。**在线**是它的性质：离线解析那一半（OAuth 的 JWT claims）不走
// 本接口，直接调 `codexproto.DeriveAccountID`（纯函数，永远可用）。
//
// 为什么签名里**没有** `*config.Config`：本仓库的 `config.Config` **没有** whoami
// 端点键（端点 base 一直由 SDK 的 env `CODEX_AUTHAPI_BASE_URL` 决定，见
// sdkbridge/codex_account_id.go 的注释）。为了让测试能打到 httptest 服务器而新增
// 一个生产配置键，是"为测试改生产面"；反过来若把 `*config.Config` 传进端口而它
// 里面没有可用键，参数就只是装饰。所以这里只传"调用所需的最小事实"（ctx + key）。
//
// **nil 语义**：未装配 ⇒ 管理面对 PAT 分支**直接留空**（不 500、不 panic、不阻塞
// 保存）。组合根恒装配；nil 只出现在测试/无网络装配场景。与之相对，
// CodexUsageProber 的 nil 语义是"null 快照"——两者都不返回错误，只是降级内容不同。
type AccountIDResolver interface {
	// ResolveAccountID 经 PAT whoami 查询该 key 的 account id。空 key → ("", nil)
	// 零出站；查询失败 → error 原样（调用方 best-effort：失败不阻塞保存）。
	ResolveAccountID(ctx context.Context, patKey string) (string, error)
}

// deriveAccountID 按凭据类型补全 codex account id（**管理面派生策略的唯一实现**）。
//
// 语义（三个调用点共用这一份——单账号保存与两个批量导入）：
//
//	凭据类型 → 若 account id 缺失 → 用对应手段补全：
//	  - codex-oauth：离线解 JWT claims（codexproto.DeriveAccountID，纯函数）
//	  - codex-pat：在线 whoami（AccountIDResolver.ResolveAccountID）
//	  - 其它类型：不派生
//	派生失败 / 不可派生 / 结果为空 → ""（**不阻塞保存**，交给 service 层行级
//	必填校验兜底；留空落库 → 下次保存重试）。
//
// 为什么收在 handler 私有方法而不是 service/domain：service 与 domain 都没有
// "取 account id" 的调用链，只有管理面这里有；下沉会为了抽象而扩大改动面。真有
// 第二个消费者时再上提。
//
// 为什么入参是散装字段而不是 *domain.AccountExt：批量导入的两个端点根本没有
// AccountExt，它们手里只有零散的 token/PAT 字段。散装签名是唯一的公共形状。
//
// 空 token / 空 key 由纯函数与实现侧各自短路（都返回空），所以这里不必前置判空
// ——多一层判断只会多一个与实现不一致的机会。
func (h *AdminAPI) deriveAccountID(ctx context.Context, typ credential.Type, oauthTok, patKey string) string {
	switch typ {
	case credential.TypeCodexOAuth:
		if id, ok := codexproto.DeriveAccountID(oauthTok); ok {
			return id
		}
		return ""
	case credential.TypeCodexPAT:
		// 未装配 ⇒ 留空（不 500、不 panic）。见 AccountIDResolver 的 nil 语义。
		if h.accountIDResolver == nil {
			return ""
		}
		if id, err := h.accountIDResolver.ResolveAccountID(ctx, patKey); err == nil && id != "" {
			return id
		}
		return ""
	default:
		return ""
	}
}
