// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 装配阶段 2/6：运行时健康投影 / 锁存 / 规则引擎 / 调度器 / 用量捕获面。
//
// 构造序不变量（rule_wiring_test 钉死）：latch.NewLatchStore → latch.NewHub →
// scheduler.NewLatchSink → scheduler.NewRulePersistFunc → rule.New → scheduler.New，
// 无 Set* 回填（sink/persist 构造期一次性注入，sched 构造即持 health）。
package main

import (
	"context"

	"github.com/redis/go-redis/v9"

	"github.com/is7qin/c3api/internal/config"
	"github.com/is7qin/c3api/internal/discovery"
	"github.com/is7qin/c3api/internal/latch"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/egress"
	"github.com/is7qin/c3api/pkg/logx"
)

// healthFace 是阶段 2 的产物：健康/锁存/规则/调度与用量捕获 worker。
type healthFace struct {
	runtimeHealth *scheduler.RuntimeHealth
	latchStore    *latch.LatchStore
	hub           *latch.Hub
	quotaQueue    *scheduler.QuotaQueue
	latchSink     *scheduler.LatchSink
	persistFn     rule.PersistFunc
	ruleEngine    *rule.RuleEngine
	sched         *scheduler.Scheduler

	// quotaWriter 只在计费开启时注入（BillingCapture=false 时 proxy finish 本就
	// 不产生 AddQuota 增量）；nil = 停用 quota writer/flush 落库面。
	quotaWriter usage.QuotaWriter
	rec         *usage.Recorder
	statsAgg    *usage.StatsAggWorker
	errlogW     *usage.ErrLogWorker
	attemptlogW *usage.AttemptLogWorker
	retention   *usage.RetentionWorker
}

// healthDeps 是阶段 2 **实际从 infra 阶段读到的全部字段**（窄化，spec §7.2）。
//
// supplierSnap 是唯一的前向槽：它保存的是字段地址（不是值），因为本阶段注册的
// 容量门回调在运行期才读，而快照由后面 billing 阶段写入。按值投影会让保护失效。
type healthDeps struct {
	cfg          *config.Config
	log          *logx.Logger
	rdb          *redis.Client
	repos        *repository.Repository
	src          string
	disco        *discovery.Discovery
	pub          *notify.Publisher
	egress       *egress.Pool
	codexClients *sdkbridge.ClientTable
	startupCtx   context.Context
	supplierSnap supplierSnapshotSlot
}

// newHealthFace 装配阶段 2（只依赖 healthDeps）。
func newHealthFace(d healthDeps) *healthFace {
	cfg, log := d.cfg, d.log
	rdb, repos, src, disco, pub := d.rdb, d.repos, d.src, d.disco, d.pub
	egressPool, codexClients, startupCtx := d.egress, d.codexClients, d.startupCtx
	f := &healthFace{}

	// 运行时健康投影先建（intelligent-routing lane）：只依赖 rdb/src/disco
	// 活体快照（与 selfID 同源），无 probe——probe 是 Start 期依赖（codex 适配器
	// 就绪后构造真 probe，经 healthWorker 在 Start 期一次性交接，无回填）。
	// sched 构造即持 health（构造注入，无 Set* 回填）。
	runtimeHealth := scheduler.NewRuntimeHealth(rdb, src, disco.LiveMembers, log)
	f.runtimeHealth = runtimeHealth
	// 429 额度补拉（设计 §5.4 / R3 裁定）：投递面是独立的 QuotaQueue——sink 只
	// 持队列、worker 只消费队列，这一层把「worker ⟷ LatchSink」的构造环拆开了。
	// 为什么先建队列后建 worker：sink 必须先于 ruleEngine、ruleEngine 必须先于
	// sched，而 worker 执行任务时要 sched.ProbeAccount 与 codex 适配器（resolver）
	// ——后两者都晚于 sink。队列本身无依赖，先建它、sink 拿它，worker 等 sched
	// 与 codex 适配器就绪后再建：依赖图无环，也不需要任何晚绑定指针或回填。
	// resolver 注册表同样先建后填（见 wiring_codex.go：codex 适配器就绪后按两个
	// 凭据类型注册，仍在装配期，worker 尚未 Start）。
	quotaQueue := scheduler.NewQuotaQueue(0)
	f.quotaQueue = quotaQueue
	// 锁存一等组件（根因重开）：main 拥有 LatchStore + Hub 各恰好一次，
	// 经构造参出借——latch→hub→sink→persistFn→ruleEngine→sched 序，无 Set* 回填。
	latchStore := latch.NewLatchStore()
	f.latchStore = latchStore
	hub := latch.NewHub()
	f.hub = hub
	latchSink := scheduler.NewLatchSink(runtimeHealth, latchStore, hub, quotaQueue)
	f.latchSink = latchSink
	persistFn := scheduler.NewRulePersistFunc(repos, latchStore, schedGroupPub{pub}, log)
	f.persistFn = persistFn
	// 规则引擎构造（不 Reload——New 只建结构；sink/persist 一次性注入）。
	ruleEngine := rule.New(rule.Config{}, repos.Rules, log, latchSink, persistFn)
	f.ruleEngine = ruleEngine
	// 编译期 fail-fast（U22 §2.1 第三步）：生产载入器必须保持 **scoped 组载入**
	// 能力。丢了它不会让别处编译失败——invalidateGroupsLocked 会静默退化（每组各
	// 建一枚模板指针，按指针键的摘要缓存只剩「每组每模板一次」，跨组零命中）。
	// 这一行是那条退化唯一的静态闸门；删掉它不会有任何测试变红，只能靠评审。
	var _ scheduler.ScopedGroupLoader = repos.Groups
	sched := scheduler.New(scheduler.Config{
		SyncInterval:   cfg.Scheduler.SyncInterval,
		StalenessProbe: repos.Groups,
		RotatePolicy:   sdkbridge.DefaultRotatePolicy(),
		Egress:         egressPool,
		// 线程退休与配置批次应用都走同一张表：退线只退役会话线程，账号资源只按
		// 控制器算好的**资源指令**（保留键集合 / 待删账号 / 批次代号）摘除——
		// 表不认识账号全集，"账号不在全集即删除""直连必须保留"都在控制器侧
		// （scheduler.accountRuntime），池注册表差分只回收会话/线程。
		RetireThread:    codexClients.RetireThread,
		ClientResources: codexClients,
	}, repos.Groups, ruleEngine, runtimeHealth, log, latchStore, hub)
	f.sched = sched
	// 启动期装配门（spec §4.7）：代理表里有启用条目、而调度器没拿到池 ⇒ 起不来。
	// 热路径不写"代理是否可用"的分支，装配缺失只能在这里暴露；否则后果是绑了
	// 代理的账号从本机 IP 出栈且无人知晓。
	if proxies, perr := repos.ListProxies(startupCtx); perr != nil {
		fatalDB("proxy pool wiring check", perr)
	} else {
		enabled := 0
		for _, px := range proxies {
			if px.Enabled {
				enabled++
			}
		}
		if enabled > 0 && !sched.EgressWired() {
			fatalf("egress pool not wired but proxy table has %d enabled entries: pass egress.NewPool(hc) as scheduler.Config.Egress", enabled)
		}
	}
	// 额度回写器只在计费开启时注入：BillingCapture=false 时 proxy finish
	// 本就不产生 AddQuota 增量，此处等价停用 quota writer/flush 落库面；
	// UsageCapture 与 quota 回写解耦（quota 走 Recorder 独立 flush 节奏）。
	if cfg.Billing.Enabled {
		f.quotaWriter = repos.Keys // 额度扣减批量回写（Recorder 节奏）
	}
	rec := usage.New(usage.UsageConfig{
		BatchSize:          cfg.Usage.BatchSize,
		FlushInterval:      cfg.Usage.FlushInterval,
		QuotaFlushInterval: cfg.Usage.QuotaFlushInterval, // quota 增量批量回写 cadence
		Workers:            cfg.Usage.FlushWorkers,
		QuotaWriter:        f.quotaWriter,
	}, repos.Usages, log)
	f.rec = rec
	// 离线聚合 worker（spec 2026-08-14 使用量统计离线聚合化）：独立 goroutine
	// 每周期从 usage_logs/err_logs 重建 usage_stats（两范围 + 三查询 + 单事务
	// DELETE+INSERT+watermark，见 usage/stats_agg.go）；0 = 禁用聚合（Start
	// 直接返回，等价不装配）。quota 回写在线保留（Recorder flushQuota），不随
	// 统计离线化搬移。
	statsAgg := usage.NewStatsAgg(usage.StatsAggConfig{
		Interval: cfg.Usage.StatsAggInterval,
	}, repos.Stats, log)
	f.statsAgg = statsAgg
	// errlog worker（分表设计）：错误明细落盘通道——与计费 flusher 完全解耦
	// （独立有界队列 + 背压采样丢弃 + 独立排空）；落盘 err_logs（瘦表审计）。
	errlogW := usage.NewErrLogWorker(usage.ErrLogConfig{
		QueueSize:     cfg.Usage.ErrLogQueueSize,
		BatchSize:     cfg.Usage.ErrLogBatchSize,
		FlushInterval: cfg.Usage.ErrLogFlushInterval,
	}, repos.ErrLogs, log)
	f.errlogW = errlogW
	// attemptlog worker（spec 2026-11-18 retry-observability §3.3）：每次**失败**
	// dispatch 一行尝试明细。与 errlog 完全解耦（独立有界队列 + 批量落库 +
	// 丢弃计数）：err_logs 记的是终态错误，本表记的是「打了几次上游」。
	//
	// 落盘节奏（8192 / 100 行 / 500ms）**刻意不进 config**：它们是写面不变量
	// （原子量级由表设计定，不是运维旋钮），只有保留期可配（attempt_retention_days）。
	attemptlogW, aerr := usage.NewAttemptLogWorker(usage.AttemptLogConfig{}, repos.AttemptLogs, log)
	if aerr != nil {
		fatalf("attemptlog worker: %v", aerr)
	}
	f.attemptlogW = attemptlogW
	// retention worker：三表按日分区保留（分表设计 + usage_stats 分区化，
	// 清理统一 DROP PARTITION O(1)——PG DELETE 不释放空间）；保留天数
	// 同源 config（usage_logs = usage.log_retention_days；err_logs =
	// usage.errlog_retention_days 默认 7 天短保留——错误审计；usage_stats =
	// usage.stats_retention_days 默认 180 天——聚合统计长保留）。
	// 供应商财务视图快照（enabled=true 才构造）经 in.supplierSnap 前向槽桥接
	// ——回调运行时点晚于 billing 阶段赋值（见 infraFace.supplierSnap 注释）。
	retentionCfg := retentionConfigFrom(cfg)
	// 容量处置（spec §3.11）：数据盘可用空间探针（best-effort，"." 所在卷）；
	// 低于下限 ⇒ 停供应商新流量（经回调置位视图快照的容量门）。
	retentionCfg.DiskProbe = usage.NewDiskProbe(".")
	// 容量门必须经槽引用：本回调在运行期才触发，而快照由 billing 阶段
	// （本阶段之后）写入。按值捕获 = 永远读到 nil（见 supplierSnapshotSlot）。
	retentionCfg.OnCapacityBlocked = capacityBlockedHook(d.supplierSnap)
	retention := usage.NewRetention(retentionCfg, repos, log)
	f.retention = retention
	// 路由观测写面守卫同源：同一份 observation_retention_days 交给分区仓，
	// UpsertFlowSnapshot 据此拒早于截止的快照（防 retention 删后重建）。
	repos.Partitions.SetRoutingObservationRetentionDays(cfg.Routing.ObservationRetentionDays)

	return f
}

// retentionConfigFrom 是 usage.RetentionConfig 的**配置投影**（唯一装配点）：
// 每个字段都从 cfg 直取，没有任何默认值/转换——这样「装配漏了一个字段」会由
// TestRetentionConfigFromProjectsEveryField 直接红，而不是等线上某张表永不清理。
//
// 运行时协作者（DiskProbe / OnCapacityBlocked / TickerInterval）不在这里：它们
// 不是配置，由调用方按装配阶段注入（本仓「配置」与「协作者」不混在同一张表）。
func retentionConfigFrom(cfg *config.Config) usage.RetentionConfig {
	return usage.RetentionConfig{
		LogRetentionDays:                cfg.Usage.LogRetentionDays,
		ErrLogRetentionDays:             cfg.Usage.ErrLogRetentionDays,
		AttemptRetentionDays:            cfg.Usage.AttemptRetentionDays,
		StatsRetentionDays:              cfg.Usage.StatsRetentionDays,
		RoutingObservationRetentionDays: cfg.Routing.ObservationRetentionDays,
	}
}
