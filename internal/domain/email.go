// SPDX-License-Identifier: AGPL-3.0-or-later
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
	ID        int64
	Email     string
	Purpose   EmailCodePurpose
	CodeSHA256 string
	ExpiresAt time.Time
	Attempts  int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// 邮件验证码常量（spec R-3）。
const (
	EmailCodeTTL        = 10 * time.Minute
	EmailCodeMaxAttempts = 5
	EmailCodeRateLimit   = 60 * time.Second
	EmailCodeDigits      = 6
)

// AppName 模板变量 app_name 常量。
const AppName = "c3api"

// DefaultEmailTemplate 编译内置中文默认模板（含 {{code}}/{{ttl_minutes}}/{{app_name}}）。
func DefaultEmailTemplate(purpose EmailTemplatePurpose) EmailTemplate {
	switch purpose {
	case EmailTemplateRegisterCode:
		return EmailTemplate{
			Purpose:  EmailTemplateRegisterCode,
			Subject:  "【{{app_name}}】注册验证码",
			BodyText: "您的注册验证码是 {{code}}，有效期 {{ttl_minutes}} 分钟。如非本人操作请忽略。",
		}
	case EmailTemplateResetCode:
		return EmailTemplate{
			Purpose:  EmailTemplateResetCode,
			Subject:  "【{{app_name}}】重置密码验证码",
			BodyText: "您的重置密码验证码是 {{code}}，有效期 {{ttl_minutes}} 分钟。如非本人操作请忽略。",
		}
	default:
		return EmailTemplate{
			Purpose:  purpose,
			Subject:  "【{{app_name}}】验证码",
			BodyText: "您的验证码是 {{code}}，有效期 {{ttl_minutes}} 分钟。",
		}
	}
}
