// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

// 账号配置权威的唯一所有者（spec §3.3b-1）：账号全集、每个账号当前的 responses
// 出口、配置批次代号，以及组级增量路径的基线。
//
// 为什么需要它：这些事实此前散在 routingPublisher（它因此被迫保存"不参与路由的
// 账号影子配置"）与 reload 调用点，两级出口资源（pkg/egress.Pool 与 sdkbridge 的
// client 表）各自推导"活跃集合"。现在：
//   - routingPublisher 只持 pending/路由发布状态；
//   - 本类型在 **publisher.mu** 下串行推进一份快照；
//   - 两级资源消费**同一份**快照（pruneEgress 的摘要集合与 ApplyConfig 的映射
//     来自同一个 accountConfig.egress）。
//
// 所有方法都由持 publisher.mu 的控制面调用；请求路径不读也不写。

import (
	"maps"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/egress"
)

// accountConfig 一个配置批次的**不可变快照**：账号全集 → 当前 responses 出口，
// 外加批次代号（publisher.mu 内取号，应用在锁外）。
type accountConfig struct {
	gen    uint64
	egress map[int64]*egress.Client
}

// accountRuntime 账号配置权威（guarded by publisher.mu）。
type accountRuntime struct {
	gen uint64
	// universe 上一批的账号全集（id → 当前 responses 出口）。issue 之后归快照
	// 所有、调用方只读；mergedLocked 每次克隆后覆盖，绝不原地改写——上一批的 map
	// 可能仍在锁外的 reclaim/ApplyConfig 手里。
	universe map[int64]*egress.Client
}

// fullLocked 全量装载的批次。`all` 是账号全集（含未编组、`MaxConcurrency=0`、
// 没有会话池的账号）。装载源没有全集能力（测试替身）⇒ all == nil，退化为路由根
// 的账号集合——那仍是该源手上能看到的全部账号，语义不破。
//
// 账号删除由这份全集的差分判定：新批次里没有的账号 ⇒ ApplyConfig 整账号摘除。
func (r *accountRuntime) fullLocked(byID map[int64]*accountSnapshot, all []*domain.Account, pool *egress.Pool) accountConfig {
	var universe map[int64]*egress.Client
	if all == nil {
		universe = make(map[int64]*egress.Client, len(byID))
		for id, as := range byID {
			if av := as.static.Load(); av != nil {
				universe[id] = av.egress
			}
		}
	} else {
		universe = make(map[int64]*egress.Client, len(all))
		for _, acc := range all {
			if acc == nil {
				continue
			}
			universe[acc.ID] = egressForAccount(acc, pool)
		}
	}
	return r.issue(universe)
}

// mergedLocked 组级增量批次：组级路径不重扫账号表，所以以上一批的账号全集为基线，
// 再用本次工作根（byID 是折叠后的全量路由根）覆盖其中的账号。
//
// 由此得到的语义（§3.3b-4）：从组里移除账号**不是**账号移除——它仍在账号全集里，
// 资源保留；真正的删除只由下一次全量装载的账号全集差分发现（与池注册表差分无关）。
func (r *accountRuntime) mergedLocked(byID map[int64]*accountSnapshot, pool *egress.Pool) accountConfig {
	universe := make(map[int64]*egress.Client, len(r.universe)+len(byID))
	maps.Copy(universe, r.universe)
	for id, as := range byID {
		if av := as.static.Load(); av != nil {
			universe[id] = av.egress
		}
	}
	return r.issue(universe)
}

// issue 取号并把快照定型为当前配置。调用方持 publisher.mu —— 代号因此按锁内顺序
// 单调，ClientTable 用"只接受更新的号"拒绝迟到的旧批次。
func (r *accountRuntime) issue(universe map[int64]*egress.Client) accountConfig {
	r.gen++
	r.universe = universe
	return accountConfig{gen: r.gen, egress: universe}
}

// egressForAccount 账号当前 responses 出口（未装配池 ⇒ nil = 直连语义）。与
// buildSnapshots 对组内账号的派生同源：有代理 → 规格 client；无 → 池直连。
func egressForAccount(acc *domain.Account, pool *egress.Pool) *egress.Client {
	if pool == nil {
		return nil
	}
	if acc.Proxy != nil {
		return pool.For(specOfDomain(acc.Proxy))
	}
	return pool.Direct()
}

// pruneEgress 回收当前配置不再引用到的出口 client（spec §8 失效矩阵：代理 CRUD /
// 改绑后旧规格的连接池必须关闭并移除，不靠 GC 猜）。
//
// 输入必须与 ClientTable 的是**同一份**快照（账号全集，含未编组与没有会话池的
// 账号）：两级资源同口径——否则未编组账号独占的代理会在全量 reload 被这里先关掉、
// 随后又被基线重建，连接池被反复打断。
//
// 池里没有规格 client（零代理部署）⇒ 判空后直接返回：不扫描账号、不分配摘要集合。
func (s *Scheduler) pruneEgress(universe map[int64]*egress.Client) {
	pool := s.cfg.Egress
	if pool == nil || pool.Size() == 0 {
		return
	}
	active := make(map[[16]byte]struct{}, pool.Size())
	for _, eg := range universe {
		if eg == nil {
			continue
		}
		// 直连（零值 Spec）也进集合：它的摘要不在注册表里，多这一项无害，
		// 却让"直连"与"某个真实规格"不靠特例区分。
		active[eg.Spec.Key()] = struct{}{}
	}
	pool.Prune(active)
}
