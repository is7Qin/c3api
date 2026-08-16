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

// UserStatusLoader 由 repository.UserRepo 实现（RequireJWT 用户状态快照）。
type UserStatusLoader interface {
	LoadUsers(ctx context.Context) (map[int64]domain.UserStatus, error)
}

// Auth 鉴权快照：key_raw（明文）→ KeyMeta（含归属用户门禁字段）+ 用户状态表 +
// 两级并发/额度内存计数（gate）。热路径零 DB、零 per-request 锁（RWMutex
// 读多写少，规格 §10.3）。用户状态变更（禁用/并发/额度调整）走 invalidate
// 回调 → Reload 全量刷新（评审 I-2），JWT 24h 长时效仅作快照失效后的
// 最终兜底。
type Auth struct {
	loader KeyLoader
	users  UserStatusLoader
	log    *logx.Logger
	mu     sync.RWMutex
	keys   map[string]domain.KeyMeta
	states map[int64]domain.UserStatus
	gate   *concurrencyGate
}

// NewAuth 构造鉴权快照（空表——首载统一由快照注册表 ReloadAll 承担，单一启动
// 入口，消灭"构造即载 + 注册表再刷"双重加载冗余；构造到首刷之间无请求流量，
// 见 main 装配序）。
func NewAuth(loader KeyLoader, users UserStatusLoader, log *logx.Logger) *Auth {
	a := &Auth{
		loader: loader,
		users:  users,
		log:    log,
		keys:   make(map[string]domain.KeyMeta),
		states: make(map[int64]domain.UserStatus),
		gate:   newConcurrencyGate(log),
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
	a.mu.Lock()
	a.keys = m
	a.states = u
	// gate.reload 迭代 m（= a.keys 当前引用）：必须持锁——Upsert/Delete
	// 并发写 a.keys 同 map 会触发 "concurrent map iteration and map write"
	// fatal（上机 128 并发建用户实测崩溃；map 赋值只换引用，写仍落 m）。
	a.gate.reload(m)
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
	a.mu.Unlock()
	a.gate.upsert(meta)
}

func (a *Auth) Delete(raw string) {
	a.mu.Lock()
	meta, ok := a.keys[raw]
	delete(a.keys, raw)
	a.mu.Unlock()
	if ok {
		a.gate.delete(meta.KeyID)
	}
}

// UserStatus 用户状态快照（RequireJWT 校验用；用户变更走 invalidate → Reload，
// 不用 DB 直查）。
func (a *Auth) UserStatus(userID int64) (domain.UserStatus, bool) {
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

// SetInstancesProvider 注入集群实例数 N 提供者（#14 多实例预算分摊；svc 构造后
// 装配——main 装配点，T3a 接线：auth.SetInstancesProvider(svc)）。N 变更
// （settings NOTIFY）后再次调用即触发预算重算（幂等 reload，在途值继承，§3.4）。
func (a *Auth) SetInstancesProvider(p InstancesProvider) {
	a.gate.SetInstancesProvider(p)
	a.mu.Lock()
	a.gate.reload(a.keys) // 预算按新 N 即时重分配
	a.mu.Unlock()
}

// --- 门禁（内存原子；热路径零 DB 零锁） ---

// Acquire 两级并发门禁：user → key 依次 CAS 抢占；key 失败回滚 user 计数
// （评审 I-3：防泄漏）。返回已 acquire 层级位掩码（release 仅释放已 acquire
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
// 认领（#14 §3.2——复核成功续预算继续放行，复核确认真尽才 429）。检查在并发
// acquire 之前（评审提醒①：失败无并发槽副作用）；未设置额度 key 短路零成本。
func (a *Auth) QuotaExhausted(meta domain.KeyMeta) bool {
	return a.gate.quotaExhausted(meta)
}

// DeductQuota 请求结束扣减（后扣模型；usage 已知；无额度 key 无计数器 → no-op）。
func (a *Auth) DeductQuota(keyID, tokens int64) {
	a.gate.deductQuota(keyID, tokens)
}

// InFlightUsers 门禁在途并发只读快照（/admin/users-top 端点用；spec 2026-08-14
// P2-3：gateSnapshot.users 未导出，经本访问器只读暴露）：gateSnapshot 整体
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
