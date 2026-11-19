// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 装配阶段 5/6：codex 面 + 请求路径（proxy）+ 补拉/重试/协调/同步 worker。
//
// 依赖 billing 阶段拿 svc/auth/clients/billHooks，依赖 health 阶段拿
// sched/rec/errlogW：这两侧的产物在本阶段**只读**（构造期一次给定，无 Set* 回填）。
package main

import (
	"context"
	"fmt"
	"math"
	"net/http"

	"github.com/redis/go-redis/v9"

	"github.com/is7qin/c3api/internal/config"
	"github.com/is7qin/c3api/internal/continuation"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/discovery"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/latch"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/pricing"
	"github.com/is7qin/c3api/internal/proxy"
	"github.com/is7qin/c3api/internal/quality"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/internal/service"
	"github.com/is7qin/c3api/internal/usage"
	"github.com/is7qin/c3api/pkg/aiclient"
	"github.com/is7qin/c3api/pkg/egress"
	"github.com/is7qin/c3api/pkg/logx"
)

// codexFace 是阶段 5 的产物。
// 编译期断言：路由编译触发面由 *scheduler.Scheduler 满足（P3 把 func 字段
// 还原成消费方接口后，赋值点不再有编译器帮助暴露签名漂移）。
var _ service.CompileTrigger = (*scheduler.Scheduler)(nil)

type codexFace struct {
	quotaResolvers *scheduler.QuotaResolvers
	quotaW         *scheduler.QuotaWorker

	contStore *continuation.Store
	contBindW *proxy.ContBindWorker
	convBook  *sdkbridge.Book
	convW     *sdkbridge.ConversationWorker
	codex     *sdkbridge.Codex
	bps       *sdkbridge.BPS

	effectiveInflight int64
	qualityRecorder   *quality.Recorder

	px          *proxy.Proxy
	retryWorker *sdkbridge.FailureRetryWorker
	probe       scheduler.ProbeFunc

	concSync    *proxy.ConcSyncWorker
	accConcSync *scheduler.AccConcSyncWorker

	pricingSync *pricing.SyncWorker
	qualitySync *quality.SyncWorker
}

// newCodexFace 装配阶段 5（依赖 infra / health / billing）。
// codexDeps 是阶段 5 实际读到的前四阶段字段（窄化，spec §7.2）。
//
// 18 个字段看着仍然宽，但它跨 infra/health/billing 三个来源：宽面时代这里是
// 「三个 Face 的并集」（约 40 个可触达字段），现在每一行都是本阶段真读到的。
type codexDeps struct {
	cfg           *config.Config
	bps           *sdkbridge.BPS
	log           *logx.Logger
	rdb           *redis.Client
	repos         *repository.Repository
	src           string
	pub           *notify.Publisher
	egress        *egress.Pool
	hc            *http.Client
	codexClients  *sdkbridge.ClientTable
	disco         *discovery.Discovery
	sched         *scheduler.Scheduler
	rec           *usage.Recorder
	errlogW       *usage.ErrLogWorker
	attemptlogW   *usage.AttemptLogWorker
	latchStore    *latch.LatchStore
	quotaQueue    *scheduler.QuotaQueue
	runtimeHealth *scheduler.RuntimeHealth
	auth          *proxy.Auth
	clients       *aiclient.Factory
	svc           *service.Service
	billHooks     *proxy.BillingHooks
}

// newCodexFace 装配阶段 5（只依赖 codexDeps）。
func newCodexFace(d codexDeps) *codexFace {
	cfg, log := d.cfg, d.log
	repos, rdb, src, pub := d.repos, d.rdb, d.src, d.pub
	egressPool, hc, codexClients := d.egress, d.hc, d.codexClients
	sched, rec, errlogW, latchStore := d.sched, d.rec, d.errlogW, d.latchStore
	attemptlogW := d.attemptlogW
	quotaQueue := d.quotaQueue
	auth, clients, svc := d.auth, d.clients, d.svc
	f := &codexFace{}

	// 429 额度补拉 resolver 注册表（设计 §5.4 / R3 第 5 条）：先建后填——
	// codex 适配器就绪后按两个凭据类型注册（仍在装配期，worker 尚未 Start）。
	quotaResolvers := scheduler.NewQuotaResolvers()
	f.quotaResolvers = quotaResolvers
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
	f.contStore = contStore
	// REST 流式异步绑定 worker：删闸门后首个响应 id 帧在写出接缝快照入队，
	// 本 worker 常驻批量落库（容量/批节奏内部默认；纳管由 managedWorkers 下方
	// 统一 wm.Register 负责 Start；反向排空顺序：contBindW 先于 errlogW/rec 关闭（无强依赖）。
	contBindW := proxy.NewContBindWorker(contStore, log, proxy.ContBindConfig{})
	f.contBindW = contBindW
	// Codex 会话簿（设计 §3）：带对话标识的 Codex 请求由它合成身份，跨副本经
	// SDK redisstore（codex:conv:* / codex:turn:*）共享；c3api 只注入 rdb，连接池
	// 生命周期归 redisx。恒构造（nil 只出现在测试 → 回到槽位路径）。
	convBook := sdkbridge.NewBook(rdb)
	f.convBook = convBook
	// 会话簿 worker（Name="codex-conversation"）：50ms 节拍 book.Flush 把轮次写
	// 批量落 Redis，停机预算内最后 Flush 一次；Stats 暴露 conv_*/shape_* 计数。
	// 纳管紧跟 contBindW（managedWorkers 下方统一 wm.Register）。
	convW := sdkbridge.NewConversationWorker(convBook, log, sdkbridge.ConversationWorkerConfig{
		LocalRejects: proxy.ConvLocalRejects,
	})
	f.convW = convW
	// codex SDK 适配层装配（§3——统一失效回调先落生图路径；全量）：
	// 适配层构造注册 WithOnAuthFatal → 统一回调 → 失效处理链（写 failed_at +
	// 调度摘除 + 审计契约）。egressPool/rotation 同构造期一次给齐（构造后
	// 不存在半装配形态）：出口池就是调度器那一个——**同一个池**才能让 codex
	// 面的 transport 与调度器解析的出口同源（`sel.Egress.NoCompression().Transport` 与
	// `Pool.For(spec)` 同源，spec-proxy-pool §4.6；Codex 是池内由底座派生的关闭
	// 自动压缩副本，与 HTTP 面同规格、各自一份连接池）；池的底座是 httpx 网关
	// 同形态的 client（SDK 默认 MaxIdleConnsPerHost=2 有压测连接风暴史；
	// Proxy=nil 直连防劫持），直连出口即那份底座。
	// rotation Upsert 部分更新（codex_oauth_token/refresh/expires_at 保旧）+
	// 回写后失效调度器 AccountExt 快照条目（下个会话重载新凭据）。
	// Latch/Publisher 必须装配：SDK 失效链据此走**围栏路径**（先锁存 → 以身份
	// 代际 K 为 guard 的 CAS 落库 → 组级 NOTIFY）；缺任一项即退化为只写 failed_at
	// 的简化路径——判决不再与"身份是否被授权变更"对齐，且对端只能靠周期兜底收敛。
	failureHandler := sdkbridge.NewFailureHandler(sdkbridge.FailureDeps{
		Store:     repos.Accounts,
		Failer:    sched,
		Log:       log,
		Latch:     latchStore,
		Publisher: schedGroupPub{pub},
	})
	codexAdapter := sdkbridge.NewCodex(failureHandler, egressPool, codexClients, sdkbridge.RotationDeps{
		Store:              repos.AccountExts,
		InvalidateSnapshot: sched.InvalidateAccount,
		Log:                log,
	})
	f.codex = codexAdapter
	// quality-sync lane（intelligent-routing）：500ms Redis 当前分钟绝对
	// 快照发布 + 5s PG quality/flow UPSERT。单实例一个串行 loop（worker.GoLoop
	// 监督），PG 写面直用 repos.Partitions（routing 分区表 absolute UPSERT+
	// dirty 同事务）。装配在 handler 之前：opsWorkers 聚合需要该引用已存在。
	effectiveInflight, err := config.EffectiveMaxInflight(cfg.Proxy.MaxInflight)
	if err != nil {
		fatalf("config: %v", err)
	}
	f.effectiveInflight = effectiveInflight
	qualityRecorder, err := quality.NewRecorder(effectiveInflight)
	if err != nil {
		fatalf("quality: %v", err)
	}
	f.qualityRecorder = qualityRecorder
	// 429 额度补拉 resolver 注册（设计 §5.4 / R3 第 5 条）：codex 两个凭据类型
	// （OAuth / PAT）共用同一个 SDK 适配器实例——usage 拉取走同一套凭据栈。
	// 注册在装配期（worker 未 Start）；未注册的凭据族由 worker 计 unsupported 跳过。
	quotaResolvers.Register(credential.TypeCodexOAuth, codexAdapter)
	quotaResolvers.Register(credential.TypeCodexPAT, codexAdapter)
	// BPS 适配器由 billing 阶段构造并注入（同一实例/同一 TokenAuth 供数据面与探测）。
	bpsAdapter := d.bps
	if bpsAdapter == nil {
		fatalf("codex: BPS adapter not wired")
	}
	// BPS has no trusted quota-window API. Register its independent no-window
	// resolver so 429 cooldowns never fabricate a Codex reset deadline.
	quotaResolvers.Register(credential.TypeBPS, sdkbridge.BPSNoWindowQuotaResolver{})
	// 补拉 worker 在此构造（sched 与 codex 适配器均已就绪）：账号解析面直接是
	// sched.ProbeAccount（与健康 probe 同一选号快照视图），resolver 注册也已完成。
	// 队列早已建好并被 sink 持有，此处只是交给消费者——仍早于 wm.StartAll，
	// 不含任何回填。
	quotaW := scheduler.NewQuotaWorker(quotaQueue, d.runtimeHealth, quotaResolvers, sched.ProbeAccount, log, scheduler.QuotaWorkerConfig{})
	f.quotaW = quotaW
	f.bps = bpsAdapter
	px := proxy.New(proxy.Config{
		MaxBodySize:           cfg.Proxy.MaxBodySize,
		UpstreamTimeout:       cfg.Proxy.UpstreamTimeout,
		UpstreamStreamTimeout: cfg.Proxy.UpstreamStreamTimeout,
		FailoverAttempts:      cfg.Proxy.FailoverAttempts,
		UsageCapture:          cfg.Proxy.UsageCapture,
		BillingCapture:        cfg.Billing.Enabled,
		BehindCDN:             cfg.Proxy.BehindCDN, // client_ip 供应商头识别开关（true = 采信供应商头，默认；false = 直取 RemoteAddr）
		// 低余额用户级并发钳制：cap 直通；阈值 USD → 毫分（×1e5 取整，与
		// convert.go 余额毫分单位一致）。
		LowBalanceConcCap:            cfg.Proxy.LowBalanceMaxConcurrency,
		LowBalanceConcThresholdMilli: int64(math.Round(cfg.Proxy.LowBalanceThresholdUSD * 1e5)),
		StreamKeepaliveInterval:      cfg.Proxy.StreamKeepaliveInterval,
	}, sched, credential.New(), rec, clients, auth, log, d.billHooks, errlogW, proxy.Deps{
		Codex:         codexAdapter,
		BPS:           bpsAdapter,
		Recorder:      qualityRecorder,
		Continuation:  contStore,
		ContBind:      contBindW,
		Conversations: convBook,
	})
	f.px = px
	// attempt_logs 投递面（spec 2026-11-18 retry-observability §3.2）：写点在
	// failover 循环里**无条件**触发，通道经 setter 注入而不是加 New 的参数（那会
	// 污染几十个测试调用点）。typed nil 必须显式判：nil 具体指针装进接口是
	// **非 nil 接口**，写点会调进 nil 接收者（与 supplierViewLoader 同款陷阱）。
	if attemptlogW != nil {
		px.SetAttemptLogs(attemptlogW)
	}
	// 规则 typed Throttle/FailAccount 双面已在构造期接线（latch fail-closed
	// 先于持久化；持久化走有界 persist queue——满可丢、写失败可弃、四指标可观测，
	// rule best-effort 契约，无 outbox）。接线唯一性由 rule_wiring_test.go 钉死。
	// SDK fatal 重试 worker 纳管（blocker：旧态惰性起循环且进程退出前永不
	// join）：managed lifecycle——Start 预起 supervised 循环，Close 先于 Redis
	// 释放 join；重试语义（backoff/fencing/进程存活期重试）原样保留。
	retryWorker := sdkbridge.NewFailureRetryWorker(log)
	f.retryWorker = retryWorker
	// 真实健康 probe 构造（blocker：构造期曾传 nil probe——所有 PROBING 记录
	// 30s TTL 后恒失败，恢复流程永远到不了 READY）。探测权威 = scheduler 选号
	// 快照（与选号门同一视图，revision fence fail-closed）；codex 凭据走 SDK
	// 适配器 usage 快照路径（fatal 权威保持）；api_key 族无合成探测面（owner
	// 裁决：/v1/models 与流量无关，已删除——探测视为通过，恢复由时间窗+真实
	// 流量判定）。超时同上游请求预算。probe 经 healthWorker 在 Start 期一次性
	// 交给 runtimeHealth（Start 前记录 fail-closed 停在 OPEN/PROBING，绝不 READY）。
	probe := newHealthProber(sched.ProbeAccount, sched.Runtime, codexAdapter, cfg.Proxy.UpstreamTimeout, func(ctx context.Context, acct *domain.Account) error {
		cred := domain.CredentialFromExt(acct.Ext)
		cred.AccountID = acct.ID
		cred.IdentityRevision = acct.IdentityRevision
		if acct.Template == nil || acct.Template.CredentialType != credential.TypeBPS || acct.Ext == nil {
			return fmt.Errorf("BPS credential unavailable")
		}
		eg := egressPool.Direct()
		if acct.Proxy != nil {
			eg = egressPool.For(acct.Proxy.EgressSpec())
		}
		return bpsAdapter.Probe(ctx, cred, eg)
	})
	f.probe = probe
	// 多实例集群 N 注入（discovery 接管，consumer spec §2.2）：gate 预算
	// ceil(剩余/N) + limit RPM ceil(rpm/N)。N = Redis 心跳活体数（disco 实时读
	// atomic），gate/limit 在每次预算分配时现读 provider（gate.go:106-121），
	// 心跳计数变化 ≤1 tick 天然生效，无需任何 reload 触发。
	px.SetInstancesProvider(d.disco)
	// scheduler 选号侧同一 N 源（spec conc-share-borrow-account §1.1）：账号并发
	// 份额除数 = Redis 心跳活体数，ReserveAttempt 入口现读，心跳变化 ≤1 tick 生效。
	sched.SetInstancesProvider(d.disco)
	// 并发门跨实例共识 worker（spec conc-share-borrow-gate §1.5）：500ms 一条
	// pipeline 双向同步受限层级在途 → gate 第二快照（clusterView），请求路径
	// 零 Redis；nil/未启动 = 无视图 = 全额本地语义。self 与 NOTIFY Src /
	// discovery 同源（同一 instanceSrc 产物，不自造第二套 ID）。
	concSync := proxy.NewConcSyncWorker(auth, rdb, src, log)
	f.concSync = concSync
	// 账号并发份额+借用跨实例共识 worker（spec conc-share-borrow-account §2）：
	// 协议孪生 conc-sync，命名空间 c3api:conc:a:*——500ms 一条 pipeline 双向同步
	// 账号在途 → Scheduler.concView，选号热路径零 Redis。src 复用同源产物。
	accConcSync := scheduler.NewConcSyncWorker(sched, rdb, src, log)
	f.accConcSync = accConcSync
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
	f.pricingSync = pricingSync
	// instanceSrc 与 discovery/conc-sync 同源产物（不自造第二套 ID）：跨实例
	// merge 按 instance_src 区分，同源身份是 merge 正确性的前提。
	// 缺陷 B 质量面：PG 落库边界成功持久新质量行 → 事件驱动编译（非阻塞、
	// 下游去抖收敛；空刷/失败静默，无定周全量）。构造器注入（nil = 未装配）；
	// 价格面见 pricingSync.Reload（service 内经 Deps.CompileNotify
	// 变化门控后通知编译）。
	qualitySync := quality.NewSyncWorker(qualityRecorder, rdb, repos.Partitions, quality.SyncConfig{InstanceSrc: src}, log, sched.RequestCompile)
	f.qualitySync = qualitySync

	return f
}
