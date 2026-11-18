// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/config"
	"github.com/is7qin/c3api/internal/db/migrate"
	"github.com/is7qin/c3api/pkg/logx"
)

// 启动分叉的行为证据（spec §2.5/§2.6）：不可服务 = NotReady，不是 CrashLoop。
//
// 这一条必须能拦住「把 serveNotReady 换成 fatalf」这类变异：
//   - 探针面真的起来了（/healthz 200、/readyz 503）——换成 fatal 就没有监听；
//   - 进程**阻塞到 ctx 取消**——换成 fatal 会立刻返回（或直接 os.Exit）；
//   - 致命出口（fatalf）零调用——stub 掉后可以直接断言。
//
// 三条各自都独立成立：少任何一条，变异都能从断言缝里溜过去。

func TestDispatchServeNotReadyServesProbesInsteadOfExiting(t *testing.T) {
	gate := newSchemaGate(migrate.Verdict{DBHead: 0, BinaryHead: migrate.RequiredVersion})
	require.False(t, gate.ready(), "空水位必须是不可服务态")

	addr := freeLoopbackAddr(t)
	in := &infraFace{
		cfg: &config.Config{
			Server: config.ServerConfig{Addr: addr, ReadHeaderTimeout: time.Second, ShutdownDrain: 10 * time.Millisecond},
			Proxy:  config.ProxyConfig{UpstreamTimeout: time.Second},
		},
		log:    mustTestLogger(t),
		schema: gate,
	}

	// 计数器是 atomic：两个写入方都在 dispatchServe 的 goroutine 里，读在主 goroutine。
	var fatalCalls, serveCalls atomic.Int32
	restoreFatal := stubFatalf(func(string, ...any) { fatalCalls.Add(1) })
	t.Cleanup(restoreFatal)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		dispatchServe(ctx, in, func() { serveCalls.Add(1) })
	}()

	base := "http://" + addr
	waitProbeUp(t, base+"/healthz", &fatalCalls)
	readyStatus, readyBody := getJSON(t, base+"/readyz")
	require.Equal(t, http.StatusServiceUnavailable, readyStatus)
	require.Equal(t, migrate.ModeReadonly, schemaObject(t, readyBody)["mode"])

	// 还在跑：不可服务不等于「跑完就退出」。
	select {
	case <-done:
		require.FailNow(t, "dispatchServe returned while the process must stay alive", "不可服务态的进程要阻塞到停机信号")
	default:
	}
	require.Zero(t, serveCalls.Load(), "不可服务时不得构建业务图（回调不许被调用）")
	require.Zero(t, fatalCalls.Load(), "不可服务 ≠ 致命：fatalf 一次都不许被调用")

	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "dispatchServe did not return after ctx cancel", "停机信号必须能让探针面收尾")
	}
}

// TestDispatchServeReadyRunsServe 是分叉的另一半：可服务时不走探针面，直接把控制权
// 交给 serve（否则「业务图被谁装配」会变成两处都可能）。
func TestDispatchServeReadyRunsServe(t *testing.T) {
	gate := newSchemaGate(migrate.Verdict{DBHead: migrate.RequiredVersion, BinaryHead: migrate.RequiredVersion})
	require.True(t, gate.ready())

	addr := freeLoopbackAddr(t)
	in := &infraFace{
		cfg:    &config.Config{Server: config.ServerConfig{Addr: addr, ShutdownDrain: 10 * time.Millisecond}},
		log:    mustTestLogger(t),
		schema: gate,
	}

	var fatalCalls atomic.Int32
	restoreFatal := stubFatalf(func(string, ...any) { fatalCalls.Add(1) })
	t.Cleanup(restoreFatal)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	serveCalls := 0
	dispatchServe(ctx, in, func() { serveCalls++ })

	require.Equal(t, 1, serveCalls, "可服务时必须走完整装配路径")
	require.Zero(t, fatalCalls.Load())
}

// waitProbeUp 等探针面起来——但**先**盯致命出口：变异（不可服务走 fatalf）的第一
// 现场是「还没监听就已经 fatal」，如果只轮询 HTTP，断言要等满 10s 才红，而且只能
// 说「连不上」，说不出「它退出了」。
//
// 轮询用 ticker 而不是 sleep：仓库禁用 sleep 做同步。
func waitProbeUp(t *testing.T, url string, fatalCalls *atomic.Int32) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if status, _, err := tryGetJSON(url); err == nil && status == http.StatusOK {
			return
		}
		if n := fatalCalls.Load(); n > 0 {
			require.FailNow(t, "unserviceable schema became a fatal exit",
				"dispatchServe 在 schema 不可服务时调用了 fatalf（%d 次）：在 k8s 这就是 CrashLoopBackOff，而契约是起探针面等人修", n)
		}
		if time.Now().After(deadline) {
			require.FailNow(t, "probe endpoint never came up",
				"%s: 10s 内没有监听（不可服务态也必须能回答 /healthz）", url)
		}
		<-tick.C
	}
}

// stubFatalf 把进程级致命出口换成记录器（不退出），返回还原函数。
//
// 为什么需要：`不可服务 ≠ 致命` 的可观察证据是「它没被调用」；真 fatalf 会
// os.Exit，断言只能退化成「整个测试进程非零退出」——看不出是哪一条契约破了。
func stubFatalf(record func(format string, args ...any)) func() {
	prev := fatalf
	fatalf = func(format string, args ...any) { record(format, args...) }
	return func() { fatalf = prev }
}

// mustTestLogger 给探针面一个安静 logger（error 级：只留真错误）。
func mustTestLogger(t *testing.T) *logx.Logger {
	t.Helper()
	log, err := logx.New("error", "")
	require.NoError(t, err)
	return log
}
