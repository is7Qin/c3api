// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package user 实现 /user 面 HTTP 处理（OpenAPI tag: user 的独立
// ServerInterface）：认证（register/login 公开；me 及业务端点 JWT 保护）+
// 业务端点（groups/keys/logs/stats）。
package user

import (
	"time"

	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/service"
	"github.com/is7qin/c3api/pkg/logx"
)

// UserAPI 实现生成的 ServerInterface（user 面唯一实现）。
type UserAPI struct {
	svc *service.Service
	iss tokenIssuer
	// rules 规则引擎（/api/user/err_logs 行级脱敏用：平台问题行 error_message 按
	// Classify 判定替换固定文案；main 装配经 Router 注入——nil = 不脱敏）。
	rules *rule.RuleEngine
	// log 面日志：/forgot-password 反枚举路径仍恒 200，发送失败不再静默吞——
	// 记 Warn 后照常返回 200（main 装配经 Router 注入；nil = 静默）。
	log *logx.Logger
	// now 可注入时钟（默认 time.Now）——P3 相对窗口 `?window=` 的服务端自持时刻
	// 从这里取。与 AdminAPI.now 是**同一套机制**（每面一个可注入字段），不是第二
	// 个时钟源：生产恒 time.Now，测试注入固定时刻，故判定与断言都无墙钟依赖。
	now func() time.Time
}

// SetClock 注入相对窗口用的时钟（nil 忽略）。生产装配不调用；测试在挂路由前
// 固定时刻，使 window= 的双界整点可断言、不读墙钟。
func (h *UserAPI) SetClock(now func() time.Time) {
	if now != nil {
		h.now = now
	}
}

// tokenIssuer JWT 签发（*auth.Issuer 实现；测试可注入替身）。ver = 签发时
// users.token_version 快照（改密撤销比对源，spec 2026-08-25-jwt-password-
// revocation）。
type tokenIssuer interface {
	Issue(userID int64, email, role string, ver int64) (string, error)
}

// New 构造契约处理器（路由由 Router 组装）。
func New(svc *service.Service, iss tokenIssuer) *UserAPI {
	return &UserAPI{svc: svc, iss: iss, now: time.Now}
}
