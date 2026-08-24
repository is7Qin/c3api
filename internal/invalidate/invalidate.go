// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package invalidate 管理面变更的去抖定向失效（Phase 6 O2）：
//
// 脏标记（Mark 路径零锁零 DB——atomic CAS 合并 + 非阻塞唤醒，不阻塞任何
// 调用方；消除 Phase 6 压测实证的 33,705 goroutine reloadMu 串行雪崩）+
// 定时窗口到点批量执行一次合并重载；后沿语义（评审 C-6：执行完成后若又脏
// 立即再执行，不按固定间隔 throttle——不与长 reload 重叠）。
//
// 接线矩阵（评审 M-1 定稿，reloadAll 实现）：
//   - 用户 CRUD（含创建）/余额变更 → auth + 余额快照全量（新用户必须即刻在
//     快照——评审 M-2，防 ≤10s 402 窗口，回归测试 tools/e2e）
//   - 模板（base_url/models/映射）→ sched 全量 + clients 失效（base_url
//     变更需按新地址重建 SDK 客户端）
//   - 账号 → sched 组级定向 InvalidateGroup（包含关系：full ⊇ 组级 ⊇ 无）
//   - 组倍率 / 用户-组专属倍率 → 余额倍率快照定向刷新（EffectiveMultiplier
//     陈旧 ≤10s 不可接受）
//   - key CRUD（#14 多实例 key 缺口）→ auth 快照全量 Reload（v1 不做增量
//     定向——单实例 auth 增量 Upsert/Delete 语义不变，多实例需全量覆盖其余
//     实例的陈旧快照）
//   - settings 变更（UpdateSetting）→ settings 快照重载（service 实现，
//     cfg.Settings 未注入则跳过）
//   - 规则 CRUD → 规则表全量重载（重载清窗口计数，全实例同步执行语义）
//   - pricing → 现状（内部 reloadPricing，不进 invalidate）
//
// 读端永不阻塞：重载在单 goroutine 串行执行；各实体快照原子替换由实体自身
// 保证（scheduler snapshotStore / Balances atomic.Pointer / Auth RWMutex 换
// 整体），本包只在变更侧去抖合并。
package invalidate

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/logx"
)

// Kind 是需要"全量重载"的脏位；纯组级定向不占位（走 State.Groups）。
type Kind uint8

const (
	// KindUsers 用户 CRUD（含创建）/余额变更：auth + 余额快照全量 Reload。
	KindUsers Kind = 1 << iota
	// KindTemplates 模板（base_url/models/映射）变更：sched 全量 + clients 失效。
	KindTemplates
	// KindClients aiclient 工厂失效（模板 base_url / 账号 upstream_key 变更）。
	KindClients
	// KindMultipliers 组倍率 / 用户-组专属倍率（price_multiplier）变更：余额
	// 倍率快照定向刷新。
	KindMultipliers
	// KindKeys key CRUD（创建/轮换/删除/改额度，#14 多实例 key 缺口）：auth
	// 快照全量 Reload（v1 不做增量定向）。
	KindKeys
	// KindSettings settings 快照变更：settings 快照全量重载。生产侧死分支
	// （仅测试引用）：dispatcher 的 settings 分支已改同步 ReloadSettings + 注册
	// 表 scope 精确重载（#36 时序），UpdateSetting 从未经去抖器 Mark（本地直连
	// Apply + NOTIFY 广播）。保留防御性——reloadAll 分支仍接线，未来若恢复
	// Mark 路径可复用。
	KindSettings
	// KindRules 规则表变更（规则 CRUD）：规则表全量重载（重载清窗口计数，
	// 全实例同步执行语义）。
	KindRules
)

// State 一次到点执行的合并脏集合（同窗口多实体变更并集）。
// 包含关系：full（KindTemplates 的 sched 全量）⊇ 组级定向 ⊇ 无——模板变更
// 与账号组变更同窗口时跳过组级重载（reloadAll）。
type State struct {
	Kinds  Kind
	Groups map[int64]struct{} // 组级定向（账号变更的受影响组）
}

// DefaultWindow 去抖窗口。生效延迟语义：管理面变更在 ≤200ms 窗口到点后执行
// 一次合并重载（总延迟 = 窗口 + 一次重载时长；评审 M-1/M-2 定稿 ≤200ms 可
// 接受——新用户 402 窗口回归测试对"建用户 → <0.5s 请求 → 200"做硬断言）。
const DefaultWindow = 200 * time.Millisecond

// SchedReloader 调度器快照重载（scheduler.Scheduler 实现）。
type SchedReloader interface {
	InvalidateAll()
	InvalidateGroup(groupID int64)
}

// ClientsReloader aiclient 工厂客户端失效（aiclient.Factory 实现）。
type ClientsReloader interface {
	InvalidateAll()
}

// AuthReloader 鉴权快照全量重载（proxy.Auth 实现）。
type AuthReloader interface {
	Reload(ctx context.Context) error
}

// BalancesReloader 余额/倍率快照（billing.Balances 实现；billing 关闭时 nil，
// reloadAll 跳过余额路径）。
type BalancesReloader interface {
	Reload(ctx context.Context) error
	ReloadMultipliers(ctx context.Context) error
}

// SettingsReloader settings 快照全量重载（service.Service 实现，T2 提供；
// 未注入 nil → reloadAll 跳过，单实例现状行为不变）。
type SettingsReloader interface {
	ReloadSettings(ctx context.Context) error
}

// RulesReloader 规则表全量重载（rule.RuleEngine 实现——现有签名
// Reload(ctx) error，T2 需加 ReloadRules 适配；重载清窗口计数，全实例同步
// 执行语义）。未注入 nil → reloadAll 跳过。
type RulesReloader interface {
	ReloadRules(ctx context.Context) error
}

// Config 装配参数（main 接线）。
type Config struct {
	Window   time.Duration    // 去抖窗口（0 → DefaultWindow）
	Sched    SchedReloader    // 必填
	Clients  ClientsReloader  // 必填
	Auth     AuthReloader     // 必填
	Balances BalancesReloader // billing.enabled=false → nil
	Settings SettingsReloader // 可选（nil → settings 分支跳过）
	Rules    RulesReloader    // 可选（nil → rules 分支跳过）
	Log      *logx.Logger     // 可空（nil = 不记日志）
}

// Debouncer 去抖器（O2 核心）：
//   - Mark 路径（Users/Templates/Accounts/Multipliers）：零锁零 DB——原子 CAS
//     合并脏状态 + 非阻塞 channel 唤醒；任何调用方（含 50k 并发 fill）不阻塞。
//   - 执行路径：单 goroutine 串行（Start 启动），窗口到点消费脏状态执行合并
//     重载；执行期间新变更 → 完成后立即再执行（后沿语义，评审 C-6）。
//
// 同时满足 service.Invalidator 接口（main 装配传给 service.New）。
type Debouncer struct {
	cfg      Config
	newTimer func(time.Duration) <-chan time.Time // 测试注入 fake 时钟（默认 time.NewTimer）
	// goFn 托管 goroutine 启动器（B4-3/p2-03：裸 goroutine → worker.Manager.Go
	// 同契约——panic 捕获 + Warn，进程不崩，worker.go:6 承诺）。默认
	// worker.New(cfg.Log).Go；测试可注入记录/替代实现。
	goFn      func(ctx context.Context, name string, fn func(context.Context))
	state     atomic.Pointer[State]
	wake      chan struct{} // 空 → 非空置脏唤醒执行 goroutine（cap 1）
	startOnce atomic.Bool
}

// New 构造去抖器（reloadAll 目标经 cfg 注入）。
func New(cfg Config) *Debouncer {
	if cfg.Window <= 0 {
		cfg.Window = DefaultWindow
	}
	d := &Debouncer{cfg: cfg, wake: make(chan struct{}, 1)}
	d.newTimer = func(dur time.Duration) <-chan time.Time { return time.NewTimer(dur).C }
	d.goFn = worker.New(cfg.Log).Go // B4-3：托管 goroutine（recover 兜底）
	return d
}

// mark 合并一次变更进脏状态（零锁零 DB）。CAS 自旋（低频 admin 路径，竞争
// 可忽略）；首次置脏（empty → non-empty）唤醒执行 goroutine。唤醒丢失无碍：
// 执行 goroutine 在 flush 后沿复查脏状态，缓冲满 = 已有待消费唤醒。
func (d *Debouncer) mark(k Kind, gids []int64) {
	for {
		old := d.state.Load()
		next := &State{Kinds: k, Groups: make(map[int64]struct{}, len(gids))}
		if old != nil {
			next.Kinds |= old.Kinds
			for g := range old.Groups {
				next.Groups[g] = struct{}{}
			}
		}
		for _, g := range gids {
			next.Groups[g] = struct{}{}
		}
		if d.state.CompareAndSwap(old, next) {
			if old == nil {
				select {
				case d.wake <- struct{}{}:
				default:
				}
			}
			return
		}
	}
}

// Users 用户 CRUD（含创建）/余额变更：auth + 余额快照全量（评审 M-2：新用户
// 必须即刻进余额快照——去抖窗口内收敛，防 ≤10s 402 窗口；回归测试
// tools/e2e "建用户 → <0.5s 请求 → 200"）。
//
// auth 定向回退规则（评审 I-3）：本轮不做 Auth.UpsertUser 定向刷新，先全量
// Reload（加载在锁外——Auth.Reload 内部先 LoadKeys/LoadUsers 再整体换快照，
// 读端 RWMutex 不阻塞）。回退规则：仅当压测复测用户 fill <100/s 且 pprof
// 显示 auth.Reload 为热点时才新增 UpsertUser 定向路径。
func (d *Debouncer) Users() { d.mark(KindUsers, nil) }

// Templates 模板（base_url/models/映射）变更：sched 全量 + clients 失效
// （base_url 变更需按新地址重建 SDK 客户端——评审发现：此前
// Factory.InvalidateAll 无人调用，模板 base_url 更新后流量仍打旧上游直至
// 重启）。
func (d *Debouncer) Templates() { d.mark(KindTemplates|KindClients, nil) }

// Clients 仅客户端工厂失效（aiclient.Factory.InvalidateAll）：模板/账号
// 变更以外独立出现的 clients 失效（#14 T3a notify Dispatcher 独立映射；
// 服务端发布点恒与 Templates 或 Groups 并排，此处是防御性兜底）。
func (d *Debouncer) Clients() { d.mark(KindClients, nil) }

// Multipliers 组倍率 / 用户-组专属倍率（price_multiplier）变更（含组创建/
// 删除与 group_assignment CRUD——新倍率须即刻进快照，防 ×1 计费窗口）：余额
// 倍率快照定向刷新（组 + assignment 两路小表单查，非全量 Reload——
// EffectiveMultiplier 陈旧 ≤10s 不可接受）。
func (d *Debouncer) Multipliers() { d.mark(KindMultipliers, nil) }

// Keys key CRUD（创建/轮换/删除/改额度，#14 多实例 key 缺口）：auth 快照全量
// Reload（v1 不做增量定向——现状 auth 增量 Upsert/Delete 是单实例语义；多实例
// 其余实例的陈旧快照需全量覆盖）。供 notify Dispatcher 远端变更转发。
func (d *Debouncer) Keys() { d.mark(KindKeys, nil) }

// Settings settings 快照变更（UpdateSetting）：settings 快照全量重载。
// 生产侧死分支（仅测试引用）：dispatcher 已改同步直连路径、UpdateSetting 从
// 未 Mark——详见 KindSettings 常量注释。
func (d *Debouncer) Settings() { d.mark(KindSettings, nil) }

// Rules 规则表变更（规则 CRUD）：规则表全量重载（重载清窗口计数——全实例
// 同步执行语义，NOTIFY 广播）。供 notify Dispatcher 远端变更转发。
func (d *Debouncer) Rules() { d.mark(KindRules, nil) }

// Accounts 账号变更（创建/更新/删除/批量）：sched 组级定向重载受影响组
// （gids；与全量位同窗口时被包含跳过）；keyChanged（upstream_key 变更）→
// clients 失效。gids 空且 keyChanged=false（无分组账号变更）→ 无任何快照
// 受影响，直接 no-op（不入脏）。
func (d *Debouncer) Accounts(gids []int64, keyChanged bool) {
	if len(gids) == 0 && !keyChanged {
		return
	}
	d.mark(0, gids)
	if keyChanged {
		d.mark(KindClients, nil)
	}
}

// Name 满足 worker.Worker 契约。
func (d *Debouncer) Name() string { return "invalidate" }

// SetSettings 后置注入 settings 重载器（#14 T3a 装配）：main 构造顺序上
// service 在 invalidate 之后（service.New 需要去抖器做 Invalidator，去抖器
// 需要 svc 做 SettingsReloader——构造环），svc 构造完成后回填。必须在 Start
// 前调用（reloadAll 读 cfg，无锁）。
func (d *Debouncer) SetSettings(r SettingsReloader) { d.cfg.Settings = r }

// Start 启动执行 goroutine（幂等：重复 Start 返回错误；worker 契约）。
func (d *Debouncer) Start(ctx context.Context) error {
	if !d.startOnce.CompareAndSwap(false, true) {
		return fmt.Errorf("invalidate: already started")
	}
	d.goFn(ctx, d.Name(), d.loop) // B4-3：裸 goroutine → 托管（Manager.Go 契约的 recover：loop panic 不崩进程）
	return nil
}

// Close 无操作：循环随 Start 的 ctx 取消退出（worker 契约：幂等、未 Start 也
// 安全）。停机不补最后 flush——DB 权威，残留变更随下次启动全量加载。
func (d *Debouncer) Close(ctx context.Context) error { return nil }

// loop 执行 goroutine（单 goroutine 串行，重载从不互相重叠）：
// 唤醒（首次置脏）→ 起窗口定时器 → 到点 flush；空唤醒（状态已被消费）跳过。
func (d *Debouncer) loop(ctx context.Context) {
	var timerC <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.wake:
			if d.state.Load() == nil {
				continue // 空唤醒（状态已被 flush 消费）
			}
			if timerC == nil {
				// 窗口自首次变更起计时（生效延迟 = 窗口 + 一次重载时长）。
				timerC = d.newTimer(d.cfg.Window)
			}
		case <-timerC:
			timerC = nil
			d.flush()
		}
	}
}

// flush 消费脏状态执行一次合并重载；完成后若仍脏（执行期间新变更）→ 立即
// 再执行（后沿语义，评审 C-6：完成后又脏立即再执行，禁止按固定间隔
// throttle——固定间隔会与长 reload 重叠放大）。窗口内新变更只并入当前窗口。
func (d *Debouncer) flush() {
	st := d.state.Swap(nil)
	if st == nil {
		return
	}
	d.reloadAll(st)
	for d.state.Load() != nil {
		st = d.state.Swap(nil)
		d.reloadAll(st)
	}
}

// reloadAll 按接线矩阵执行一次合并重载（评审 M-1）：
// 用户 → auth + 余额全量（加载在锁外——Auth.Reload 内部构建后整体换；
// 余额 Reload 构建后原子换指针）；模板 → sched 全量 + clients；组级 → sched
// InvalidateGroup 逐个（full 位存在时被包含跳过）；组倍率 → 余额倍率定向
// 刷新。全部 fail-safe：Warn + 保留旧快照（调度器 ≤30s 同步 /
// BalanceRefreshInterval ticker 兜底收敛）。
func (d *Debouncer) reloadAll(st *State) {
	if st.Kinds&KindUsers != 0 {
		// Auth.Reload 内部已对失败打 Warn（覆盖 NewAuth 启动/无 logger 调用方），
		// 此处 Debug 防双 Warn（评审 I-3）；错误本身仍由内部 Warn 报告。
		if err := d.cfg.Auth.Reload(context.Background()); err != nil && d.cfg.Log != nil {
			d.cfg.Log.Debug("auth reload failed", logx.Error(err))
		}
		if d.cfg.Balances != nil {
			_ = d.cfg.Balances.Reload(context.Background()) // fail-safe：内部 Warn + 保留旧快照
		}
	}
	if st.Kinds&KindTemplates != 0 {
		d.cfg.Sched.InvalidateAll()
	}
	if len(st.Groups) > 0 && st.Kinds&KindTemplates == 0 {
		for gid := range st.Groups {
			d.cfg.Sched.InvalidateGroup(gid)
		}
	}
	if st.Kinds&(KindTemplates|KindClients) != 0 {
		d.cfg.Clients.InvalidateAll()
	}
	if st.Kinds&KindMultipliers != 0 && d.cfg.Balances != nil {
		_ = d.cfg.Balances.ReloadMultipliers(context.Background()) // fail-safe：内部 Warn + 保留旧快照
	}
	if st.Kinds&KindKeys != 0 {
		// key CRUD 缺口：auth 快照全量（与 KindUsers 的 auth 分支同一调用；不
		// 加余额快照——key 变更不影响余额）。
		if err := d.cfg.Auth.Reload(context.Background()); err != nil && d.cfg.Log != nil {
			d.cfg.Log.Debug("auth reload failed", logx.Error(err))
		}
	}
	if st.Kinds&KindSettings != 0 && d.cfg.Settings != nil {
		if err := d.cfg.Settings.ReloadSettings(context.Background()); err != nil && d.cfg.Log != nil {
			d.cfg.Log.Warn("settings reload failed", logx.Error(err))
		}
	}
	if st.Kinds&KindRules != 0 && d.cfg.Rules != nil {
		// B4-4/p2-12：规则快照无周期兜底（对照 auth 60s / sched 30s / balances
		// 10s）——本分支 Background 双保险：事件驱动的全量重载不随任何请求 ctx
		// 取消（周期 ticker 属行为新增，spec 标注可选裁决，本批次不实现）。
		if err := d.cfg.Rules.ReloadRules(context.Background()); err != nil && d.cfg.Log != nil {
			d.cfg.Log.Warn("rules reload failed", logx.Error(err))
		}
	}
}
