// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// —— codex 设备码登录编排（ChatGPT OAuth device authorization grant）——
//
// **两个无状态调用，网关不持有待授权会话**：
//
//	发起：StartCodexOAuthDeviceLogin —— 向上游申请用户码，把 device_auth_id /
//	      user_code 连同展示信息返回给发起者（已鉴权的管理台/供应商）。
//	完成：CompleteCodexOAuthDeviceLogin —— 用户浏览器授权后由前端触发，服务端
//	      **当场**轮询一次：未授权 → pending（可再点）；授权完成 → 换码并走既有
//	      导入核心落库；过期/拒绝 → 对应状态。
//
// 为什么不用服务端会话（Redis）：会话的真实生命周期由上游设备码（≤15 分钟）与
// 用户浏览器决定，存进网关只是把「上游事实」复制成本地状态——随之而来的是 TTL
// 语义、跨实例原子消费、归属锚点、装配缺失分支，全部为可避免的复杂度。无状态
// 形态下任意实例都能处理「完成」，进程重启/多副本零影响。
//
// 落库仍只走 importCodexAccounts（单行 upsert）：新建走单行事务，已存在只换
// 凭据列；副作用（组级失效 + NOTIFY）由导入核心批末一次完成——设备码只是凭据
// 来源，不是第二条写路径。

import (
	"context"
	"errors"
	"fmt"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	serviceerr "github.com/is7qin/c3api/internal/service/errors"
	"github.com/is7qin/c3api/pkg/logx"
)

// CodexLoginProvider 设备码登录协议面（生产实现 = sdkbridge.CodexLogin；
// nil = 未装配 → 端点 503）。PollDeviceFlow 只在 authorized 时返回 tokens，
// pending/expired/denied 走状态枚举（错误只表示上游/网络故障——调用方可重试）。
type CodexLoginProvider interface {
	StartDeviceFlow(ctx context.Context) (*domain.CodexDeviceCode, error)
	PollDeviceFlow(ctx context.Context, authID, userCode string) (domain.CodexLoginStatus, *domain.CodexLoginTokens, error)
}

// StartCodexOAuthDeviceLogin 申请设备码（协议第 1 步）。无参数：模板/分组在
// 「完成」时才需要（发起与落库职责分离——发起失败不产生任何本地状态）。
func (s *Service) StartCodexOAuthDeviceLogin(ctx context.Context) (*domain.CodexLoginChallenge, error) {
	if s.codexLogin == nil {
		return nil, fmt.Errorf("%w: codex 设备码登录未装配", serviceerr.ErrUnavailable)
	}
	code, err := s.codexLogin.StartDeviceFlow(ctx)
	if err != nil {
		if s.log != nil {
			s.log.Warn("codex device login: start flow failed", logx.Error(err))
		}
		// 上游细节只进日志：认证端点错误体可能含内部追踪串，不透给客户端。
		return nil, fmt.Errorf("%w: 设备码申请失败（上游不可用）", serviceerr.ErrUpstream)
	}
	interval := int(code.Interval.Seconds())
	if interval <= 0 {
		interval = 5
	}
	return &domain.CodexLoginChallenge{
		DeviceAuthID:     code.AuthID,
		UserCode:         code.UserCode,
		VerificationURL:  code.VerificationURL,
		IntervalSeconds:  interval,
		ExpiresInSeconds: int(code.ExpiresIn.Seconds()),
	}, nil
}

// CompleteCodexOAuthDeviceLogin 完成登录（协议第 2、3 步 + 落库）。前端在用户
// 浏览器授权后调用；每次调用**只轮询一次**（pending 时由用户再次触发，避免网关
// 长轮询/定时任务）。
//
// 顺序很重要：模板/分组校验**先于**上游轮询——上游的授权码是一次性的，先换码
// 再拒绝会把用户刚授权的凭据烧掉。
func (s *Service) CompleteCodexOAuthDeviceLogin(ctx context.Context, authID, userCode string, tplID int64, groupID *int64) (*domain.CodexLoginResult, error) {
	if s.codexLogin == nil {
		return nil, fmt.Errorf("%w: codex 设备码登录未装配", serviceerr.ErrUnavailable)
	}
	if authID == "" || userCode == "" {
		return nil, fmt.Errorf("%w: device_auth_id 与 user_code 必填", ErrInvalidInput)
	}
	// 与批量导入同一套顶层校验（模板必须 codex-oauth / 分组存在）——失败即返回，
	// 不动上游授权码。
	if err := s.checkCodexImportTemplate(ctx, &tplID, credential.TypeCodexOAuth); err != nil {
		return nil, err
	}
	if groupID != nil {
		if err := s.checkGroupsExist(ctx, []int64{*groupID}); err != nil {
			return nil, err
		}
	}

	status, tokens, err := s.codexLogin.PollDeviceFlow(ctx, authID, userCode)
	if err != nil {
		if s.log != nil {
			s.log.Warn("codex device login: poll failed", logx.Error(err))
		}
		return nil, fmt.Errorf("%w: 设备码轮询失败（上游不可用）", serviceerr.ErrUpstream)
	}
	switch status {
	case domain.CodexLoginPending:
		return &domain.CodexLoginResult{Status: domain.CodexLoginPending}, nil
	case domain.CodexLoginExpired, domain.CodexLoginDenied:
		return &domain.CodexLoginResult{Status: status}, nil
	case domain.CodexLoginAuthorized:
	default:
		return nil, fmt.Errorf("service: 未知设备码登录状态 %q", status)
	}
	return s.importCodexOAuthLogin(ctx, tokens, tplID, groupID)
}

// importCodexOAuthLogin 把换到的凭据写进号池（单行 upsert）+ 回读账号定位。
// 授权码已在上游消费：任何失败都要求用户重新登录（loud，不静默半成品）。
func (s *Service) importCodexOAuthLogin(ctx context.Context, tokens *domain.CodexLoginTokens, tplID int64, groupID *int64) (*domain.CodexLoginResult, error) {
	// **同一校验入口**：批量导入的必填/成对规则（email、account id、token+refresh）
	// 在这里原样生效——设备码登录不是「可信来源」，缺 refresh token 时若直接落库
	// 会新建不可用账号，并把既有账号的有效 RT 覆盖为空（后者是凭据破坏）。
	if tokens == nil {
		return nil, fmt.Errorf("%w: 登录响应缺少凭据，请重新登录", serviceerr.ErrUpstream)
	}
	row, err := newCodexOAuthRow(0, tokens.Email, tokens.AccountID, tokens.AccessToken, tokens.RefreshToken, tokens.ExpiresAt)
	if err != nil {
		if s.log != nil {
			s.log.Warn("codex device login: token bundle rejected", logx.Error(err))
		}
		// 校验文案是导入面词汇（codex_oauth_*）：对外给可操作提示，细节只进日志。
		return nil, fmt.Errorf("%w: 登录响应凭据不完整，请重新登录", serviceerr.ErrUpstream)
	}
	// 与批量导入**同一个**落库核心：核心返回逐行结果（提交点给出的账号 id/名称），
	// 因此这里既不回读账号表，也不会把成功信息丢失成「authorized + 空字段」。
	outcomes := s.importCodexAccounts(ctx, []codexImportRow{row}, tplID, groupID, credential.TypeCodexOAuth, domain.CodexImportConfig{})
	if len(outcomes) != 1 {
		return nil, fmt.Errorf("service: codex 设备码登录落库结果缺失")
	}
	out := outcomes[0]
	if out.Err != nil {
		if s.log != nil {
			s.log.Warn("codex device login: import failed",
				logx.String("email", tokens.Email), logx.Error(out.Err))
		}
		// 冲突（键已存在/账号已删除）与存储故障是两类事实：前者可操作，后者是
		// 服务器故障。上游授权码已消费，两者都要求重新登录，但状态码不能混。
		if errors.Is(out.Err, ErrConflict) {
			return nil, fmt.Errorf("%w: 登录成功但账号写入失败——请在账号列表确认或重新登录", ErrConflict)
		}
		// 非冲突：细节只进日志（WriteServiceErr 对未命中哨兵的错误统一 500 且不回显）。
		return nil, out.Err
	}
	return &domain.CodexLoginResult{
		Status:         domain.CodexLoginAuthorized,
		AccountID:      out.AccountID,
		AccountName:    out.AccountName,
		Email:          tokens.Email,
		CodexAccountID: tokens.AccountID,
		Imported:       out.Imported,
	}, nil
}
