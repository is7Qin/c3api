// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 「schema 不可服务」时的运行面（spec §2.6：不可服务 = NotReady，不是 CrashLoop）。
//
// 为什么这里是**最小**服务器，而不是把闸门塞进每个装配阶段：阶段构造函数里有
// 启动期 DB 读（代理装配门、供应商残留校验、settings 首载、快照首刷），库与二进制
// 不兼容时它们只会 fatal——`os.Exit` 在 k8s 就是 CrashLoopBackOff，会把一次坏发布
// 放大成整集群事故。库都用不了的时候，构建业务图没有任何收益，只会给"进程为什么
// 退出"增加更多可能。所以判定在 main 里分叉：能服务 → 完整装配；不能服务 → 只答
// /healthz 与 /readyz，业务面 503，零 worker、零快照。
package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/is7qin/c3api/internal/db/migrate"
	"github.com/is7qin/c3api/internal/server"
	"github.com/is7qin/c3api/pkg/logx"
)

// dispatchServe 是启动期的**唯一分叉**（spec §2.5/§2.6）：
//
//   - schema 可服务 → 执行 serve（完整阶段图 + 请求面）；
//   - 不可服务 → 只起探针面，**阻塞到 ctx 取消**，绝不把自己变成致命错误。
//
// 抽成函数是为了让「不可服务 = NotReady 而不是 CrashLoop」这条承诺可以被直接
// 驱动：测试用 not-ready gate 调它，断言探针面真的起来了、serve 没被调用、
// 致命出口（fatalf）没被调用（见 dispatch_test.go）。分叉写在 main 里的话，
// 这三件事都只能靠「整棵装配图 + 子进程退出码」间接推断。
//
// serve 是回调而不是预构建的阶段图：阶段构造函数里有启动期 DB 读，库不兼容时
// 它们只会 fatal——回调在不可服务时不被调用，这条顺序就是「进程活着」的保证。
func dispatchServe(ctx context.Context, in *infraFace, serve func()) {
	if in.schema.ready() {
		serve()
		return
	}
	serveNotReady(ctx, in)
}

// serveNotReady 起一个只答探针的 HTTP 面并等到停机信号。
//
// 它不做 worker 管理（没有 worker），但**必须有**优雅收尾：监听器要在退出前
// Shutdown，否则连探针长连接都会变成 RST——编排器看到的是"killed"，不是
// "terminated"，排障时又是一条假线索。
func serveNotReady(ctx context.Context, in *infraFace) {
	log := in.log
	gate := in.schema
	status, _ := gate.status()
	log.Warn("schema gate: this binary cannot serve the database schema it found; only /healthz and /readyz will answer, business surfaces return 503",
		logx.String("mode", status.Mode),
		logx.String("reason", gate.decision.Reason),
		logx.Int64("db_head", status.DB),
		logx.Int64("db_contract", status.DBContract),
		logx.Int64("binary_head", status.Binary),
		logx.Int64("binary_required", migrate.RequiredVersion),
	)

	srv := server.NewServer(server.Options{
		Ready:  gate.status,
		WebFS:  webUI(),
		Logger: log,
	})
	httpSrv := &http.Server{
		Addr:              in.cfg.Server.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: in.cfg.Server.ReadHeaderTimeout,
		ReadTimeout:       in.cfg.Proxy.UpstreamTimeout,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    in.cfg.Server.MaxHeaderBytes,
	}

	// 与就绪路径同款监听参数（KeepAliveConfig 归内核，见 wiring_run.go 的解释）。
	lc := net.ListenConfig{
		KeepAliveConfig: net.KeepAliveConfig{
			Enable:   true,
			Idle:     30 * time.Second,
			Interval: 10 * time.Second,
			Count:    3,
		},
	}
	ln, err := lc.Listen(context.Background(), "tcp", in.cfg.Server.Addr)
	if err != nil {
		fatalf("server: listen %s: %v", in.cfg.Server.Addr, err)
	}
	go func() {
		log.Info("server listening (schema gate: not ready)", logx.String("addr", in.cfg.Server.Addr))
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatalf("server: %v", err)
		}
	}()

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), in.cfg.Server.ShutdownBudget())
	defer cancel()
	if err := httpSrv.Shutdown(shutdownCtx); err != nil {
		log.Warn("server shutdown incomplete", logx.Error(err))
	}
	_ = httpSrv.Close()
	_ = log.Sync()
}
