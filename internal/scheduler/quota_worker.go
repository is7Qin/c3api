// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 429 → 有界异步额度补拉 worker（设计 §5.4 / R3 裁定）。
//
// 职责边界（provider-agnostic）：本 worker 只知道「某账号在某身份代际 K 上被判
// 了一条 RETRY_AFTER 冷却，但这条判决没有绝对截止」——补拉就是把那条判决的
// **时长**补全到窗口 reset。因此：
//
//   - 判决种类/时长全部由 LatchSink 的既有写入决定，本 worker **不调用
//     MarkResult**（R3 第 1 条：不得产生第二条 rule 事件）；
//   - 唯一写入面是 health.ExtendCooldown（绝对 until + 原子 max ⇒ 只延长，且
//     只延长仍然存在/状态相同/未过截止的那一条），键与状态由入队方（sink）
//     算好的四个分量原样带回；
//   - 凭据族无关：凭据类型 → QuotaResolver 的映射由注册表在装配期定，worker
//     不认识 codex。
//
// 纪律（逐条都是门禁）：
//   - 投递非阻塞：队列满 / 已关闭 → 丢弃 + 计数，绝不给请求路径加等待；
//   - 代际校验：执行时按 accountID 重新解析账号，K ≠ 任务的 K ⇒ 丢弃（旧代际的
//     额度结论不得作用到新身份上）；
//   - 超时不毒化循环：单任务独立 ctx 超时，超时按 failed 计数，循环继续；
//   - 关闭：停止取新任务 + 取消在途 ctx + 有界等待（Close 的 ctx 到期也返回）；
//   - 失败/空集/未注册凭据族**一律不写**（门禁 6：无有效窗口不得延长健康 TTL）。
//
// 上界（§5.2）由 resolver 侧（SDK 的 TTL + 失败冷却 + singleflight）保证；本
// worker 不做任何轮询、不重试、不合并。
package scheduler

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/logx"
)

// 内部默认（不引入运维配置键）：零值即取默认，构造期一次性落定。
const quotaTimeoutDefault = 3 * time.Second

// QuotaResolver 单凭据族的补拉面：给定账号快照，返回该账号"额度打满后最晚
// 可信 reset"。
//
// 三段语义（ok=false 的三种来由都合法且不可区分）：非本凭据族/无凭据 ⇒
// (zero,false,nil)；补拉成功但没有可信窗口 ⇒ (zero,false,nil)；补拉失败 ⇒
// (zero,false,err)。调用方据此决定「不写 / 不写 / 不写（计 failed）」——本
// 契约不允许把失败降级成 ok=false 之外的任何"部分成功"。
type QuotaResolver interface {
	ResolveThrottleDeadline(ctx context.Context, acct *domain.Account) (until time.Time, ok bool, err error)
}

// QuotaResolverRegistry 凭据类型 → 补拉面的注册表。
//
// Register 只允许**装配期**调用（组合根在 worker 启动前一次性注册）。读侧
// （Resolve）在 worker 的运行期被调用，故实现必须并发安全；这里不做"锁外读
// map"的优化——补拉是慢路径，一次 RLock 无足轻重。
type QuotaResolverRegistry interface {
	Resolve(t credential.Type) (QuotaResolver, bool)
	Register(t credential.Type, r QuotaResolver)
}

// QuotaResolvers 注册表实现（接口的唯一生产实现；测试另用替身）。
type QuotaResolvers struct {
	mu sync.RWMutex
	m  map[credential.Type]QuotaResolver
}

// NewQuotaResolvers 空注册表（组合根构造后按凭据类型注册）。
func NewQuotaResolvers() *QuotaResolvers {
	return &QuotaResolvers{m: make(map[credential.Type]QuotaResolver)}
}

// Register 装配期注册。重复注册同类型覆盖前值（装配期重入的确定性语义，
// 不做"首次胜出"这类隐式规则）；nil resolver 注册后等价"未注册"。
func (r *QuotaResolvers) Register(t credential.Type, res QuotaResolver) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[t] = res
}

// Resolve 取某凭据类型的补拉面。
func (r *QuotaResolvers) Resolve(t credential.Type) (QuotaResolver, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	res, ok := r.m[t]
	return res, ok && res != nil
}

var _ QuotaResolverRegistry = (*QuotaResolvers)(nil)

// QuotaHealthWriter 补拉结论的唯一写入口（*RuntimeHealth 满足）。只暴露
// ExtendCooldown：补拉是「对既有判决的时长补全」，不是新判决——记录不存在/
// 状态已被别的判决改写/已过截止 ⇒ Lua 拒绝（返回 0），补拉不得重建或改写。
// 故不给 Throttle/ThrottleUntil/MarkReady 等其它写入面（编译期钉死，写得少
// 就错得少）。
type QuotaHealthWriter interface {
	ExtendCooldown(ctx context.Context, key HealthKey, state HealthState, until time.Time) (int64, error)
}

var _ QuotaHealthWriter = (*RuntimeHealth)(nil)

// quotaEnqueuer LatchSink 侧的投递面（*QuotaQueue 满足；nil = 未装配 ⇒ no-op）。
type quotaEnqueuer interface{ Enqueue(QuotaTask) }

// QuotaTask 一条补拉任务：入队只带**健康键的全部四个分量**与判决状态——不含
// 凭据、不含 Selection（请求结束即失效；执行时按 K 重新解析并比对）。
//
// 为什么键要在入队时定死：补拉回来时若重新推导 Quality/指纹，算出来的可能
// 已经是"另一个键"（模板/映射在途中改过），写进去的就是一条没人会查询的记录。
type QuotaTask struct {
	AccountID        int64
	IdentityRevision int64  // 判决时的身份代际 K（0 = 未知，执行时按 stale 丢弃）
	Fingerprint      string // 候选身份指纹 I（HealthKey.Identity）
	Quality          string // 质量类 hex 或 "*"（HealthKey.Quality）
	State            HealthState
}

// QuotaWorkerConfig 构造参数（零值字段取内部默认）。队列容量归 QuotaQueue，
// 故此处不再持有 QueueSize。
type QuotaWorkerConfig struct {
	Timeout time.Duration // 单任务补拉超时（独立 ctx，超时只影响该任务）
}

// quotaWorkerStats /ops/workers 观测快照（handler.StatsProvider 形态）。
// json 键清单（enqueued/dropped/extended/rejected/empty/failed/stale/unsupported/queued）
// 的契约位置只有一处：openapi/openapi.yaml 的 WorkerStatus.stats 字段清单——改
// 字段必须同步那里（同步义务钉在 handler/ops.go 的 StatsProvider 注释）。
type quotaWorkerStats struct {
	Enqueued    int64 `json:"enqueued"`    // 成功入队累计（关闭排空时按条转入 dropped：两者之和 == Enqueue 调用次数）
	Dropped     int64 `json:"dropped"`     // 队列满/已关闭丢弃累计（含关闭排空转来的剩余）
	Extended    int64 `json:"extended"`    // 经 ExtendCooldown 实际延长冷却累计
	Rejected    int64 `json:"rejected"`    // 补拉被闸门拒绝（记录已清除/异状态/已过截止）累计
	Empty       int64 `json:"empty"`       // 补拉成功但无有效窗口 ⇒ 不写累计
	Failed      int64 `json:"failed"`      // 补拉/写入失败（含 ctx 超时）⇒ 不写累计
	Stale       int64 `json:"stale"`       // 代际不符/账号不可解析 ⇒ 丢弃累计
	Unsupported int64 `json:"unsupported"` // 凭据类型未注册 resolver ⇒ 跳过累计
	Queued      int   `json:"queued"`      // 当前队列积压
}

// QuotaWorker 429 补拉 worker（worker.Worker + handler.StatsProvider 契约，
// Name="quota-deadline"）。单消费 goroutine：补拉是低频慢路径（受 resolver 侧
// TTL 约束），并发收益接近零，串行反而把"单账号单代际一次上游"的上界变得显然。
type QuotaWorker struct {
	health   QuotaHealthWriter
	registry QuotaResolverRegistry
	lookup   func(accountID int64) (*domain.Account, bool)
	log      *logx.Logger
	cfg      QuotaWorkerConfig

	q *QuotaQueue // 消费面：队列由 sink/组合根持有，worker 只消费其 channel

	// mu 保护 started/closed/cancel/loopDone（Close 置位后再取消，Start/Close
	// 与派生的 runCtx 互斥串行）。
	mu       sync.Mutex
	started  bool
	closed   bool
	cancel   context.CancelFunc
	loopDone <-chan struct{}

	closeOnce sync.Once

	extended    atomic.Int64
	rejected    atomic.Int64
	empty       atomic.Int64
	failed      atomic.Int64
	stale       atomic.Int64
	unsupported atomic.Int64
}

// NewQuotaWorker 构造补拉 worker。health/registry/lookup 均须非 nil（生产装配
// 点：runtimeHealth / QuotaResolvers / sched.ProbeAccount）；log 可 nil（静默）。
// 缺失协作者不 panic：每条任务按 failed 计数（装配缺陷在 /ops/workers 可见，
// 循环不倒）。
func NewQuotaWorker(q *QuotaQueue, health QuotaHealthWriter, registry QuotaResolverRegistry,
	lookup func(accountID int64) (*domain.Account, bool),
	log *logx.Logger, cfg QuotaWorkerConfig) *QuotaWorker {
	if q == nil {
		q = NewQuotaQueue(0)
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = quotaTimeoutDefault
	}
	return &QuotaWorker{
		q:        q,
		health:   health,
		registry: registry,
		lookup:   lookup,
		log:      log,
		cfg:      cfg,
	}
}

// Name worker.Worker 契约。
func (w *QuotaWorker) Name() string { return "quota-deadline" }

// Start 非阻塞启动消费 goroutine（worker.Loop 托管，panic 不崩进程）。幂等：
// 二次 Start 返回错误。派生的 runCtx 是 Close 取消在途补拉的把手（上层 ctx 被
// 取消同样生效）。
func (w *QuotaWorker) Start(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	// closed 与 started 必须一起判：Close 先于 Start（装配期早退/测试乱序）之后
	// 再 Start，loopDone 早已关闭且 closeOnce 已耗尽——那个 goroutine 再也关不掉。
	// 终态语义：关过的 worker 不允许重新启动。
	if w.closed {
		return fmt.Errorf("quota-deadline worker: closed")
	}
	if w.started {
		return fmt.Errorf("quota-deadline worker: already started")
	}
	runCtx, cancel := context.WithCancel(ctx)
	w.cancel = cancel
	w.started = true
	w.loopDone = worker.GoLoop(runCtx, "quota-deadline", w.log, w.loop)
	return nil
}

// loop 消费循环：逐条处理，ctx 取消即退出（在途任务的 ctx 随之取消）。
func (w *QuotaWorker) loop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case task := <-w.q.ch:
			w.handle(ctx, task)
		}
	}
}

// handle 单条补拉：独立超时 ctx → 解析账号 → 代际校验 → 按凭据族取 resolver
// → 补拉 → 有 until 才写（ExtendCooldown）。任何一步不满足都**不写**。
func (w *QuotaWorker) handle(ctx context.Context, task QuotaTask) {
	if w.health == nil || w.registry == nil || w.lookup == nil {
		w.failed.Add(1) // 装配缺陷：计数可见，不 panic（worker 循环必须活着）
		return
	}
	cctx, cancel := context.WithTimeout(ctx, w.cfg.Timeout)
	defer cancel()
	// lookup = scheduler 选号快照（与健康 probe 同一视图）。拿不到账号行一律
	// 按 stale 丢弃（不报错刷日志——429 高频）。
	acct, ok := w.lookup(task.AccountID)
	if !ok || acct == nil || acct.IdentityRevision != task.IdentityRevision {
		w.stale.Add(1)
		return
	}
	if acct.Template == nil {
		w.unsupported.Add(1) // 无模板 = 无凭据族 = 无人能补拉
		return
	}
	resolver, ok := w.registry.Resolve(acct.Template.CredentialType)
	if !ok {
		w.unsupported.Add(1)
		return
	}
	until, ok, err := resolver.ResolveThrottleDeadline(cctx, acct)
	if err != nil {
		w.failed.Add(1)
		// Debug：补拉失败含上游 429/网络抖动，属预期面；可见性交给 failed 计数。
		w.log.Debug("quota-deadline resolve failed",
			logx.Int64("account_id", task.AccountID),
			logx.Error(err))
		return
	}
	if !ok {
		w.empty.Add(1)
		return
	}
	// 唯一写健康通道：把**既有的**这条判决延长到窗口 reset 的绝对截止。只延长
	// 由 Lua 的原子 max 保证；而「既有」由 ExtendCooldown 的存在性/状态/截止闸门
	// 保证——迟到的任务是补全它自己那条判决，不得重建/改写/复活别人的记录。
	// 不 MarkResult——补拉不产生第二条 rule 事件（R3 第 1 条）。
	key := HealthKey{
		AccountID:        task.AccountID,
		Quality:          task.Quality,
		Identity:         task.Fingerprint,
		IdentityRevision: task.IdentityRevision,
	}
	gen, err := w.health.ExtendCooldown(cctx, key, task.State, until)
	if err != nil {
		w.failed.Add(1)
		w.log.Debug("quota-deadline extend failed",
			logx.Int64("account_id", task.AccountID),
			logx.Error(err))
		return
	}
	if gen == 0 {
		// 记录已不存在 / 状态被别的判决改写 / 已过截止：补拉被判拒绝。
		// 这是运维恢复或其他失败类别赢下了这场竞争，不是错误；不得计入 extended。
		w.rejected.Add(1)
		w.log.Debug("quota-deadline extend rejected",
			logx.Int64("account_id", task.AccountID))
		return
	}
	w.extended.Add(1)
}

// Close 幂等关闭：置位 closed（worker 终态）→ 取消 runCtx（在途补拉的 ctx
// 立即到期）→ 等 loop 退出（受 ctx 预算约束，**不死等**）→ 关闭队列（此后外部
// Enqueue 一律丢弃计数、不入队；剩余条目由队列在同锁置位后锁外排空并计入
// dropped）。未 Start 也安全（无 goroutine 可等）。
func (w *QuotaWorker) Close(ctx context.Context) error {
	w.closeOnce.Do(func() {
		w.mu.Lock()
		w.closed = true
		cancel, started, done := w.cancel, w.started, w.loopDone
		w.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		if started {
			select {
			case <-done:
			case <-ctx.Done():
				w.log.Warn("quota-deadline close: loop did not exit in time")
			}
		}
		// 队列随 worker 一起进入终态：此后 sink 侧的 Enqueue 直接丢弃计数（不再
		// 往一个没有消费者的 channel 里塞）。排空归 QuotaQueue.Close——置位与投递
		// 互斥在同一把锁里，故它排空的是终态剩余（worker 不再单独排空：那会把同一
		// 批任务在账上记两次）。
		w.q.Close()
	})
	return nil
}

// EnqueuedCount 成功入队的累计（观测/测试；取自队列）。
func (w *QuotaWorker) EnqueuedCount() int64 {
	enqueued, _, _ := w.q.Stats()
	return enqueued
}

// Dropped 队列满/已关闭丢弃计数（背压观测；取自队列）。
func (w *QuotaWorker) Dropped() int64 {
	_, dropped, _ := w.q.Stats()
	return dropped
}

// Extended 经 ExtendCooldown 实际延长冷却的累计。
func (w *QuotaWorker) Extended() int64 { return w.extended.Load() }

// Rejected 补拉被闸门拒绝（记录已清除/异状态/已过截止）的累计：补拉输给了
// 运维恢复、其他失败类别或自然到期，不是错误。
func (w *QuotaWorker) Rejected() int64 { return w.rejected.Load() }

// Empty 补拉成功但无有效窗口（不写）的累计。
func (w *QuotaWorker) Empty() int64 { return w.empty.Load() }

// Failed 补拉/写入失败（含 ctx 超时）的累计。
func (w *QuotaWorker) Failed() int64 { return w.failed.Load() }

// Stale 代际不符/账号不可解析而丢弃的累计。
func (w *QuotaWorker) Stale() int64 { return w.stale.Load() }

// Unsupported 凭据类型未注册 resolver（或无模板）跳过的累计。
func (w *QuotaWorker) Unsupported() int64 { return w.unsupported.Load() }

// Queued 当前队列积压条数（背压观测；取自队列）。
func (w *QuotaWorker) Queued() int {
	_, _, queued := w.q.Stats()
	return queued
}

// Stats 满足 handler.StatsProvider（观测面：入队/丢弃/补全/拒绝/空集/失败/
// 代际丢弃/未注册/积压）。
func (w *QuotaWorker) Stats() any {
	enqueued, dropped, queued := w.q.Stats()
	return quotaWorkerStats{
		Enqueued:    enqueued,
		Dropped:     dropped,
		Extended:    w.extended.Load(),
		Rejected:    w.rejected.Load(),
		Empty:       w.empty.Load(),
		Failed:      w.failed.Load(),
		Stale:       w.stale.Load(),
		Unsupported: w.unsupported.Load(),
		Queued:      queued,
	}
}

var _ worker.Worker = (*QuotaWorker)(nil)
