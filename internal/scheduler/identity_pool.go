// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
//
// 调度器侧与会话池之间只剩三处缝（P5b）：
//
//	Spec      构造参数（buildIdentityPools 组一次）
//	Claim     请求路径领取（claimCodexSession）
//	Reclaimer 锁内只收集退役（poolReclaim 嵌入 identitypool.Reclaim）
//
// 池本体（Session/Pool/Registry/Resize/Sync/CollectRetiredPools）已下沉到
// internal/identitypool：它不知道 accountConfig / egressapi.Client / 资源批次，也
// 不 import 本包（方向由 internal/architecture 硬钉）。这里只留"哪些账号该有池"
// （读账号快照/静态根）与"退役之外的锁外副作用"（配置批次 + 出口 client）。
package scheduler

import (
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/identitypool"
	// 只留 egress.CloseClients（实现）；出口的值类型走 pkg/egressapi。
	"github.com/is7qin/c3api/pkg/egress"
	"github.com/is7qin/c3api/pkg/egressapi"
	"github.com/is7qin/c3api/pkg/logx"
)

// poolReclaim 是重载路径的**锁外副作用批次**：线程退役与配置面 client 回收都只在
// 锁内收集，由持锁方的调用点在**解锁之后**执行（§3.2「锁外 RetireThread / 锁外
// Close」）。
//
// 线程退役部分**就是** `identitypool.Reclaim`（嵌入，不复制）：池侧只认
// `Reclaimer` 这一个接口，收集实现只有一份、路径只有一条；配置/出口两段仍属
// 调度器——它们与池无关，池也不该知道 `accountConfig`/`egressapi.Client`。
type poolReclaim struct {
	identitypool.Reclaim
	// config 本批次的账号配置快照（账号全集 → 当前出口 + 资源指令 + 批次代号）。
	// nil = 本批次不携带配置（单元用例直接建池）。应用在锁外、与线程退役同一个批次。
	config *accountConfig
	// egress 本批次被摘出注册表的出口 client（egress.Pool.Detach 的返回）：与配置
	// 指令共享"锁内只收集、锁外才动系统调用"的纪律——关闭空闲连接可能在连接收尾上
	// 等待，不能留在 publisher.mu 里。
	egress []*egressapi.Client
	retire func(accountID int64, threadKey string)
	apply  func(batch domain.ResourceBatch)
}

// run 执行收集到的副作用（必须在 publisher.mu 之外调用；nil 安全）。
func (b *poolReclaim) run() {
	if b == nil {
		return
	}
	// 线程退役先跑（先摘连接、再关 client、最后应用配置）：retire 为 nil 时
	// Reclaim.Run 是 no-op，等价于改前的 `if b.retire != nil { … }`。
	b.Reclaim.Run(b.retire)
	// 出口 client 与线程退役同一批、同一纪律：锁内只摘，锁外关闭。
	egress.CloseClients(b.egress)
	// 配置面回收放最后：账号删除与出口可见性必须在**同一个** gen 准入之后应用——
	// 旧批次删掉的可能是新配置刚建出来的条目。
	if b.config != nil && b.apply != nil {
		b.apply(b.config.batch)
	}
}

// retireSink 把本批次暴露成池侧的退役汇：本批次**没有**线程退役出口
// （`retire == nil`，测试/纯选号形态）时返回**裸 nil 接口**，池侧因此一条都不收集
// ——与改前 `reclaim.retire == nil ⇒ collectRetiredPools 直接返回` 逐字等价。
//
// 为什么不能直接传 b：`*poolReclaim` 恒非 nil，那会把"未装配"变成"装配了但不执行"
// ——收集期仍要逐槽 CAS（假忙窗口），是一处真实的行为差异。
func (b *poolReclaim) retireSink() identitypool.Reclaimer {
	if b == nil || b.retire == nil {
		return nil
	}
	return b
}

// buildIdentityPools 按当前 byID 重建 codex 会话池注册表：仅 codex **通道**
// （ChannelOf == ChannelCodex，今天 = oauth/pat；表外类型落静态通道 ⇒ 不建池）
// 且 MaxConcurrency>0 建池；与静态叶**同发布点**（挂
// StaticView.identityPools），使门禁读的 MaxConcurrency 与池 K 恒同源。prevReg
// 供复用（未变的池原样保留在途 busy 与会话身份）；删除/非 codex 的账号不在新
// 表中 → 旧池随之 GC。K 的上界已在 buildSnapshots 装载期施加于 acc.MaxConcurrency。
//
// 退役只写进 reclaim（锁外执行）；reclaim 为 nil / 未装配退役出口时（单元用例直接
// 建池）退役不执行——调用方自己决定要不要真退役。**账号移除不在这里判定**：池注册表
// 只回答"这个账号此刻有没有会话池"，账号资源回收只由账号全集差分（accountConfig）驱动。
func (s *Scheduler) buildIdentityPools(byID map[int64]*accountSnapshot, prevReg *identitypool.Registry, reclaim *poolReclaim) *identitypool.Registry {
	pools := make(map[int64]*identitypool.Pool)
	sink := reclaim.retireSink()
	for id, as := range byID {
		av := as.static.Load()
		if av == nil || av.tpl == nil || av.acc.Ext == nil {
			continue
		}
		if credential.ChannelOf(av.tpl.CredentialType) != credential.ChannelCodex {
			continue
		}
		k := av.acc.MaxConcurrency
		if k <= 0 {
			continue
		}
		pools[id] = identitypool.Sync(prevReg.Lookup(id), identitypool.Spec{
			AccountID:      id,
			K:              k,
			InstallationID: installationIDOf(av.acc.Ext),
			Policy:         s.cfg.RotatePolicy,
			Retire:         s.cfg.RetireThread,
		}, sink)
	}
	return &identitypool.Registry{Pools: pools}
}

// prevIdentityPoolsLocked 取当前「最新」静态根的池注册表作为复用源：优先
// staged（pending）根，否则已发布根——与 reload 取 oldByID 同纪律（持
// publisher.mu 读取安全）。
func (s *Scheduler) prevIdentityPoolsLocked() *identitypool.Registry {
	if p := s.publisher.pending; p != nil {
		return p.identityPools
	}
	if cur := s.view.Load(); cur != nil && cur.static != nil {
		return cur.static.identityPools
	}
	return nil
}

// claimCodexSession 为一次**已成功预留**的 codex 调用认领会话：入参是已解析好的
// **凭据通道**（调用方在装配点时算过一次 ChannelOf，不再重复查表）；仅
// ChannelCodex 认领；其余通道 / 无池 → nil（不认领、不注入会话身份）。pools 取自
// 本次预留所用视图的静态根（与门禁 limit 同源）；全忙兜底 = 临时一次性身份
// （不落池、无代际/退役面，计一条 warn）——installation/policy 取自池的**不可变
// 字段**，不在租借之外读任何会话状态。
func (s *Scheduler) claimCodexSession(pools *identitypool.Registry, accountID int64, ch credential.Channel) (*identitypool.Session, *identitypool.Pool) {
	if ch != credential.ChannelCodex || pools == nil {
		return nil, nil
	}
	pool := pools.Lookup(accountID)
	if pool == nil {
		return nil, nil
	}
	if session := pool.Claim(); session != nil {
		return session, pool
	}
	s.log.Warn("codex session pool exhausted; using ephemeral identity", logx.Int64("account_id", accountID))
	return identitypool.NewSession(pool.InstallationID(), pool.RotatePolicy()), nil
}

// installationIDOf 取账号 ext 的安装 ID（账号级稳定项；缺列 → 空串）。
func installationIDOf(ext *domain.AccountExt) string {
	if ext == nil || ext.CodexIdentity == nil {
		return ""
	}
	return ext.CodexIdentity.InstallationID
}
