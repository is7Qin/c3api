// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// github.com/is7qin/c3api 入口：配置 → DB/ent → 各模块装配 → 优雅退出。
package main

import (
	"context"
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"syscall"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/stdlib"

	jwtauth "github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/config"
	"github.com/is7qin/c3api/internal/continuation"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/discovery"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler"
	userapi "github.com/is7qin/c3api/internal/handler/user"
	"github.com/is7qin/c3api/internal/invalidate"
	"github.com/is7qin/c3api/internal/latch"
	"github.com/is7qin/c3api/internal/notification"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/pricing"
	"github.com/is7qin/c3api/internal/proxy"
	"github.com/is7qin/c3api/internal/quality"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/internal/server"
	"github.com/is7qin/c3api/internal/service"
	"github.com/is7qin/c3api/internal/settingssnap"
	"github.com/is7qin/c3api/internal/snapshot"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/internal/verification"
	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/aiclient"
	"github.com/is7qin/c3api/pkg/httpx"
	"github.com/is7qin/c3api/pkg/logx"
	"github.com/is7qin/c3api/pkg/redisx"
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
	// 必填校验（admin.token/auth.jwt_secret/db.dsn/redis.addr）已内聚到 config.Load，
	// 此处只做错误处理。
	// server.time_zone 仅服务定价/规则时间条件（pricing 保持既有 nil/进程本地
	// 回落语义）——统计读取时区是请求级参数（浏览器 IANA 名，handler 边界解析），
	// 绝不从此配置派生任何进程级统计时区（request-browser-timezone-stats）。
	var svcLoc *time.Location
	if cfg.Server.TimeZone != "" {
		l, err := time.LoadLocation(cfg.Server.TimeZone)
		if err != nil {
			fatalf("server.time_zone: invalid IANA timezone %q: %v", cfg.Server.TimeZone, err)
		}
		svcLoc = l
	}

	// Redis 必选依赖（foundation spec 2026-08-25-redis-foundation-design §2.3）：
	// config.Load 之后立即构造（addr 缺失已在 Load fatal；Ping 失败此处 fatal——
	// 无"未启用"分支，消费方零 nil 容忍）。全仓唯一构造点 redisx.Open；运行期
	// 连接丢失 ≠ 启动失败（连接池自带重连，降级语义由 discovery 定义）。
	rdb, err := redisx.Open(redisx.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: cfg.Redis.DB})
	if err != nil {
		fatalf("redis: %v", err)
	}

	// 启动期 DB 操作统一 30s 预算（OpenPG + ent migrate + 三分区 bootstrap）：
	// 超时/失败经 fatalDB 明确文案（"db bootstrap timed out after 30s" 可归因）。
	startupCtx, cancelStartup := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelStartup()

	pool, err := repository.OpenPG(startupCtx, cfg.DB.DSN, int32(cfg.DB.MaxConns))
	if err != nil {
		fatalDB("db", err)
	}
	defer pool.Close()
	// ent v0.14.6 的 entsql.OpenDB 只接受 *sql.DB：pgxpool 经 pgx/stdlib 桥接（用户决策 2026-08-05）
	db := stdlib.OpenDBFromPool(pool)
	drv := entsql.OpenDB(dialect.Postgres, db)
	repos, err := repository.NewWithPG(startupCtx, drv, true, pool) // pool 供 Stats.Upsert COPY 两阶段批量写与 Billing 结算语句直连事务 + 会话锁专用连接（ledger-cursor）
	if err != nil {
		fatalDB("migrate", err)
	}
	// 统计桶界时区不在装配面：持久化恒规范 UTC，浏览器时区逐请求解析注入
	// （handler → service → repository 方法参数），无进程级可变状态。
	// usage_logs/err_logs/usage_stats 分区 bootstrap（分表设计 +
	// 用户裁决 2026-08-11 三表统一分区机制）：ent migrate 已跳过三表
	// （migrateHookExcludesPartitioned——atlas 对分区表 diff 规划期必失败，实测
	// 结论见 internal/repository/partition.go），此处独占建分区表 + 预建当日/明日
	// 分区 + 索引；幂等（已分区 → 仅补齐分区），失败即 fatal（明细/审计/统计表
	// 不可缺）。
	if err := repos.EnsureUsageLogPartitioned(startupCtx, time.Now()); err != nil {
		fatalDB("usagelog partition bootstrap", err)
	}
	if err := repos.EnsureErrLogPartitioned(startupCtx, time.Now()); err != nil {
		fatalDB("err_logs partition bootstrap", err)
	}
	if err := repos.EnsureUsageStatsPartitioned(startupCtx, time.Now()); err != nil {
		fatalDB("usage_stats partition bootstrap", err)
	}
	if err := repos.EnsureUsageEntityStatsPartitioned(startupCtx, time.Now()); err != nil {
		fatalDB("usage_entity_stats partition bootstrap", err)
	}
	if err := repos.Partitions.EnsureRoutingPartitions(startupCtx, time.Now()); err != nil {
		fatalDB("routing partition bootstrap", err)
	}
	if err := repos.EnsurePriceVariantsEffectCheck(startupCtx); err != nil {
		fatalDB("price_variants effect check bootstrap", err)
	}
	if err := repos.EnsureCodexSearchSeed(startupCtx); err != nil {
		fatalDB("codex-search price seed bootstrap", err)
	}
	// NOTIFY 发布器（多实例广播，设计文档 §2）。实例 ID = hostname-pid-
	// nonce（config 无实例字段，最小方案；容器化多实例同 hostname、
	// pid namespace 各自 pid 1 → 纯 hostname-pid 互相碰撞 → 互把对方 NOTIFY 当
	// 自播跳过 → users/templates/groups/keys/rules 失效静默全灭；随机 nonce 保证
	// 跨实例唯一）。发布在 DB 写成功后（与 inv.* 调用点并排）；计费路径永不发布。
	host, err := os.Hostname()
	if err != nil {
		fatalf("hostname: %v", err)
	}
	src, err := instanceSrc(host, os.Getpid())
	if err != nil {
		fatalf("instance src: %v", err)
	}
	pub := notify.NewPublisher(pool, src, log)
	// 实例发现（consumer spec 2026-08-25-redis-instance-discovery-design §2.2）：
	// ZSET 心跳成员协议，活体计数即集群 N——替换手工 cluster.instances 设置。
	// self 与 NOTIFY Src 同源（hostname-pid-nonce 生成模式复用）。
	disco := discovery.New(rdb, src, log)

	// 运行时健康投影先建（intelligent-routing lane）：只依赖 rdb/src/disco
	// 活体快照（与 selfID 同源），无 probe——probe 是 Start 期依赖（codex 适配器
	// 就绪后构造真 probe，经 healthWorker 在 Start 期一次性交接，无回填）。
	// sched 构造即持 health（构造注入，无 Set* 回填）。
	runtimeHealth := scheduler.NewRuntimeHealth(rdb, src, disco.LiveMembers, log)
	// 锁存一等组件（根因重开）：main 拥有 LatchStore + Hub 各恰好一次，
	// 经构造参出借——latch→hub→sink→persistFn→ruleEngine→sched 序，无 Set* 回填。
	latchStore := latch.NewLatchStore()
	hub := latch.NewHub()
	latchSink := scheduler.NewLatchSink(runtimeHealth, latchStore, hub)
	persistFn := scheduler.NewRulePersistFunc(repos, latchStore, schedGroupPub{pub}, log)
	// 规则引擎构造（不 Reload——New 只建结构；sink/persist 一次性注入）。
	ruleEngine := rule.New(rule.Config{}, repos.Rules, log, latchSink, persistFn)
	sched := scheduler.New(scheduler.Config{
		SyncInterval:   cfg.Scheduler.SyncInterval,
		StalenessProbe: repos.Groups,
	}, repos.Groups, ruleEngine, runtimeHealth, log, latchStore, hub)
	// 额度回写器只在计费开启时注入：BillingCapture=false 时 proxy finish
	// 本就不产生 AddQuota 增量，此处等价停用 quota writer/flush 落库面；
	// UsageCapture 与 quota 回写解耦（quota 走 Recorder 独立 flush 节奏）。
	var quotaWriter usage.QuotaWriter
	if cfg.Billing.Enabled {
		quotaWriter = repos.Keys // 额度扣减批量回写（Recorder 节奏）
	}
	rec := usage.New(usage.UsageConfig{
		BatchSize:          cfg.Usage.BatchSize,
		FlushInterval:      cfg.Usage.FlushInterval,
		QuotaFlushInterval: cfg.Usage.QuotaFlushInterval, // quota 增量批量回写 cadence
		Workers:            cfg.Usage.FlushWorkers,
		QuotaWriter:        quotaWriter,
	}, repos.Usages, log)
	// 离线聚合 worker（spec 2026-08-14 使用量统计离线聚合化）：独立 goroutine
	// 每周期从 usage_logs/err_logs 重建 usage_stats（两范围 + 三查询 + 单事务
	// DELETE+INSERT+watermark，见 usage/stats_agg.go）；0 = 禁用聚合（Start
	// 直接返回，等价不装配）。quota 回写在线保留（Recorder flushQuota），不随
	// 统计离线化搬移。
	statsAgg := usage.NewStatsAgg(usage.StatsAggConfig{
		Interval: cfg.Usage.StatsAggInterval,
	}, repos.Stats, log)
	// errlog worker（分表设计）：错误明细落盘通道——与计费 flusher 完全解耦
	// （独立有界队列 + 背压采样丢弃 + 独立排空）；落盘 err_logs（瘦表审计）。
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{
		QueueSize:     cfg.Usage.ErrLogQueueSize,
		BatchSize:     cfg.Usage.ErrLogBatchSize,
		FlushInterval: cfg.Usage.ErrLogFlushInterval,
	}, repos.ErrLogs, log)
	// retention worker：三表按日分区保留（分表设计 + usage_stats 分区化，
	// 清理统一 DROP PARTITION O(1)——PG DELETE 不释放空间，用户裁决）；保留天数
	// 同源 config（usage_logs = usage.log_retention_days；err_logs =
	// usage.errlog_retention_days 默认 7 天短保留——错误审计；usage_stats =
	// usage.stats_retention_days 默认 180 天——聚合统计长保留）。
	retention := usage.NewRetention(usage.RetentionConfig{
		LogRetentionDays:                cfg.Usage.LogRetentionDays,
		ErrLogRetentionDays:             cfg.Usage.ErrLogRetentionDays,
		StatsRetentionDays:              cfg.Usage.StatsRetentionDays,
		RoutingObservationRetentionDays: cfg.Routing.ObservationRetentionDays,
	}, repos, log)
	// 路由观测写面守卫同源：同一份 observation_retention_days 交给分区仓，
	// UpsertFlowSnapshot 据此拒早于截止的快照（防 retention 删后重建）。
	repos.Partitions.SetRoutingObservationRetentionDays(cfg.Routing.ObservationRetentionDays)

	auth := proxy.NewAuth(repos.Keys, repos.Users, log, cfg.Billing.Enabled)
	hc := httpx.NewClient(httpx.TransportConfig{
		MaxIdleConns:        cfg.Upstream.MaxIdleConns,
		MaxIdleConnsPerHost: cfg.Upstream.MaxIdleConnsPerHost,
		IdleConnTimeout:     cfg.Upstream.IdleConnTimeout,
		DialTimeout:         cfg.Upstream.DialTimeout,
		ForceHTTP2:          cfg.Upstream.ForceHTTP2,
		// Proxy 显式直连（防劫持）：HTTP_PROXY 环境变量不再静默改道
		// 上游请求（含 x-api-key/Authorization 凭据，WS 升级大概率失败）；
		// 压测行为不随部署环境漂移。
		Proxy: nil,
	})
	clients := aiclient.NewFactory(hc, aiclient.Config{
		UpstreamTimeout:       cfg.Proxy.UpstreamTimeout,
		UpstreamStreamTimeout: cfg.Proxy.UpstreamStreamTimeout,
	})
	// 管理端变更统一经 invalidate 去抖器生效（接线矩阵）：
	// - 用户 CRUD（含创建）/余额变更 → auth + 余额快照全量 Reload（去抖窗口
	//   内合并；新用户必须即刻进余额快照——防 ≤10s 402 窗口）
	// - 模板（base_url/models/映射）→ sched 全量 + clients 失效（base_url
	//   变更需按新地址重建 SDK 客户端；评审发现：此前 Factory.InvalidateAll
	//   无人调用，模板 base_url 更新后流量仍打旧上游直至重启）
	// - 账号 → sched 组级定向 InvalidateGroup（full ⊇ 组级 ⊇ 无）；upstream_key
	//   变更 → clients 失效
	// - 组倍率 / 用户-组专属倍率（group_assignment，按组）→ 余额倍率
	//   快照定向刷新（EffectiveMultiplier 陈旧 ≤10s 不可接受）
	// - key CRUD（扩展）→ auth 快照全量 Reload（本地仍走 auth 增量
	//   Upsert/Delete——单实例快路径；Keys() 分支覆盖远端实例的陈旧快照）
	// - settings 变更（UpdateSetting）→ 发布端 inv.Settings()（本地 auth
	//   快照全量 Reload，gate 预算按新 N 重算；≤200ms 去抖窗口与其余 Kind
	//   一致）；远端 NOTIFY → dispatcher 同步 ReloadSettings + scope 声明方
	// - 规则 CRUD → 规则表全量重载（ruleEngine.ReloadRules，重载清窗口计数——
	//   全实例同步执行语义）
	// - 定价快照变更 → 对端同步 ReloadPricingCtx（dispatcher 直连，settings
	//   同款；缺价 402 窗口跨实例收敛）
	// 去抖窗口 200ms：管理面变更生效延迟 ≤ 窗口 + 一次重载时长；后沿语义
	// （完成后又脏立即再执行，不按固定间隔 throttle——不与长 reload
	// 重叠）。读端永不阻塞：Mark 路径零锁零 DB，重载单 goroutine 串行（消除
	// 压测实证的 33,705 goroutine reloadMu 串行雪崩）。
	//
	// 计费装配提前到 svc 之前：去抖器装配需要余额快照引用；billHooks 仍需
	// svc，在 svc 之后组装。
	var billFlusher *billing.Flusher
	var billHooks *proxy.BillingHooks
	var billBalances *billing.Balances
	// invBalances 接口声明而非具体类型：billing 关闭时保持 nil 接口（非 typed
	// nil）。若用 *billing.Balances 声明，关闭时接口 = (*Balances)(nil)，类型部分
	// 非 nil → invalidate.reloadAll 的 `!= nil` 检查判 TRUE → Reload 调用 nil
	// receiver panic（2026-08-10 管理端建用户实证：Debouncer.loop panic）。
	var invBalances invalidate.BalancesReloader
	if cfg.Billing.Enabled {
		// loader = Repository 门面（BalanceLoader：余额 → Users，组倍率 +
		// assignment 专属倍率 → Groups，修正按组）。
		billBalances = billing.NewBalances(repos, log)
		invBalances = billBalances
		// 首载不在此（fail-safe 语义由注册表 ReloadAll 承担：错误独立 Warn 保留
		// 空快照 → 预检全 402 拒绝，安全侧）——单一启动入口，消灭双重加载。
	}
	inv := invalidate.New(invalidate.Config{
		Window:   invalidate.DefaultWindow, // 200ms（生效延迟语义见 invalidate 包注释）
		Sched:    sched,
		Clients:  clients,
		Auth:     auth,
		Balances: invBalances, // billing.enabled=false → nil 接口（flush 跳过余额路径）
		Rules:    ruleEngine,  // 规则 CRUD → 全实例规则表重载（ruleEngine 先于 invalidate 构造）
		Log:      log,
	})
	// ruleReload 独立于 invalidate：规则 CRUD 后全量重载（重载会重置窗口计数，
	// 不能随模板/账号/分组等任意资源变更触发）。
	// 浏览器时区原始行分组 horizon 跟随 raw 双表（usage_logs + err_logs 都
	// 被读）的最小正保留：一个 <=0 取另一个，双禁用 → 不限；缺省 errlog
	// 7d < log 30d → 8d 窗口。统计读时区本身是请求级参数，不进装配面。
	rawDays := cfg.Usage.ErrLogRetentionDays
	if d := cfg.Usage.LogRetentionDays; d > 0 && (rawDays <= 0 || d < rawDays) {
		rawDays = d
	}
	// 余额预警已知键清理（Redis）：构造期一次建好，ServiceDeps 与
	// wireBalanceWarning 共用同一实例。
	bwCooldown := notification.NewCooldown(rdb)
	// svc 一次性装配（定价时区/原始行 horizon/验证码存储/预警清理/
	// recover→PROBING 全部构造参数；路由编译触发 CompileNotify 同步
	// 构造注入（sched 先于 svc 存在，func 值无 import 环）——空时区配置 = nil
	// → 进程本地，语义不变；验证码 Redis 必选 ⇒ 无 nil 分支，误接线 New 内 panic）。
	// 根因重开：settings 快照提升为一等组件——main 先构造单个共享
	// *settingssnap.Snapshot（首载失败仅 Warn，由 Snapshot.Load 调用方保持
	// fail-safe），mailW 与 svc 同源共享该指针（NOTIFY 只刷一处，无分叉）；
	// mailW.Enqueue 经 ServiceDeps.MailEnqueue 一次注入，零 Set* 回填。
	settingsSnap := settingssnap.New(repos, log)
	if err := settingsSnap.Load(context.Background()); err != nil && log != nil {
		log.Warn("settings snapshot initial load failed", logx.Error(err))
	}
	mailW := service.NewMailWorker(service.MailDeps{Log: log, Settings: settingsSnap, Templates: repos})
	svc := service.New(repos, sched, inv, pub, ruleEngine, auth, log, service.ServiceDeps{
		EmailCodeStore:                  verification.New(rdb),
		TimeLocation:                    svcLoc,
		StatsRawRetentionDays:           rawDays,
		ClearBalanceWarningCooldown:     bwCooldown.Clear,
		RecoverProber:                   runtimeHealth,
		RecoverLatch:                    latchStore,
		RecoverHealthClear:              runtimeHealth,
		DefaultMaxConcurrency:           cfg.Scheduler.DefaultMaxConcurrency,
		RoutingObservationRetentionDays: cfg.Routing.ObservationRetentionDays,
		CompileNotify:                   sched.RequestCompile,
		MailEnqueue:                     mailW.Enqueue,
		SettingsSnapshot:                settingsSnap,
	})
	// 快照注册表装配（统一生命周期）：五路快照（auth/scheduler/rules/pricing/
	// balances——billing 关闭不注册）登记 scope 与 Reload。注册只登记元数据
	// （零 DB），首刷统一在构造链完成后执行（见下 ReloadAll——单一启动入口，
	// 各模块构造内不自行 reload）。scope 分发 = settings 变更 → ScopeSettings
	// 声明方（auth gate N 预算）；其余变更类型仍走去抖器，注册表不重复
	// 接管（周期 ticker 亦各模块自管，边界见 internal/snapshot 包注释）。
	snapReg := snapshot.New()
	for _, s := range []snapshot.Snapshot{
		authSnapshot{auth},
		schedSnapshot{sched},
		ruleSnapshot{ruleEngine},
		pricingSnapshot{svc},
	} {
		if err := snapReg.Register(s); err != nil {
			fatalf("snapshot register: %v", err)
		}
	}
	if billBalances != nil { // 与 invBalances 同纪律：billing 关闭不注册
		if err := snapReg.Register(balanceSnapshot{billBalances}); err != nil {
			fatalf("snapshot register: %v", err)
		}
	}
	// NOTIFY 监听装配。变更分发器放装配侧——notify 不 import
	// invalidate（依赖环约束）。
	disp := &dispatcher{
		inv:       inv,
		svc:       svc,
		snapshots: snapReg,
		log:       log,
	}
	// NOTIFY 监听 worker（Name="notify"）：独立 pgx 连接 LISTEN c3api_invalidate；
	// 断线指数退避重连 + 重连即全量刷新（R8）；Src 跳过自播（省重复 reload）。
	listener := notify.NewListener(notify.ListenerConfig{
		DSN:        cfg.DB.DSN,
		Src:        src,
		Dispatcher: disp,
		Log:        log,
	})
	// 60s 周期鉴权快照兜底（NOTIFY 丢失/断连期间 key 与用户变更最长 60s
	// 收敛；现状 auth 无周期 reload，是兜底缺口——auth-sync worker 补位。
	// 在其 Reload 内接入 N 与预算重分配，本 worker 侧无需再改）。
	authSync := newAuthSync(auth, 0, log)
	var warningW *notification.Worker
	if cfg.Billing.Enabled {
		// ledger-cursor（spec 2026-08-23）：游标消费者——不再注入 rec（内存
		// pending 队列已删，billable 行由 usage flusher 单写落库 billed=false，
		// 本 worker 只消费账本游标）；LogRetentionDays 接线 lag 护栏（最老
		// unbilled 行超保留期 80% 高声 Warn）。
		// 构造序反转：warning worker 先建、flusher 后建——sink 经
		// NewFlusher 构造参数一次注入，禁止事后回填。warningW 为具体非 nil
		// *notification.Worker（notification.New 恒非 nil，nil cooldown 即
		// panic），无 typed-nil 风险；旧 "real nil interface" 守卫保护的是
		// 相反方向（flusher→setter 接口装箱），随 setter 删除而失效、此处
		// 不再需要。billing 关闭 → warningW/billFlusher 均为 nil（与旧分支一致）。
		warningW = wireBalanceWarning(bwCooldown, svc, mailW, log)
		billFlusher = billing.NewFlusher(billing.FlushConfig{
			FlushInterval:          cfg.Billing.FlushInterval,
			BalanceRefreshInterval: cfg.Billing.BalanceRefreshInterval,
			LogRetentionDays:       cfg.Usage.LogRetentionDays,
		}, repos, billBalances, log, warningW)
		billHooks = &proxy.BillingHooks{
			Resolver:   svc,
			Balances:   billBalances,
			Flusher:    billFlusher,
			TierPolicy: svc.ServiceTierPolicy,
		}
	}
	// 三协作者一律 px 之前构造、经 proxy.Deps 一次注入（构造重排，
	// 零语义变化——各构造失败仍 fail-fast，nil 语义由 proxy 内部保持）。
	// 硬续接绑定存储（Responses REST/WS create-ACK + previous_response_id
	// 钉选）：HMAC 密钥由 auth.jwt_secret 经 HKDF 派生（同源密钥，零新增
	// secret），Redis 复用 rdb 生命周期（store 无独立 Close——连接池归
	// redisx 统一释放）。构造失败 = 装配失败（fail-fast；jwt_secret 非空
	// 已由 config.Load 校验）。生产恒非 nil：普通请求零 Redis，
	// previous_response_id 请求 fail-closed 由 proxy 内部保证。
	contStore, err := continuation.New(rdb, cfg.Auth.JWTSecret)
	if err != nil {
		fatalf("continuation: %v", err)
	}
	// codex SDK 适配层装配（§3——统一失效回调先落生图路径；全量）：
	// 适配层构造注册 WithOnAuthFatal → 统一回调 → 失效处理链（写 failed_at +
	// 调度摘除 + 审计契约）。transport/rotation 同构造期一次给齐（构造后
	// 不存在半装配形态）：transport 用 httpx 网关同形态（SDK 默认
	// MaxIdleConnsPerHost=2 有压测连接风暴史；Proxy=nil 直连防劫持）；
	// rotation Upsert 部分更新（codex_oauth_token/refresh/expires_at 保旧）+
	// 回写后失效调度器 AccountExt 快照条目（下个会话重载新凭据）。
	// Latch/Publisher 必须装配：SDK 失效链据此走**围栏路径**（先锁存 → 以身份
	// 代际 K 为 guard 的 CAS 落库 → 组级 NOTIFY）；缺任一项即退化为只写 failed_at
	// 的简化路径——判决不再与"身份是否被授权变更"对齐，且对端只能靠周期兜底收敛。
	codexAdapter := sdkbridge.NewCodex(sdkbridge.NewFailureHandler(sdkbridge.FailureDeps{
		Store:     repos.Accounts,
		Failer:    sched,
		Log:       log,
		Latch:     latchStore,
		Publisher: schedGroupPub{pub},
	}), httpx.NewTransport(httpx.TransportConfig{
		MaxIdleConns:        cfg.Upstream.MaxIdleConns,
		MaxIdleConnsPerHost: cfg.Upstream.MaxIdleConnsPerHost,
		MaxConnsPerHost:     cfg.Upstream.MaxIdleConnsPerHost,
		IdleConnTimeout:     cfg.Upstream.IdleConnTimeout,
		DialTimeout:         cfg.Upstream.DialTimeout,
		ForceHTTP2:          cfg.Upstream.ForceHTTP2,
		Proxy:               nil,
	}), sdkbridge.RotationDeps{
		Store:              repos.AccountExts,
		InvalidateSnapshot: sched.InvalidateAccount,
		Log:                log,
	})
	// quality-sync lane（intelligent-routing）：500ms Redis 当前分钟绝对
	// 快照发布 + 5s PG quality/flow UPSERT。单实例一个串行 loop（worker.GoLoop
	// 监督），PG 写面直用 repos.Partitions（routing 分区表 absolute UPSERT+
	// dirty 同事务）。装配在 handler 之前：opsWorkers 聚合需要该引用已存在。
	effectiveInflight, err := config.EffectiveMaxInflight(cfg.Proxy.MaxInflight)
	if err != nil {
		fatalf("config: %v", err)
	}
	qualityRecorder, err := quality.NewRecorder(effectiveInflight)
	if err != nil {
		fatalf("quality: %v", err)
	}
	px := proxy.New(proxy.Config{
		MaxBodySize:           cfg.Proxy.MaxBodySize,
		UpstreamTimeout:       cfg.Proxy.UpstreamTimeout,
		UpstreamStreamTimeout: cfg.Proxy.UpstreamStreamTimeout,
		FailoverAttempts:      cfg.Proxy.FailoverAttempts,
		UsageCapture:          cfg.Proxy.UsageCapture,
		BillingCapture:        cfg.Billing.Enabled,
		BehindCDN:             cfg.Proxy.BehindCDN, // client_ip 供应商头识别开关（false = 直取 RemoteAddr）
	}, sched, credential.New(), rec, clients, auth, log, billHooks, errlogW, proxy.Deps{
		Codex:        codexAdapter,
		Recorder:     qualityRecorder,
		Continuation: contStore,
	})
	// 规则 typed Throttle/FailAccount 双面已在构造期接线（latch fail-closed
	// 先于持久化；持久化走有界 persist queue——满可丢、写失败可弃、四指标可观测，
	// rule best-effort 契约，无 outbox）。接线唯一性由 rule_wiring_test.go 钉死。
	// SDK fatal 重试 worker 纳管（blocker：旧态惰性起循环且进程退出前永不
	// join）：managed lifecycle——Start 预起 supervised 循环，Close 先于 Redis
	// 释放 join；重试语义（backoff/fencing/进程存活期重试）原样保留。
	retryWorker := sdkbridge.NewFailureRetryWorker(log)
	// 真实健康 probe 构造（blocker：构造期曾传 nil probe——所有 PROBING 记录
	// 30s TTL 后恒失败，恢复流程永远到不了 READY）。探测权威 = scheduler 选号
	// 快照（与选号门同一视图，revision fence fail-closed）；codex 凭据走 SDK
	// 适配器 usage 快照路径（fatal 权威保持）；api_key 族无合成探测面（owner
	// 裁决：/v1/models 与流量无关，已删除——探测视为通过，恢复由时间窗+真实
	// 流量判定）。超时同上游请求预算。probe 经 healthWorker 在 Start 期一次性
	// 交给 runtimeHealth（Start 前记录 fail-closed 停在 OPEN/PROBING，绝不 READY）。
	probe := newHealthProber(sched.ProbeAccount, codexAdapter, cfg.Proxy.UpstreamTimeout)
	// 多实例集群 N 注入（discovery 接管，consumer spec §2.2）：gate 预算
	// ceil(剩余/N) + limit RPM ceil(rpm/N)。N = Redis 心跳活体数（disco 实时读
	// atomic），gate/limit 在每次预算分配时现读 provider（gate.go:106-121），
	// 心跳计数变化 ≤1 tick 天然生效，无需任何 reload 触发。
	px.SetInstancesProvider(disco)
	// scheduler 选号侧同一 N 源（spec conc-share-borrow-account §1.1）：账号并发
	// 份额除数 = Redis 心跳活体数，ReserveAttempt 入口现读，心跳变化 ≤1 tick 生效。
	sched.SetInstancesProvider(disco)
	// 并发门跨实例共识 worker（spec conc-share-borrow-gate §1.5）：500ms 一条
	// pipeline 双向同步受限层级在途 → gate 第二快照（clusterView），请求路径
	// 零 Redis；nil/未启动 = 无视图 = 全额本地语义。self 与 NOTIFY Src /
	// discovery 同源（同一 instanceSrc 产物，不自造第二套 ID）。
	concSync := proxy.NewConcSyncWorker(auth, rdb, src, log)
	// 账号并发份额+借用跨实例共识 worker（spec conc-share-borrow-account §2）：
	// 协议孪生 conc-sync，命名空间 c3api:conc:a:*——500ms 一条 pipeline 双向同步
	// 账号在途 → Scheduler.concView，选号热路径零 Redis。src 复用同源产物。
	accConcSync := scheduler.NewConcSyncWorker(sched, rdb, src, log)
	// litellm 价格同步 worker：启动异步拉取一次（不阻塞启动）+ price_sync_cron
	// 定期循环；source_url/cron 每轮从 svc 的 settings 快照现读（变更下次循环
	// 生效，无热加载通道）；同步成功后刷新 svc 价格快照（计费读零 DB）。
	// 手动 sync/preview 端点（/api/admin/pricing/sync）直调同一 worker：
	// service 侧 SetPriceFetcher 回填已删——fetcher 唯一主人是本 worker，经
	// SyncWorkerConfig 一次性构造注入；预览 membership 读 svc 定价快照。
	// log：方案 A 多档位 Warn 目标（nil 则静默——不传即退化为无告警）。
	priceFetcher := pricing.NewFetcher(hc, log)
	pricingSync := pricing.NewSyncWorker(pricing.SyncWorkerConfig{
		Fetcher:  priceFetcher,
		Repo:     repos,
		Settings: svc,
		// 统一快照：拉取成功后刷新 pricing 快照（price_entries+price_variants），
		// 快照变化同时按需惊动路由编译（同值同步静默——比较在 service 内，
		// 编译道经 lane-local diff 定作用域）。
		Reload: svc.ReloadPricingAndNotifyCompiler,
		// 预览 membership：svc 定价快照（nil 快照 = 全量 ToAdd）。
		Snapshot: svc,
		Log:      log,
	})
	// quality-flow-owner（async-routing-quality-telemetry）：跨请求 flow 累计器
	// 的唯一 state owner。请求结算只做一次不可变非阻塞 Submit，同分钟身份归并
	// 全部离开请求 goroutine（owner 循环 / sync 消费侧）。注册必须先于
	// qualitySync：反向排空先关 quality-sync（其失败 refill 需要 owner 仍开），
	// 后关 owner（停提交 + 排空队列 + residual 可见）。owner 溢出只是遥测丢失，
	// 不触碰 HTTP/failover/健康/配额/usage/计费。
	qualityFlowOwner := qualityRecorder.FlowOwner()
	// instanceSrc 与 discovery/conc-sync 同源产物（不自造第二套 ID）：跨实例
	// merge 按 instance_src 区分，同源身份是 merge 正确性的前提。
	// 缺陷 B 质量面：PG 落库边界成功持久新质量行 → 事件驱动编译（非阻塞、
	// 下游去抖收敛；空刷/失败静默，无定周全量）。构造器注入（nil = 未装配）；
	// 价格面见 pricingSync.Reload（service 内经 ServiceDeps.CompileNotify
	// 变化门控后通知编译）。
	qualitySync := quality.NewSyncWorker(qualityRecorder, rdb, repos.Partitions, quality.SyncConfig{InstanceSrc: src}, log, sched.RequestCompile)
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
	userHandler := userapi.Router(svc, iss, auth, ruleEngine)

	// /api/admin/ops/workers 运维观测（spec 2026-08-11，用户裁决并入管理面）：
	// 独立 Stats 契约不改 worker.Worker——装配侧类型断言聚合（各模块已持
	// 具体引用，断言实现 handler.StatsProvider 的入列；快照注册表状态单独
	// 经 Status 直出）。WithOps 注入，路由由契约 chi-server 生成。
	// 同一有序切片同时供 ops 聚合与 Manager 注册，避免观测面与生命周期面漏接。
	// optional worker 先转为 real nil interface，禁止 typed-nil 穿透条件装配。
	var warningWorker worker.Worker
	if warningW != nil {
		warningWorker = warningW
	}
	var billingWorker worker.Worker
	if billFlusher != nil {
		billingWorker = billFlusher
	}
	// health 启动适配：真 probe 在 Start 期一次性交接（位置与 Name 不变——
	// 注册序=反序排空语义与 worker_order_test 的 ident 断言依赖）。
	healthW := healthWorker{h: runtimeHealth, probe: probe}
	// 编译源启动适配（双 setter 已删）：同一位置同一顺序注册 schedW
	// （Name="scheduler" 不变）——反序排空语义与 worker_order_test 断言依赖。
	schedW := schedWorker{s: sched, src: schedSrc}
	managedWorkers := orderedWorkers(mailW, warningWorker, billingWorker,
		inv, schedW, ruleEngine, retryWorker, healthW, rec, errlogW, pricingSync, retention, statsAgg, qualityFlowOwner, qualitySync)
	opsCandidates := append([]worker.Worker{}, managedWorkers...)
	opsCandidates = append(opsCandidates, listener, authSync)
	// （spec 2026-08-13）：StatsProvider 断言失败 Warn 一次；无 Stats 的
	// worker 合法，但启动期明确提示其不会出现在运维端点。
	opsWorkers := statsProviders(opsCandidates, log)
	// discovery 实例发现观测（foundation spec §2.4）：alive N / last_tick_ok /
	// consecutive_errors——Redis 故障冻结期在运维面可见（instances 停走 +
	// consecutive_errors 增长）。
	opsWorkers = append(opsWorkers, disco)
	// conc-sync ×2 协调面观测（spec conc-sync-ops-stats）：fail-open 静默退化的
	// 唯一可见痕迹——视图冻结时 last_tick_ok 翻 false、consecutive_errors 增长。
	opsWorkers = append(opsWorkers, concSync, accConcSync)
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
		BillingAlerts: func() handler.BillingAlerts {
			if billFlusher == nil {
				return handler.BillingAlerts{}
			}
			s := billFlusher.Stats().(billing.FlusherStats)
			return handler.BillingAlerts{
				LagMs:           s.LagMs,
				UnbilledRows:    s.UnbilledRows,
				QuarantinedRows: s.QuarantinedRows,
			}
		},
	})

	srv := server.NewServer(server.Options{
		AdminToken:        cfg.Admin.Token,
		JWTIssuer:         iss,
		UserStatus:        auth,
		MaxInflight:       effectiveInflight,
		ReadHeaderTimeout: cfg.Server.ReadHeaderTimeout,
		MaxHeaderBytes:    cfg.Server.MaxHeaderBytes,
		AdminHandler:      h.RoutesMux(),
		UserHandler:       userHandler,
		AIHandler:         planReadyGate(sched, aiRouter),
		WebFS:             webUI(),
		Logger:            log,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 统一启动就绪（快照注册表）：构造链完成后全量首刷（并行；各快照错误独立
	// Warn 不阻塞启动——DB 故障时快照保持空/旧值，模块周期 ticker / listener
	// FullRefresh 兜底收敛）。取代此前分散的 ruleEngine.Reload fatalf +
	// sched.InvalidateAllSync fatalf + auth/billBalances 构造时加载：scheduler
	// 首刷在此完成（Select 在 nil 快照上 panic 的窗口随单一入口消灭），规则
	// 空表种子亦在此写入（失败降级 Warn——注册表 Status 可观测）。
	errs := snapReg.ReloadAll(ctx)
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
		disp.bootLoaded.Store(true)
	}
	// 统一 worker 管理：顺序启动、反向排空。注册序 email → notification(条件)
	// → billing(条件) → 业务 worker，保证停机时 usage recorder 先落完整账本，
	// billing 再终扫游标，随后 notification 排空已提交告警，最后 email 排空独立
	// 的注册/重置队列。billing 关闭时 email 仍无条件注册，管理端 channel-test 与
	// auth 邮件行为保持独立。
	wm := worker.New(log)
	wm.Register(managedWorkers...) // invalidate 去抖器执行 goroutine（单 goroutine 串行）；errlog 错误明细排空在 rec 之后注册 → 反向排空先于 rec；retention/stats-agg 顺序无依赖（覆盖语义幂等，停摆窗口由追赶上限收敛）。billFlusher 缺位时：计费关闭，billable 行出生即 billed=true 吸收态，无未扣积压
	// conc-sync 业务区段尾部（spec §1.5：协调态可丢、无排空顺序依赖——停机即停
	// tick，在途 HASH 字段 ≤4s ts 出局 + 16s EXPIRE 自灭，Close 无清理义务）。
	wm.Register(concSync)
	// 账号层孪生同段并排（spec conc-share-borrow-account §2：同款协调态语义）。
	wm.Register(accConcSync)
	// discovery 在业务 worker 之后、listener/authSync 之前注册（foundation spec
	// §2.3 装配序）：反向排空时 listener 先停接收、discovery 随即 ZREM 自身缩容
	// 掉出 N，再排业务 worker——billing 游标终扫前集群基数已收敛。
	wm.Register(disco)
	// listener/auth-sync 最后注册 → 反向排空最先关：停止接收/周期刷新后再排空
	// 业务 worker（scheduler 排空回写仍会发布 NOTIFY，自播跳过、其它实例接收，
	// 无依赖）。
	wm.Register(listener, authSync)
	if err := wm.StartAll(ctx); err != nil {
		fatalf("worker start: %v", err)
	}
	// 调度器初始加载已由上方注册表 ReloadAll 完成（先于 StartAll 与流量）——
	// 此处不再单独 InvalidateAllSync（单一启动入口）。

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
	ln, err := lc.Listen(context.Background(), "tcp", cfg.Server.Addr)
	if err != nil {
		fatalf("server: listen %s: %v", cfg.Server.Addr, err)
	}
	wm.Go(ctx, "http-server", func(_ context.Context) {
		log.Info("server listening", logx.String("addr", cfg.Server.Addr))
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fatalf("server: %v", err)
		}
	})

	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// 优雅停机链（计费不丢窗口）：
	// 1) Shutdown(2s) 优雅窗口：快速请求收尾（长连接流式超时留给 Close 强断）
	// 2) Close 强制断长连接 → 客户端断开 → recordStreamAbort → finish（断前
	//    usage 帧照常计费）
	// 3) waitForInflight：等在途归零（100ms 轮询；超时 Warn 继续不阻塞退出）
	// 4) wm.Shutdown 反向排空：listener/authSync 最先停止接收 → disco ZREM 缩容
	//    → accConcSync/concSync → statsAgg/retention/pricingSync/errlogW
	//    → rec 排空明细 → rule/sched/inv → billing 终扫完整账本
	//    → notification 排空余额告警 → email 排空 auth 邮件
	//    （quality-sync 的 Close 在此链内排空 Redis/PG；PG 失败时 refill 回
	//    recorder——recorder 必须仍开着，见 shutdown.go 的排空/refill 注释）
	// 5) 停机尾部（worker 排空 → recorder 终态 → Redis 释放 → 终态日志）收敛
	//    在 shutdownTail：排空失败不再 `_ =` 静默——Error 级 "shutdown
	//    incomplete" 显式上报且不宣称 clean shutdown（review blocker
	//    2026-08-30）。
	srvCtx, cancelSrv := context.WithTimeout(shutdownCtx, 2*time.Second)
	// （spec 2026-08-13）：httpSrv 两项错误并入 shutdown Warn（旧实现
	// `_ =` 全丢弃；wm.Shutdown 内部已对 worker Close 失败 Warn，此处补齐
	// httpSrv 静默面）。
	if err := httpSrv.Shutdown(srvCtx); err != nil {
		log.Warn("http server shutdown failed", logx.Error(err))
	}
	cancelSrv()
	if err := httpSrv.Close(); err != nil {
		log.Warn("http server close failed", logx.Error(err))
	}
	px.CloseAllWS()
	waitForInflight(px, shutdownCtx, log)
	shutdownTail(shutdownCtx, wm, qualityRecorder, rdb, log)
	_ = log.Sync()
}

// instanceSrc 生成实例 ID（NOTIFY Src）：hostname-pid-nonce：
// 容器化多实例同 hostname、pid namespace 各自 pid 1 → 纯 hostname-pid 碰撞 →
// 互把对方 NOTIFY 当自播跳过 → 失效静默全灭；crypto/rand 随机 nonce 保证跨
// 实例唯一（6B 熵，同宿主两实例碰撞概率 ~2^-48，可忽略）。
func instanceSrc(host string, pid int) (string, error) {
	nonce := make([]byte, 6)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return fmt.Sprintf("%s-%d-%x", host, pid, nonce), nil
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
