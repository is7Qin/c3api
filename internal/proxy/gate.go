// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// InstancesProvider 集群实例数 N 提供者（service.Service 实现 ClusterInstances；
// 多实例预算分摊 #14 §3.1——N 存 DB settings，所有实例同源，config 文件可漂移）。
// nil（未装配）按 N=1（单实例语义）。N 变更经 settings NOTIFY 传播，装配侧再次
// 调用 SetInstancesProvider 即触发预算重算（§3.4）。
type InstancesProvider interface {
	ClusterInstances() int
}

// QuotaUsedReader 预算复核的 DB 只读接口（repository.KeyRepo 实现）：预算耗尽
// 时读 key 当前已用额度（DB 权威值——usage.Recorder 批量增量回写 quota_used，
// 复核时刻分配预算用）。热路径不调（仅复核慢路径）。
type QuotaUsedReader interface {
	QuotaUsed(ctx context.Context, keyID int64) (int64, error)
}

// concurrencyGate 两级并发/额度内存门禁：user/key 在途计数 + key 额度本地预算
// （#14 §3.2 多实例分摊）。快照原子换入换出（reload 时重建），在途/已扣值跨
// reload 继承（复用 scheduler reload 继承教训：跨 reload 的 Release/deduct 命中
// 新快照的继承值，计数不丢不拉负）。热路径零锁零 DB（仅 atomic 读）——预算
// 耗尽才触发 DB 复核（慢路径单飞，见 reclaim）。
//
// 无额度 key（quota=0）不建 quota 条目——HasQuota 短路：检查与扣减均走
// 计数器存在性，路径与现状（无门禁）成本相当（1 次快照读 + map 查）。
type concurrencyGate struct {
	store atomic.Pointer[gateSnapshot]
	// reclaimer 复核 DB 读（NewAuth 从 loader 类型断言注入；nil = 无复核能力，
	// 预算耗尽直接 429——与单实例现状语义一致，仅测试/未装配形态）。
	reclaimer QuotaUsedReader
	// instances N 提供者（装配期 SetInstancesProvider 注入；nil → N=1）。
	instances atomic.Pointer[InstancesProvider]
	log       *logx.Logger
}

type gateSnapshot struct {
	users  map[int64]*atomic.Int64 // user_id → 在途请求数
	keys   map[int64]*atomic.Int64 // key_id → 在途请求数
	quotas map[int64]*keyQuota     // key_id → 额度预算状态（无额度 key 无条目）
}

// keyQuota 单 key 额度状态（多实例本地预算模型 #14 §3.2 + #37 P1 收敛修正）：
//
//	budget = consumed + ceil(剩余额/N)   —— 复核时刻分配（reload/upsert/耗尽复核）
//	Allow  = consumed < budget          —— 热路径两原子读，零锁零 DB
//	耗尽   → 触发 DB 复核认领：剩余额扣本地未反映消耗后 > 0 → budget 重分配
//	          继续放行；≤ 0 → 429；DB 错 → Warn + 本请求放行 + 退避 10s
//	          （软门禁语义：放行不产生错计费，扣费恒为条件 UPDATE 精确，
//	          见 billing flusher）
//
// 收敛（#37 P1，击穿 §3.2 误差上界的修复）：quota_used 由 usage.Recorder 每
// quota_flush_interval 批写一次，两次回写间复核读到的 DB 值恒定——若每次复核
// 都重新分配 ceil(remaining/N)，复核循环会无限续额（N=2 压测实证超跑 14 倍）。
// 复核认领因此扣除本地已消耗但 DB 未反映的部分（unreported，见 reclaim）：
// 本实例总放行 ≤ quota - used(DB) + 基线差（≤1 个 flush 窗口）≤ quota（评审
// I-1：与初始份额无关）。429 点 = remainingEff ≤ 0（剩余额扣本地未反映消耗后
// 不足）。保守窗口（评审 I-2）：本实例上次复核后 flush 而基线未前移时，
// unreported 会重复计入 DB 已回写量 → remainingEff 先于真尽触 0 → 提前 429，
// 欠分配非超分配，下次 reload（R1 兜底 ≤60s）前移基线自愈。旧"允许 ≈1 flush
// 窗口超跑"语义（N=1 评审 I-1 注记）随本修正收紧；扣费恒条件 UPDATE 精确的
// 软门禁兜底不变（放行不产生错计费）。
//
// 单飞：同 key 并发复核只允许一个进 DB，其余按旧预算判定（复核窗口 ≈ 1 次 DB
// 往返，额度边缘的瞬时 429 可接受）。所有字段原子——复核与 reload/upsert 重建
// 并发安全（命中旧快照的复核写旧对象，随快照换出作废，无跨快照污染）。
type keyQuota struct {
	consumed   atomic.Int64 // 本地已消耗 token（单调递增；跨 reload 继承，同现状）
	budget     atomic.Int64 // 本地预算绝对上限；consumed >= budget → 触发复核
	reclaiming atomic.Bool  // 复核单飞标志（CAS 抢占，defer 释放）
	exhausted  atomic.Bool  // DB 复核确认真尽（此后 429 短路；reload/upsert 重建时重置）
	retryAt    atomic.Int64 // 复核失败退避截止（unix nano；防 DB 错复核风暴）
	// quotaUsedAtReclaim 复核基准：上次复核/reload 读到的 DB quota_used（全局值，
	// 含所有实例已回写消耗）。unreported = consumed - quotaUsedAtReclaim 衡量
	// "本实例已消耗但 DB 未反映"的量——复核认领时从 remaining 扣除，防止 DB 滞后
	// （usage.Recorder 每 quota_flush_interval 批写）期间复核循环无限续额
	// （压测实证：N=2 单 key quota=20000 超跑 14 倍，2026-08-10）。
	quotaUsedAtReclaim atomic.Int64
}

func newConcurrencyGate(log *logx.Logger) *concurrencyGate {
	g := &concurrencyGate{log: log}
	g.store.Store(&gateSnapshot{
		users:  make(map[int64]*atomic.Int64),
		keys:   make(map[int64]*atomic.Int64),
		quotas: make(map[int64]*keyQuota),
	})
	return g
}

// setReclaimer 注入复核 DB 读（NewAuth 从 loader 类型断言；构造期调用，不可变）。
func (g *concurrencyGate) setReclaimer(r QuotaUsedReader) { g.reclaimer = r }

// SetInstancesProvider 注入集群实例数 N（装配期；nil 清空 → N=1）。N 在每次
// 预算分配（reload/upsert/复核）现读，下次分配即生效；由 Auth.SetInstancesProvider
// 触发 reload 完成即时重算（#14 §3.4）。
func (g *concurrencyGate) SetInstancesProvider(p InstancesProvider) {
	g.instances.Store(&p)
}

// instancesN 当前集群实例数（N ≥ 1；provider 缺失/非法值 → 1）。
func (g *concurrencyGate) instancesN() int {
	if p := g.instances.Load(); p != nil && *p != nil {
		if n := (*p).ClusterInstances(); n > 0 {
			return n
		}
	}
	return 1
}

// reload 从鉴权快照重建计数器；在途值跨 reload 继承（旧快照与新快照共有的
// user/key 计数平移——跨 reload 的 Release/deduct 命中新快照继承值）。
// 额度预算按最新快照重新分配（#14 §3.3：key CRUD → NOTIFY → 全实例 Reload →
// 预算按新 quota_used 重算）。
func (g *concurrencyGate) reload(metas map[string]domain.KeyMeta) {
	snap := &gateSnapshot{
		users:  make(map[int64]*atomic.Int64, len(metas)),
		keys:   make(map[int64]*atomic.Int64, len(metas)),
		quotas: make(map[int64]*keyQuota),
	}
	old := g.store.Load()
	for _, meta := range metas {
		if _, ok := snap.users[meta.UserID]; !ok {
			c := &atomic.Int64{}
			if o, ok := old.users[meta.UserID]; ok {
				c.Store(o.Load()) // 在途继承
			}
			snap.users[meta.UserID] = c
		}
		kc := &atomic.Int64{}
		if o, ok := old.keys[meta.KeyID]; ok {
			kc.Store(o.Load())
		}
		snap.keys[meta.KeyID] = kc
		if meta.HasQuota {
			q := &keyQuota{}
			if o, ok := old.quotas[meta.KeyID]; ok {
				q.consumed.Store(o.consumed.Load()) // 在途额度继承（评审提醒②）
			} else {
				q.consumed.Store(meta.QuotaUsed)
			}
			g.allocBudget(q, meta) // 预算按最新快照重分配（§3.2 复核时刻）
			snap.quotas[meta.KeyID] = q
		}
	}
	g.store.Store(snap)
}

// upsert 增量刷新单 key 计数器（创建/轮换后；已存在条目不动在途值；
// 低频管理路径，重建快照可接受）。预算随最新 meta 重分配（额度调整即时生效）；
// 额度取消（quota→0）→ 门禁条目移除，不再拦截。
func (g *concurrencyGate) upsert(meta domain.KeyMeta) {
	old := g.store.Load()
	snap := &gateSnapshot{
		users:  cloneCounters(old.users),
		keys:   cloneCounters(old.keys),
		quotas: cloneCounters(old.quotas),
	}
	if _, ok := snap.users[meta.UserID]; !ok {
		snap.users[meta.UserID] = &atomic.Int64{}
	}
	if _, ok := snap.keys[meta.KeyID]; !ok {
		snap.keys[meta.KeyID] = &atomic.Int64{}
	}
	if meta.HasQuota {
		q, ok := snap.quotas[meta.KeyID]
		if !ok {
			q = &keyQuota{}
			q.consumed.Store(meta.QuotaUsed)
			snap.quotas[meta.KeyID] = q
		}
		g.allocBudget(q, meta) // 配额调整即时生效（在途 consumed 不动）
	} else {
		delete(snap.quotas, meta.KeyID)
	}
	g.store.Store(snap)
}

// delete 移除 key 计数器与额度条目（user 计数保留——紧随其后的 invalidate
// → Reload 会按剩余 key 重建；删除到 0 的用户在下一次 reload 收敛）。
func (g *concurrencyGate) delete(keyID int64) {
	old := g.store.Load()
	if _, ok := old.keys[keyID]; !ok {
		return
	}
	snap := &gateSnapshot{
		users:  cloneCounters(old.users),
		keys:   cloneCounters(old.keys),
		quotas: cloneCounters(old.quotas),
	}
	delete(snap.keys, keyID)
	delete(snap.quotas, keyID)
	g.store.Store(snap)
}

func cloneCounters[T any](m map[int64]*T) map[int64]*T {
	out := make(map[int64]*T, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// acquire 抢占门禁槽位。返回已 acquire 层级位掩码（1=user、2=key、3=两者；
// release 按位释放——user 位含义 = "user 计数已 +1"，release 按位减恰一次）。
// user 层计数与限流解耦（spec 2026-08-15：实时并发排行数据源 = users 计数器，
// 不限并发的用户也计数）——无条件 Add(1)（排行可见），限流条件化：UserMaxConc
// > 0 时 Load 检查超限 → Add(-1) 回滚 + 429（热路径无 CAS 循环：不限时每请求
// 1 次原子 Add，限流时 2 次原子操作）。瞬时超限窗口可接受（展示近似，稳态由
// 回滚保证不超限）：边界竞争多个请求同时观察超限并回滚 → 保守双拒（多拒不放
// 行），方向安全，同 keyQuota 软门禁先例。
// 两步回滚（评审 I-3）：user 成功 key 失败 → 复原 user 计数再返回失败，防泄漏。
// 回滚竞态闭合：计数器为单一原子总量，每个 -1 与同一 goroutine 的 +1 配对
// （acquire 回滚或 release 按 level 位恰一次）→ 恒非负、N 并发全回滚净 0。
func (g *concurrencyGate) acquire(meta domain.KeyMeta) (int, bool) {
	snap := g.store.Load()
	level := 0
	if c, ok := snap.users[meta.UserID]; ok && c != nil {
		c.Add(1) // 无条件计数（排行数据源；不限并发也计数）
		level |= 1
		if meta.UserMaxConc > 0 && c.Load() > int64(meta.UserMaxConc) {
			c.Add(-1) // 回滚计数（稳态不超限）
			level &^= 1
			return 0, false // user 层超限 → 429（计数已复原，无占用）
		}
	}
	if meta.KeyMaxConc > 0 {
		if c, ok := snap.keys[meta.KeyID]; ok && c != nil {
			if !casInc(c, meta.KeyMaxConc) {
				if level&1 != 0 {
					if uc, ok := snap.users[meta.UserID]; ok {
						uc.Add(-1) // 回滚 user 计数
					}
				}
				return 0, false
			}
			level |= 2
		}
	}
	return level, true
}

func (g *concurrencyGate) release(meta domain.KeyMeta, level int) {
	if level == 0 {
		return
	}
	snap := g.store.Load()
	if level&1 != 0 {
		if c, ok := snap.users[meta.UserID]; ok && c != nil {
			c.Add(-1)
		}
	}
	if level&2 != 0 {
		if c, ok := snap.keys[meta.KeyID]; ok && c != nil {
			c.Add(-1)
		}
	}
}

// quotaExhausted 额度检查：本地预算快读（两原子读，零锁零 DB）→ 预算耗尽时
// 触发 DB 复核认领（#14 §3.2：不直接 429——先复核，有剩余重分配预算继续放行）。
// 复核是慢路径（DB 读）但单飞去重 + 原子更新 budget：热路径读永不阻塞。
// 无额度 key 短路 false。
func (g *concurrencyGate) quotaExhausted(meta domain.KeyMeta) bool {
	if !meta.HasQuota {
		return false
	}
	snap := g.store.Load()
	q, ok := snap.quotas[meta.KeyID]
	if !ok || q == nil {
		return meta.QuotaUsed >= meta.Quota // 无内存计数（新 key/竞态窗口）→ 快照值
	}
	if q.exhausted.Load() {
		return true // 复核确认真尽：429 短路（直到 reload/upsert 重建）
	}
	if q.consumed.Load() < q.budget.Load() {
		return false // 热路径：本地预算充足
	}
	return !g.reclaim(meta, q)
}

// reclaim 预算耗尽后的 DB 复核认领（#14 §3.2 公式 + #37 P1 收敛修正）：
//
//	remaining = quota - quota_used(DB 复核读)
//	unreported = max(0, consumed - quotaUsedAtReclaim)  —— 本地已消耗但 DB
//	              未反映的量（quota_used 每 quota_flush_interval 批写一次，
//	              两次回写间 DB 值恒定；不扣除则每次复核重新分配 full 份额
//	              → 复核循环无限续额，压测实证超跑 14 倍）
//	remaining_eff = remaining - unreported
//	remaining_eff > 0 → budget = consumed + ceil(remaining_eff/N)，放行，
//	                     quotaUsedAtReclaim = 本次 quota_used（基准前移）
//	remaining_eff ≤ 0 → exhausted（真尽，此后 429 短路）
//	DB 错         → Warn + 本请求放行（预算补 1）+ 退避 10s——软门禁语义：
//	                 放行不产生错计费（扣费恒条件 UPDATE，DB 错时同样失败）；
//	                 退避期内预算耗尽按 429，防复核风暴
//	无复核能力    → 429（与单实例现状语义一致）
//
// 收敛论证（#37 P1）：两次回写间 used 恒定、quotaUsedAtReclaim = 上次复核读值，
// 复核循环的每次认领 = ceil((Q - used - max(0, consumed - 基线))/N)，随 consumed
// 单调增长等比收缩 → 本实例总放行收敛 ≤ 初始份额 + 滞后差（滞后差 = 本实例在
// 上次回写后消耗但 DB 未反映的量，≤1 个 flush 窗口）——不复核则不续额，不再是
// 每次复核都全额重发。
//
// 单飞：CAS 抢到才进 DB；同 key 并发到达按当前 budget 判定（复核窗口 ≈ 1 次
// DB 往返，额度边缘瞬时 429 可接受）。复核用独立超时 ctx（不用请求 ctx——
// 请求中途断开不能悬挂 reclaiming 标志，否则该 key 永久 429 直到 reload）。
//
// 缺失 key（ErrNotFound，已删）与瞬时 DB 错同等对待（同上"Warn+放行"策略，
// 评审 I-2）：删除传播存在快照残留期（≤60s，R1 兜底），其间该 key 退避涓流
// 放行 ≤6 笔/60s，可接受；残留条目本身由 Reload 移除收敛。
//
// budget 更新为读-改-写（consumed.Load + Store），并发扣减可能落在两次原子
// 操作之间 → 丢失（lost-update，评审 I-3）。方向保守：budget 偏低 → 更早触发
// 下次复核 → 更早再认领，无超限风险。
//
// 返回 true = 本请求放行。
func (g *concurrencyGate) reclaim(meta domain.KeyMeta, q *keyQuota) bool {
	if !q.reclaiming.CompareAndSwap(false, true) {
		return q.consumed.Load() < q.budget.Load() // 他人复核中：用旧预算判定
	}
	defer q.reclaiming.Store(false)
	if q.retryAt.Load() > time.Now().UnixNano() {
		return false // 复核失败退避期：保守 429（防 DB 风暴）
	}
	if g.reclaimer == nil {
		q.exhausted.Store(true) // 无复核能力：耗尽即 429（仅测试/未装配形态）
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), quotaReclaimTimeout)
	defer cancel()
	used, err := g.reclaimer.QuotaUsed(ctx, meta.KeyID)
	if err != nil {
		q.retryAt.Store(time.Now().Add(quotaReclaimRetry).UnixNano())
		if g.log != nil {
			g.log.Warn("quota budget reclaim failed (read quota_used)", logx.Error(err),
				logx.Int64("key_id", meta.KeyID))
		}
		// 软门禁：DB 错不误伤——本请求放行，预算补 1（退避期内其余到达 429）
		q.budget.Store(q.consumed.Load() + 1)
		return true
	}
	if remaining := meta.Quota - used; remaining > 0 {
		// 复核认领扣除本地未反映消耗（#37 P1 收敛修复）：DB quota_used 每
		// quota_flush_interval 批写一次，两次回写间 used 恒定 → 若不加扣除，
		// 每次复核重新分配 ceil(remaining/N) → 复核循环无限续额（超跑实证）。
		// unreported = consumed - 上次复核基线（本地已消耗但 DB 未反映的量）；
		// remainingEff 扣掉它 → 复核循环收敛（评审 I-1）：每实例独立收敛
		// ≤ quota - used(DB) + 基线差（与初始份额无关）；多实例 + DB 恒滞后
		// 病理形态总量有界 ≈2Q - U（生产 flush 推进 U，总量 ≈ Q + N×flush
		// 窗口滞后）。
		unreported := q.consumed.Load() - q.quotaUsedAtReclaim.Load()
		if unreported < 0 {
			unreported = 0 // 其他实例回写使 DB 值领先于本地 → 无未反映消耗
		}
		if remainingEff := remaining - unreported; remainingEff > 0 {
			q.quotaUsedAtReclaim.Store(used) // 基准前移：本次复核读到的 DB 值
			q.budget.Store(q.consumed.Load() + ceilDiv(remainingEff, int64(g.instancesN())))
			return true
		}
		q.exhausted.Store(true) // 剩余不足覆盖本地未反映消耗 → 真尽（额度边缘）
		return false
	}
	q.exhausted.Store(true) // 额度真尽 → 429（budget 不动，exhausted 短路后续复核）
	return false
}

// allocBudget 复核时刻预算分配（#14 §3.2 公式）：budget = consumed + ceil(remaining/N)。
// remaining = quota - quota_used（快照值；reload/upsert 携带）。remaining ≤ 0 →
// exhausted（真尽，429 短路直到下次重建）。consumed 恒不动（在途纪律）。
// 复核基准 quotaUsedAtReclaim 同步前移 = 快照 quota_used（DB 刷新后 unreported 复位，
// 压测 P1：防止陈旧基准使剩余额被过度扣除）。
func (g *concurrencyGate) allocBudget(q *keyQuota, meta domain.KeyMeta) {
	remaining := meta.Quota - meta.QuotaUsed
	if remaining <= 0 {
		q.budget.Store(q.consumed.Load())
		q.exhausted.Store(true)
		return
	}
	q.quotaUsedAtReclaim.Store(meta.QuotaUsed)
	q.budget.Store(q.consumed.Load() + ceilDiv(remaining, int64(g.instancesN())))
	q.exhausted.Store(false)
}

// deductQuota 请求结束扣减（后扣模型；无额度 key 无条目 → no-op，恒 0）。
func (g *concurrencyGate) deductQuota(keyID, tokens int64) {
	if keyID <= 0 || tokens <= 0 {
		return
	}
	snap := g.store.Load()
	if q, ok := snap.quotas[keyID]; ok && q != nil {
		q.consumed.Add(tokens)
	}
}

// ceilDiv 向上取整除法（§3.2 ceil(remaining/N)）；b ≥ 1（N 恒 ≥ 1），
// a+b-1 溢出仅在 a 接近 MaxInt64 时可能——额度量级远小于此。
func ceilDiv(a, b int64) int64 {
	if a <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

// casInc CAS 循环自增：超过 max 返回 false（不占用）。
func casInc(c *atomic.Int64, max int) bool {
	for {
		cur := c.Load()
		if cur >= int64(max) {
			return false
		}
		if c.CompareAndSwap(cur, cur+1) {
			return true
		}
	}
}

// 复核超时与退避（DB 复核是慢路径：单次上限防慢 DB 拖垮请求；失败退避防
// 复核风暴——DB 错窗口内每 key 至多一次复核）。
const (
	quotaReclaimTimeout = 3 * time.Second
	quotaReclaimRetry   = 10 * time.Second
)
