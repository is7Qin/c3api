// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 装配阶段 4/6：鉴权面 / 失效去抖器 / 供应商持久面 / 支付注册表 / service /
// 快照注册表 / NOTIFY 监听 / 计费装配。
//
// 这里持有 billing 关闭时的全部 nil 语义态（billBalances / billFlusher /
// billHooks / warningW / supplierViewLoader）：`nil = 未启用`，消费方
// （px、run 的 BillingAlerts 回调、snapReg）只在**调用时**读字段，不在注册时
// 抄一份副本——否则装配顺序一变就静默失效。
package main

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/config"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/invalidate"
	"github.com/is7qin/c3api/internal/notification"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/paymentregistry"
	accountprobe "github.com/is7qin/c3api/internal/probe"
	"github.com/is7qin/c3api/internal/proxy"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/internal/service"
	"github.com/is7qin/c3api/internal/snapshot"
	"github.com/is7qin/c3api/internal/supplier"
	"github.com/is7qin/c3api/internal/verification"
	"github.com/is7qin/c3api/pkg/aiclient"
	"github.com/is7qin/c3api/pkg/logx"
)

// billingFace 是阶段 4 的产物。
// 编译期断言：三个消费方接口由生产实现满足。放在这里而不是实现包里——
// 依赖方向是「实现方不必知道消费方」（service 定义接口，scheduler /
// MailWorker / Cooldown 各自实现），所以断言只能在装配点。
var (
	_ service.MailEnqueuer    = (*service.MailWorker)(nil)
	_ service.CooldownClearer = (*notification.Cooldown)(nil)
)

type billingFace struct {
	auth    *proxy.Auth
	clients *aiclient.Factory

	// 计费三件套：billing.enabled=false 时全部保持 nil（与旧分支一致）。
	billBalances *billing.Balances
	billFlusher  *billing.Flusher
	billHooks    *proxy.BillingHooks
	// invBalances 接口声明而非具体类型：billing 关闭时保持 nil 接口（非 typed
	// nil）。若用 *billing.Balances 声明，关闭时接口 = (*Balances)(nil)，类型部分
	// 非 nil → invalidate.reloadAll 的 `!= nil` 检查判 TRUE → Reload 调用 nil
	// receiver panic（2026-08-10 管理端建用户实证：Debouncer.loop panic）。
	invBalances invalidate.BalancesReloader

	inv *invalidate.Debouncer

	suppRepo           *repository.SupplierRepo
	supplierViewLoader *supplier.ViewLoader
	paymentReg         *domain.ProviderRegistry

	svc      *service.Service
	snapReg  *snapshot.Registry
	disp     *dispatcher
	listener *notify.Listener
	authSync *authSync

	warningW *notification.Worker
}

// newBillingFace 装配阶段 4（依赖 infra / health / notify）。
func newBillingFace(cfg *config.Config, log *logx.Logger, in *infraFace, hl *healthFace, nf *notifyFace) *billingFace {
	repos, rdb, src, pub := in.repos, in.rdb, in.src, in.pub
	hc, egressPool := in.hc, in.egress
	settingsSnap, svcLoc := in.settingsSnap, in.svcLoc
	sched, ruleEngine := hl.sched, hl.ruleEngine
	runtimeHealth, latchStore := hl.runtimeHealth, hl.latchStore
	bwCooldown, mailW := nf.cooldown, nf.mailW
	f := &billingFace{}

	auth := proxy.NewAuth(repos.Keys, repos.Users, repos.ManagementKeys, log, cfg.Billing.Enabled)
	f.auth = auth
	clients := aiclient.NewFactory(hc, aiclient.Config{
		UpstreamTimeout:       cfg.Proxy.UpstreamTimeout,
		UpstreamStreamTimeout: cfg.Proxy.UpstreamStreamTimeout,
	})
	f.clients = clients
	// 单账号单模型探测执行器（account-model-test §5.1）：与 HTTP 面共用同一个
	// aiclient Factory 与出口池。准入（4 执行 + 16 等待）与 30s 总期限在 service
	// 内；本构造只传执行依赖。探测不落库、不计费、不进调度/规则/失败统计。
	accountProber := accountprobe.New(clients, egressPool)
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
	if cfg.Billing.Enabled {
		// loader = Repository 门面（BalanceLoader：余额 → Users，组倍率 +
		// assignment 专属倍率 → Groups，修正按组）。
		billBalances := billing.NewBalances(repos, log)
		f.billBalances = billBalances
		f.invBalances = billBalances
		// 首载不在此（fail-safe 语义由注册表 ReloadAll 承担：错误独立 Warn 保留
		// 空快照 → 预检全 402 拒绝，安全侧）——单一启动入口，消灭双重加载。
	}
	inv := invalidate.New(invalidate.Config{
		Window:   invalidate.DefaultWindow, // 200ms（生效延迟语义见 invalidate 包注释）
		Sched:    sched,
		Clients:  clients,
		Auth:     auth,
		Balances: f.invBalances, // billing.enabled=false → nil 接口（flush 跳过余额路径）
		Rules:    ruleEngine,    // 规则 CRUD → 全实例规则表重载（ruleEngine 先于 invalidate 构造）
		Log:      log,
	})
	f.inv = inv
	// ruleReload 独立于 invalidate：规则 CRUD 后全量重载（重载会重置窗口计数，
	// 不能随模板/账号/分组等任意资源变更触发）。
	// svc 一次性装配（定价时区/原始行 horizon/验证码存储/预警清理/
	// recover→PROBING 全部构造参数；路由编译触发 CompileNotify 同步
	// 构造注入（sched 先于 svc 存在，func 值无 import 环）——空时区配置 = nil
	// → 进程本地，语义不变；验证码 Redis 必选 ⇒ 无 nil 分支，误接线 New 内 panic）。
	// 供应商持久面 + 财务视图装载器（spec 2026-10-09 §4.3/§4.6/§6.4）：持久面
	// **无条件**构造（enabled=false 时管理面余额/结算单列表仍须可用——余额与结算单
	// 永久保留，§2.7）；财务视图装载器仅在 enabled=true 构造，并经 Service.Deps
	// .SupplierViewReload 注入账号归属/启用写面——写成功后触发本地 Reload，先让
	// 旧归属停止入选再换归属（§4.6.3 发布屏障）。首刷 + ticker 在 px/sched 装配后接线。
	suppRepo := repos.SupplierRepo(repository.SupplierRepoConfig{
		GranularitySeconds:   int64(cfg.Supplier.ThawGranularity / time.Second),
		FreezeEnabled:        cfg.Supplier.FreezeEnabled,
		FreezeHoursDefault:   cfg.Supplier.FreezeHours,
		ShareBpDefault:       cfg.Supplier.ShareBpDefault,
		BillingEnabled:       cfg.Billing.Enabled,
		PayoutMaxBacklogRows: cfg.Supplier.PayoutMaxBacklogRows,
		PayoutMaxBacklogAge:  cfg.Supplier.PayoutMaxBacklogAge,
		PayoutMaxObserveAge:  cfg.Supplier.PayoutMaxObserveAge,
		RiskReviewMaxAge:     cfg.Supplier.RiskReviewMaxAge,
	})
	f.suppRepo = suppRepo
	if !cfg.Supplier.Enabled {
		// enabled=false 时停用 liability 校验（I6/A23，§5.5）：残留冻结/在途单/
		// available 任一非零 ⇒ 拒绝启动并打印分项规模。bootstrap 后、接流量前执行。
		initCtx, initCancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := validateSupplierDisabled(initCtx, suppRepo); err != nil {
			initCancel()
			fatalf("supplier disabled startup validation: %v", err)
		}
		initCancel()
	} else {
		// A16⑦ 启动校验：override 集合按全局 g 重算上界，越界拒绝启动并列出 uid。
		initCtx, initCancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := validateSupplierOverrides(initCtx, suppRepo, cfg.Supplier.ThawGranularity); err != nil {
			initCancel()
			fatalf("supplier startup validation: %v", err)
		}
		initCancel()
		// 前向槽写入点（唯一允许的那一个）：retention 的容量处置回调在运行期读
		// infraFace.supplierSnap；本写入发生在装配期、早于 wm.StartAll。
		in.supplierSnap = supplier.NewSupplierSnapshot(supplierViewStaleThreshold)
		supplierViewLoader := supplier.NewViewLoader(supplier.ViewConfig{Interval: cfg.Billing.BalanceRefreshInterval}, suppRepo, in.supplierSnap, log)
		f.supplierViewLoader = supplierViewLoader
		// 三态观测注入（typed observation；§4.4/A13④）——否则「功能开了但从未装载
		// 成功」在 ops 面不可见。
		supplierViewLoader.SetObsProvider(func(now time.Time) supplier.SupplierSnapshotObs {
			return in.supplierSnap.Obs(now)
		})
	}
	// 支付协议注册表（spec 支付网关 v2 §4，N53–N55）：清单**只增不减**——下线协议
	// 也必须保留实现与公私钥才能对历史订单 sync、给迟到回调验签（账户 enabled=false
	// 只影响可售性），故这里无条件构造。协议实现**无状态**：凭据在
	// payment_accounts.config（每账户一份），改密钥/汇率走管理面账户编辑
	// （config_revision++ → 配置缓存失效）即时生效——因此**不传 settings 快照**。
	// HTTP 客户端 10s 超时：查单是管理面同步动作，不能让上游挂死请求 goroutine。
	// 注册表装配错误（nil 元素/空名/重名）必须在启动期炸掉，不能把 nil 交给 service。
	paymentHTTP := &http.Client{Timeout: 10 * time.Second}
	paymentReg, err := paymentregistry.Registry(paymentHTTP)
	if err != nil {
		fatalf("payment registry: %v", err)
	}
	f.paymentReg = paymentReg
	// Deps 先落到具名变量（不是内联字面量）：Validate 要能在 New 之前拿到可读错误，
	// Features 要能打一行「这次开了什么」。这两行的顺序是契约——先校验再构造。
	deps := service.Deps{Store: repos, Scheduler: sched, Invalidate: inv, Publisher: pub, RuleReload: ruleEngine, Auth: auth, Log: log, EmailCodeStore: verification.New(rdb),
		// codex 设备码登录：无服务端会话（发起返回凭据、完成时一次性轮询落库），
		// 因此只需协议面装配，无 Redis 依赖。
		CodexLogin:         sdkbridge.NewCodexLogin(),
		Prober:             accountProber,
		SupplierViewReload: supplierViewReloader(f.supplierViewLoader),
		TimeLocation:       svcLoc,
		// Retention 三表原件（不预先折 min：读 N 张表取最保守 floor 是
		// domain.StatsKinds 的 Tables 推论，spec §4.5）。
		Retention: domain.Retention{
			Log:    cfg.Usage.LogRetentionDays,
			ErrLog: cfg.Usage.ErrLogRetentionDays,
			Stats:  cfg.Usage.StatsRetentionDays,
		},
		ClearBalanceWarningCooldown:     bwCooldown,
		RecoverProber:                   runtimeHealth,
		RecoverLatch:                    latchStore,
		RecoverHealthClear:              runtimeHealth,
		DefaultMaxConcurrency:           cfg.Scheduler.DefaultMaxConcurrency,
		RoutingObservationRetentionDays: cfg.Routing.ObservationRetentionDays,
		SupplierThawGranularity:         cfg.Supplier.ThawGranularity,
		CompileNotify:                   sched,
		MailEnqueue:                     mailW,
		PaymentRegistry:                 paymentReg,
		SettingsSnapshot:                settingsSnap,
	}
	if err := deps.Validate(); err != nil {
		fatalf("service deps: %v", err)
	}
	log.Info("service assembly", logx.String("features", strings.Join(deps.Features(), ",")))
	svc := service.New(deps)
	f.svc = svc
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
	f.snapReg = snapReg
	if f.billBalances != nil { // 与 invBalances 同纪律：billing 关闭不注册
		if err := snapReg.Register(balanceSnapshot{f.billBalances}); err != nil {
			fatalf("snapshot register: %v", err)
		}
	}
	// 供应商财务视图快照登记（enabled=true 才构造）：使视图三态
	// （loaded/revision/last_success/stale_age，§4.4/A13④）进入快照注册表——
	// /ops snapshots 段呈现 LastReload/LastError（NotReady = 失败轮错误，保留
	// 旧视图）；workers 段呈现 loader 计数 + Obs（见 supplierViewLoader）。
	if f.supplierViewLoader != nil {
		if err := snapReg.Register(supplierViewSnapshot{loader: f.supplierViewLoader}); err != nil {
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
	f.disp = disp
	// NOTIFY 监听 worker（Name="notify"）：独立 pgx 连接 LISTEN c3api_invalidate；
	// 断线指数退避重连 + 重连即全量刷新（R8）；Src 跳过自播（省重复 reload）。
	listener := notify.NewListener(notify.ListenerConfig{
		DSN:        cfg.DB.DSN,
		Src:        src,
		Dispatcher: disp,
		Log:        log,
	})
	f.listener = listener
	// 60s 周期鉴权快照兜底（NOTIFY 丢失/断连期间 key 与用户变更最长 60s
	// 收敛；现状 auth 无周期 reload，是兜底缺口——auth-sync worker 补位。
	// 在其 Reload 内接入 N 与预算重分配，本 worker 侧无需再改）。
	authSync := newAuthSync(auth, 0, log)
	f.authSync = authSync
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
		f.warningW = wireBalanceWarning(bwCooldown, svc, mailW, log)
		f.billFlusher = billing.NewFlusher(billing.FlushConfig{
			FlushInterval:          cfg.Billing.FlushInterval,
			BalanceRefreshInterval: cfg.Billing.BalanceRefreshInterval,
			LogRetentionDays:       cfg.Usage.LogRetentionDays,
		}, repos, f.billBalances, log, f.warningW)
		f.billHooks = &proxy.BillingHooks{
			Resolver:   svc,
			Balances:   f.billBalances,
			Flusher:    f.billFlusher,
			TierPolicy: svc.ServiceTierPolicy,
		}
	}

	return f
}
