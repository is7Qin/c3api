// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/service"
)

// Router 组装 /user 组路由（挂载于 /user/*）：
// 公开路径（/user/auth/register、/user/auth/login）跳过 JWT；其余路径
// RequireJWT（验证 + 内存快照用户状态校验）。生成路由的 spec 路径自带
// /user 前缀（与 /admin 的 spec 相对路径不同——/logs 等路径已被管理面占用，
// 不能同 spec 路径共存），故无 BaseURL。
// rl 为公开面 bcrypt 节流（F3，per-IP token 桶——见 ratelimit.go）；nil =
// 不限速（测试面；生产 main 恒传 NewIPRateLimiter 生产参数）。
func Router(svc *service.Service, iss *auth.Issuer, users auth.UserStatusProvider, rl *IPRateLimiter) http.Handler {
	publicPaths := map[string]bool{
		"/user/auth/register": true,
		"/user/auth/login":    true,
	}
	api := New(svc, iss)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if publicPaths[req.URL.Path] {
				// F3：bcrypt 登录/注册节流（防公网并发 login/register 烧 CPU——
				// bcrypt 单次 50-100ms CPU）。限流在 handler 之前：超速请求不触
				// 达 bcrypt。per-IP 内存桶——多实例各自独立，防单点 CPU 烧毁的
				// 足够防线（注释见 ratelimit.go）。
				if rl != nil && !rl.Allow(clientIP(req)) {
					httpface.WriteErr(w, http.StatusTooManyRequests, "rate limited")
					return
				}
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
