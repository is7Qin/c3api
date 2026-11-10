// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"errors"
	"time"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/domain"
)

// —— codex 设备码登录协议面（ChatGPT OAuth device authorization grant）——
//
// 协议实现全部在 codex-sdk（DeviceAuthClient：usercode → 轮询 → 表单换码，
// 对齐真客户端 codex-rs/login/src/device_code_auth.rs）；本文件是**唯一**
// 把 codexsdk 类型翻译成 domain 类型的适配点（codexsdk import 仅限本文件族，
// 同 codex.go:23）。哨兵错误在此收敛为状态枚举，service 不感知 SDK 错误类型。

// CodexLogin 设备码登录适配器（无状态；并发安全——SDK client 无可变状态）。
type CodexLogin struct {
	client *codexsdk.DeviceAuthClient
}

// NewCodexLogin 构造设备码登录适配器（默认 issuer / app client_id）。
// **不暴露 SDK 类型**：调用方（main/service）只见本包的 domain 面——codexsdk
// import 仅限本文件族（同 codex.go:23 分层纪律）。
func NewCodexLogin() *CodexLogin {
	return &CodexLogin{client: codexsdk.NewDeviceAuthClient()}
}

// newCodexLogin 注入已构造的 SDK client（**测试专用**：codexsdk.WithIssuer 指向
// httptest；同包内可见，生产唯一入口 NewCodexLogin）。
func newCodexLogin(client *codexsdk.DeviceAuthClient) *CodexLogin {
	return &CodexLogin{client: client}
}

// StartDeviceFlow 申请用户码（协议第 1 步）。错误原样上抛（含 *DeviceAuthError
// 的 HTTP 状态——issuer 未开启设备码登录时为 404）。
func (l *CodexLogin) StartDeviceFlow(ctx context.Context) (*domain.CodexDeviceCode, error) {
	code, err := l.client.StartDeviceFlow(ctx)
	if err != nil {
		return nil, err
	}
	return &domain.CodexDeviceCode{
		AuthID:          code.AuthID,
		UserCode:        code.UserCode,
		VerificationURL: code.VerificationURL,
		Interval:        code.Interval,
		ExpiresIn:       code.ExpiresIn,
	}, nil
}

// PollDeviceFlow 单次轮询（协议第 2 步）+ 授权完成时换码（第 3 步）。
//
// 状态映射：SDK 哨兵 → domain 状态枚举（pending/expired/denied）；其余错误
// （网络/5xx/424 之外的限流/协议异常）原样作为 err 返回——调用方**不得**把
// 它当作终态销毁会话（会话由 Redis TTL 兜底，下一次轮询可重试）。
func (l *CodexLogin) PollDeviceFlow(ctx context.Context, authID, userCode string) (domain.CodexLoginStatus, *domain.CodexLoginTokens, error) {
	tokens, err := l.client.PollDeviceFlow(ctx, &codexsdk.DeviceCode{AuthID: authID, UserCode: userCode})
	if err != nil {
		switch {
		case errors.Is(err, codexsdk.ErrDeviceAuthorizationPending):
			return domain.CodexLoginPending, nil, nil
		case errors.Is(err, codexsdk.ErrDeviceCodeExpired):
			return domain.CodexLoginExpired, nil, nil
		case errors.Is(err, codexsdk.ErrDeviceCodeDenied):
			return domain.CodexLoginDenied, nil, nil
		default:
			return "", nil, err
		}
	}
	return domain.CodexLoginAuthorized, &domain.CodexLoginTokens{
		AccessToken:  tokens.AccessToken,
		RefreshToken: tokens.RefreshToken,
		AccountID:    tokens.AccountID,
		Email:        tokens.Email,
		ExpiresAt:    expiresAtOrNil(tokens.ExpiresAt),
	}, nil
}

// expiresAtOrNil 零值时刻 → nil（domain 契约：nil = 过期时刻未知 → 401 自愈）。
func expiresAtOrNil(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	at := t
	return &at
}
