// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	serviceerr "github.com/is7qin/c3api/internal/service/errors"
	"github.com/is7qin/c3api/pkg/logx"
)

// CodexLoginProvider implements one device-code request and one authorization
// check per call. An authorized check also exchanges the grant for tokens.
type CodexLoginProvider interface {
	StartDeviceFlow(ctx context.Context) (*domain.CodexDeviceCode, error)
	PollDeviceFlow(ctx context.Context, authID, userCode string) (domain.CodexLoginStatus, *domain.CodexLoginTokens, error)
}

type BPSLoginProvider interface {
	CodexLoginProvider
	GenerateProfile() ([]byte, error)
}

func (s *Service) loginProvider(provider domain.DeviceLoginProvider) (CodexLoginProvider, credential.Type, error) {
	if provider == "" {
		provider = domain.DeviceLoginCodex
	}
	switch provider {
	case domain.DeviceLoginCodex:
		if s.codexLogin == nil {
			return nil, "", fmt.Errorf("%w: codex 设备码登录未装配", serviceerr.ErrUnavailable)
		}
		return s.codexLogin, credential.TypeCodexOAuth, nil
	case domain.DeviceLoginBPS:
		if s.bpsLogin == nil {
			return nil, "", fmt.Errorf("%w: BPS 设备码登录未装配", serviceerr.ErrUnavailable)
		}
		return s.bpsLogin, credential.TypeBPS, nil
	default:
		return nil, "", fmt.Errorf("%w: unknown login provider %q", ErrInvalidInput, provider)
	}
}

// StartDeviceLogin is the shared ChatGPT/BPS entry point. It only requests a
// device code and never starts a background poll.
func (s *Service) StartDeviceLogin(ctx context.Context, provider domain.DeviceLoginProvider) (*domain.CodexLoginChallenge, error) {
	p, _, err := s.loginProvider(provider)
	if err != nil {
		return nil, err
	}
	code, err := p.StartDeviceFlow(ctx)
	if err != nil {
		s.log.Warn("device login: start flow failed", logx.Error(err))
		// 上游细节只进日志：认证端点错误体可能含内部追踪串，不透给客户端。
		return nil, fmt.Errorf("%w: 设备码申请失败（上游不可用）", serviceerr.ErrUpstream)
	}
	interval := int(code.Interval.Seconds())
	if interval <= 0 {
		interval = 5
	}
	return &domain.CodexLoginChallenge{
		DeviceAuthID: code.AuthID, UserCode: code.UserCode,
		VerificationURL: code.VerificationURL,
		IntervalSeconds: interval, ExpiresInSeconds: int(code.ExpiresIn.Seconds()),
	}, nil
}

// CompleteDeviceLogin validates the local template/group before performing
// exactly one upstream poll. For an authorized BPS login, the provider SDK
// generates the DeviceProfile server-side before persistence. Pending is
// returned to the browser for a manual retry; no service-side timer, Redis
// session, or automatic request exists.
func (s *Service) CompleteDeviceLogin(ctx context.Context, provider domain.DeviceLoginProvider, authID, userCode string, tplID int64, groupID *int64) (*domain.CodexLoginResult, error) {
	p, credType, err := s.loginProvider(provider)
	if err != nil {
		return nil, err
	}
	if authID == "" || userCode == "" {
		return nil, fmt.Errorf("%w: device_auth_id 与 user_code 必填", ErrInvalidInput)
	}
	if err := s.checkCodexImportTemplate(ctx, &tplID, credType); err != nil {
		return nil, err
	}
	if groupID != nil {
		if err := s.checkGroupsExist(ctx, []int64{*groupID}); err != nil {
			return nil, err
		}
	}
	status, tokens, err := p.PollDeviceFlow(ctx, authID, userCode)
	if err != nil {
		s.log.Warn("device login: poll failed", logx.Error(err))
		return nil, fmt.Errorf("%w: 设备码轮询失败（上游不可用）", serviceerr.ErrUpstream)
	}
	switch status {
	case domain.CodexLoginPending, domain.CodexLoginExpired, domain.CodexLoginDenied:
		return &domain.CodexLoginResult{Status: status}, nil
	case domain.CodexLoginAuthorized:
	default:
		return nil, fmt.Errorf("service: 未知设备码登录状态 %q", status)
	}
	var profile []byte
	if credType == credential.TypeBPS {
		profile, err = s.bpsLogin.GenerateProfile()
		if err != nil {
			return nil, fmt.Errorf("%w: BPS profile 生成失败", serviceerr.ErrUpstream)
		}
	}
	return s.importOAuthLogin(ctx, tokens, tplID, groupID, credType, profile)
}

func (s *Service) importOAuthLogin(ctx context.Context, tokens *domain.CodexLoginTokens, tplID int64, groupID *int64, credType credential.Type, profile []byte) (*domain.CodexLoginResult, error) {
	if tokens == nil {
		return nil, fmt.Errorf("%w: 登录响应缺少凭据，请重新登录", serviceerr.ErrUpstream)
	}
	var row codexImportRow
	var err error
	if credType == credential.TypeBPS {
		row, err = newBPSOAuthRow(0, tokens.Email, tokens.AccountID, tokens.AccessToken, tokens.RefreshToken, tokens.ExpiresAt, profile)
	} else {
		row, err = newCodexOAuthRow(0, tokens.Email, tokens.AccountID, tokens.AccessToken, tokens.RefreshToken, tokens.ExpiresAt)
	}
	if err != nil {
		s.log.Warn("device login: token bundle rejected", logx.Error(err))
		// 校验文案是导入面词汇（codex_oauth_*）：对外给可操作提示，细节只进日志。
		return nil, fmt.Errorf("%w: 登录响应凭据不完整，请重新登录", serviceerr.ErrUpstream)
	}
	outcomes := s.importCodexAccounts(ctx, []codexImportRow{row}, tplID, groupID, credType, domain.CodexImportConfig{})
	if len(outcomes) != 1 {
		return nil, fmt.Errorf("service: 设备码登录落库结果缺失")
	}
	out := outcomes[0]
	if out.Err != nil {
		s.log.Warn("device login: import failed", logx.String("email", tokens.Email), logx.Error(out.Err))
		// 冲突（键已存在/账号已删除）与存储故障是两类事实：前者可操作，后者是
		// 服务器故障。上游授权码已消费，两者都要求重新登录，但状态码不能混。
		if errors.Is(out.Err, ErrConflict) {
			return nil, fmt.Errorf("%w: 登录成功但账号写入失败——请在账号列表确认或重新登录", ErrConflict)
		}
		return nil, out.Err
	}
	return &domain.CodexLoginResult{
		Status: domain.CodexLoginAuthorized, AccountID: out.AccountID,
		AccountName: out.AccountName, Email: tokens.Email,
		CodexAccountID: tokens.AccountID, Imported: out.Imported,
	}, nil
}
