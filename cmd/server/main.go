// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// github.com/is7qin/c3api 入口：配置 → 日志/观测汇 → 分阶段装配 → 运行期生命周期。
//
// 装配分 6 个阶段（wiring_*.go，每文件一个构造函数，只允许"后阶段依赖前阶段"）：
//
//	1 newInfra      Redis/PG/仓库/NOTIFY 发布器/实例发现/上游底座/出口池/settings 快照
//	2 newHealthFace 健康投影/锁存/Hub/规则引擎/调度器/用量捕获 worker
//	3 newNotifyFace 余额预警冷却 + 邮件 worker
//	4 newBillingFace 鉴权/失效去抖/供应商面/支付注册表/service/快照注册表/NOTIFY 监听/计费
//	5 newCodexFace  codex 适配层/补拉与重试 worker/请求路径(px)/协调同步/价格同步
//	6 run           请求面(handler/router/HTTP) + worker 注册序 + 启动 + 信号 + 停机链
//
// 唯一允许的"前向"引用是 infraFace.supplierSnap（billing 阶段写入、retention 回调
// 运行期读）：其余符号一律不许晚绑定，也不许引入服务定位器/聚合根。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/is7qin/c3api/internal/config"
	"github.com/is7qin/c3api/internal/protoconv"
	"github.com/is7qin/c3api/pkg/logx"
)

// version 是二进制版本注入点（REL spec 2026-08-15）：构建链 -ldflags
// "-X main.version=..." 注入 tag 值（如 v0.0.1-beta.1）；本地 dev 构建不注入 =
// "dev"。查询方式：c3api -version——不带入 /healthz（无鉴权端点保持最小面）。
var version = "dev"

func main() {
	cfgPath := flag.String("config", "config.toml", "path to TOML config")
	pprofAddr := flag.String("pprof", "", "listen addr for /debug/pprof (heap/goroutine profile under load)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	// -version 纯打印退出：不加载配置/不连 DB，任何环境（含容器外裸二进制）可查。
	if *showVersion {
		fmt.Printf("c3api %s\n", version)
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		// 附 -config 路径与 CWD：相对路径文件缺失/校验失败可归因；
		// env-only 部署（-config ""）报错时此处即线索。
		wd, _ := os.Getwd()
		fatalf("config: %v (path: %s, cwd: %s)", err, *cfgPath, wd)
	}
	log, err := logx.New(cfg.Log.Level, cfg.Log.Output)
	if err != nil {
		fatalf("logger: %v", err)
	}
	// 工具 schema 方言的退化必须可见（spec §3.5 / I3 护栏④）：任一退化或兜底
	// 都记 WARN，带 direction/tool/reason/schema_bytes/root_keys；计数由
	// protoconv 自己累计（protoconv.SchemaRepaired，供健康检查读取）。
	protoconv.SetSchemaSink(func(e protoconv.SchemaEvent) {
		// 触发条件：退化（simplify/bail）、源侧没有 schema（missing）、或"有值但不是
		// 对象/字符串形态"（no-schema 但没有 missing）——三者都要可见。
		if e.Degenerate == "none" && !e.Missing && !hasReason(e.Reasons, "no-schema") {
			return
		}
		log.Warn("tool schema degraded for the Messages target",
			logx.String("direction", e.Direction),
			logx.String("tool", e.Tool),
			logx.String("reasons", strings.Join(e.Reasons, ",")),
			logx.String("degenerate", e.Degenerate),
			logx.Int("schema_bytes", e.SchemaBytes),
			logx.Int("root_keys", e.RootKeys),
			logx.Bool("schema_missing", e.Missing),
		)
	})
	// 丢弃观测（spec §7 I3）：转换把"目标协议无载体"的字段丢掉时必须可见。
	// 以前 SetDropSink 从未被调用——计数在涨、日志一条没有，线上只能靠翻代码猜。
	//
	// 按事件（方向 + 协议面 + 字段）去重、每个三元组只 WARN 一次：noteReqDrop /
	// noteRespDrop 在请求/响应热路径上逐次调用（孤儿 tool_result、document、
	// mcp_servers… 每单都可能命中），不去重会把日志刷爆并淹没真正的异常。
	//
	// 去重表是单层 sync.Map + **直接拿 DropEvent 当键**（值类型、可比较）：不拼
	// 字符串、每次调用只做一次 LoadOrStore（内部按 key 分片），不会让所有丢弃
	// 事件抢同一把全局锁。
	var dropSeen sync.Map
	protoconv.SetDropSink(func(e protoconv.DropEvent) {
		if _, dup := dropSeen.LoadOrStore(e, struct{}{}); dup {
			return
		}
		log.Warn("conversion dropped fields the target protocol cannot carry",
			logx.String("direction", string(e.Dir)),
			logx.String("face", e.Face.String()),
			logx.String("field", e.Field),
		)
	})
	// pprof 监听失败可观测（spec 2026-08-13）：旧实现 `_ =` 全静默——监听
	// 失败零日志零观测。goroutine 在 logx.New 之后启动：闭包捕获 log 恒非 nil
	// （若保留原位置，端口占用等启动期失败时 log 尚 nil，Warn 判空即被丢弃，
	// 观测仍缺失）；失败 Warn 不 fatal（pprof 非关键面，服务照常启动）。
	if *pprofAddr != "" {
		go func() {
			if err := http.ListenAndServe(*pprofAddr, nil); err != nil {
				log.Warn("pprof server failed", logx.Error(err))
			}
		}() // net/http/pprof 自动挂载
	}

	// 阶段装配（只允许后阶段依赖前阶段）。顺序是依赖图而不是风格：billing 的
	// svc/warningW 需要 notify 的 cooldown/mailW，codex 的 px 需要 billing 的
	// auth/clients/billHooks，run 需要全部。
	in := newInfra(cfg, log)
	// defer 留在这一层（不下移到阶段构造函数）：阶段返回后各阶段仍持有这些句柄，
	// 只有 main 的生命周期能保证"进程退出前最后一步才释放"。
	defer in.cancelStartup()
	defer in.pool.Close()

	hl := newHealthFace(cfg, log, in)
	nf := newNotifyFace(log, in)
	bl := newBillingFace(cfg, log, in, hl, nf)
	cx := newCodexFace(cfg, log, in, hl, bl)

	// 停机信号与本层同生命周期：stop 必须晚于 run 的停机链（run 里读 ctx）。
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	run(ctx, in, hl, nf, bl, cx)
}

// fatalDB 启动期 DB 操作失败 fatal：30s 预算超时 → 明确可归因文案。
func fatalDB(step string, err error) {
	if errors.Is(err, context.DeadlineExceeded) {
		fatalf("db bootstrap timed out after 30s (%s): %v", step, err)
	}
	fatalf("%s: %v", step, err)
}

func fatalf(format string, args ...any) {
	_, _ = os.Stderr.WriteString("fatal: " + fmt.Sprintf(format, args...) + "\n")
	os.Exit(1)
}

// hasReason 判定事件里是否含某个 reason。
func hasReason(reasons []string, want string) bool {
	for _, r := range reasons {
		if r == want {
			return true
		}
	}
	return false
}
