// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
//
// codex 伪装身份轮换（账号级槽位池）：账号级恒定身份 → 账号级 K 槽位池；每个
// 槽按完成轮数确定性演化 thread/window，使上游见「多条独立会话、各自演化」。
// 槽 = codexsdk.IdentityState（InstallationID 账号级共享；ThreadID/Turns/WindowN/
// NextWindowAt/WMax 各槽私有）。整族**无锁**：认领 = 旋转游标 + 逐槽 CAS，
// 归还 = Store(false)——无 channel、无 mutex、请求路径零每请求锁。
package scheduler

import (
	"sync/atomic"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// identitySlot 槽位池中的单个槽（无锁）：busy 认领标志（CAS 认领 / Store 归还）
// + 身份状态（atomic.Pointer 承载，Step 推进经 CAS 换入新状态）。policy 槽
// 构造期写入、此后只读（Step 用它采样 WMax）。复用（占用）**不改
// 身份**——身份演化只由完成轮数驱动。
type identitySlot struct {
	busy   atomic.Bool
	policy codexsdk.RotatePolicy
	state  atomic.Pointer[codexsdk.IdentityState]
}

// identityPool 账号级槽位池（无锁）：容量 K = 账号 MaxConcurrency；cursor 为
// 认领的旋转起点（round-robin 打散并发）。slots 切片发布后不再增删；逐槽状态
// 经 atomic 可变。轮换策略单一来源在**槽**（slot.policy，Step 读它）——pool 不再
// 各存一份（需要时经 rotatePolicy 从槽派生）。installationID 供 resize / 兜底开
// 新线程。
type identityPool struct {
	slots          []*identitySlot
	cursor         atomic.Uint64
	installationID string
}

// rotatePolicy 返回本池的轮换策略（单一来源 = 槽：池内各槽构造期写入同一策略）。
func (p *identityPool) rotatePolicy() codexsdk.RotatePolicy {
	if p == nil || len(p.slots) == 0 {
		return codexsdk.RotatePolicy{}
	}
	return p.slots[0].policy
}

// identityRegistry 账号 ID → 池 的只读注册表：**发布后不可变**（map 不增删），
// reload 用 copy-modify-store 换入（与 scheduler.go 的 view atomic.Pointer 同款
// 纪律）。请求路径 Load() 读、无每请求锁、无可变 map 竞态。
type identityRegistry struct {
	pools map[int64]*identityPool
}

// newIdentitySlot 构造一个槽：身份状态用 installationID 开新线程（WMax 按 policy
// 抽样、Turns/WindowN 归零）。
func newIdentitySlot(installationID string, policy codexsdk.RotatePolicy) *identitySlot {
	st := codexsdk.NewIdentityState(installationID, policy)
	s := &identitySlot{policy: policy}
	s.state.Store(&st)
	return s
}

// claim 认领一个空闲槽（无锁：旋转游标 + 逐槽 CAS）。全忙（理论不可达——gate
// 保证 ≤K 在途）返回 nil，由调用方兜底临时身份（不落池）。
func (p *identityPool) claim() *identitySlot {
	if p == nil || len(p.slots) == 0 {
		return nil
	}
	k := uint64(len(p.slots))
	base := p.cursor.Add(1)
	for i := uint64(0); i < k; i++ {
		s := p.slots[(base+i)%k]
		if s.busy.CompareAndSwap(false, true) {
			return s
		}
	}
	return nil
}

// release 归还槽（非阻塞 Store——无 channel 锁、无容量死锁）；nil 安全。
func (s *identitySlot) release() {
	if s != nil {
		s.busy.Store(false)
	}
}

// resizeIdentityPool 按新容量重建池：按位次迁移旧槽身份至 min(old,new)。迁移
// 判据为**原子预留**（旧槽 busy CAS false→true）：只有成功预留的旧槽才迁移其
// 身份——杜绝与在途认领 TOCTOU 共享同一 thread（旧池虽将被 GC，但在注册表换入
// 前仍可被请求 Load 到并认领，故判据必须原子）。CAS 失败（在途持有）→ 该位次
// 新开线程（在途持旧槽指针、其 Step 写在旧槽，该窗口轮换丢失，spec D2 可接受）；
// 其余位次新开线程。新槽 busy=false 起始。旧池 GC。
func resizeIdentityPool(old *identityPool, k int, installationID string, policy codexsdk.RotatePolicy) *identityPool {
	if k < 1 {
		k = 1
	}
	np := &identityPool{installationID: installationID, slots: make([]*identitySlot, k)}
	for i := 0; i < k; i++ {
		if old != nil && i < len(old.slots) {
			os := old.slots[i]
			if os.busy.CompareAndSwap(false, true) {
				if cur := os.state.Load(); cur != nil {
					st := *cur
					// installation_id 是账号级项、与注入同源（Selection.CodexIdentity
					// 读槽 state）：迁移沿用旧 thread/window 演化，但 installation
					// 必须取本次生效值——否则槽状态与 ext 静默不一致（P1-3）。
					st.InstallationID = installationID
					slot := &identitySlot{policy: policy}
					slot.state.Store(&st)
					np.slots[i] = slot
					continue
				}
			}
		}
		np.slots[i] = newIdentitySlot(installationID, policy)
	}
	return np
}

// syncIdentityPool 取本次 reload 生效的池：容量/安装 ID/策略均未变则**原样复用**
// （保留在途 busy 标志与槽身份，杜绝无关重载重置会话）；任一变化才重建。
func syncIdentityPool(prev *identityPool, k int, installationID string, policy codexsdk.RotatePolicy) *identityPool {
	if prev != nil && len(prev.slots) == k && prev.installationID == installationID && prev.rotatePolicy() == policy {
		return prev
	}
	return resizeIdentityPool(prev, k, installationID, policy)
}

// maxIdentityPoolSlots 是 codex 槽位池容量 K 的防御性上界。K = 账号
// MaxConcurrency，而 reload 在持 publisher.mu 期间按 K **同步**分配 K 个 UUID
// （newIdentitySlot → NewIdentityState；tools/loadtest 曾配 100000）——无上界即
// 让一次 reload 的锁内分配量随配置线性放大。1024 覆盖任何合理并发会话数
// （账号默认 8，实测运维 ≤ 数十），同时把锁内 UUID 分配压在常数级。上界只在
// 装载期施加于 codex 账号（见 buildSnapshots），非 codex 账号的并发门禁不变；
// 门禁与池读同一份 acc，故二者仍恒同源。
const maxIdentityPoolSlots = 1024

// codexPoolCredential 判定凭据类型是否走 codex 槽位池（oauth/pat）。
func codexPoolCredential(ct credential.Type) bool {
	return ct == credential.TypeCodexOAuth || ct == credential.TypeCodexPAT
}

// buildIdentityPools 按当前 byID 重建 codex 槽位池注册表：仅 codex 凭据
// （oauth/pat）且 MaxConcurrency>0 建池；与静态叶**同发布点**（挂
// StaticView.identityPools），使门禁读的 MaxConcurrency 与池 K 恒同源。prevReg
// 供复用（未变的池原样保留在途 busy 与槽身份）；删除/非 codex 的账号不在新
// 表中 → 旧池随之 GC。K 的上界已在 buildSnapshots 装载期施加于 acc.MaxConcurrency。
func (s *Scheduler) buildIdentityPools(byID map[int64]*accountSnapshot, prevReg *identityRegistry) *identityRegistry {
	pools := make(map[int64]*identityPool)
	for id, as := range byID {
		av := as.static.Load()
		if av == nil || av.tpl == nil || av.acc.Ext == nil {
			continue
		}
		if !codexPoolCredential(av.tpl.CredentialType) {
			continue
		}
		k := av.acc.MaxConcurrency
		if k <= 0 {
			continue
		}
		var prev *identityPool
		if prevReg != nil {
			prev = prevReg.pools[id]
		}
		pools[id] = syncIdentityPool(prev, k, installationIDOf(av.acc.Ext), s.cfg.RotatePolicy)
	}
	return &identityRegistry{pools: pools}
}

// prevIdentityPoolsLocked 取当前「最新」静态根的池注册表作为复用源：优先
// staged（pending）根，否则已发布根——与 reload 取 oldByID 同纪律（持
// publisher.mu 读取安全）。
func (s *Scheduler) prevIdentityPoolsLocked() *identityRegistry {
	if p := s.publisher.pending; p != nil {
		return p.identityPools
	}
	if cur := s.view.Load(); cur != nil && cur.static != nil {
		return cur.static.identityPools
	}
	return nil
}

// claimIdentitySlot 为一次**已成功预留**的 codex 调用认领槽身份：仅 codex 凭据
// （oauth/pat）认领；非 codex / 无池 → nil（不认领、不注入槽身份）。pools 取自
// 本次预留所用视图的静态根（与门禁 limit 同源）；全忙兜底 = 临时一次性身份
// （不落池，计一条 warn）——installation/policy 取自池（单一来源，不再回 ext）。
func (s *Scheduler) claimIdentitySlot(pools *identityRegistry, accountID int64, credType credential.Type) *identitySlot {
	if credType != credential.TypeCodexOAuth && credType != credential.TypeCodexPAT {
		return nil
	}
	if pools == nil {
		return nil
	}
	pool := pools.pools[accountID]
	if pool == nil {
		return nil
	}
	if slot := pool.claim(); slot != nil {
		return slot
	}
	if s.log != nil {
		s.log.Warn("identity pool exhausted; using ephemeral identity", logx.Int64("account_id", accountID))
	}
	return newIdentitySlot(pool.installationID, pool.rotatePolicy())
}

// installationIDOf 取账号 ext 的安装 ID（账号级稳定项；缺列 → 空串）。
func installationIDOf(ext *domain.AccountExt) string {
	if ext == nil || ext.CodexIdentity == nil {
		return ""
	}
	return ext.CodexIdentity.InstallationID
}
