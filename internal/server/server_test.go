// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package server

import (
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/pkg/aiclient"
)

func TestHealthz(t *testing.T) {
	s := NewServer(Options{AdminToken: "tok", Logger: nil})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/healthz")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, 200, resp.StatusCode)
	var body map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Contains(t, body, "inflight")
	require.Contains(t, body, "goroutines")
	require.Contains(t, body, "heap")
}

func TestUnknownPath404(t *testing.T) {
	s := NewServer(Options{AdminToken: "tok"})
	req := httptest.NewRequest(http.MethodGet, "/nope", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, 404, rec.Code)
}

// 规格 §6.1：/admin/* 需要 Bearer admin token；无/错 token → 401。
func TestAdminAuthRequired(t *testing.T) {
	admin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	})
	s := NewServer(Options{AdminToken: "tok", AdminHandler: admin})

	for _, tc := range []struct {
		name, auth string
		want       int
	}{
		{"no token", "", 401},
		{"wrong token", "Bearer nope", 401},
		{"non-bearer", "tok", 401},
		{"right token", "Bearer tok", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/admin/groups", nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			require.Equal(t, tc.want, rec.Code)
		})
	}
}

// 回归：生产 main.go 同时设置 AdminHandler + AIHandler（各自 Mount("/") 曾
// 触发 chi 重复 Mount panic）。断言 /admin/* 与 /v1/* 分别打到对应 handler。
func TestAdminAndAIHandlersCoexist(t *testing.T) {
	admin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpface.WriteJSON(w, http.StatusOK, map[string]any{"handler": "admin", "path": r.URL.Path})
	})
	ai := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpface.WriteJSON(w, http.StatusOK, map[string]any{"handler": "ai", "path": r.URL.Path})
	})
	s := NewServer(Options{AdminToken: "tok", AdminHandler: admin, AIHandler: ai})

	for _, tc := range []struct {
		path, auth, wantHandler string
	}{
		{"/admin/templates", "Bearer tok", "admin"},
		{"/admin/templates/1", "Bearer tok", "admin"},
		{"/v1/chat/completions", "", "ai"},
		{"/v1/images/generations", "", "ai"},
		{"/v1/images/edits", "", "ai"},
	} {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		if tc.auth != "" {
			req.Header.Set("Authorization", tc.auth)
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		require.Equal(t, 200, rec.Code, "path %s: %s", tc.path, rec.Body.String())
		var body map[string]string
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		require.Equal(t, tc.wantHandler, body["handler"], "path %s", tc.path)
		require.Equal(t, tc.path, body["path"], "path must be forwarded unchanged")
	}
}

// 回归：/favicon.svg 必须返回真实 SVG（index.html 引用 dist 根 favicon），
// 不能落入 SPA fallback 返回 text/html —— 浏览器会拒绝渲染错误 MIME 的 favicon。
func TestFaviconServedFromWebFS(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte(`<link rel="icon" href="/favicon.svg" />`)},
		"favicon.svg":   &fstest.MapFile{Data: []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`)},
		"assets/app.js": &fstest.MapFile{Data: []byte(`console.log("x")`)},
	}
	s := NewServer(Options{AdminToken: "tok", WebFS: fsys})

	req := httptest.NewRequest(http.MethodGet, "/favicon.svg", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, 200, rec.Code)
	require.Equal(t, "image/svg+xml", rec.Header().Get("Content-Type"))
	require.Contains(t, rec.Body.String(), "<svg")
	require.NotContains(t, rec.Body.String(), "<link") // 不能是 index.html

	// 对照组：SPA fallback 路径仍回 index.html。
	req = httptest.NewRequest(http.MethodGet, "/groups", nil)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, 200, rec.Code)
	require.Equal(t, "text/html; charset=utf-8", rec.Header().Get("Content-Type"))
}

// O1 收尾（评审项）：/assets/* 不得渲染 HTML 目录列表——目录请求 404、文件
// 200（go:embed all:dist 裸 FileServerFS 会把目录枚举成 HTML 列表，静态资源
// 被遍历暴露）。
func TestAssetsNoDirectoryListing(t *testing.T) {
	fsys := fstest.MapFS{
		"index.html":    &fstest.MapFile{Data: []byte(`<html></html>`)},
		"assets":        &fstest.MapFile{Mode: fs.ModeDir},
		"assets/app.js": &fstest.MapFile{Data: []byte(`console.log(1)`)},
	}
	s := NewServer(Options{AdminToken: "tok", WebFS: fsys})

	req := httptest.NewRequest(http.MethodGet, "/assets/", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code, "目录请求 404（不渲染目录列表）")

	req = httptest.NewRequest(http.MethodGet, "/assets/app.js", nil)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "文件请求不受影响")
	require.Contains(t, rec.Body.String(), "console.log(1)")
}

// 规格 §10.6：全局在途上限，超限立即 429 + Retry-After: 1。
func TestInflightLimiterRejects(t *testing.T) {
	release := make(chan struct{})
	ai := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(200)
	})
	s := NewServer(Options{AdminToken: "tok", MaxInflight: 1, AIHandler: ai})

	firstDone := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		firstDone <- rec.Code
	}()
	for s.inflight.Load() < 1 { // 等首请求进入 limiter
		runtime.Gosched()
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, "1", rec.Header().Get("Retry-After"))

	close(release)
	require.Equal(t, http.StatusOK, <-firstDone)
	require.Zero(t, s.inflight.Load())
}

// --- Phase 3a：/admin 鉴权扩展（静态 token OR platform_admin JWT）+ /user 挂载 ---

// fakeUserStatus 测试替身（快照用户状态 provider）。
type fakeUserStatus struct{ disabled map[int64]bool }

func (f fakeUserStatus) UserStatus(userID int64) (domain.UserStatus, bool) {
	if f.disabled[userID] {
		return domain.UserStatusDisabled, true
	}
	return domain.UserStatusActive, true
}

// 规格 Phase 3a：/admin = 静态 token OR platform_admin JWT（两个都过才拒）。
func TestAdminAuthTokenOrPlatformJWT(t *testing.T) {
	iss := auth.NewIssuer("secret")
	adminTok, err := iss.Issue(1, "admin@example.com", string(domain.RolePlatformAdmin))
	require.NoError(t, err)
	userTok, err := iss.Issue(2, "user@example.com", string(domain.RoleUser))
	require.NoError(t, err)
	admin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	s := NewServer(Options{
		AdminToken:   "tok",
		JWTIssuer:    iss,
		UserStatus:   fakeUserStatus{},
		AdminHandler: admin,
	})

	for _, tc := range []struct {
		name, auth string
		want       int
	}{
		{"static token", "Bearer tok", 200},
		{"platform_admin JWT", "Bearer " + adminTok, 200},
		{"user JWT", "Bearer " + userTok, 401},
		{"no token", "", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/admin/groups", nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			require.Equal(t, tc.want, rec.Code)
		})
	}
}

// 中间件注入（决策 5）：JWT 鉴权路径把 claims.UserID 写入 context（兑换码
// created_by 用）；静态 admin token 路径不注入（handler 读到 0 = 系统）。
func TestAdminUserIDContextInjection(t *testing.T) {
	iss := auth.NewIssuer("secret")
	tok, err := iss.Issue(7, "admin@example.com", string(domain.RolePlatformAdmin))
	require.NoError(t, err)
	var gotID int64
	var gotOK bool
	admin := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotID, gotOK = UserIDFromContext(r.Context())
		w.WriteHeader(200)
	})

	for _, tc := range []struct {
		name, auth string
		wantOK     bool
		wantID     int64
	}{
		{"platform_admin JWT 注入 UserID", "Bearer " + tok, true, 7},
		{"静态 admin token 不注入", "Bearer tok", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewServer(Options{AdminToken: "tok", JWTIssuer: iss, UserStatus: fakeUserStatus{}, AdminHandler: admin})
			req := httptest.NewRequest(http.MethodGet, "/admin/groups", nil)
			req.Header.Set("Authorization", tc.auth)
			rec := httptest.NewRecorder()
			s.Handler().ServeHTTP(rec, req)
			require.Equal(t, 200, rec.Code)
			require.Equal(t, tc.wantOK, gotOK, "context 注入标记")
			require.Equal(t, tc.wantID, gotID, "UserID 值")
		})
	}

	// 无 UserStatus provider（UserStatus=nil）的 JWT 路径同样注入。
	t.Run("UserStatus nil 仍注入", func(t *testing.T) {
		s := NewServer(Options{AdminToken: "tok", JWTIssuer: iss, AdminHandler: admin})
		req := httptest.NewRequest(http.MethodGet, "/admin/groups", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		require.Equal(t, 200, rec.Code)
		require.True(t, gotOK)
		require.Equal(t, int64(7), gotID)
	})
}

// 禁用 platform_admin 的 JWT → /admin 401（快照校验，不用 DB 直查）。
func TestAdminPlatformJWTPartialAdmin(t *testing.T) {
	iss := auth.NewIssuer("secret")
	tok, err := iss.Issue(1, "admin@example.com", string(domain.RolePlatformAdmin))
	require.NoError(t, err)
	s := NewServer(Options{
		AdminToken:   "tok",
		JWTIssuer:    iss,
		UserStatus:   fakeUserStatus{disabled: map[int64]bool{1: true}},
		AdminHandler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }),
	})
	req := httptest.NewRequest(http.MethodGet, "/admin/groups", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, 401, rec.Code, "禁用 platform_admin JWT 必须拒绝")
}

// /user 挂载：注册公开可达；/user 其余路径经用户面路由器处理（401 无 JWT）。
func TestUserMount(t *testing.T) {
	userH := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	s := NewServer(Options{AdminToken: "tok", UserHandler: userH})
	req := httptest.NewRequest(http.MethodGet, "/user/whatever", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	require.Equal(t, 200, rec.Code, "/user/* 必须由 UserHandler 处理")
}

// --- 补压测修复回归（resp-ws 501 + statusWriter Hijack 语义） ---

// TestWSUpgradeThroughMiddlewareChain resp-ws 升级走完整中间件链必须成功
// （补压测发现：statusWriter 缺 http.Hijacker → accessLog 包裹后
// coder/websocket Accept 拿不到 Hijacker → 全部升级被 501 拒）。既有 proxy
// 单测直打 handler 绕过 server 中间件链，未暴露——此处必须过完整链
// （recoverer + accessLog + inflightLimiter）后升级并真实收发帧。
func TestWSUpgradeThroughMiddlewareChain(t *testing.T) {
	ai := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := aiclient.AcceptResponsesWS(w, r)
		if err != nil {
			return // Accept 已写出 4xx/501（回归点：此处不得走到）
		}
		defer conn.CloseNow()
		typ, msg, err := conn.Read(context.Background()) // 读一帧回一帧，证明连接真实可用
		if err != nil {
			return
		}
		_ = conn.Write(context.Background(), typ, msg)
	})
	s := NewServer(Options{AdminToken: "tok", AIHandler: ai})
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()

	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http")
	conn, _, err := websocket.Dial(context.Background(), wsURL+"/v1/responses", nil)
	require.NoError(t, err, "resp-ws 升级必须经完整中间件链成功（回归：statusWriter 缺 Hijacker → 501）")
	defer conn.CloseNow()
	require.NoError(t, conn.Write(context.Background(), websocket.MessageText, []byte("hi")))
	typ, msg, err := conn.Read(context.Background())
	require.NoError(t, err)
	require.Equal(t, websocket.MessageText, typ)
	require.Equal(t, "hi", string(msg))
}

// TestStatusWriterHijackSemantics statusWriter.Hijack 转发语义：Hijack 必须
// 直达底层 writer（接入真实 net/http server——hijack 后裸写 HTTP 响应，
// 客户端原样收到即证明转发成功）；底层不支持 Hijacker（httptest.Recorder）
// → 明确报错而非 panic。写头后的语义由底层自带（Go 1.26 写头后 Hijack 先
// flush 再接管——coder/websocket Accept 正是先 WriteHeader(101) 后 Hijack
// 的调用顺序），包装层不添加状态判定。
func TestStatusWriterHijackSemantics(t *testing.T) {
	t.Run("forwards to underlying", func(t *testing.T) {
		type hres struct{ err error }
		res := make(chan hres, 1)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, rw, err := (&statusWriter{ResponseWriter: w}).Hijack()
			if err != nil {
				res <- hres{err}
				return
			}
			defer conn.Close()
			_, _ = rw.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok")
			_ = rw.Flush()
			res <- hres{}
		}))
		defer srv.Close()
		resp, err := http.Get(srv.URL)
		require.NoError(t, err)
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		require.Equal(t, "ok", string(b), "hijack 后裸写响应必须原样到达客户端（转发失败的信号）")
		require.NoError(t, (<-res).err, "Hijack 必须转发成功")
	})

	t.Run("errors when underlying lacks hijacker", func(t *testing.T) {
		rec := httptest.NewRecorder()
		_, _, err := (&statusWriter{ResponseWriter: rec}).Hijack()
		require.Error(t, err)
		require.Contains(t, err.Error(), "does not implement http.Hijacker")
	})
}

// TestStatusWriterUnwrapResponseController statusWriter.Unwrap 语义（C-P2-1
// 前置）：包装 statusWriter 后 http.ResponseController.SetWriteDeadline 必须
// 沿 Unwrap 链穿透到真实 writer——无 Unwrap 的包装层全链 ErrNotSupported，
// sserelay 的写侧 deadline 与 ctx 取消联动（半开客户端写卡死修复）在
// accessLog 包裹后直接失效。
func TestStatusWriterUnwrapResponseController(t *testing.T) {
	t.Run("SetWriteDeadline reaches underlying through statusWriter", func(t *testing.T) {
		wr := &deadlineRecorder{deadline: make(chan time.Time, 1)}
		err := http.NewResponseController(&statusWriter{ResponseWriter: wr}).SetWriteDeadline(time.Now())
		require.NoError(t, err)
		select {
		case dl := <-wr.deadline:
			require.False(t, dl.IsZero(), "deadline 必须原样穿透")
		default:
			t.Fatal("SetWriteDeadline 未穿透 statusWriter 到达底层 writer")
		}
	})

	t.Run("errors when underlying lacks SetWriteDeadline", func(t *testing.T) {
		sw := &statusWriter{ResponseWriter: httptest.NewRecorder()}
		err := http.NewResponseController(sw).SetWriteDeadline(time.Now())
		require.Error(t, err, "httptest.Recorder 无 SetWriteDeadline → ErrNotSupported（语义同底层不支持）")
	})
}

// deadlineRecorder 记录 SetWriteDeadline 调用的最小 writer（验证 Unwrap 链穿透）。
type deadlineRecorder struct {
	httptest.ResponseRecorder
	deadline chan time.Time
}

func (w *deadlineRecorder) SetWriteDeadline(t time.Time) error {
	w.deadline <- t
	return nil
}
