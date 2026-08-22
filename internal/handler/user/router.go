// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/service"
)

// Router 组装 /api/user 组路由（挂载于 /api/user/*）：
// 公开路径（/api/user/auth/register、/api/user/auth/login）跳过 JWT；其余路径
// RequireJWT（验证 + 内存快照用户状态校验）。生成路由的 spec 路径自带
// /api/user 前缀，故无独立 BaseURL，HandlerWithOptions 直接使用 spec 路径。
// rules 为规则引擎（/api/user/err_logs 行级脱敏用；main 装配注入——非 New，
// 测试构造零回归；nil = 不脱敏）。
func Router(svc *service.Service, iss *auth.Issuer, users auth.UserStatusProvider, rules *rule.RuleEngine) http.Handler {
	publicPaths := map[string]bool{
		"/api/user/auth/register":        true,
		"/api/user/auth/login":           true,
		"/api/user/auth/register-code":   true,
		"/api/user/auth/forgot-password": true,
		"/api/user/auth/reset-password":  true,
	}
	api := New(svc, iss)
	api.rules = rules
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if publicPaths[req.URL.Path] {
				next.ServeHTTP(w, req)
				return
			}
			auth.RequireJWT(iss, users)(next).ServeHTTP(w, req)
		})
	})
	// BaseRouter 传入带中间件的路由（否则 HandlerWithOptions 内部新建裸路由，
	// 公开/受保护分流失效）。
	return HandlerWithOptions(api, ChiServerOptions{
		BaseRouter: r,
		ErrorHandlerFunc: func(w http.ResponseWriter, req *http.Request, err error) {
			httpface.WriteErr(w, http.StatusBadRequest, err.Error())
		},
	})
}
