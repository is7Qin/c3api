// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// KeyLoader 由 repository.KeyRepo 实现（keys 独立表鉴权快照）。
type KeyLoader interface {
	LoadKeys(ctx context.Context) (map[string]domain.KeyMeta, error)
}

// UserStatusLoader 由 repository.UserRepo 实现（RequireJWT 用户状态 + adminAuth
// 快照 role 校验的数据源）。
type UserStatusLoader interface {
	LoadUsers(ctx context.Context) (map[int64]domain.UserSnapshot, error)
}

// ManagementKeyLoader 由 repository.ManagementKeyRepo 实现（管理 key mk-
// 鉴权快照数据源，spec 2026-10-09）。
type ManagementKeyLoader interface {
	LoadManagementKeys(ctx context.Context) (map[string]domain.ManagementKeyMeta, error)
}

// Auth 鉴权快照：key_raw（明文）→ KeyMeta（含归属用户门禁字段）+ 用户快照表
// （status+role）+ 两级并发/额度内存计数（gate）。热路径零 DB、零 per-request
// 锁（RWMutex 读多写少，规格 §10.3）。用户变更（禁用/降权/并发/额度调整）
// 走 invalidate 回调 → Reload 全量刷新，JWT 24h 长时效仅作快照
// 失效后的最终兜底。
type Auth struct {
	loader     KeyLoader
	users      UserStatusLoader
	mgmtLoader ManagementKeyLoader // 管理 key 数据源（NewAuth 构造注入；nil = 不加载）
	log        *logx.Logger
	mu         sync.RWMutex
	keys       map[string]domain.KeyMeta
	states     map[int64]domain.UserSnapshot
	mgmt       map[string]domain.ManagementKeyMeta
	gate       *concurrencyGate

	// 生成计数器（评审 2026-10-09，A4）：keys/states/mgmt 的本地增量变更
	// （Upsert/Delete/UpsertUser/RemoveUser/UpsertManagementKey/DeleteManagementKey）
	// 在 a.mu 下逐个自增。Reload 在 DB 读前捕获、换引用前比对：捕获后被推进 ⇒
	// 本地变更比 DB 快照更新 ⇒ 保留 live map（否则过期快照会复活刚禁用/删除的 key）。
	keysGen   uint64
	statesGen uint64
	mgmtGen   uint64
}

// NewAuth 构造鉴权快照（空表——首载统一由快照注册表 ReloadAll 承担，单一启动
// 入口，消灭"构造即载 + 注册表再刷"双重加载冗余；构造到首刷之间无请求流量，
// 见 main 装配序）。
func NewAuth(loader KeyLoader, users UserStatusLoader, mgmtLoader ManagementKeyLoader, log *logx.Logger, quotaEnabled bool) *Auth {
	a := &Auth{
		loader:     loader,
		users:      users,
		mgmtLoader: mgmtLoader,
		log:        log,
		keys:       make(map[string]domain.KeyMeta),
		states:     make(map[int64]domain.UserSnapshot),
		mgmt:       make(map[string]domain.ManagementKeyMeta),
		gate:       newConcurrencyGate(log, quotaEnabled),
	}
	// 复核 DB 读自装配：生产 loader（repository.KeyRepo）同时实现 QuotaUsedReader；
	// 测试 fake 未实现 → 无复核能力（预算耗尽即 429，单实例现状语义）。
	if r, ok := loader.(QuotaUsedReader); ok {
		a.gate.setReclaimer(r)
	}
	return a
}

// Reload 全量刷新鉴权快照（注册表首刷/周期 auth-sync/用户变更 invalidate）：
// keys 元数据 + 用户状态 + 门禁计数器（在途值跨 reload 继承）。
// 失败必打 Warn（含调用方是否忽略错误——invalidate 回调等吞错路径）：加载
// 失败若被忽略，快照保持旧值/空表 → 鉴权全部 401 或用旧 key 放行，静默恶化
// （IN 超限事故的"运行中静默失败"形态即此类）。
func (a *Auth) Reload(ctx context.Context) error {
	// 先捕获生成计数器与 mgmt loader（持 RLock，快进快出），再在锁外做 DB I/O。
	// 关键：**不得** 持 a.mu 跨 DB I/O（会阻塞热路径鉴权）；捕获后发生的本地变更
	// 会推进对应 gen，换引用时据此判定谁更新（见下）。
	a.mu.RLock()
	keysGen := a.keysGen
	statesGen := a.statesGen
	mgmtGen := a.mgmtGen
	mgmtLoader := a.mgmtLoader
	a.mu.RUnlock()

	m, err := a.loader.LoadKeys(ctx)
	if err != nil {
		a.logWarn("auth snapshot reload failed (load keys)", err)
		return err
	}
	u, err := a.users.LoadUsers(ctx)
	if err != nil {
		a.logWarn("auth snapshot reload failed (load users)", err)
		return err
	}
	// 管理 key 快照：仅当 loader 已注入（NewAuth 构造参数）才加载；失败
	// Warn 且保留旧快照（与 keys/users 同款——绝不静默清空）。
	var mg map[string]domain.ManagementKeyMeta
	if mgmtLoader != nil {
		mg, err = mgmtLoader.LoadManagementKeys(ctx)
		if err != nil {
			a.logWarn("auth snapshot reload failed (load management keys)", err)
			return err
		}
	}
	a.mu.Lock()
	// 逐 map 按生成计数器判定是否换引用：仅当捕获后该 map 无本地增量变更（gen 未变）
	// 才用 DB 快照替换；否则保留 live map。理由（评审 MAJOR，A4）：本地变更
	// （disable/delete）已即时落到 live map，若此处仍换成"变更前读取的" DB 快照，
	// 会把刚禁用/删除的 key 复活为可用凭据。本地变更赢，下一次 reload 收敛。
	if a.keysGen == keysGen {
		a.keys = m
	}
	if a.statesGen == statesGen {
		a.states = u
	}
	// keys/states/mgmt 作为逻辑快照在同一把 a.mu 下换引用——禁止为 mgmt 引入独立
	// 锁/独立替换（否则与 key/user 快照不一致）。
	if mgmtLoader != nil && a.mgmtGen == mgmtGen {
		a.mgmt = mg
	}
	// gate 从当前 live keys 重建（上面若已换引用则 == m，否则为含本地增量的 live
	// map）：必须持锁——Upsert/Delete 并发写 a.keys 同 map 会触发 "concurrent map
	// iteration and map write" fatal（上机 128 并发建用户实测崩溃；map 赋值只换引用，
	// 写仍落 m）。
	a.gate.reload(a.keys)
	a.mu.Unlock()
	return nil
}

func (a *Auth) logWarn(msg string, err error) {
	if a.log != nil {
		a.log.Warn(msg, logx.Error(err))
	}
}

// Upsert 增量刷新单个 key（key 创建/轮换/更新后调用；门禁计数器同步）。
func (a *Auth) Upsert(raw string, meta domain.KeyMeta) {
	a.mu.Lock()
	a.keys[raw] = meta
	a.keysGen++ // 本地变更 → Reload 换引用前据此判定保留 live map
	a.mu.Unlock()
	a.gate.upsert(meta)
}

func (a *Auth) Delete(raw string) {
	a.mu.Lock()
	meta, ok := a.keys[raw]
	delete(a.keys, raw)
	a.keysGen++
	a.mu.Unlock()
	if ok {
		a.gate.delete(meta.KeyID)
	}
}

// UpsertUser 增量刷新单个用户状态（本地立即可见，不等去抖窗口）。
// 供 admin 创建用户 / 注册 / 状态变更后本地实例立即对 RequireJWT 可见，
// 消除 200ms 窗口内新建用户 401；远端实例仍经 NOTIFY → 全量 Reload 收敛。
func (a *Auth) UpsertUser(userID int64, snap domain.UserSnapshot) {
	a.mu.Lock()
	a.states[userID] = snap
	a.statesGen++
	a.mu.Unlock()
}

// RemoveUser 增量移除用户（暂未使用，防御性；与 UpsertUser 对称）。
func (a *Auth) RemoveUser(userID int64) {
	a.mu.Lock()
	delete(a.states, userID)
	a.statesGen++
	a.mu.Unlock()
}

// AuthenticateManagement 解析管理面凭证并返回管理 key 快照条目。仅认
// Authorization: Bearer，**要求前缀 mk-**（无前缀直接 false）。前缀先判、失败
// 不回退：token 以 mk- 开头但查表失败/禁用 ⇒ 直接 false（中间件 401），绝不
// 回落 JWT 校验（否则前缀判别不成立，A3 由此时立）。owner 状态由中间件经
// UserSnapshot 统一校验，fail-closed。
func (a *Auth) AuthenticateManagement(r *http.Request) (domain.ManagementKeyMeta, bool) {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return domain.ManagementKeyMeta{}, false
	}
	raw := strings.TrimPrefix(h, "Bearer ")
	if !strings.HasPrefix(raw, "mk-") {
		return domain.ManagementKeyMeta{}, false
	}
	a.mu.RLock()
	meta, ok := a.mgmt[raw]
	a.mu.RUnlock()
	if !ok || meta.Status != domain.ManagementKeyStatusActive {
		return domain.ManagementKeyMeta{}, false
	}
	return meta, true
}

// UpsertManagementKey 增量刷新单个管理 key（创建/启用/禁用后调用；禁用即时
// 401——查表按 Status==active 门控）。懒初始化 map（loader 未注入时也安全）。
func (a *Auth) UpsertManagementKey(raw string, meta domain.ManagementKeyMeta) {
	a.mu.Lock()
	if a.mgmt == nil {
		a.mgmt = make(map[string]domain.ManagementKeyMeta)
	}
	a.mgmt[raw] = meta
	a.mgmtGen++
	a.mu.Unlock()
}

// DeleteManagementKey 增量移除单个管理 key（删除后本实例即时 401）。
func (a *Auth) DeleteManagementKey(raw string) {
	a.mu.Lock()
	delete(a.mgmt, raw)
	a.mgmtGen++
	a.mu.Unlock()
}

// UserSnapshot 用户快照（RequireJWT 状态校验 + adminAuth 快照 role 校验共用；
// 用户变更走 invalidate → Reload，不用 DB 直查）。单次查找同时取 status+role
// （热路径零分配）。
func (a *Auth) UserSnapshot(userID int64) (domain.UserSnapshot, bool) {
	a.mu.RLock()
	s, ok := a.states[userID]
	a.mu.RUnlock()
	return s, ok
}

// Authenticate 解析网关 key 并返回 KeyMeta。兼容两种客户端口径：
// OpenAI 客户端发 Authorization: Bearer；Anthropic 官方 SDK / Claude Code
// 发 x-api-key 头。两者同时提供时以 Authorization 为准。
// key 或归属用户被禁用 → 快照直接拒绝（401，即时失效）。
// 快照 map key = 明文，等值直查（零哈希）；meta 无 key 字符串字段——
// 鉴权失败日志天然不落明文。
func (a *Auth) Authenticate(r *http.Request) (domain.KeyMeta, bool) {
	raw := ""
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		raw = strings.TrimPrefix(h, "Bearer ")
	} else if h := r.Header.Get("x-api-key"); h != "" {
		raw = h
	}
	if raw == "" {
		return domain.KeyMeta{}, false
	}
	a.mu.RLock()
	meta, ok := a.keys[raw]
	a.mu.RUnlock()
	if !ok {
		return domain.KeyMeta{}, false
	}
	if meta.KeyStatus != domain.KeyStatusActive || meta.UserStatus != domain.UserStatusActive {
		return domain.KeyMeta{}, false
	}
	return meta, true
}

// SetInstancesProvider 注入集群实例数 N 提供者（多实例预算分摊；discovery
// 装配——main 装配点，spec 2026-08-25-redis-instance-discovery-design §2.2）。
// 注入即触发预算重算（幂等 reload，在途值继承）；此后 N 在每次预算分配现读，
// 心跳计数变化 ≤1 tick 天然生效。
func (a *Auth) SetInstancesProvider(p InstancesProvider) {
	a.gate.SetInstancesProvider(p)
	a.mu.Lock()
	a.gate.reload(a.keys) // 预算按新 N 即时重分配
	a.mu.Unlock()
}

// --- 门禁（内存原子；热路径零 DB 零锁） ---

// Acquire 两级并发门禁：user → key 依次 CAS 抢占；key 失败回滚 user 计数
// （防泄漏）。返回已 acquire 层级位掩码（release 仅释放已 acquire
// 层级）。未设置上限（max=0）或计数器缺失（跨 reload 竞态窗口）→ 该层跳过。
func (a *Auth) Acquire(meta domain.KeyMeta) (int, bool) {
	return a.gate.acquire(meta)
}

// Release 释放并发计数（仅释放 acquire 返回的层级；跨 reload 命中新快照的
// 继承计数，与 scheduler Release 同语义）。
func (a *Auth) Release(meta domain.KeyMeta, level int) {
	a.gate.release(meta, level)
}

// QuotaExhausted 额度检查：本地预算快读（零锁零 DB）；预算耗尽触发 DB 复核
// 认领（§3.2——复核成功续预算继续放行，复核确认真尽才 429）。检查在并发
// acquire 之前（评审提醒①：失败无并发槽副作用）；未设置额度 key 短路零成本。
func (a *Auth) QuotaExhausted(meta domain.KeyMeta) bool {
	return a.gate.quotaExhausted(meta)
}

// DeductQuota 请求结束扣减（后扣模型；usage 已知；无额度 key 无计数器 → no-op）。
func (a *Auth) DeductQuota(keyID, cost int64) int64 {
	return a.gate.deductQuota(keyID, cost)
}

// InFlightUsers 门禁在途并发只读快照（/api/admin/users-top 端点用；spec 2026-08-14
// gateSnapshot.users 未导出，经本访问器只读暴露）：gateSnapshot 整体
// 原子换入换出（reload/upsert 重建，不可变），store.Load() 零锁取当前引用后
// 遍历 + 原子读各计数器 → map[int64]int64 拷贝（含 0——过滤由调用方做）。
// 冷面调用（管理端聚合，不涉请求热路径）；多实例部署下为本实例在途计数。
func (a *Auth) InFlightUsers() map[int64]int64 {
	snap := a.gate.store.Load()
	out := make(map[int64]int64, len(snap.users))
	for uid, c := range snap.users {
		if c != nil {
			out[uid] = c.Load()
		}
	}
	return out
}
