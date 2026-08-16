// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package user 实现 /user 面 HTTP 处理（OpenAPI tag: user 的独立
// ServerInterface）：认证（register/login 公开；me 及业务端点 JWT 保护）+
// 业务端点（groups/keys/logs/stats，Phase 3a Task 4）。
package user

import (
	"encoding/json"
	"net/http"

	"github.com/is7qin/c3api/internal/service"
)

// UserAPI 实现生成的 ServerInterface（user 面唯一实现）。
type UserAPI struct {
	svc *service.Service
	iss tokenIssuer
}

// tokenIssuer JWT 签发（*auth.Issuer 实现；测试可注入替身）。
type tokenIssuer interface {
	Issue(userID int64, email, role string) (string, error)
}

// New 构造契约处理器（路由由 Router 组装）。
func New(svc *service.Service, iss tokenIssuer) *UserAPI {
	return &UserAPI{svc: svc, iss: iss}
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	return dec.Decode(v)
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
