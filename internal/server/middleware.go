// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// adminUserIDKey /admin 认证中间件写入的 platform_admin 用户 id（created_by 用，
// 决策 5：0 = 系统）。静态 admin token 路径不写入（handler 读到 0）。
type adminUserIDKey struct{}

// UserIDFromContext 取 /admin JWT 鉴权路径注入的用户 id（兑换码生成 created_by
// 用）；静态 admin token 路径未注入 → (0, false)。
func UserIDFromContext(ctx context.Context) (int64, bool) {
	id, ok := ctx.Value(adminUserIDKey{}).(int64)
	return id, ok
}

// adminAuth 管理面鉴权（/admin 组，含 /admin/ops/workers 运维观测）= 静态
// admin token OR platform_admin JWT（两个都过才拒）。JWT 路径同样做快照用户
// 状态校验（禁用即拒；评审定夺②）。
// admin.token 可空（spec 2026-08-15）：空 = 不启用静态 token 鉴权，/admin
// 仅接受 platform_admin JWT。空守卫使静态路径永不匹配——理由 = 语义显式化
// + h2/TLS 纵深防御：h1 下 Go textproto 修剪头值两端 OWS，"Bearer 尾空击穿"
// 不存在（实测见 spec 背景 6）；Go http2 server 不修剪头值，未来启用 h2 后
// 守卫防击穿。
func adminAuth(opts Options) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			authz := req.Header.Get("Authorization")
			if opts.AdminToken != "" && authz == "Bearer "+opts.AdminToken {
				// 静态 admin token 路径不注入 UserID（决策 5：handler 读到 0 = 系统）
				next.ServeHTTP(w, req)
				return
			}
			if opts.JWTIssuer != nil && strings.HasPrefix(authz, "Bearer ") {
				claims, err := opts.JWTIssuer.Verify(strings.TrimPrefix(authz, "Bearer "))
				if err == nil && claims.Role == string(domain.RolePlatformAdmin) {
					active := false
					if opts.UserStatus == nil {
						active = true
					} else if st, ok := opts.UserStatus.UserStatus(claims.UserID); ok && st == domain.UserStatusActive {
						active = true
					}
					if active {
						// JWT 路径注入 claims.UserID（兑换码 created_by 用，决策 5）
						ctx := context.WithValue(req.Context(), adminUserIDKey{}, claims.UserID)
						next.ServeHTTP(w, req.WithContext(ctx))
						return
					}
				}
			}
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
		})
	}
}

type inflightCounter struct{ v atomic.Int64 }

func (c *inflightCounter) Inc() int64  { return c.v.Add(1) }
func (c *inflightCounter) Dec()        { c.v.Add(-1) }
func (c *inflightCounter) Load() int64 { return c.v.Load() }

// inflightLimiter 全局在途上限（规格 §10.6）：超限立即 429。
func inflightLimiter(max int64, c *inflightCounter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c.Inc() > max {
				c.Dec()
				w.Header().Set("Retry-After", "1")
				writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "server overloaded"})
				return
			}
			defer c.Dec()
			next.ServeHTTP(w, r)
		})
	}
}

// accessLog 请求级 Debug 追踪（规格 §7.1：生产 warn 不输出）。
func accessLog(log *logx.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			reqID := r.Header.Get("X-Request-Id")
			if reqID == "" {
				reqID = uuid.NewString()
			}
			r.Header.Set("X-Request-Id", reqID)
			sw := &statusWriter{ResponseWriter: w, status: 200}
			next.ServeHTTP(sw, r)
			if log != nil {
				log.Debug("http request",
					logx.String("request_id", reqID),
					logx.String("method", r.Method),
					logx.String("path", r.URL.Path),
					logx.Int("status", sw.status),
					logx.Duration("duration", time.Since(start)),
				)
			}
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

// Flush 委托给内层 writer（SSE 事件级冲刷必需）。
// 嵌入 http.ResponseWriter 只提升 Header/Write/WriteHeader 三个方法，
// 不带 Flush——包装层不透传的话，下游 sseWriter 拿到的 Flusher 是 nil，
// 流只能攒 4KB 缓冲批量放出，首字节延迟实测 ~145ms（Task 9 压测发现）。
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack 委托给内层 writer（WS 升级必需——coder/websocket Accept 要求
// http.Hijacker，补压测发现：accessLog 包裹后 resp-ws 全部升级被 501 拒）。
// 纯转发不添加状态判定：header 是否已写等语义由底层 net/http 自带（实测
// Go 1.26 写头后 Hijack 先 flush 再接管——coder/websocket Accept 正是先
// WriteHeader(101) 后 Hijack 的调用顺序，转发即兼容）。Hijack 后连接脱离
// HTTP 服务管理（http.Hijacker 文档语义）。
func (w *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, errors.New("underlying ResponseWriter does not implement http.Hijacker")
}

// Unwrap 暴露内层 ResponseWriter（与 Hijack 同款的纯转发模式）：
// http.ResponseController 的 SetWriteDeadline/EnableFullDuplex 等沿
// Unwrap 链下探到真实 writer 才能生效——无此转发全链 ErrNotSupported
// （C-P2-1 写侧 deadline 与 ctx 取消联动的前置修复：accessLog 包裹后
// sserelay 的取消联动 deadline 必须能穿透本层）。
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func recoverer(log *logx.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					if log != nil {
						log.Error("panic recovered", logx.Any("panic", rec))
					}
					writeJSON(w, http.StatusInternalServerError, map[string]any{"error": "internal error"})
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
