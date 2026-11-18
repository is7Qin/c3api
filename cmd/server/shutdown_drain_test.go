// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestShutdownHTTPServerHonorsDrain 钉住 shutdownHTTPServer 的两步语义：
//
//	drain 足够 → 在途请求自然完成（200），优雅停机真的生效；
//	drain 不足 → Shutdown 超时、Close 强断，客户端拿到错误（这是第 2 步存在的理由：
//	             长流不会因为 drain 短就把进程卡住）。
//
// 负例：把 drain 写死成常量（忽略入参）⇒ "drain 不足"那支会变成 200，红；
// 只 Shutdown 不 Close ⇒ "drain 不足"那支请求会一直挂着直到超时，红。
func TestShutdownHTTPServerHonorsDrain(t *testing.T) {
	const handlerSleep = 150 * time.Millisecond

	for _, tc := range []struct {
		name    string
		drain   time.Duration
		wantOK  bool
		comment string
	}{
		{"drain 足够：在途请求自然收尾", time.Second, true, "1s > handler 150ms"},
		{"drain 不足：到点强断", 10 * time.Millisecond, false, "10ms < handler 150ms"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, _ := newShutdownTestLogger(t)
			started := make(chan struct{})
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started) // 通知测试"handler 已经进去"，别用 sleep 猜
				time.Sleep(handlerSleep)
				w.WriteHeader(http.StatusOK)
			})}
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			require.NoError(t, err)
			go func() { _ = srv.Serve(ln) }()
			t.Cleanup(func() { _ = srv.Close() })

			// 先让请求进入 handler，再触发停机——否则测的是"拨号被拒绝"而不是 drain。
			done := make(chan error, 1)
			go func() {
				resp, err := http.Get("http://" + ln.Addr().String() + "/")
				if err != nil {
					done <- err
					return
				}
				defer resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					done <- fmt.Errorf("status %d", resp.StatusCode)
					return
				}
				done <- nil
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("请求没有进入 handler：门禁失效（不能靠 sleep 猜时序）")
			}

			start := time.Now()
			shutdownHTTPServer(context.Background(), srv, tc.drain, log)
			elapsed := time.Since(start)

			select {
			case err := <-done:
				if tc.wantOK {
					require.NoError(t, err, tc.comment)
				} else {
					require.Error(t, err, "drain 不足时请求必须被强断，而不是照样成功")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("请求既没完成也没被打断：只 Shutdown 不 Close 会让长流把进程挂住")
			}
			require.Less(t, elapsed, 3*time.Second, "停机窗口不应超出 drain 太多")
		})
	}
}

// TestMainWiresConfiguredDrain 钉住"配置真的进了调用点"。handler 级门禁只证明
// shutdownHTTPServer 尊重入参；写回 2*time.Second 常量时它照样全绿——用户要的那条链
// （硬编码 2s → 配置值）恰好没有任何东西在看。用 AST 只认 main 函数体里那一次调用的
// 第三个实参，不靠正则。
//
// 负例：main 里改成 shutdownHTTPServer(shutdownCtx, httpSrv, 2*time.Second, log) ⇒ 红。
func TestMainWiresConfiguredDrain(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "wiring_run.go", nil, 0)
	require.NoError(t, err)

	// 两级钉：停机链必须用 runtime 上的 drain 字段，而该字段必须由 cfg 投影而来。
	// 只钉其中一级都拦不住「写回常量」：直接传 cfg 的旧形态拦不住字段被写死，
	// 只钉字段也拦不住投影时写死默认值。
	wiredField := false
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != "buildRuntime" || fn.Body == nil {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || len(call.Args) < 3 {
				return true
			}
			if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "shutdownHTTPServer" && dottedExpr(call.Args[2]) == "r.drain" {
				wiredField = true
			}
			return true
		})
		return true
	})

	projected := false
	ast.Inspect(file, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok || fn.Name == nil || fn.Name.Name != "buildRuntime" || fn.Body == nil {
			return true
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			cl, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if tn, ok := cl.Type.(*ast.Ident); !ok || tn.Name != "appRuntime" {
				return true
			}
			for _, el := range cl.Elts {
				kv, ok := el.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if id, ok := kv.Key.(*ast.Ident); ok && id.Name == "drain" && dottedExpr(kv.Value) == "cfg.Server.ShutdownDrain" {
					projected = true
				}
			}
			return true
		})
		return true
	})

	require.True(t, wiredField,
		"停机链必须用 r.drain 调 shutdownHTTPServer（写回常量会让配置完全失效）")
	require.True(t, projected,
		"buildRuntime 必须把 cfg.Server.ShutdownDrain 投进 appRuntime.drain")
}

// dottedExpr 把 a.b.c 形态的表达式拍平成 "a.b.c"；其它形态返回空串。
func dottedExpr(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.SelectorExpr:
		if prefix := dottedExpr(v.X); prefix != "" {
			return prefix + "." + v.Sel.Name
		}
	}
	return ""
}
