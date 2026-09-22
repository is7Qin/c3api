// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package auth 承载用户认证体系：bcrypt 密码哈希（与 sub2api 同参数）、
// JWT 签发/验证（HS256，TTL 24h）与 RBAC 中间件（/user 组 RequireJWT +
// 快照用户状态校验；/admin = 静态 token OR platform_admin JWT）。
package auth

import (
	"errors"

	"golang.org/x/crypto/bcrypt"
)

// ErrPasswordTooLong bcrypt 截断限制（≤72 字节；注册/改密拒绝超长）。
var ErrPasswordTooLong = errors.New("auth: password exceeds 72 bytes (bcrypt limit)")

// HashPassword bcrypt DefaultCost(10)，与 sub2api 完全一致——sub2api 迁移
// 存量 hash 直接可验证（同参数）。
func HashPassword(plain string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(plain), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// VerifyPassword 校验明文与 bcrypt hash（sub2api 同参数 hash 直接可验证）。
func VerifyPassword(hash, plain string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(plain)) == nil
}

// ValidatePasswordLen 密码 ≤72 字节校验（bcrypt 截断限制；超长拒绝而非
// 静默截断）。
func ValidatePasswordLen(plain string) error {
	if len([]byte(plain)) > 72 {
		return ErrPasswordTooLong
	}
	return nil
}
