// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package server 装配 chi 路由：/api/admin/*（静态 token OR platform_admin JWT）+
// /api/user/*（JWT 保护，register/login 公开）+ 三个 AI 端点 + /healthz。
// 架构约束：所有 API 统一收口于 /api/*，前端 SPA 占用根及 /api/user/*、/app/*，
// 两者无前缀重叠，SPA fallback 可无歧义地回 index.html。
package server

import (
	"io/fs"
	"net/http"
	"runtime"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/pkg/logx"
)

type Options struct {
	AdminToken        string
	JWTIssuer         *auth.Issuer            // platform_admin JWT 鉴权（/api/admin 扩展）
	UserStatus        auth.UserStatusProvider // 用户快照 status+role（JWT 鉴权路径禁用/降权即拒；nil = JWT 路径全拒）
	MaxInflight       int64
	ReadHeaderTimeout time.Duration
	MaxHeaderBytes    int
	AdminHandler      http.Handler // 已挂 /api/admin/* 路由
	UserHandler       http.Handler // 已挂 /api/user/* 路由（内部完成公开/JWT 分流）
	AIHandler         http.Handler // proxy 三个端点
	WebFS             fs.FS        // 前端构建产物（nil = 不挂静态资源）
	Logger            *logx.Logger
}

type Server struct {
	opts     Options
	inflight inflightCounter
	handler  http.Handler
}

func NewServer(opts Options) *Server {
	if opts.ReadHeaderTimeout == 0 {
		opts.ReadHeaderTimeout = 10 * time.Second
	}
	if opts.MaxHeaderBytes == 0 {
		opts.MaxHeaderBytes = 1 << 20
	}
	if opts.MaxInflight == 0 {
		opts.MaxInflight = 50000
	}
	s := &Server{opts: opts}

	r := chi.NewRouter()
	// 顺序：accessLog 最外层、recoverer 内层（F4）——recoverer 的 w 即
	// statusWriter，已写头判定（headersWritten）与 500 状态回写都经同一包装；
	// 旧序（recoverer 外层）recoverer 只见裸 writer，无法感知已写头。
	r.Use(accessLog(opts.Logger))
	r.Use(recoverer(opts.Logger))

	r.Get("/healthz", func(w http.ResponseWriter, r *http.Request) {
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		httpface.WriteJSON(w, http.StatusOK, map[string]any{
			"inflight":   s.inflight.Load(),
			"goroutines": runtime.NumGoroutine(),
			"heap":       ms.HeapAlloc,
		})
	})

	r.Group(func(r chi.Router) {
		// 管理面鉴权（adminAuth，定义见 middleware.go）：静态 admin token OR
		// platform_admin JWT（两个都过才拒）。/api/admin/ops/workers 运维观测在
		// AdminHandler 内（生成路由），同组鉴权。
		r.Use(adminAuth(opts))
		if opts.AdminHandler != nil {
			// 用 Handle 而非 Mount：chi v5.3.1 对同一 pattern 重复 Mount 会 panic，
			// 且 AI 组已挂 Mount("/", ...)。Handle 不剥离前缀，AdminHandler
			// （HandlerWithOptions BaseURL="/api/admin"）按完整路径 /api/admin/* 匹配。
			r.Handle("/api/admin/*", opts.AdminHandler)
		}
	})

	// /api/user 组：内部完成公开（register/login）与 JWT 保护分流。
	if opts.UserHandler != nil {
		r.Handle("/api/user/*", opts.UserHandler)
	}

	r.Group(func(r chi.Router) {
		r.Use(inflightLimiter(opts.MaxInflight, &s.inflight))
		if opts.AIHandler != nil {
			r.Mount("/", opts.AIHandler)
		}
	})

	// 静态资源 + SPA fallback：必须在 admin/AI/healthz 之后注册。
	// 说明：chi 的 NotFound() 会向已 Mount 的子路由传播（updateSubRoutes），
	// 因此这里在 Mount("/", AIHandler) 之后设置 NotFound，AI 路由的未匹配
	// 路径会进入同一 fallback；/api/admin/* 经 Handle 注册不受影响。
	if opts.WebFS != nil {
		web := webFSNoDirs{fs: opts.WebFS} // 目录请求 → 404（不渲染 HTML 目录列表）
		r.Handle("/assets/*", http.FileServerFS(web))
		// favicon 位于 dist 根（index.html 引用 /favicon.svg），不走 SPA fallback，
		// 否则会返回 index.html（content-type text/html，浏览器拒绝渲染）。
		r.Handle("/favicon.svg", http.FileServerFS(web))
		r.Get("/", func(w http.ResponseWriter, r *http.Request) {
			index, err := fs.ReadFile(opts.WebFS, "index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(index)
		})
		// SPA fallback：API 404 与页面路由彻底分离（架构根治）。
		// 所有 API 统一在 /api/* 与 /v1/*，未知 API 直接 404；其余路径
		// （/、/user/*、/app/* 等前端路由）仅对浏览器导航（Accept: text/html）
		// 回 index.html。
		r.NotFound(func(w http.ResponseWriter, r *http.Request) {
			p := r.URL.Path
			if strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/v1/") || strings.HasPrefix(p, "/v1") || strings.HasPrefix(p, "/assets/") || p == "/healthz" || p == "/favicon.svg" {
				http.NotFound(w, r)
				return
			}
			if !strings.Contains(r.Header.Get("Accept"), "text/html") {
				http.NotFound(w, r)
				return
			}
			index, err := fs.ReadFile(opts.WebFS, "index.html")
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(index)
		})
	}

	s.handler = r
	return s
}

func (s *Server) Handler() http.Handler { return s.handler }

// webFSNoDirs 包装 fs.FS：目录请求返回 fs.ErrNotExist（404）。/assets/* 若裸用
// http.FileServerFS，目录会被渲染成 HTML 目录列表（go:embed all:dist 全部内容
// 可枚举，静态资源被遍历暴露）。文件请求不受影响（Open 后 Stat 判定 IsDir）。
type webFSNoDirs struct{ fs fs.FS }

func (w webFSNoDirs) Open(name string) (fs.File, error) {
	f, err := w.fs.Open(name)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.IsDir() {
		f.Close()
		return nil, fs.ErrNotExist
	}
	return f, nil
}
