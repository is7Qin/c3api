// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 装配阶段 6/6：请求面（handler/router/HTTP server）+ 生命周期（worker 注册序 →
// 启动 → 监听 → 信号 → 停机链）。
//
// 为什么这一段不拆成独立阶段：`opsWorkers` 派生自 `managedWorkers`（同一份有序
// 切片同时喂 Manager 注册与 /ops 聚合，避免观测面与生命周期面漏接），而
// `wm.Register` 的**注册序**（= 反向排空序）必须与产物切片在同一个函数里才能被
// worker_order_test 一次读完；拆到两个文件会让"注册序"这条不变量失去单一主语。
//
// defer 只出现在本文件的 run() 里（停机预算 cancel）与 main 里（连接池/信号/
// 启动预算）：阶段构造函数内零 defer，return 即完成装配。
package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	jwtauth "github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/config"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler"
	paymentapi "github.com/is7qin/c3api/internal/handler/payment"
	"github.com/is7qin/c3api/internal/handler/paymentadmin"
	userapi "github.com/is7qin/c3api/internal/handler/user"
	"github.com/is7qin/c3api/internal/proxy"
	"github.com/is7qin/c3api/internal/quality"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/server"
	"github.com/is7qin/c3api/internal/snapshot"
	"github.com/is7qin/c3api/internal/supplier"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/internal/worker"
	"github.com/redis/go-redis/v9"

	"github.com/is7qin/c3api/pkg/logx"
)

// workerSlot 是注册序的一格。name 是装配期语义名（不是 worker.Name()）。
//
// skipWhen 回答「这个槽为什么可以缺席」：它拿 cfg，返回 true 才允许缺席。用谓词
// 而不是 bool——bool 只能表达「允许缺席」，表达不了「为什么」，于是「billing
// 关闭」与「有人漏写了 map 赋值」变得无法区分（后者会让 billing 游标不推进，
// 而且没有日志、没有错误）。
type workerSlot struct {
	name     string
	skipWhen func(*config.Config) bool // nil = 恒不可缺席
	expand   bool                      // 该槽是一组 worker，按切片顺序展开
}

// workerOrder 是注册序的唯一事实源：注册按正序，排空按反序（worker.Manager 的
// 契约）。改这里就是改生命周期语义，worker_order_test 与本文件的表测试会一起看。
var workerOrder = []workerSlot{
	{name: "mailW"},
	{name: "warningWorker", skipWhen: func(c *config.Config) bool { return !c.Billing.Enabled }},
	{name: "billingWorker", skipWhen: func(c *config.Config) bool { return !c.Billing.Enabled }},
	{name: "inv"},
	{name: "schedW"},
	{name: "ruleEngine"},
	{name: "retryWorker"},
	{name: "healthW"},
	// supplier 组：内部顺序（thaw → credit）与「谁出现」仍归 supplierWorkersFor，
	// 已有 supplier_assembly_test 覆盖。不拆成两行——optional 只能表达「可以缺席」，
	// 表达不了「为什么缺席」；拆开后 enabled=true 但 credit 缺失会被静默跳过。
	{name: "supplierWorkers", skipWhen: func(c *config.Config) bool { return !c.Supplier.Enabled }, expand: true},
	{name: "rec"},
	{name: "errlogW"},
	// attemptlogW 紧跟在 errlogW **之后**登记 ⇒ 停机时它先于 errlogW 停（反向排空）。
	// 理由：两张表是同一次失败的两个粒度——attempt_logs 记每一跳，err_logs 记
	// 终态 + 那一跳的 ordinal。让更粗、更重要的终态面活得比逐跳明细久一点：
	// 倒过来（先停 errlogW）会留下「有尝试明细、没终态」的请求，链视图只能猜它最终
	// 怎么了；现在的残留（有终态、少了某跳）仍能从 ordinal 的缺口看出缺哪一跳。
	// 两者都是各自 Close 排空，所以这只是停机预算真被砍断时的取值。
	{name: "attemptlogW"},
	{name: "contBindW"},
	{name: "convW"},
	{name: "quotaW"},
	{name: "pricingSync"},
	{name: "retention"},
	{name: "statsAgg"},
	{name: "qualityFlowOwner"},
	{name: "qualitySync"},
	{name: "supplierViewLoader", skipWhen: func(c *config.Config) bool { return !c.Supplier.Enabled }},
	{name: "concSync"},
	{name: "accConcSync"},
	{name: "disco"},
	{name: "listener"},
	{name: "authSync"},
}

// workerEntry 是装配期解析出来的一格：名字 + 具体 worker。
type workerEntry struct {
	name string
	w    worker.Worker
}

// workerSet 是装配期收集到的全部 worker：单例按名字，可展开组按切片。
type workerSet struct {
	byName map[string]worker.Worker
	groups map[string][]worker.Worker
}

// assembleWorkers 把 workerSet 归一成有序 plan。唯一缺席规则：
//
//	w, ok := set.byName[name]; 缺席 := !ok || w == nil
//
// 三种结果：
//   - 缺席 && skipWhen(cfg)        → 跳过（合法关闭）
//   - 缺席 && !skipWhen(cfg)       → panic（漏装配，或键名写错）
//   - 存在 && skipWhen(cfg)        → panic（cfg 说该关，却给了值）
//
// 为什么「缺失」与「nil 值」必须同义：warningWorker/billingWorker 是
// `var w worker.Worker; if cond { w = … }`——关闭态留下的是**值为 nil 的键**，
// 不是缺键。只判缺失会让 nil 溜进 plan，最终 wm.Register(nil) → StartAll 在
// nil 接口上 panic，进程起不来。
func assembleWorkers(set workerSet, cfg *config.Config) []workerEntry {
	plan := make([]workerEntry, 0, len(workerOrder))
	for _, slot := range workerOrder {
		skipOK := slot.skipWhen != nil && slot.skipWhen(cfg)

		if slot.expand {
			live := make([]worker.Worker, 0, len(set.groups[slot.name]))
			for _, w := range set.groups[slot.name] {
				if w != nil {
					live = append(live, w)
				}
			}
			switch {
			case len(live) > 0 && skipOK:
				panic("worker assembly: 组 " + slot.name + " 有值但 cfg 要求关闭")
			case len(live) > 0:
				for _, w := range live {
					plan = append(plan, workerEntry{name: slot.name, w: w})
				}
			case skipOK:
				// 合法缺席
			default:
				panic("worker assembly: 必需组 " + slot.name + " 为空（cfg 不允许缺席）")
			}
			continue
		}

		w, ok := set.byName[slot.name]
		absent := !ok || w == nil
		switch {
		case absent && skipOK:
			// 合法缺席
		case absent:
			panic("worker assembly: 必需 worker " + slot.name + " 缺席（cfg 不允许缺席；漏装配或键名写错）")
		case skipOK:
			panic("worker assembly: " + slot.name + " 有值但 cfg 要求关闭（装配与 cfg 不一致）")
		default:
			plan = append(plan, workerEntry{name: slot.name, w: w})
		}
	}
	return plan
}

// buildOps 把注册序投影成 /ops/workers 的候选集：plan 派生项在前，extra 追加在后。
//
// extra 承载「只进 ops、不进注册」的观测器（proxy.NewDispatchObserverStats）——
// 它是「观测拒收」的唯一可查痕迹，不能被 plan 派生顺手吃掉，所以显式参数化。
func buildOps(plan []workerEntry, extra []handler.StatsProvider, log *logx.Logger) []handler.StatsProvider {
	ws := make([]worker.Worker, 0, len(plan))
	for _, e := range plan {
		ws = append(ws, e.w)
	}
	return append(statsProviders(ws, log), extra...)
}

// shutdown 执行五步停机链（顺序即语义，见 run 的注释）。
//
// 抽成方法只为「顺序可测」：四个副作用点是字段，测试替换后能断言完整调用序；
// shutdownCtx 仍由 run 构造并 defer cancel——预算时钟必须从信号点起算。
func (r *appRuntime) shutdown(ctx context.Context) {
	r.shutdownHTTP(ctx)
	r.closeWS()
	r.waitInflight(ctx)
	r.tail(ctx)
	_ = r.log.Sync()
}

// run 装配阶段 6 并驱动整个生命周期（`ctx` 由 main 的 signal.NotifyContext 提供）。
// appRuntime 是「构造完成、尚未启动」的装配产物：run 不再持有二十几个局部变量，
// 启动与停机都从这里取。字段刻意只放**跨阶段使用**的东西——构造期用完即弃的
// 中间量（router、issuer、srv）留在 buildRuntime 里。
type appRuntime struct {
	cfg     *config.Config
	log     *logx.Logger
	plan    []workerEntry
	httpSrv *http.Server
	wm      *worker.Manager
	px      *proxy.Proxy
	quality *quality.Recorder
	rdb     *redis.Client
	snapReg *snapshot.Registry
	disp    *dispatcher
	drain   time.Duration

	// 停机链的四个副作用点：出厂接真实实现，测试整体替换以断言调用序。
	// 不抽接口：*http.Server / *proxy.Proxy 的方法集远大于这里要的几个。
	shutdownHTTP func(ctx context.Context)
	closeWS      func()
	waitInflight func(ctx context.Context)
	tail         func(ctx context.Context)
}

// attemptDropCounter 把 attempt_logs worker 收窄成 handler 要的「读丢弃总数」
// 的函数值（见 handler.OpsOptions.AttemptSampling）。nil ⇒ nil 函数值 = 未装配：
// 取方法值这一步是唯一的 nil 指针风险点（w.Dropped 会捕获一个 nil 接收者，
// 调用时解引用即 panic），所以在这里、且只在这里判一次空。
func attemptDropCounter(w *usage.AttemptLogWorker) func() int64 {
	if w == nil {
		return nil
	}
	return w.Dropped
}

// buildRuntime 装配阶段 6 的构造部分（handler/router/HTTP server/worker plan）。
//
// 注意三个闭包必须在**调用时**读字段，不能改成构造期快照：
//   - schedSrc.Prices 读 svc 的定价快照（每次编译都要最新价）；
//   - Snapshots 读 snapReg 状态（注册表随 ReloadAll 变化）；
//   - BillingAlerts 读 billFlusher.Stats()（lag 必须实时，否则运维看到的是启动值）。
func buildRuntime(ctx context.Context, in *infraFace, hl *healthFace, nf *notifyFace, bl *billingFace, cx *codexFace) *appRuntime {
	// 解包前阶段产物为与旧 main 同名的局部（只读；语句级零改动）。写回一律走
	// 阶段 struct，别名不回写——避免出现"注册时抄一份"的副本。
	cfg, log := in.cfg, in.log
	repos, rdb, disco := in.repos, in.rdb, in.disco
	runtimeHealth, sched, ruleEngine := hl.runtimeHealth, hl.sched, hl.ruleEngine
	rec, errlogW, retention, statsAgg := hl.rec, hl.errlogW, hl.retention, hl.statsAgg
	attemptlogW := hl.attemptlogW
	mailW := nf.mailW
	auth, svc, inv := bl.auth, bl.svc, bl.inv
	snapReg, disp, listener, authSync := bl.snapReg, bl.disp, bl.listener, bl.authSync
	supplierViewLoader, suppRepo := bl.supplierViewLoader, bl.suppRepo
	effectiveInflight, qualityRecorder := cx.effectiveInflight, cx.qualityRecorder
	px, codexAdapter, bpsAdapter, probe := cx.px, cx.codex, cx.bps, cx.probe
	retryWorker, quotaW := cx.retryWorker, cx.quotaW
	contBindW, convW, pricingSync := cx.contBindW, cx.convW, cx.pricingSync
	qualitySync := cx.qualitySync
	concSync, accConcSync := cx.concSync, cx.accConcSync

	// quality-flow-owner（async-routing-quality-telemetry）：跨请求 flow 累计器
	// 的唯一 state owner。请求结算只做一次不可变非阻塞 Submit，同分钟身份归并
	// 全部离开请求 goroutine（owner 循环 / sync 消费侧）。注册必须先于
	// qualitySync：反向排空先关 quality-sync（其失败 refill 需要 owner 仍开），
	// 后关 owner（停提交 + 排空队列 + residual 可见）。owner 溢出只是遥测丢失，
	// 不触碰 HTTP/failover/健康/配额/usage/计费。
	qualityFlowOwner := qualityRecorder.FlowOwner()
	// 路由编译源装配（双 setter 已删，编译双源 Start 期结构注入）：
	// quality 源 = M 缓存窗口 provider（settled PG 分钟 + recorder 未落库活体
	// 行合并，基线仅 PG；见 routing_sources.go），价格源 = svc 定价快照基底
	// 解析（缺价模型缺席 → compiler costKnown=false 落 Explore）。两源一次性
	// 给齐 both-or-nothing，经 schedWorker 在 Start 期交接（healthWorker
	// 先例）；编译失败保留旧视图是编译道契约（routing_compiler_wire.go），
	// 失败/成功新鲜度经 scheduler Stats 上运维面。
	schedSrc := &scheduler.CompilerSources{
		Quality: NewWindowedQualityProvider(qualityRecorder, repos.Partitions),
		Prices: func() map[string]domain.ResolvedPrices {
			return svc.ResolvedPricesByModel(time.Now())
		},
	}
	aiRouter := proxy.AIRouter(px)
	iss := jwtauth.NewIssuer(cfg.Auth.JWTSecret)
	userHandler := userapi.Router(svc, iss, auth, ruleEngine, log, userapi.Features{Supplier: cfg.Supplier.Enabled})
	// 支付公开回调面（/api/payment/*，spec §7-2）：**无身份鉴权**（网关不带 JWT），
	// 由 server.go 挂在非 admin 鉴权组；协议真伪由 provider 验签判定，入账只经
	// svc.CompleteOrder。同一份 providers 装配同时给 service（注册表）与本包
	// （URL 段路由表）——两处必须同源，否则会出现"能建单但回调 404"。
	// 协议注册表与配置缓存都在 service 内：handler 只拿 svc（两份装配一定会分叉）。
	paymentHandler := paymentapi.Router(svc, log)
	// 管理面账户/渠道（spec §10-3）：路由带 /api/admin 前缀，必须挂在 adminAuth
	// 组内——账户行里有商户私钥，绝不能落到无鉴权路径。
	paymentAdminHandler := paymentadmin.New(svc, log).Router()

	// optional worker 先转为 real nil interface，禁止 typed-nil 穿透条件装配。
	var warningWorker worker.Worker
	if bl.warningW != nil {
		warningWorker = bl.warningW
	}
	var billingWorker worker.Worker
	if bl.billFlusher != nil {
		billingWorker = bl.billFlusher
	}
	// health 启动适配：真 probe 在 Start 期一次性交接（位置与 Name 不变——
	// 注册序=反序排空语义与 worker_order_test 的 ident 断言依赖）。
	healthW := healthWorker{h: runtimeHealth, probe: probe}
	// 编译源启动适配（双 setter 已删）：同一位置同一顺序注册 schedW
	// （Name="scheduler" 不变）——反序排空语义与 worker_order_test 断言依赖。
	schedW := schedWorker{s: sched, src: schedSrc}

	// 供应商记账/解冻 worker 装配（spec 2026-10-09 §5.5）：仅在 enabled=true 构造
	// （关闭态 supplierWorkers 为 nil，append 不贡献元素）。财务视图快照/装载器
	// 已在 billing 阶段（svc 之前）构造并经 Deps 注入写面 Reload；此处接线 px/sched + 首刷。
	var supplierWorkers []worker.Worker
	if cfg.Supplier.Enabled {
		// 前向槽的读点（构造期）：billing 阶段已写入；写入早于 wm.StartAll，
		// 且 retention 的容量处置回调在运行期读的是同一个字段。
		px.SetSupplierSnapshot(in.supplierSnap)
		sched.SetSupplierAdmission(in.supplierSnap)
		initCtx, initCancel := context.WithTimeout(context.Background(), 15*time.Second)
		// 财务视图首刷在构造期同步执行（尽量落在监听端口之前；失败不阻塞启动，
		// 进入 NotReady ⇒ 带归属账号不入调度，ticker 兜底重试）。
		if !supplierViewLoader.LoadOnce(initCtx) {
			log.Warn("supplier view initial load failed; accounts with supplier ownership are not scheduled until a reload succeeds (NotReady)")
		}
		initCancel()
		creditW := supplier.NewCredit(supplier.CreditConfig{
			Interval:      200 * time.Millisecond,
			BatchLimit:    500,
			DrainBudget:   supplierCreditDrainBudget,
			FreezeEnabled: cfg.Supplier.FreezeEnabled,
			LagFullEvery:  20,
		}, suppRepo, suppRepo, log)
		var thawW worker.Worker
		if cfg.Supplier.FreezeEnabled {
			thawW = supplier.NewThaw(supplier.ThawConfig{
				Interval:    time.Second,
				BatchLimit:  1000,
				DrainBudget: 3 * time.Second,
			}, suppRepo, log)
		}
		supplierWorkers = supplierWorkersFor(cfg.Supplier.Enabled, cfg.Supplier.FreezeEnabled, thawW, creditW)
	}

	// 注册序 = workerOrder 表（唯一的顺序事实源）。缺席规则与 fail-fast 全在
	// assembleWorkers 里：缺键与 nil 值同义，cfg 说不该缺席就必须有值。
	byName := map[string]worker.Worker{
		"mailW":            mailW,
		"inv":              inv,
		"schedW":           schedW,
		"ruleEngine":       ruleEngine,
		"retryWorker":      retryWorker,
		"healthW":          healthW,
		"rec":              rec,
		"errlogW":          errlogW,
		"attemptlogW":      attemptlogW,
		"contBindW":        contBindW,
		"convW":            convW,
		"quotaW":           quotaW,
		"pricingSync":      pricingSync,
		"retention":        retention,
		"statsAgg":         statsAgg,
		"qualityFlowOwner": qualityFlowOwner,
		"qualitySync":      qualitySync,
		"concSync":         concSync,
		"accConcSync":      accConcSync,
		"disco":            disco,
		"listener":         listener,
		"authSync":         authSync,
	}
	// 可选 worker 用**不塞键**表达缺席。绝不能写成「nil 也塞进 map」：
	// `var p *supplier.ViewLoader = nil` 装进 map[string]worker.Worker 是非 nil
	// 接口（动态类型就绪、值才是 nil），assembleWorkers 的缺席判据 `!ok || w == nil`
	// 读不到它，于是「关闭态」被判成「cfg 说该关却给了值」并 panic——默认配置
	// （supplier.enabled=false，也是 deploy/config.toml 的取值）曾在启动时就这么崩。
	if warningWorker != nil {
		byName["warningWorker"] = warningWorker
	}
	if billingWorker != nil {
		byName["billingWorker"] = billingWorker
	}
	if supplierViewLoader != nil {
		byName["supplierViewLoader"] = supplierViewLoader
	}
	plan := assembleWorkers(workerSet{
		byName: byName,
		groups: map[string][]worker.Worker{"supplierWorkers": supplierWorkers},
	}, cfg)

	// /api/admin/ops/workers 运维观测（spec 2026-08-11，并入管理面）：
	// 独立 Stats 契约不改 worker.Worker——装配侧类型断言聚合（各模块已持
	// 具体引用，断言实现 handler.StatsProvider 的入列；快照注册表状态单独
	// 经 Status 直出）。WithOps 注入，路由由契约 chi-server 生成。
	// dispatch-observer 只进 ops、不进注册：它是「观测拒收」的唯一可查痕迹，
	// 必须显式作为 extra 传进去，否则 plan 派生会顺手把它吃掉。
	opsWorkers := buildOps(plan, []handler.StatsProvider{proxy.NewDispatchObserverStats(px)}, log)
	// /api/admin/overview + /api/admin/users-top（spec 2026-08-14）：门禁在途快照
	// （Auth.InFlightUsers 只读访问器——零锁冷面）与 billing 游标积压 lag 族观测
	// （flusher 直读；未装配 nil → 端点空/零值）经 OpsOptions 注入——不改
	// service.New 签名（main.go:376-390 注入先例）。
	h := handler.New(svc, handler.OpsOptions{
		Workers:       opsWorkers,
		UsageSnap:     codexAdapter, // codex 额度快照：handler fan-out 经构造直调（TTL 缓存/有界并发/失败冷却全在适配层）
		PricingSync:   pricingSync,  // 价格手动同步/预览：sync 端点经构造直调 worker（零 service 中介）
		Log:           log,          // fan-out 未知上游错误 Warn
		Snapshots:     func() []handler.SnapshotState { return snapshotStates(snapReg.Status()) },
		InFlightUsers: auth.InFlightUsers,
		// /ops/attempts/{request_id} 的 sampling 段：只注入「读丢弃总数」这一件事
		// （函数值），不把整个 worker 交给 handler。worker 恒非 nil（健康阶段构造
		// 失败即 fatalf），仍显式判空以保留「未装配 ⇒ 省略字段」的语义。
		AttemptSampling: attemptDropCounter(attemptlogW),
		BillingAlerts: func() handler.BillingAlerts {
			// 调用时读字段（不是注册时快照）：billing 关闭时字段恒 nil = 端点空值，
			// 开启时字段在 billing 阶段已就位。
			if bl.billFlusher == nil {
				return handler.BillingAlerts{}
			}
			s := bl.billFlusher.Stats().(billing.FlusherStats)
			return handler.BillingAlerts{
				LagMs:           s.LagMs,
				UnbilledRows:    s.UnbilledRows,
				QuarantinedRows: s.QuarantinedRows,
			}
		},
	})

	// 供应商面（/api/user/supplier/*）：仅在 enabled=true 时挂载（§6.4 关闭态短路
	// 清单第 4 项）。门控 RequireJWT + 快照基 RequireRole(supplier|platform_admin)
	// + 作用域注入均在 NewSupplierSurface 内组装；**暴露面由 openapi 里 tag
	// `supplier` 的 path 决定**（codegen 生成，未登记者未注册 ⇒ 404），账号/分组/
	// 模板 op 转发到管理面同一批 handler（字段层零差异化），作用域由 repository
	// 每处 WHERE AND 归属谓词（§2.5）。
	var supplierHandler http.Handler
	if cfg.Supplier.Enabled {
		supplierHandler = handler.NewSupplierSurface(h, iss, auth)
	}

	srv := server.NewServer(server.Options{
		JWTIssuer:           iss,
		Auth:                auth,
		MaxInflight:         effectiveInflight,
		ReadHeaderTimeout:   cfg.Server.ReadHeaderTimeout,
		MaxHeaderBytes:      cfg.Server.MaxHeaderBytes,
		AdminHandler:        h.Router(),
		UserHandler:         userHandler,
		SupplierHandler:     supplierHandler,
		PaymentHandler:      paymentHandler,
		PaymentAdminHandler: paymentAdminHandler,
		AIHandler:           planReadyGate(sched, aiRouter),
		WebFS:               webUI(),
		Logger:              log,
		// /readyz 与业务面 503 的读点（schema 启动门）。启动期已判定为可服务才
		// 走到这里，所以运行期它恒为 200——那份状态仍要接进来：探针读到的是
		// **本进程真实的判定依据**，不是一句"能跑到这里就算好了"。
		Ready: in.schema.status,
	})

	// http.Server 超时：IdleTimeout 防 keep-alive 空闲连接
	// 与 goroutine 无限驻留（有效 key 吃满 50000 并发面数小时即修复面）；ReadTimeout
	// = proxy.upstream_timeout 同源单一事实源（120s）——只限请求头+体读取时长，不
	// 限制响应写出（net/http 语义），SSE 长流不受影响。slowloris 场景：1KB/s ×
	// 4MB ≈ 4096s → 120s 截断。
	// 不设 WriteTimeout：会切断 SSE 长流（03-streaming.md 依赖节）；写侧
	// 防线是 C 方向的 SetWriteDeadline。
	httpSrv := &http.Server{
		Addr:              cfg.Server.Addr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		ReadTimeout:       cfg.Proxy.UpstreamTimeout,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    cfg.Server.MaxHeaderBytes,
	}

	r := &appRuntime{
		cfg:     cfg,
		log:     log,
		plan:    plan,
		httpSrv: httpSrv,
		px:      px,
		quality: qualityRecorder,
		rdb:     rdb,
		snapReg: snapReg,
		disp:    disp,
		drain:   cfg.Server.ShutdownDrain,
	}
	// 停机链的副作用点接真实实现；测试替换这四个字段即可断言调用序。
	r.shutdownHTTP = func(ctx context.Context) { shutdownHTTPServer(ctx, r.httpSrv, r.drain, r.log) }
	r.closeWS = r.px.CloseAllWS
	r.waitInflight = func(ctx context.Context) { waitForInflight(r.px, ctx, r.log) }
	r.tail = func(ctx context.Context) {
		if bpsAdapter != nil {
			if err := bpsAdapter.Close(); err != nil {
				r.log.Warn("shutdown: BPS client cache close failed", logx.Error(err))
			}
		}
		shutdownTail(ctx, r.wm, r.quality, r.rdb, r.log)
	}
	return r
}

// start 执行启动段：快照首刷 → worker 注册与启动 → 监听端口 → Serve goroutine。
//
// worker 注册必须在这里而不是 buildRuntime：注册序与「启动」是同一个生命周期
// 决策，且 ReloadAll 必须早于 StartAll（单一启动入口，见下）。
func (r *appRuntime) start(ctx context.Context) {
	log := r.log
	// 统一启动就绪（快照注册表）：构造链完成后全量首刷（并行；各快照错误独立
	// Warn 不阻塞启动——DB 故障时快照保持空/旧值，模块周期 ticker / listener
	// FullRefresh 兜底收敛）。取代此前分散的 ruleEngine.Reload fatalf +
	// sched.InvalidateAllSync fatalf + auth/billBalances 构造时加载：scheduler
	// 首刷在此完成（Select 在 nil 快照上 panic 的窗口随单一入口消灭），规则
	// 空表种子亦在此写入（失败降级 Warn——注册表 Status 可观测）。
	errs := r.snapReg.ReloadAll(ctx)
	for name, err := range errs {
		logSnapshotReloadErr(log, "snapshot initial reload failed", name, err)
	}
	// 启动双刷：ReloadAll 返回空 map = 全部成功（snapshot.go 契约
	// ——成功者不出现）→ 置位首连跳过标志（dispatcher.bootLoaded，wm.StartAll
	// 之前——程序序保证监听器首连必见标志）：首连的 FullRefresh CAS 消费后跳过
	// 五路 ReloadAll（单实例健康启动下第二遍纯冗余，大表启动 DB 负载/就绪延迟
	// 约翻倍）、仅补 ReloadSettings；多实例 pre-LISTEN 漏窗 ≤30s sched 同步 /
	// 60s auth-sync 兜底收敛，可接受。部分失败不置位 → 首连仍全量（兜底收敛
	// 不破坏）。
	if len(errs) == 0 {
		r.disp.bootLoaded.Store(true)
	}
	// 统一 worker 管理：顺序启动、反向排空。注册序由 workerOrder 表决定
	// （email → notification(条件) → billing(条件) → 业务 worker → 协调/发现/
	// 监听），保证停机时 usage recorder 先落完整账本，billing 再终扫游标，
	// 随后 notification 排空已提交告警，最后 email 排空独立的注册/重置队列。
	//
	// 单点注册：循环必须**正序**消费 plan。反向遍历会让排空也反（Manager 按
	// 注册逆序关），rec 在 billing 之前关 ⇒ 明细静默丢——这条没有任何运行期
	// 门能看见，只有 TestAssembleWorkersOrder 与这行注释守。
	r.wm = worker.New(log)
	for _, e := range r.plan {
		r.wm.Register(e.w)
	}
	if err := r.wm.StartAll(ctx); err != nil {
		fatalf("worker start: %v", err)
	}
	// 调度器初始加载已由上方注册表 ReloadAll 完成（先于 StartAll 与流量）——
	// 此处不再单独 InvalidateAllSync（单一启动入口）。

	// 对端存活归传输层（第一性原理：内核知道对端没在 ACK，应用层不重复发明）：
	// listener 级 KeepAliveConfig 被所有已接受连接继承——含 WS hijacked 连接
	// （net/http Server 不覆盖该设置）。对端死亡 → 内核断言连接失效 → 读侧报错
	// → 既有 abort 分类链收尾落账。空闲/活跃会话同覆盖，零应用层成本。
	// 不检测"内核活但应用冻结"（CLI 客户端罕见，接受）。
	lc := net.ListenConfig{
		KeepAliveConfig: net.KeepAliveConfig{
			Enable:   true,
			Idle:     30 * time.Second,
			Interval: 10 * time.Second,
			Count:    3,
		},
	}
	ln, err := lc.Listen(context.Background(), "tcp", r.cfg.Server.Addr)
	if err != nil {
		fatalf("server: listen %s: %v", r.cfg.Server.Addr, err)
	}
	r.wm.Go(ctx, "http-server", func(_ context.Context) {
		log.Info("server listening", logx.String("addr", r.cfg.Server.Addr))
		if err := r.httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatalf("server: %v", err)
		}
	})
}

// run 装配阶段 6 并驱动整个生命周期（`ctx` 由 main 的 signal.NotifyContext 提供）。
//
// 停机链**内联在本函数**（不抽方法）：它与 shutdownCtx 的构造时机、defer cancel
// 的层级是一件事，而预算时钟必须从收到信号那一刻起算——提前构造会让 ctx 在
// 运行几分钟后早已超时（在途流强断、billing 无终扫）。
func run(ctx context.Context, in *infraFace, hl *healthFace, nf *notifyFace, bl *billingFace, cx *codexFace) {
	r := buildRuntime(ctx, in, hl, nf, bl, cx)
	r.start(ctx)

	// 1) 等停机信号（SIGINT/SIGTERM）：之前的全部语句都是装配，不接流量。
	<-ctx.Done()
	// 停机总预算 = 配置的 HTTP 优雅窗口 + 固定尾部（对账/排空/终态日志）。窗口之所以
	// 可配：流式请求的收尾时长随部署规模变化（默认 2s 是为公网短请求调的），而尾部
	// 预算是"排空能不能做完"的下限，不是策略，故不给配。
	shutdownCtx, cancel := context.WithTimeout(context.Background(), r.cfg.Server.ShutdownBudget())
	// 为什么 defer 在这一层：budget 的 cancel 必须晚于下面 5 步（每步都读这个 ctx），
	// 且必须早于 main 的 pool.Close()/stop()——所以它与停机链同层，不下移到阶段函数。
	defer cancel()
	// 优雅停机链（计费不丢窗口）：
	// 1) Shutdown(server.shutdown_drain) 优雅窗口：在途请求收尾（长连接流式留 Close 强断）
	// 2) Close 强制断长连接 → 客户端断开 → recordStreamAbort → finish（断前
	//    usage 帧照常计费）
	// 3) waitForInflight：等在途归零（100ms 轮询；超时 Warn 继续不阻塞退出）
	// 4) wm.Shutdown 反向排空：listener/authSync 最先停止接收 → disco ZREM 缩容
	//    → accConcSync/concSync → statsAgg/retention/pricingSync/attemptlogW/errlogW
	//    → rec 排空明细 → rule/sched/inv → billing 终扫完整账本
	//    → notification 排空余额告警 → email 排空 auth 邮件
	//    （quality-sync 的 Close 在此链内排空 Redis/PG；PG 失败时 refill 回
	//    recorder——recorder 必须仍开着，见 shutdown.go 的排空/refill 注释）
	// 5) 停机尾部（worker 排空 → recorder 终态 → Redis 释放 → 终态日志）收敛
	//    在 shutdownTail：排空失败不再 `_ =` 静默——Error 级 "shutdown
	//    incomplete" 显式上报且不宣称 clean shutdown（review blocker
	//    2026-08-30）。
	r.shutdown(shutdownCtx)
}

// waitForInflight 等在途请求归零（优雅停机第 3 步）：100ms 轮询 px.Inflight()；
// 超出剩余预算 → Warn 继续（不阻塞退出——极限情况丢 ≤1 flush 窗口，可接受，
// 见计划风险节）。
func waitForInflight(px *proxy.Proxy, ctx context.Context, log *logx.Logger) {
	for px.Inflight() > 0 {
		select {
		case <-ctx.Done():
			log.Warn("shutdown: inflight requests not drained, continuing")
			return
		case <-time.After(100 * time.Millisecond):
		}
	}
}
