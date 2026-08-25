// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

// 账号并发份额+借用跨实例共识（spec conc-share-borrow-account）：scheduler 选号
// 账号并发上限的集群视图——对账式聚合，非增量借贷。协议孪生 =
// internal/proxy/concsync.go（gate 层 user/key 先例，已合入 main@610b28e）：
// 协议常量与 value 格式逐字一致，实现自持（proxy 已 import scheduler，反向必成环；
// 两处重复是有意的 YAGNI——第三个消费方出现才提炼 internal/concproto）。差异点：
// per-layer 双 map → 单 accounts 直键；判定嵌入 pickFrom 扫描循环（借位拒绝=换号，
// 非拒流）。三条公理与 gate 层同款：
//
//  1. 选号路径 100% 本地判定、零 Redis 命令：Redis 只被本 worker 每 tick 一条
//     pipeline 双向读写（上报 + 拉回聚合），聚合结果成为 Scheduler 的 concView。
//  2. N=1 结构性短路：share=limit → 超份额分支数学上不可达（concShare 公式性质，
//     非 if 分支）。
//  3. 无模式无转换：N 变化只改变除数（instancesN 现读），发布翻转/启动窗口均非事件。
//
// 死亡自愈零协议：实例崩溃/在途归零 → 停止上报 → 其字段 ≤4s 陈旧出局；整键
// EXPIRE 16s 兜底自灭。没有增量就没有孤儿租约与续期循环。
//
// nil client / worker 未启动 = 永远无视图 = 全额本地语义（fail-open 结构性质：
// 视图新鲜度布尔，不是错误分支）。

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/is7qin/c3api/pkg/logx"
)

// 协议常量（spec §2：命名常量不做配置项；与 proxy/concsync.go 逐字一致）。
const (
	// concAccountPrefix 每账号一键的命名空间前缀（u/k/a 三命名空间互不串扰）。
	concAccountPrefix = "c3api:conc:a:"
	// concSyncInterval 同步周期：视图滞后真值 ≤1 tick（漂移上界）。
	concSyncInterval = 500 * time.Millisecond
	// concFieldStale 字段新鲜度阈值：ts 早于 now-concFieldStale 的字段不计入聚合
	// （死亡实例 ≤4s 出局；偏斜预算覆盖心跳相位 + 成对时钟偏斜）。
	concFieldStale = 4 * time.Second
	// concKeyTTL 整键过期（16s ≫ 500ms 上报间隔）：存活集群内每 tick 续期恒先于
	// 过期，过期只在全体实例同时消亡后自灭键——防遗弃 HASH 累积。
	concKeyTTL = 16 * time.Second
	// concTickTimeout 单次 pipeline 预算（≤2 tick，超时按失败走冻结语义）。
	concTickTimeout = time.Second
	// concWarnThrottle 故障告警节流（≥30s 一次，沿 discovery warnThrottle 先例）。
	concWarnThrottle = 30 * time.Second
	// concViewStale 视图级陈旧阈值（选号判定侧）：at 超龄即整视图 fail-open。
	concViewStale = 4 * time.Second
)

// InstancesProvider 集群实例数 N 提供者（discovery.Discovery 实现 ClusterInstances；
// 与 proxy.InstancesProvider 同名同构异包——禁 import proxy 会成环，Go 结构化类型
// 下 discovery.Discovery 天然双满足，零适配代码）。nil（未装配）按 N=1。
type InstancesProvider interface {
	ClusterInstances() int
}

// concSnap 单账号聚合快照（与孪生 concSnap 逐字段对齐：total/selfLast/at，
// 保持孪生 diff-ability）。
type concSnap struct {
	total    int64     // Σ 新鲜实例的在途报告
	selfLast int64     // 自身本次上报值（判定精化用）
	at       time.Time // 构建时刻（视图级新鲜度判定）
}

// clusterView 集群账号并发视图（Scheduler 第二 atomic 快照）：单 accounts 直键
// （gate 层是 users+keys 双层，此处只有账号一个受限维度）。每 tick 全量重建换入
// （条目数 = 本实例活跃账号数，无 clone 需要）；整体可丢可重建、无在途状态。
type clusterView struct {
	accounts map[int64]concSnap // account_id → 聚合快照
}

// SetInstancesProvider 注入集群实例数 N（装配期 main 注入 disco；nil 清空 → N=1）。
// N 在每次 Select 的 pickFrom 入口现读，心跳计数变化 ≤1 tick 天然生效。
func (s *Scheduler) SetInstancesProvider(p InstancesProvider) { s.instN.Store(&p) }

// instancesN 当前集群实例数（N ≥ 1；provider 缺失/非法值 → 1）。
func (s *Scheduler) instancesN() int {
	if p := s.instN.Load(); p != nil && *p != nil {
		if n := (*p).ClusterInstances(); n > 0 {
			return n
		}
	}
	return 1
}

// AccConcSyncWorker 账号并发双向同步 worker：实现 worker.Worker。客户端经 main
// 注入（pkg/redisx 单构造点纪律，本包不自建连接）；Scheduler 同包直访私有字段
// （store.byID 读 + concView 换入）。nil client = Start no-op（测试/降级形态零
// 专门分支）。
type AccConcSyncWorker struct {
	client *redis.Client
	self   string     // 实例 ID（main instanceSrc 产物，与 NOTIFY Src / discovery 同源）
	sched  *Scheduler // 计数器读（byID） + clusterView 换入点
	log    *logx.Logger

	errs      atomic.Int64 // 连续 tick 失败数（恢复归零；测试确定性信号）
	lastWarn  atomic.Int64 // 上次 Warn unixnano（仅 sync goroutine 读写）
	startOnce sync.Once
	stopOnce  sync.Once
	cancel    context.CancelFunc
	done      chan struct{}
}

// NewConcSyncWorker 构造（零副作用：Start 才起 goroutine，Close 未 Start 时安全）。
func NewConcSyncWorker(sched *Scheduler, client *redis.Client, self string, log *logx.Logger) *AccConcSyncWorker {
	return &AccConcSyncWorker{client: client, self: self, sched: sched, log: log}
}

// Name worker 名（worker.Worker 契约）；与 proxy "conc-sync" 区分——ops/workers
// 聚合键唯一。
func (w *AccConcSyncWorker) Name() string { return "account-conc-sync" }

// Start 非阻塞启动同步循环（幂等；自持 ctx，同 twin 惯用法）。nil client 直接短路：
// 无视图装配能力即全额本地语义，循环无存在意义。
func (w *AccConcSyncWorker) Start(_ context.Context) error {
	if w.client == nil {
		return nil
	}
	w.startOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		w.cancel, w.done = cancel, make(chan struct{})
		go func() {
			defer close(w.done)
			t := time.NewTicker(concSyncInterval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					w.tick(ctx)
				}
			}
		}()
	})
	return nil
}

// Close 幂等优雅停机：停 tick 即完事（协调态可丢、无排空顺序依赖——在途字段由
// ts 新鲜度 ≤4s 出局、整键 EXPIRE 自灭，Close 无清理义务）。
func (w *AccConcSyncWorker) Close(ctx context.Context) error {
	w.stopOnce.Do(func() {
		if w.cancel == nil {
			return // 未 Start 过：Close 安全 no-op（worker 契约）
		}
		w.cancel()
		select {
		case <-w.done:
		case <-ctx.Done():
			if w.log != nil {
				w.log.Warn("account-conc-sync: loop stop timed out", logx.Error(ctx.Err()))
			}
		}
	})
	return nil
}

// concSyncStats ops 观测快照（handler.StatsProvider 形态，spec conc-sync-ops-stats）。
// 与 proxy 孪生同键集；tracked_entries=账号条目数（nil 视图=0）。
type concSyncStats struct {
	LastTickOk        bool  `json:"last_tick_ok"`
	ConsecutiveErrors int64 `json:"consecutive_errors"`
	TrackedEntries    int64 `json:"tracked_entries"`
}

// Stats worker 观测（协调面冻结可见性）：fail-open 静默退化时这是运维面唯一
// 痕迹——视图停止换入则 last_tick_ok 翻 false、consecutive_errors 增长。
func (w *AccConcSyncWorker) Stats() any {
	errs := w.errs.Load()
	var tracked int64
	if cv := w.sched.concView.Load(); cv != nil {
		tracked = int64(len(cv.accounts))
	}
	return concSyncStats{
		LastTickOk:        errs == 0,
		ConsecutiveErrors: errs,
		TrackedEntries:    tracked,
	}
}

// concTarget 本 tick 的一个上报对象（在途 > 0 的账号）。
type concTarget struct {
	rkey string // Redis HASH 键
	id   int64  // account_id
	val  int64  // 本地当前在途值（本次上报值）
}

// collect 收集上报范围（spec §2 上行）：遍历 byID 中 concurrency.Load() > 0 的账号。
// byID 是整体原子换入的不可变快照 map（重建路径复用实例指针——O-2 继承纪律），
// 遍历零锁安全；孤儿对象在途值由 Release 自然衰减后自动退出上报集。
func (w *AccConcSyncWorker) collect() []concTarget {
	byID, ok := w.sched.store.byID.Load().(map[int64]*accountSnapshot)
	if !ok {
		return nil
	}
	targets := make([]concTarget, 0, len(byID))
	for id, a := range byID {
		if v := a.concurrency.Load(); v > 0 {
			targets = append(targets, concTarget{
				rkey: concAccountPrefix + strconv.FormatInt(id, 10), id: id, val: v,
			})
		}
	}
	return targets
}

// tick 单条 pipeline 双向同步（spec §2）：上行 HSET(field=self,
// value="<L> <now_ms>") + EXPIRE 续期；下行同 pipeline 对相同 key 集 HGETALL
// （pipeline 内命令按序执行，HGETALL 必见自身刚写的字段）→ 过滤新鲜字段聚合 →
// 全量重建新视图 → atomic 换入。失败冻结旧视图（不 Store），视图自然陈旧后
// 选号判定 fail-open 全额本地。无活跃账号：无键可报亦无可聚合，跳过本 tick
// （非成功非失败——既有视图自然陈旧出局，语义正确：无人占用则无需共识）。
func (w *AccConcSyncWorker) tick(ctx context.Context) {
	targets := w.collect()
	if len(targets) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, concTickTimeout)
	defer cancel()
	nowMS := time.Now().UnixMilli()
	pipe := w.client.Pipeline()
	gets := make([]*redis.MapStringStringCmd, len(targets))
	for i, t := range targets {
		v := strconv.FormatInt(t.val, 10) + " " + strconv.FormatInt(nowMS, 10)
		pipe.HSet(ctx, t.rkey, w.self, v)
		pipe.Expire(ctx, t.rkey, concKeyTTL)
		gets[i] = pipe.HGetAll(ctx, t.rkey)
	}
	if _, err := pipe.Exec(ctx); err != nil {
		w.onTickError(err)
		return
	}
	view := &clusterView{accounts: make(map[int64]concSnap, len(targets))}
	for i, t := range targets {
		snap := concSnap{selfLast: t.val, at: time.Now()} // selfLast = 本次上报值
		for _, raw := range gets[i].Val() {
			inflight, ts, ok := parseConcValue(raw)
			if !ok || ts < nowMS-int64(concFieldStale/time.Millisecond) {
				continue // 畸形/陈旧字段出局（死亡实例 ≤4s 自愈）
			}
			snap.total += inflight
		}
		view.accounts[t.id] = snap
	}
	w.sched.concView.Store(view)
	w.errs.Store(0)
}

// parseConcValue 解析 "<inflight> <unixmilli>" 上报值；畸形返回 false。
func parseConcValue(raw string) (inflight, tsMS int64, ok bool) {
	left, right, found := strings.Cut(raw, " ")
	if !found {
		return 0, 0, false
	}
	inflight, err := strconv.ParseInt(left, 10, 64)
	if err != nil || inflight < 0 {
		return 0, 0, false
	}
	tsMS, err = strconv.ParseInt(right, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return inflight, tsMS, true
}

// onTickError 冻结上次视图 + 节流告警（≥concWarnThrottle 一次；仅 sync goroutine
// 调用，无并发）。恢复由下一个成功 tick 自然换入新视图并清错误态。
func (w *AccConcSyncWorker) onTickError(err error) {
	w.errs.Add(1)
	if w.log == nil {
		return
	}
	if time.Since(time.Unix(0, w.lastWarn.Load())) < concWarnThrottle {
		return
	}
	w.lastWarn.Store(time.Now().UnixNano())
	w.log.Warn("account-conc-sync: pipeline failed, freezing last cluster view",
		logx.Int64("consecutive_errors", w.errs.Load()), logx.Error(err))
}

// concShare 份额公式（spec §1.1）：max(1, floor(limit/N))。floor 保证 Σ 各实例
// fast-path 准入 ≤ limit（无借用路径自身不超限硬保证）；max(1) 处理 limit<N
// 退化形态（此时全部准入超份额 → 全部走视图判定，纯内存操作）。N=1 时
// share=limit → 超份额分支数学上不可达（结构性短路，Redis 零触碰）。
func concShare(limit, n int) int {
	if s := limit / n; s > 0 {
		return s
	}
	return 1
}

// concAllows 超份额借位判定（纯内存两读 + map 查找，零远程零错误分支零新增锁；
// view 由 pickFrom 入口一次性取用——单代纪律，整轮扫描共用同一代视图）：
//
//	effective = total − selfLast + L_now   —— 剔除自身滞后报告、代入本地实时值
//	放行 ⟺ effective < limit
//
// 无视图 / 条目缺失 / 条目陈旧 → fail-open 按「全额 limit」本地判定
// （lnow ≤ limit）= 引入 Redis 前现状语义。并发是建议性协调态（软于合同限额，
// 方向保守），Redis 故障在此降维成视图新鲜度布尔。放行时 effective 含 L_now ⇒
// 本地计数 < 真上限恒成立（借用不破真上限兜底；随后仍以真上限 CAS 兜底占用，
// 竞态失败按保守多拒处理——换下一候选而非拒流）。
func concAllows(view *clusterView, accID, limit, lnow int64) bool {
	if view == nil {
		return lnow <= limit // worker 未装配 / 尚无首个 tick：全额本地语义
	}
	snap, ok := view.accounts[accID]
	if !ok || time.Since(snap.at) >= concViewStale {
		return lnow <= limit // 条目缺失 / 陈旧：fail-open 全额本地
	}
	return snap.total-snap.selfLast+lnow < limit
}
