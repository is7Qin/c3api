// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// 并发门份额+借用跨实例共识（spec conc-share-borrow-gate §1.2/§1.5）：gate 层
// user/key 并发上限的集群视图——对账式聚合，非增量借贷。三条公理：
//
//  1. 请求路径 100% 本地判定、零 Redis 命令：Redis 只被本 worker 每 tick 一条
//     pipeline 双向读写（上报 + 拉回聚合），聚合结果成为 gate 第二 atomic 快照
//     （clusterView，与 gateSnapshot 同构并列）。
//  2. N=1 结构性短路：share=limit → 超份额分支数学上不可达（concShare 公式性质，
//     非 if 分支）。
//  3. 无模式无转换：N 变化只改变除数（instancesN 现读），发布翻转/启动窗口均非事件。
//
// 死亡自愈零协议：实例崩溃/在途归零 → 停止上报 → 其字段 ≤4s 陈旧出局；整键
// EXPIRE 16s 兜底自灭。没有增量就没有孤儿租约与续期循环。
//
// nil client / worker 未启动 = 永远无视图 = 全额本地语义（fail-open 结构性质：
// 视图新鲜度布尔，不是错误分支）。

// 协议常量（spec §1.5：命名常量不做配置项，沿 discovery 先例）。
const (
	// concUserPrefix / concKeyPrefix 每限流层级每对象一键的命名空间前缀。
	concUserPrefix = "c3api:conc:u:"
	concKeyPrefix  = "c3api:conc:k:"
	// concSyncInterval 同步周期：视图滞后真值 ≤1 tick（spec §1.2 漂移上界）。
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
	// concViewStale 视图级陈旧阈值（gate 判定侧）：at 超龄即整视图 fail-open。
	concViewStale = 4 * time.Second
)

// concSnap 单对象聚合快照（spec §1.2 结构体定义）。
type concSnap struct {
	total    int64     // Σ 新鲜实例的在途报告
	selfLast int64     // 自身本次上报值（判定精化用）
	at       time.Time // 构建时刻（视图级新鲜度判定）
}

// clusterView 集群并发视图（gate 第二 atomic 快照）：per-layer 直键免结构体哈希，
// 对齐 gateSnapshot 形态。每 tick 全量重建换入（条目数 = 本实例活跃受限对象数，
// 无 clone 需要）；整体可丢可重建、无在途状态。
type clusterView struct {
	users map[int64]concSnap // user_id → 聚合快照
	keys  map[int64]concSnap // key_id → 聚合快照
}

// concSyncStats ops 观测快照（handler.StatsProvider 形态，spec conc-sync-ops-stats）。
// json 键清单同步义务：openapi.yaml WorkerStatus.stats description + web locales
// ops.stats.*（前端缺 key 兜底显示原始字段名）。
type concSyncStats struct {
	LastTickOk        bool  `json:"last_tick_ok"`
	ConsecutiveErrors int64 `json:"consecutive_errors"`
	TrackedEntries    int64 `json:"tracked_entries"` // clusterView 条目数（users+keys；nil 视图=0）
}

// Stats worker 观测（协调面冻结可见性）：fail-open 静默退化时这是运维面唯一
// 痕迹——视图停止换入则 last_tick_ok 翻 false、consecutive_errors 增长。
func (w *ConcSyncWorker) Stats() any {
	errs := w.errs.Load()
	var tracked int64
	if cv := w.gate.cluster.Load(); cv != nil {
		tracked = int64(len(cv.users) + len(cv.keys))
	}
	return concSyncStats{
		LastTickOk:        errs == 0,
		ConsecutiveErrors: errs,
		TrackedEntries:    tracked,
	}
}

// concTarget 本 tick 的一个上报对象（受限层级且在途 > 0）。
type concTarget struct {
	rkey  string // Redis HASH 键
	id    int64  // user_id 或 key_id
	val   int64  // 本地当前在途值（本次上报值）
	isKey bool   // true=key 层，false=user 层
}

// ConcSyncWorker 并发门双向同步 worker：实现 worker.Worker。客户端经 main 注入
// （pkg/redisx 单构造点纪律，本包不自建连接）；gate 引用取自 Auth（同包直访
// 私有字段，无需导出接口）。nil client = Start no-op（测试/降级形态零专门分支）。
type ConcSyncWorker struct {
	client *redis.Client
	self   string           // 实例 ID（main instanceSrc 产物，与 NOTIFY Src / discovery 同源）
	auth   *Auth            // 受限层级元数据源（keys 快照）
	gate   *concurrencyGate // 计数器读 + clusterView 换入点
	log    *logx.Logger

	errs      atomic.Int64 // 连续 tick 失败数（恢复归零；测试确定性信号）
	lastWarn  atomic.Int64 // 上次 Warn unixnano（仅 sync goroutine 读写）
	startOnce sync.Once
	stopOnce  sync.Once
	cancel    context.CancelFunc
	done      chan struct{}
}

// NewConcSyncWorker 构造（零副作用：Start 才起 goroutine，Close 未 Start 时安全）。
func NewConcSyncWorker(auth *Auth, client *redis.Client, self string, log *logx.Logger) *ConcSyncWorker {
	return &ConcSyncWorker{client: client, self: self, auth: auth, gate: auth.gate, log: log}
}

// Name worker 名（worker.Worker 契约）。
func (w *ConcSyncWorker) Name() string { return "conc-sync" }

// Start 非阻塞启动同步循环（幂等；自持 ctx，同 discovery baseCtx 惯用法）。
// nil client 直接短路：无视图装配能力即全额本地语义，循环无存在意义。
func (w *ConcSyncWorker) Start(_ context.Context) error {
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

// Close 幂等优雅停机：停 tick 即完事（spec §1.5 协调态可丢、无排空义务——
// 在途字段由 ts 新鲜度 ≤4s 出局、整键 EXPIRE 自灭，无清理命令）。
func (w *ConcSyncWorker) Close(ctx context.Context) error {
	w.stopOnce.Do(func() {
		if w.cancel == nil {
			return // 未 Start 过：Close 安全 no-op（worker 契约）
		}
		w.cancel()
		select {
		case <-w.done:
		case <-ctx.Done():
			if w.log != nil {
				w.log.Warn("conc-sync: loop stop timed out", logx.Error(ctx.Err()))
			}
		}
	})
	return nil
}

// collect 收集上报范围（spec §1.2 上行）：受限层级（UserMaxConc>0 / KeyMaxConc>0）
// 且本地在途 > 0 的对象。元数据读 auth.keys 快照（RLock 内拷贝所需字段），在途
// 值现读 gate 计数器原子。多 key 同用户只报一次（user 层按 uid 聚合计数）。
func (w *ConcSyncWorker) collect() []concTarget {
	a := w.auth
	a.mu.RLock()
	metas := make([]domain.KeyMeta, 0, len(a.keys))
	for _, m := range a.keys {
		metas = append(metas, m)
	}
	a.mu.RUnlock()

	snap := w.gate.store.Load()
	targets := make([]concTarget, 0, len(metas))
	seenU := make(map[int64]bool, len(metas))
	for _, m := range metas {
		if m.UserMaxConc > 0 && !seenU[m.UserID] {
			seenU[m.UserID] = true
			if c := snap.users[m.UserID]; c != nil {
				if v := c.Load(); v > 0 {
					targets = append(targets, concTarget{
						rkey: concUserPrefix + strconv.FormatInt(m.UserID, 10), id: m.UserID, val: v,
					})
				}
			}
		}
		if m.KeyMaxConc > 0 {
			if c := snap.keys[m.KeyID]; c != nil {
				if v := c.Load(); v > 0 {
					targets = append(targets, concTarget{
						rkey: concKeyPrefix + strconv.FormatInt(m.KeyID, 10), id: m.KeyID, val: v, isKey: true,
					})
				}
			}
		}
	}
	return targets
}

// tick 单条 pipeline 双向同步（spec §1.2）：上行 HSET(field=self,
// value="<L> <now_ms>") + EXPIRE 续期；下行同 pipeline 对相同 key 集 HGETALL
// （pipeline 内命令按序执行，HGETALL 必见自身刚写的字段）→ 过滤新鲜字段聚合 →
// 全量重建新视图 → atomic 换入。失败冻结旧视图（不 Store），视图自然陈旧后
// gate 判定 fail-open 全额本地。
func (w *ConcSyncWorker) tick(ctx context.Context) {
	targets := w.collect()
	if len(targets) == 0 {
		return // 无活跃受限对象：无键可报亦无可聚合，跳过（非成功非失败）
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
	view := &clusterView{
		users: make(map[int64]concSnap, len(targets)),
		keys:  make(map[int64]concSnap, len(targets)),
	}
	for i, t := range targets {
		snap := concSnap{selfLast: t.val, at: time.Now()} // selfLast = 本次上报值（spec §1.2 精化）
		for _, raw := range gets[i].Val() {
			inflight, ts, ok := parseConcValue(raw)
			if !ok || ts < nowMS-int64(concFieldStale/time.Millisecond) {
				continue // 畸形/陈旧字段出局（死亡实例 ≤4s 自愈）
			}
			snap.total += inflight
		}
		if t.isKey {
			view.keys[t.id] = snap
		} else {
			view.users[t.id] = snap
		}
	}
	w.gate.cluster.Store(view)
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
func (w *ConcSyncWorker) onTickError(err error) {
	w.errs.Add(1)
	if w.log == nil {
		return
	}
	if time.Since(time.Unix(0, w.lastWarn.Load())) < concWarnThrottle {
		return
	}
	w.lastWarn.Store(time.Now().UnixNano())
	w.log.Warn("conc-sync: pipeline failed, freezing last cluster view",
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

// concAllows 超份额借位判定（spec §1.3 视图判定；纯内存两原子读 + map 查找，
// 零远程零错误分支零新增锁）：
//
//	effective = total − selfLast + L_now   —— 剔除自身滞后报告、代入本地实时值
//	放行 ⟺ effective < limit
//
// 无视图 / 条目缺失 / 视图陈旧 → fail-open 按「全额 limit」本地判定
// （lnow ≤ limit）= 引入 Redis 前现状语义。并发是建议性协调态，Redis 故障在此
// 降维成视图新鲜度布尔——#37 P1 的额度热路径复核特权（资金语义）在此无必要。
// 放行时 effective 含 L_now ⇒ 本地计数 < 真上限恒成立（借用不破真上限兜底；
// key 层随后仍以真上限 CAS 兜底占用，竞态失败按保守多拒处理）。
func (g *concurrencyGate) concAllows(isKey bool, id, limit, lnow int64) bool {
	cv := g.cluster.Load()
	if cv == nil {
		return lnow <= limit // worker 未装配 / 尚无首个 tick：全额本地语义
	}
	m := cv.users
	if isKey {
		m = cv.keys
	}
	snap, ok := m[id]
	if !ok || time.Since(snap.at) >= concViewStale {
		return lnow <= limit // 条目缺失 / 视图陈旧：fail-open 全额本地
	}
	return snap.total-snap.selfLast+lnow < limit
}
