// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
package domain

import "time"

// EmailTemplatePurpose 邮件模板用途。
type EmailTemplatePurpose string

const (
	EmailTemplateRegisterCode EmailTemplatePurpose = "register_code"
	EmailTemplateResetCode    EmailTemplatePurpose = "reset_code"
)

func (p EmailTemplatePurpose) Valid() bool {
	switch p {
	case EmailTemplateRegisterCode, EmailTemplateResetCode:
		return true
	}
	return false
}

// EmailTemplate 邮件模板（DB 行；缺行时走 DefaultEmailTemplate 回退）。
type EmailTemplate struct {
	Purpose   EmailTemplatePurpose
	Subject   string
	BodyText  string
	UpdatedAt time.Time
}

// EmailCodePurpose 验证码用途。
type EmailCodePurpose string

const (
	EmailCodeRegister EmailCodePurpose = "register"
	EmailCodeReset    EmailCodePurpose = "reset"
)

func (p EmailCodePurpose) Valid() bool {
	switch p {
	case EmailCodeRegister, EmailCodeReset:
		return true
	}
	return false
}

func (p EmailCodePurpose) TemplatePurpose() EmailTemplatePurpose {
	switch p {
	case EmailCodeRegister:
		return EmailTemplateRegisterCode
	case EmailCodeReset:
		return EmailTemplateResetCode
	default:
		return EmailTemplateRegisterCode
	}
}

// EmailCode 验证码行（email+purpose 唯一）。
type EmailCode struct {
	ID         int64
	Email      string
	Purpose    EmailCodePurpose
	CodeSHA256 string
	ExpiresAt  time.Time
	Attempts   int
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// 邮件验证码常量（spec R-3）。
const (
	EmailCodeTTL         = 10 * time.Minute
	EmailCodeMaxAttempts = 5
	EmailCodeRateLimit   = 60 * time.Second
	EmailCodeDigits      = 6
)

// AppName 模板变量 app_name 常量。
const AppName = "c3api"

// DefaultEmailTemplate 编译内置英文默认模板（占位符 {{code}}/{{ttl_minutes}}/{{app_name}}；
// 开源项目默认英文，管理台可按语言自定义覆盖）。
func DefaultEmailTemplate(purpose EmailTemplatePurpose) EmailTemplate {
	switch purpose {
	case EmailTemplateRegisterCode:
		return EmailTemplate{
			Purpose:  EmailTemplateRegisterCode,
			Subject:  "{{app_name}} verification code",
			BodyText: "Your verification code is {{code}}. It expires in {{ttl_minutes}} minutes. If you did not request this, please ignore this email.",
		}
	case EmailTemplateResetCode:
		return EmailTemplate{
			Purpose:  EmailTemplateResetCode,
			Subject:  "{{app_name}} password reset code",
			BodyText: "Your password reset code is {{code}}. It expires in {{ttl_minutes}} minutes. If you did not request this, please ignore this email.",
		}
	default:
		return EmailTemplate{
			Purpose:  purpose,
			Subject:  "{{app_name}} verification code",
			BodyText: "Your verification code is {{code}}. It expires in {{ttl_minutes}} minutes.",
		}
	}
}
