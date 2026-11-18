// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 装配阶段 1/6：基础设施面（Redis / PG / repository / NOTIFY 发布器 / 实例发现 /
// 上游 HTTP 底座 / 出口池 / codex 传输资源表 / settings 快照）。
//
// 依赖规则（cmd/server/wiring_*.go 共通）：每个阶段构造函数只接受**更早阶段** +
// cfg/log；阶段的产物在函数体里用与旧 main 同名的局部变量装配（语句级零改动），
// 出口统一收进阶段 struct。唯一例外是 infraFace.supplierSnap（billing 阶段写入、
// retention 回调运行期读，见字段注释）。
package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"net/http"
	"os"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"

	"github.com/is7qin/c3api/internal/config"
	"github.com/is7qin/c3api/internal/db/migrate"
	"github.com/is7qin/c3api/internal/discovery"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/internal/settingssnap"
	"github.com/is7qin/c3api/internal/supplier"
	"github.com/is7qin/c3api/pkg/egress"
	"github.com/is7qin/c3api/pkg/httpx"
	"github.com/is7qin/c3api/pkg/logx"
	"github.com/is7qin/c3api/pkg/redisx"
)

// partitionHorizon 是分区预建窗口（spec §2.7）：`EnsureAllTablePartitions(now, now+24h)`。
//
// 两个调用点（`-migrate up` 退出前、正常启动在写 worker 之前）必须用同一个值：
// 窗口一边 24h 一边 48h 不会报错，只会在某个日界让其中一条路径漏建分区。
const partitionHorizon = 24 * time.Hour

// infraFace 是阶段 1 的产物：所有"进程级基础设施句柄"。后续每个阶段都只从它取
// 连接/仓库/发布器，不自己 Open 第二份（redisx.Open 全仓唯一构造点）。
type infraFace struct {
	cfg *config.Config
	log *logx.Logger

	// svcLoc = server.time_zone 解析结果（nil = 进程本地时区）。定价/规则时间条件
	// 用的那个 location，与统计读取时区无关（后者是请求级参数）。
	svcLoc *time.Location

	rdb   *redis.Client
	pool  *pgxpool.Pool
	repos *repository.Repository

	// startupCtx 是启动期 DB 操作的统一 30s 预算；cancelStartup 由 main 在同层
	// defer（defer 不下移到阶段构造函数：阶段返回后连接仍被各阶段引用）。
	startupCtx    context.Context
	cancelStartup context.CancelFunc

	// src = 实例 ID（hostname-pid-nonce）：NOTIFY Src、discovery self、conc-sync、
	// quality-sync 同源复用，不自造第二套 ID。
	src   string
	pub   *notify.Publisher
	disco *discovery.Discovery

	hc           *http.Client
	egress       *egress.Pool
	codexClients *sdkbridge.ClientTable

	settingsSnap *settingssnap.Snapshot

	// schema 是启动门（spec §2.5/§2.6）：能不能服务、水位是多少。main 用它在
	// 构建业务图之前分叉；server 的 /readyz 与业务面 503 都读它。
	schema *schemaGate

	// supplierSnap 是**唯一允许的前向槽**（spec §2.2）：由 billing 阶段在
	// supplier.enabled 分支写入，health 阶段的 retention 容量处置回调在**运行期**
	// 读；写入（装配期，同 goroutine）早于 wm.StartAll，因此读者到 Start 之后才可能
	// 观察到非 nil。除它之外任何"后阶段产物"都不得晚绑定。
	supplierSnap *supplier.SupplierSnapshot
}

// supplierSnapshotSlot 是前向槽的**引用载体**：保存字段地址，而不是字段值。
//
// 为什么需要它：health 阶段注册的回调在**运行期**才读这个槽，而值由后一步的
// billing 阶段写入。若把 `in.supplierSnap` 按值拷进窄结构（窄化的默认做法），
// 拷贝发生在 billing 之前，快照永远是 nil——磁盘满是保护会静默失效，编译器、
// 单测、diff review 都看不出来。槽保存 `**T`，写入与读取落到同一个字段上。
//
// 同理，窄结构里不允许出现 `*infraFace` 本身：那会把整个宽面重新暴露给
// health 阶段，抵消窄化的目的。
type supplierSnapshotSlot struct{ p **supplier.SupplierSnapshot }

func (s supplierSnapshotSlot) get() *supplier.SupplierSnapshot {
	if s.p == nil {
		return nil // 槽未接线：与「billing 尚未写入」同语义
	}
	return *s.p
}

func (s supplierSnapshotSlot) set(v *supplier.SupplierSnapshot) {
	if s.p != nil {
		*s.p = v
	}
}

// capacityBlockedHook 把「磁盘满是 ⇒ 停供应商新流量」的回调与槽解耦，便于单测：
// 装配期 health 先注册、billing 后写入，回调必须在**调用时**才解引用。
func capacityBlockedHook(slot supplierSnapshotSlot) func(bool) {
	return func(blocked bool) {
		if snap := slot.get(); snap != nil {
			snap.SetCapacityBlocked(blocked)
		}
	}
}

// newInfra 装配阶段 1（依赖 cfg/log）。
func newInfra(cfg *config.Config, log *logx.Logger) *infraFace {
	f := &infraFace{cfg: cfg, log: log}

	// 必填校验（auth.jwt_secret/db.dsn/redis.addr）已内聚到 config.Load，
	// 此处只做错误处理。
	// server.time_zone 仅服务定价/规则时间条件（pricing 保持既有 nil/进程本地
	// 回落语义）——统计读取时区是请求级参数（浏览器 IANA 名，handler 边界解析），
	// 绝不从此配置派生任何进程级统计时区（request-browser-timezone-stats）。
	if cfg.Server.TimeZone != "" {
		l, err := time.LoadLocation(cfg.Server.TimeZone)
		if err != nil {
			fatalf("server.time_zone: invalid IANA timezone %q: %v", cfg.Server.TimeZone, err)
		}
		f.svcLoc = l
	}

	// Redis 必选依赖（foundation spec 2026-08-25-redis-foundation-design §2.3）：
	// config.Load 之后立即构造（addr 缺失已在 Load fatal；Ping 失败此处 fatal——
	// 无"未启用"分支，消费方零 nil 容忍）。全仓唯一构造点 redisx.Open；运行期
	// 连接丢失 ≠ 启动失败（连接池自带重连，降级语义由 discovery 定义）。
	rdb, err := redisx.Open(redisx.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: cfg.Redis.DB})
	if err != nil {
		fatalf("redis: %v", err)
	}
	f.rdb = rdb

	// 启动期 DB 操作统一 30s 预算（OpenPG + ent migrate + 三分区 bootstrap）：
	// 超时/失败经 fatalDB 明确文案（"db bootstrap timed out after 30s" 可归因）。
	f.startupCtx, f.cancelStartup = context.WithTimeout(context.Background(), 30*time.Second)
	startupCtx := f.startupCtx

	pool, err := repository.OpenPG(startupCtx, cfg.DB.DSN, int32(cfg.DB.MaxConns))
	if err != nil {
		fatalDB("db", err)
	}
	f.pool = pool
	// ent v0.14.6 的 entsql.OpenDB 只接受 *sql.DB：pgxpool 经 pgx/stdlib 桥接（用户决策 2026-08-05）
	db := stdlib.OpenDBFromPool(pool)
	drv := entsql.OpenDB(dialect.Postgres, db)
	// **启动不再建表**（spec §1 承诺 1）：schema 只归版本化迁移管，app 进程连
	// 「顺手补齐」都没有——建表真相多一处，`-migrate up` 就不再是唯一的升级动作。
	// 需要建表的人去跑 `-migrate up`；本进程只读水位。
	repos, err := repository.NewWithoutMigrate(startupCtx, drv, pool) // pool 供 Stats.Upsert COPY 两阶段批量写与 Billing 结算语句直连事务 + 会话锁专用连接（ledger-cursor）
	if err != nil {
		fatalDB("repository", err)
	}
	f.repos = repos

	// 版本门（spec §2.5）：先算水位，再决定「这个进程能不能服务这个库」。
	// 迁移永不在这里执行——`up` 只是显式命令（§2.10/§2.14）。
	verdict, err := migrate.Verify(startupCtx, db)
	if err != nil {
		fatalDB("schema verify", err)
	}
	gate := newSchemaGate(verdict)
	f.schema = gate
	if !gate.ready() {
		// 不 fatal：进程要活着，由 /readyz 摘流（§2.6）。真正的收口在 main：
		// 判定为不可服务时连后续阶段图都不构建，避免启动期 DB 读把进程打死。
		log.Warn("schema gate: not serviceable by this binary",
			logx.String("reason", gate.decision.Reason),
			logx.Int64("db_head", verdict.DBHead),
			logx.Int64("db_contract", verdict.DBContract),
			logx.Int64("binary_head", verdict.BinaryHead),
			logx.Int64("binary_required", migrate.RequiredVersion),
		)
	} else {
		for _, warning := range gate.decision.Warnings {
			log.Warn("schema gate: serving on the expand-only guarantee", logx.String("detail", warning))
		}
		// 分区补齐（spec §2.7）：**唯一入口**，且必须早于任何写路径（worker/快照/
		// 种子）。父表由 0001 建立；父表缺失 ⇒ 报「run `c3api -migrate up`」，
		// 不再由启动建表。
		//
		// 统计桶界时区不在装配面：持久化恒规范 UTC，浏览器时区逐请求解析注入
		// （handler → service → repository 方法参数），无进程级可变状态。
		now := time.Now()
		if err := repos.EnsureAllTablePartitions(startupCtx, now, now.Add(partitionHorizon)); err != nil {
			fatalDB("table partitions", err)
		}
		// 下面两条仍是启动期 bootstrap（spec §3「本批不做」把它们收敛进迁移）：
		// price_variants 的 effect CHECK 与 codex-search 按次价种子。
		if err := repos.EnsurePriceVariantsEffectCheck(startupCtx); err != nil {
			fatalDB("price_variants effect check bootstrap", err)
		}
		if err := repos.EnsureCodexSearchSeed(startupCtx); err != nil {
			fatalDB("codex-search price seed bootstrap", err)
		}
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
	f.src = src
	pub := notify.NewPublisher(pool, src, log)
	f.pub = pub
	// 实例发现（consumer spec 2026-08-25-redis-instance-discovery-design §2.2）：
	// ZSET 心跳成员协议，活体计数即集群 N——替换手工 cluster.instances 设置。
	// self 与 NOTIFY Src 同源（hostname-pid-nonce 生成模式复用）。
	disco := discovery.New(rdb, src, log)
	f.disco = disco

	// settings 快照提升为一等组件（根因重开）：main 先构造单个共享
	// *settingssnap.Snapshot（首载失败仅 Warn，由 Snapshot.Load 调用方保持
	// fail-safe），mailW 与 svc 同源共享该指针（NOTIFY 只刷一处，无分叉）；
	// mailW.Enqueue 经 Deps.MailEnqueue 一次注入，零 Set* 回填。
	settingsSnap := settingssnap.New(repos, log)
	if gate.ready() {
		if err := settingsSnap.Load(context.Background()); err != nil {
			log.Warn("settings snapshot initial load failed", logx.Error(err))
		}
	} else {
		// 库都不可用时首载只会失败：跳过，别把"schema 不兼容"伪装成"settings 读失败"。
		log.Warn("settings snapshot initial load skipped: schema gate is not ready")
	}
	f.settingsSnap = settingsSnap

	// 上游 client 与出口池**共用同一个底座**（spec-proxy-pool §4.4/§4.2）：直连
	// 与"经代理"只在 transport 的 Proxy 一项上不同，连接参数/超时/HTTP2 全部同源。
	// 提到 sched 之前是因为 scheduler.Config 要拿这个池。
	hc := httpx.NewClient(httpx.TransportConfig{
		MaxIdleConns:        cfg.Upstream.MaxIdleConns,
		MaxIdleConnsPerHost: cfg.Upstream.MaxIdleConnsPerHost,
		IdleConnTimeout:     cfg.Upstream.IdleConnTimeout,
		DialTimeout:         cfg.Upstream.DialTimeout,
		ForceHTTP2:          cfg.Upstream.ForceHTTP2,
		HTTP2Shards:         cfg.Upstream.HTTP2Shards,
		// Proxy 显式直连（防劫持）：HTTP_PROXY 环境变量不再静默改道
		// 上游请求（含 x-api-key/Authorization 凭据，WS 升级大概率失败）；
		// 压测行为不随部署环境漂移。
		Proxy: nil,
	})
	f.hc = hc
	// 分片依赖 h2：force_http2=false 时 h1 每请求独占连接，本来就没有共享连接窗，
	// httpx.NewClient 会退回单 transport（不报错），此处只留一条启动日志说明
	// "http2_shards 没生效"，否则运维会以为已打开。
	if cfg.Upstream.HTTP2Shards > 1 && !cfg.Upstream.ForceHTTP2 {
		log.Warn("upstream.http2_shards ignored: force_http2=false",
			logx.Int("http2_shards", cfg.Upstream.HTTP2Shards))
	}
	egressPool := egress.NewPool(hc)
	f.egress = egressPool
	// codex 传输资源表（账号 → 条目 + 出口×传输 的 client）：**先于调度器构造**
	// 的独立组件，同一个实例分别交给 scheduler（线程退役 / 账号移除）与适配层。
	// 这样两侧是单向依赖，不存在"晚绑定闭包回填一个后构造的指针"（AGENTS.md
	// 「禁止新增事后回填」）。
	codexClients := sdkbridge.NewClientTable()
	f.codexClients = codexClients

	return f
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
