// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
package service

import (
	"context"
	"fmt"
	"time"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// SendRegisterCode 注册验证码发送（public endpoint）：
func (s *Service) SendRegisterCode(ctx context.Context, email string) error {
	if s.settingValue("signup_enabled") != "true" {
		return ErrSignupDisabled
	}
	if s.settingValue("mail.register_verification") != "true" {
		return fmt.Errorf("%w: %s", ErrInvalidInput, EmailVerificationRequired)
	}
	if !validEmail(email) {
		return ErrInvalidInput
	}
	// 查重：已注册 → 静默抑制发送仍视为成功（不泄露存在性恶化，本就 409 泄露，此处不恶化）
	existing, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		return err
	}
	if existing != nil {
		return nil // suppress send, still 200 at handler
	}
	// 限频：updated_at <60s → 429
	if code, err := s.store.GetEmailCode(ctx, email, string(domain.EmailCodeRegister)); err == nil && code != nil {
		if time.Since(code.UpdatedAt) < domain.EmailCodeRateLimit {
			return ErrTooManyRequests
		}
	} else if err != nil {
		return err
	}
	plain, sha, err := generateCode()
	if err != nil {
		return err
	}
	expires := time.Now().Add(domain.EmailCodeTTL)
	if _, err := s.store.UpsertEmailCode(ctx, email, string(domain.EmailCodeRegister), sha, expires); err != nil {
		return err
	}
	// 同步发送；若 mail 未配置（disabled），SendRegisterCode 调用方决定：verif=on 时必须可发，否则 500？
	// spec: register-code 在 verif 关闭时直接 400 同哨兵（无人合法调用，防探测）——由 handler 层 gate。
	// 此处若 mail not configured → 返回错误由 handler 转 500（邮件服务不可用）。
	if err := s.sendMail(ctx, email, domain.EmailCodeRegister, plain); err != nil {
		if s.log != nil {
			s.log.Error("register code mail failed", logx.String("email", email), logx.Error(err))
		}
		return err
	}
	return nil
}

// SendForgotPasswordCode 忘记密码发码：恒 200 空转，实际发送仅当 enabled && 账号存在 && 未限频。
func (s *Service) SendForgotPasswordCode(ctx context.Context, email string) error {
	// 反枚举：无论何种情况调用方都返回 200，内部静默
	if !validEmail(email) {
		return nil
	}
	if !s.mailEnabled() {
		return nil
	}
	existing, err := s.store.GetUserByEmail(ctx, email)
	if err != nil || existing == nil {
		return nil
	}
	// 限频
	if code, err := s.store.GetEmailCode(ctx, email, string(domain.EmailCodeReset)); err == nil && code != nil {
		if time.Since(code.UpdatedAt) < domain.EmailCodeRateLimit {
			return nil // suppress but still 200 at handler
		}
	} else if err != nil {
		return nil // ignore error, still 200
	}
	plain, sha, err := generateCode()
	if err != nil {
		if s.log != nil {
			s.log.Error("generate reset code failed", logx.Error(err))
		}
		return nil
	}
	expires := time.Now().Add(domain.EmailCodeTTL)
	if _, err := s.store.UpsertEmailCode(ctx, email, string(domain.EmailCodeReset), sha, expires); err != nil {
		return nil
	}
	if err := s.sendMail(ctx, email, domain.EmailCodeReset, plain); err != nil {
		if s.log != nil {
			s.log.Error("reset code mail failed", logx.String("email", email), logx.Error(err))
		}
		return nil // still 200
	}
	return nil
}

// verifyAndConsume 校验并消费验证码（原子：校验失败递增 attempts，成功删除）。
func (s *Service) verifyAndConsume(ctx context.Context, email, purpose, plain string) error {
	row, err := s.store.GetEmailCode(ctx, email, purpose)
	if err != nil {
		return err
	}
	if row == nil {
		return fmt.Errorf("%w: code invalid", ErrInvalidInput)
	}
	if time.Now().After(row.ExpiresAt) {
		_ = s.store.DeleteEmailCode(ctx, email, purpose)
		return fmt.Errorf("%w: code expired", ErrInvalidInput)
	}
	if row.Attempts >= domain.EmailCodeMaxAttempts {
		return fmt.Errorf("%w: too many attempts", ErrInvalidInput)
	}
	if row.CodeSHA256 != hashCode(plain) {
		newAttempts, err := s.store.IncrementEmailCodeAttempts(ctx, email, purpose)
		if err != nil {
			return err
		}
		if newAttempts >= domain.EmailCodeMaxAttempts {
			return fmt.Errorf("%w: too many attempts", ErrInvalidInput)
		}
		return fmt.Errorf("%w: code mismatch", ErrInvalidInput)
	}
	// 成功消费
	_ = s.store.DeleteEmailCode(ctx, email, purpose)
	return nil
}

// RegisterUserWithCode 带验证码校验的注册入口（handler 侧根据 verif 开关分发）。
func (s *Service) RegisterUserWithCode(ctx context.Context, email, password, code string) (*domain.User, error) {
	if password == "" || auth.ValidatePasswordLen(password) != nil {
		return nil, ErrInvalidInput
	}
	verifOn := s.settingValue("mail.register_verification") == "true"
	if verifOn {
		if code == "" {
			return nil, fmt.Errorf("%w: %s", ErrInvalidInput, EmailVerificationRequired)
		}
		if err := s.verifyAndConsume(ctx, email, string(domain.EmailCodeRegister), code); err != nil {
			return nil, err
		}
	}
	// 走既有管线（复用 RegisterUser 逻辑，但已通过校验，直接复用其后续步骤；为避免重复校验，内联调用）
	// 为保持 bootstrap/默认值/bonus/token 契约，委托给 RegisterUser 的后半段：直接调用 RegisterUser 会重复验 signup/book？
	// 简化：直接调用 RegisterUser（其内部不再做验证码校验，verif 已处理）
	return s.RegisterUser(ctx, email, password)
}

// ResetPassword 验证码校验后更新密码。
func (s *Service) ResetPassword(ctx context.Context, email, code, newPassword string) error {
	if !validEmail(email) {
		return ErrInvalidInput
	}
	if newPassword == "" || auth.ValidatePasswordLen(newPassword) != nil {
		return ErrInvalidInput
	}
	if err := s.verifyAndConsume(ctx, email, string(domain.EmailCodeReset), code); err != nil {
		return err
	}
	u, err := s.store.GetUserByEmail(ctx, email)
	if err != nil {
		return err
	}
	if u == nil {
		return ErrNotFound
	}
	hash, err := auth.HashPassword(newPassword)
	if err != nil {
		return err
	}
	return s.store.UpdateUserPassword(ctx, u.ID, hash)
}
