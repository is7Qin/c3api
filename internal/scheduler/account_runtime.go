// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

// 账号配置权威的唯一所有者（spec §3.3b-1）：账号全集、每个账号当前的 responses
// 出口、配置批次代号，以及组级增量路径的基线。
//
// 为什么需要它：这些事实此前散在 routingPublisher（它因此被迫保存"不参与路由的
// 账号影子配置"）与 reload 调用点，两级出口资源（EgressPool 与 sdkbridge 的
// client 表）各自推导"活跃集合"。现在：
//   - routingPublisher 只持 pending/路由发布状态；
//   - 本类型在 **publisher.mu** 下串行推进一份快照；
//   - 两级资源消费**同一份**快照：出口池的摘要集合与传输资源表的
//     `domain.ResourceBatch` 都从 `accountConfig` 派生。
//
// **策略只在这里**（§3.3b-1）：\"账号不在全集 ⇒ 资源删除\"、\"直连恒保留\"这两条都是
// 配置生命周期语义，由本类型翻译成**类型明确的资源指令**；传输资源表只执行指令，
// 不认识账号全集。
//
// 所有方法都由持 publisher.mu 的控制面调用；请求路径不读也不写。

import (
	"maps"

	"github.com/is7qin/c3api/internal/domain"
	// 出口的值类型与端口都在中立侧：本包只认 egressapi.Client 与 EgressPool。
	"github.com/is7qin/c3api/pkg/egressapi"
)

// accountConfig 一个配置批次的**不可变快照**：账号全集 → 当前 responses 出口，
// 批次代号，以及派生出的资源指令。
type accountConfig struct {
	gen    uint64
	egress map[int64]*egressapi.Client
	// batch 交给传输资源表的指令（账号动作 + 保留键 + 批次代号）。表不认识账号
	// 全集与"直连必须保留"——那些判断已经在本文件里做完。
	batch domain.ResourceBatch
}

// accountRuntime 账号配置权威（guarded by publisher.mu）。
type accountRuntime struct {
	gen uint64
	// universe 上一批的账号全集（id → 当前 responses 出口）。issue 之后归快照
	// 所有、调用方只读；mergedLocked 每次克隆后覆盖，绝不原地改写——上一批的 map
	// 可能仍在锁外的 reclaim/ApplyBatch 手里。
	universe map[int64]*egressapi.Client
}

// fullLocked 全量装载的批次。`all` 是账号全集（含未编组、`MaxConcurrency=0`、
// 没有会话池的账号）。装载源没有全集能力（测试替身）⇒ all == nil，退化为路由根
// 的账号集合——那仍是该源手上能看到的全部账号，语义不破。
//
// `held` 是传输资源表当前持有的账号 id 快照（配置权威向表要；表不自己差分）。
// 账号删除由这份全集的差分判定：表里持有、全集里没有 ⇒ 点名 ResourceDelete。
//
// 出口解析经 egressClientFor（与 buildSnapshots **同一个**解析点）：绑定账号 + 池
// 未注入 ⇒ 返回装配违例，整批配置不取号（调用方在 stage 之前就返回）。
//
// **绑定但未展开**（ProxyID 在、Proxy 为空；spec §4.7 L8）的账号在池已注入时**不进
// universe**：它已由 routableFor 判为不可路由，永远不会成为请求出口，给它登记一个直连
// 出口只会制造"绑定账号持直连"的假事实（并让传输资源表按直连键保留一个用不到的桶）。
// 池未注入时**不跳过**——那一侧的判据是"有绑定就必须有池"（ErrEgressNotWired），陈旧
// 绑定不能把它吃掉。两处 universe（全集分支与 byID 回退分支）同一条规则，否则这条
// 不变量会随装载源能力而变。可达性边界见 egressClientFor 的注释。
func (r *accountRuntime) fullLocked(byID map[int64]*accountSnapshot, all []*domain.Account, pool EgressPool, held []int64) (accountConfig, error) {
	var universe map[int64]*egressapi.Client
	if all == nil {
		universe = make(map[int64]*egressapi.Client, len(byID))
		for id, as := range byID {
			av := as.static.Load()
			if av == nil || egressConfigSkip(pool, &av.acc) {
				continue
			}
			universe[id] = av.egress
		}
	} else {
		universe = make(map[int64]*egressapi.Client, len(all))
		for _, acc := range all {
			if acc == nil || egressConfigSkip(pool, acc) {
				continue
			}
			// 与 buildSnapshots 同一个解析点：未编组账号也要过装配违例判定
			// （buildSnapshots 只看得到编组账号）。
			eg, err := egressClientFor(pool, acc)
			if err != nil {
				return accountConfig{}, err
			}
			universe[acc.ID] = eg
		}
	}
	return r.issue(universe, held), nil
}

// mergedLocked 组级增量批次：组级路径不重扫账号表，所以以上一批的账号全集为基线，
// 再用本次工作根（byID 是折叠后的全量路由根）覆盖其中的账号。
//
// 它**不解析出口**（universe 全部取自已发布叶的 av.egress，那些出口是 buildSnapshots
// 在同一个锁内解析好的）——所以它既不需要池形参，也不可能产生装配违例。此前那个
// 形参从未被读过：装配违例的判定只应有一处（egressClientFor）。
//
// 由此得到的语义（§3.3b-4）：从组里移除账号**不是**账号移除——它仍在账号全集里，
// 资源保留；真正的删除只由下一次全量装载的账号全集差分发现（与池注册表差分无关）。
func (r *accountRuntime) mergedLocked(byID map[int64]*accountSnapshot, held []int64) accountConfig {
	universe := make(map[int64]*egressapi.Client, len(r.universe)+len(byID))
	maps.Copy(universe, r.universe)
	for id, as := range byID {
		if av := as.static.Load(); av != nil {
			universe[id] = av.egress
		}
	}
	return r.issue(universe, held)
}

// issue 取号并把快照定型为当前配置。调用方持 publisher.mu —— 代号因此按锁内顺序
// 单调，传输资源表用"只接受更新的号"拒绝迟到的旧批次。
func (r *accountRuntime) issue(universe map[int64]*egressapi.Client, held []int64) accountConfig {
	r.gen++
	r.universe = universe
	return accountConfig{gen: r.gen, egress: universe, batch: resourceBatch(r.gen, universe, held)}
}

// resourceBatch 把「账号全集 → 当前出口」翻译成**资源指令**（策略的唯一定义点）：
//
//   - 全集内的账号：ResourceRetain，保留键 = 该账号当前出口（若它不是直连）+ 直连。
//     **直连恒保留**是这里的策略而不是传输表的特例：usage 快照面没有 Selection、
//     恒走直连，它的桶必须在配置批次之后仍然可用（否则每批 reload 都会摘掉它、
//     "跨窗口不重连"当场失效）。
//   - 表里还持有、但全集里已经没有的账号：ResourceDelete（账号移除的唯一判据 =
//     账号全集差分，在这里落实成点名删除；传输表不做任何"没被提到就删"的推断）。
//
// 没有会话池的账号同样在全集里（池注册表差分看不见它们）。
func resourceBatch(gen uint64, universe map[int64]*egressapi.Client, held []int64) domain.ResourceBatch {
	direct := domain.EgressResourceKey{}
	directives := make([]domain.AccountResourceDirective, 0, len(universe)+len(held))
	for id, eg := range universe {
		keep := make([]domain.EgressResourceKey, 0, 2)
		if k := domain.EgressResourceKey(egressapi.ResourceKey(eg)); k != direct {
			keep = append(keep, k)
		}
		// 直连键恒入列（同上：策略在这里，不在表里）。代理出口 ⇒ 两项；直连出口
		// ⇒ 只有它自己（保留集是集合语义，同一键不重复）。
		keep = append(keep, direct)
		directives = append(directives, domain.AccountResourceDirective{
			AccountID: id, Action: domain.ResourceRetain, Keep: keep,
		})
	}
	for _, id := range held {
		if _, ok := universe[id]; ok {
			continue
		}
		directives = append(directives, domain.AccountResourceDirective{
			AccountID: id, Action: domain.ResourceDelete,
		})
	}
	return domain.ResourceBatch{Gen: gen, Directives: directives}
}

// hasDropped 报告这份 `Accounts()` 快照里是否有账号**已不在账号全集里**——即
// "该点名删除"的差额。它是配置权威对**自己**那份快照的只读判断（表侧仍然不做差分）：
// 静默轮用它决定要不要取号下发，避免每 tick 空转一个新代号、反复下发内容相同的指令。
//
// 必须每次现算、不得缓存"曾经回收过"：请求侧可能在任何时刻为已移除账号新建资源，
// 判据只能是最新的表快照（否则迟到资源会被永久漏掉）。
func (r *accountRuntime) hasDropped(held []int64) bool {
	if r == nil {
		return false
	}
	for _, id := range held {
		if _, ok := r.universe[id]; !ok {
			return true
		}
	}
	return false
}

// pruneEgress 回收当前配置不再引用到的出口 client（spec §8 失效矩阵：代理 CRUD /
// 改绑后旧规格的连接池必须关闭并移除，不靠 GC 猜）。
//
// 输入必须与传输资源表的是**同一份**快照（账号全集，含未编组与没有会话池的
// 账号）：两级资源同口径——否则未编组账号独占的代理会在全量 reload 被这里先关掉、
// 随后又被基线重建，连接池被反复打断。
//
// 池里没有规格 client（零代理部署）⇒ 判空后直接返回：不扫描账号、不分配摘要集合。
//
// **只摘不关**：返回被摘除的 client，由调用方在 publisher.mu 之外关闭
// （EgressPool.Detach 的纪律：CloseIdleConnections 可能在连接收尾上等待）。
func (s *Scheduler) pruneEgress(universe map[int64]*egressapi.Client) []*egressapi.Client {
	pool := s.cfg.Egress
	if pool == nil || pool.Size() == 0 {
		return nil
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
	return pool.Detach(active)
}
