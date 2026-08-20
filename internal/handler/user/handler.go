// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package user 实现 /user 面 HTTP 处理（OpenAPI tag: user 的独立
// ServerInterface）：认证（register/login 公开；me 及业务端点 JWT 保护）+
// 业务端点（groups/keys/logs/stats，Phase 3a Task 4）。
package user

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/service"
)

// UserAPI 实现生成的 ServerInterface（user 面唯一实现）。
type UserAPI struct {
	svc *service.Service
	iss tokenIssuer
	// rules 规则引擎（/api/user/err_logs 行级脱敏用：平台问题行 error_message 按
	// Classify 判定替换固定文案；main 装配经 Router 注入——nil = 不脱敏）。
	rules *rule.RuleEngine
}

// tokenIssuer JWT 签发（*auth.Issuer 实现；测试可注入替身）。
type tokenIssuer interface {
	Issue(userID int64, email, role string) (string, error)
}

// New 构造契约处理器（路由由 Router 组装）。
func New(svc *service.Service, iss tokenIssuer) *UserAPI {
	return &UserAPI{svc: svc, iss: iss}
}

// decode 严格解码（用户面全部 JSON 入参共用，与管理面 handler.decode 同款）：
// 未知字段 → 错误（拼错字段名从 200 静默不生效变 400 显式——spec 2026-08-17
// 边界收敛）；二次 Decode 拒尾随数据（io.EOF 才算完）。
func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return errors.New("unexpected trailing data after JSON body")
		}
		return err
	}
	return nil
}

// deref 返回指针指向的值；nil 时返回零值。
func deref[T any](p *T) T {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

// ptr 返回指向 v 的指针（构造契约指针字段）。
func ptr[T any](v T) *T { return &v }
