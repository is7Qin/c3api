// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
//
// codex 伪装身份轮换（账号级槽位池）：账号级恒定身份 → 账号级 K 槽位池；每个
// 槽按观测水位确定性演化 thread/window，使上游见「多条独立会话、各自演化」。
// 槽 = codexsdk.IdentityState（InstallationID 账号级共享；ThreadID/WindowN/
// Baseline/Armed/WMax 各槽私有）。整族**无锁**：认领 = 旋转游标 + 逐槽 CAS，
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
// 构造期写入、此后只读（Step 用它采样 WMax / 判定口径）。复用（占用）**不改
// 身份**——身份演化只由水位轴驱动。
type identitySlot struct {
	busy   atomic.Bool
	policy codexsdk.RotatePolicy
	state  atomic.Pointer[codexsdk.IdentityState]
}

// identityPool 账号级槽位池（无锁）：容量 K = 账号 MaxConcurrency；cursor 为
// 认领的旋转起点（round-robin 打散并发）。slots 切片发布后不再增删；逐槽状态
// 经 atomic 可变。installationID/policy 供 resize / 兜底开新线程。
type identityPool struct {
	slots          []*identitySlot
	cursor         atomic.Uint64
	policy         codexsdk.RotatePolicy
	installationID string
}

// identityRegistry 账号 ID → 池 的只读注册表：**发布后不可变**（map 不增删），
// reload 用 copy-modify-store 换入（与 scheduler.go 的 view atomic.Pointer 同款
// 纪律）。请求路径 Load() 读、无每请求锁、无可变 map 竞态。
type identityRegistry struct {
	pools map[int64]*identityPool
}

// newIdentitySlot 构造一个槽：身份状态用 installationID 开新线程（WMax 按 policy
// 抽样、WindowN=0、Armed=true）。
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
	np := &identityPool{policy: policy, installationID: installationID, slots: make([]*identitySlot, k)}
	for i := 0; i < k; i++ {
		if old != nil && i < len(old.slots) {
			os := old.slots[i]
			if os.busy.CompareAndSwap(false, true) {
				if cur := os.state.Load(); cur != nil {
					st := *cur
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

// identityPoolFor 取本次 reload 生效的池：容量/安装 ID/策略均未变则**原样复用**
// （保留在途 busy 标志与槽身份，杜绝无关重载重置会话）；任一变化才重建。
func identityPoolFor(prev *identityPool, k int, installationID string, policy codexsdk.RotatePolicy) *identityPool {
	if prev != nil && len(prev.slots) == k && prev.installationID == installationID && prev.policy == policy {
		return prev
	}
	return resizeIdentityPool(prev, k, installationID, policy)
}

// claimIdentitySlot 为一次**已成功预留**的 codex 调用认领槽身份：仅 codex 凭据
// （oauth/pat）认领；非 codex / 无池 → nil（不认领、不注入槽身份）。全忙兜底 =
// 临时一次性身份（不落池，计一条 warn）。
func (s *Scheduler) claimIdentitySlot(accountID int64, credType credential.Type, ext *domain.AccountExt) *identitySlot {
	if credType != credential.TypeCodexOAuth && credType != credential.TypeCodexPAT {
		return nil
	}
	reg := s.identityPools.Load()
	if reg == nil {
		return nil
	}
	pool := reg.pools[accountID]
	if pool == nil {
		return nil
	}
	if slot := pool.claim(); slot != nil {
		return slot
	}
	if s.log != nil {
		s.log.Warn("identity pool exhausted; using ephemeral identity", logx.Int64("account_id", accountID))
	}
	return newIdentitySlot(installationIDOf(ext), pool.policy)
}

// installationIDOf 取账号 ext 的安装 ID（账号级稳定项；缺列 → 空串）。
func installationIDOf(ext *domain.AccountExt) string {
	if ext == nil || ext.CodexIdentity == nil {
		return ""
	}
	return ext.CodexIdentity.InstallationID
}
