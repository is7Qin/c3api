// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"slices"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// This file owns the folded group-invalidation path (T1): a debouncer window
// that invalidates several groups collapses into ONE publisher.mu critical
// section that loads each group singly (Loader.LoadGroupAccounts — singular
// Group, plural Accounts), folds it into one mutable working root (with
// step-wise identity-pool conversion), and freezes/stages ONCE at the end.

// InvalidateGroups 组失效批量合并：同一 publisher.mu 内逐组单组
// LoadGroupAccounts，折叠进同一可变工作根（含逐步池转换），末尾一次冻结
// newStaticView + stage + scope + wake。ids 排序去重；空集 no-op。
//
// 等价契约：在固定 loader 响应、质量/价格、时钟、incident 初态与相同成功/
// 失败序列下，其最终 canonical decision bytes 与静态拓扑 == 按同一排序去重
// ids 顺序执行单组路径并最终编译的结果（允许合并中间 generation/发布序列）。
func (s *Scheduler) InvalidateGroups(ids []int64) {
	s.publisher.mu.Lock()
	defer s.publisher.mu.Unlock()
	s.invalidateGroupsLocked(ids)
}

// invalidateGroupsLocked 是加锁私有本体（调用方已持 publisher.mu）。禁止
// 私有本体/helper 内再调用公开加锁包装（防重入死锁）。失败义务：任一 gid
// load 失败 → Warn + 同锁置位 s.reloadRequired（粘性，仅 full reload 清）+
// 跳过该 gid 的直接 load/替换（不主动覆盖 m[gid]、不建空快照）但继续；失败
// gid 仍可被后续成功 gid 的共享账号 fix-up 依单组规则修改其共享引用/池。
// 仅当至少一组成功（succeeded）才 stage：一次 newStaticView + 一次 stage +
// 一次 enqueueCompileScope（有序去重）+ 一次 RequestCompile（先 scope 后 wake）。
func (s *Scheduler) invalidateGroupsLocked(ids []int64) {
	gids := sortedUniqueInt64(ids)
	if len(gids) == 0 {
		return
	}
	// refresh-first baseline (never after — see refreshProbeBaseline).
	s.refreshProbeBaseline(context.Background())
	m, byID, pools := s.workingRootLocked()
	var scope []int64
	succeeded := false
	for _, gid := range gids {
		accs, err := s.loader.LoadGroupAccounts(context.Background(), gid)
		if err != nil {
			if s.log != nil {
				s.log.Warn("group reload failed", logx.Int64("group_id", gid), logx.Error(err))
			}
			s.reloadRequired = true
			continue
		}
		succeeded = true
		var stepScope []int64
		stepScope, pools = s.invalidateOneIntoLocked(m, byID, pools, gid, accs)
		scope = unionScope(scope, stepScope)
	}
	if !succeeded {
		return
	}
	sv := newStaticView(m, byID, pools)
	s.publisher.stageLocked(sv)
	// this staging touches exactly scope — the lane recomputes only their
	// routes. Scope-first, then wake.
	s.enqueueCompileScope(scope, nil, scopeCauseGroup)
	s.RequestCompile()
}

// workingRootLocked builds a fresh shallow-copied mutable working root
// (groups + byID) from the newest staged-or-published static root, plus the
// latest pending-or-published identity registry as the prev pool source.
// Caller must hold publisher.mu.
func (s *Scheduler) workingRootLocked() (map[int64]*groupSnapshot, map[int64]*accountSnapshot, *identityRegistry) {
	cur := s.view.Load()
	var m map[int64]*groupSnapshot
	var byID map[int64]*accountSnapshot
	if s.publisher.pending != nil {
		m = s.publisher.pending.groups
		byID = s.publisher.pending.byID
	} else if cur != nil && cur.static != nil {
		m = cur.static.groups
		byID = cur.static.byID
	} else {
		m = map[int64]*groupSnapshot{}
		byID = map[int64]*accountSnapshot{}
	}
	// byID 兼作复用查询源（oldByID）：组级重载同样复用旧实例——errRate/errCount
	// 跨组级 NOTIFY 重载保留，静态字段 DB 权威同步。持 publisher.mu 读取安全。
	return cloneSnapMap(m), cloneSnapMap(byID), s.prevIdentityPoolsLocked()
}

// invalidateOneIntoLocked folds ONE group's reload into the mutable working
// root IN PLACE: m/byID are private mutable working maps and pools is the
// step-wise identity registry; the caller holds publisher.mu. It does NOT build
// global facts/index, stage, or wake. Returns the step's affected group scope
// and the next pool source (callers MUST thread pools forward — never re-read
// prevIdentityPoolsLocked, which would snapshot the pre-batch source).
//
// Equivalence to the single-group path: the pre-step read source is captured
// explicitly (oldGroup := m[gid] BEFORE the overwrite; each removed account's
// old membership/runtime is read before its byID entry is overwritten); other
// groups' fix-ups read the CURRENT step's working m (seeing prior groups and
// this step's removed-group replacement); pools are rebuilt once per successful
// group (step-wise buildIdentityPools over the updated working byID).
func (s *Scheduler) invalidateOneIntoLocked(m map[int64]*groupSnapshot, byID map[int64]*accountSnapshot, pools *identityRegistry, gid int64, accs []*domain.Account) (scope []int64, poolsOut *identityRegistry) {
	// Capture the pre-step source: oldGroup must be read before m[gid] is
	// replaced by this step's fresh snapshot.
	oldGroup := m[gid]
	gs, _ := buildSnapshots(map[int64][]*domain.Account{gid: accs}, byID)
	newAccs := gs[gid].accounts
	// 直接复用 buildSnapshots 产出的快照：accounts 与 routes 一并生效，
	// 避免组级重载后 routes 为 nil（编译车道枚举域断裂）。
	m[gid] = gs[gid]
	scope = []int64{gid}
	// 从组移除的账号（旧组有、新组无）：仍属其它组 → 保留实例并摘本组引用；
	// 已不属于任何组 → 从 byID 删除（其它组引用随实例保留/删除，路由无需重建）。
	// 先建 新组账号ID 索引再单遍扫描——嵌套循环对 50k 大组批量删
	// 25k 是 ≈1.25e9 次比较 ≈1s 停顿（去抖单 goroutine 内拉大所有失效延迟/
	// 新用户 402 窗口），索引后 O(旧组大小)。
	// staged groups bound the scoped fire — the reloaded group plus
	// every other group sharing its accounts (their snapshots are rebuilt
	// with the new leaves, so their routes must recompute too).
	if oldGroup != nil {
		newIDs := make(map[int64]struct{}, len(newAccs))
		for _, ns := range newAccs {
			newIDs[ns.static.Load().acc.ID] = struct{}{}
		}
		// Track leaves needing other-group replacement for removed-but-still-present accounts.
		removedOtherRefs := make(map[int64][]*accountSnapshot)
		for _, os := range oldGroup.accounts {
			ost := os.static.Load()
			if _, stillIn := newIDs[ost.acc.ID]; stillIn {
				continue
			}
			newGids := removeGid(append([]int64(nil), ost.groupIDs...), gid)
			if len(newGids) == 0 {
				delete(byID, ost.acc.ID)
				continue
			}
			// Changed account gets new immutable static leaf sharing separate runtime, old root stable.
			// gid 必须随 groupIDs 一起重派生：组被移除后，旧 gid 可能已不在
			// newGids 中（原 gid 恰为被移除组，且是当时的最小值）——
			// 沿用 ost.gid 会让 gid ∉ groupIDs，破坏「gid = min(groupIDs)」
			// 不变量，事件投递归组会指向一个该账号已不属于的组。
			newStatic := newSnapshotStatic(ost.acc, ost.tpl, newGids)
			newLeaf := &accountSnapshot{accountID: ost.acc.ID, runtime: os.runtime}
			newLeaf.static.Store(newStatic)
			byID[ost.acc.ID] = newLeaf
			// Record for other group replacement.
			for _, og := range newGids {
				removedOtherRefs[og] = append(removedOtherRefs[og], newLeaf)
			}
		}
		// Apply other-group replacements for removed accounts.
		for og, leaves := range removedOtherRefs {
			scope = append(scope, og)
			ogp, ok := m[og]
			if !ok {
				continue
			}
			repl := make([]*accountSnapshot, len(ogp.accounts))
			copy(repl, ogp.accounts)
			leafByID := make(map[int64]*accountSnapshot, len(leaves))
			for _, lf := range leaves {
				leafByID[lf.static.Load().acc.ID] = lf
			}
			for i, oas := range repl {
				if nl, ok := leafByID[oas.static.Load().acc.ID]; ok {
					repl[i] = nl
				}
			}
			m[og] = &groupSnapshot{accounts: repl, routes: buildRoutes(repl)}
		}
	}
	// 新实例替换 byID + 其它组引用（多组账号：旧实例在其它组路由中的位置换成
	// 新实例并重建该组路由——共享实例纪律；单组账号 otherGids 为空，零开销）。
	// 其它组引用替换同禁嵌套扫描——每其它组先建 账号ID→位置 索引
	// （O(该组大小)），替换 O(1)，总量 O(受影响组账号和)。
	type ogRef struct {
		gs  *groupSnapshot
		idx map[int64]int
	}
	otherRefs := make(map[int64]*ogRef)
	for _, ns := range newAccs {
		var otherGids []int64
		nst := ns.static.Load()
		if oa, ok := byID[nst.acc.ID]; ok {
			// Preserve other groupIDs from old immutable root.
			for _, g := range oa.static.Load().groupIDs {
				if g != gid {
					otherGids = append(otherGids, g)
				}
			}
			// Share runtime: new leaf already shares oa.runtime via buildSnapshots,
			// no extra Store needed. Ensure pointer sharing.
			if ns.runtime != oa.runtime {
				ns.runtime = oa.runtime
			}
		}
		// New leaf is local (not yet published), safe to mutate static before publish.
		ns.static.Store(newSnapshotStatic(nst.acc, nst.tpl, append([]int64{gid}, otherGids...)))
		byID[nst.acc.ID] = ns
		for _, og := range otherGids {
			if _, ok := otherRefs[og]; ok {
				continue
			}
			ogp, ok := m[og]
			if !ok {
				continue
			}
			ref := &ogRef{gs: ogp, idx: make(map[int64]int, len(ogp.accounts))}
			for i, oas := range ogp.accounts {
				ref.idx[oas.static.Load().acc.ID] = i
			}
			otherRefs[og] = ref
		}
	}
	for og, ref := range otherRefs {
		scope = append(scope, og)
		repl := make([]*accountSnapshot, len(ref.gs.accounts))
		copy(repl, ref.gs.accounts)
		for _, ns := range newAccs {
			if i, ok := ref.idx[ns.static.Load().acc.ID]; ok {
				repl[i] = ns
			}
		}
		m[og] = &groupSnapshot{accounts: repl, routes: buildRoutes(repl)}
	}
	// codex 槽位池与静态叶**同一发布点**：组级重载同样按新 byID 重建注册表
	// （P0-3）——否则调高 max_concurrency / 新增账号的窗口内门禁上限已变新而池
	// 仍旧（新增账号无池 → 不注入身份；旧 K 槽不足 → claim==nil 兜底）。
	// 逐步池转换：每成功组一次全表重建，prev 源 = 上一步工作池，不得重取批前源。
	poolsOut = s.buildIdentityPools(byID, pools)
	return scope, poolsOut
}

// unionScope 合并有序去重 scope：保留 scope 的既有顺序，追加 add 中尚未出现
// 的组（首次出现序）。
func unionScope(scope, add []int64) []int64 {
	if len(add) == 0 {
		return scope
	}
	seen := make(map[int64]struct{}, len(scope)+len(add))
	for _, g := range scope {
		seen[g] = struct{}{}
	}
	for _, g := range add {
		if _, ok := seen[g]; ok {
			continue
		}
		seen[g] = struct{}{}
		scope = append(scope, g)
	}
	return scope
}

// sortedUniqueInt64 升序排序去重（空返回 nil）。批路径 ids 的确定性顺序：
// 等价契约按「同一排序去重 ids 顺序」执行单组路径。
func sortedUniqueInt64(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	out := append([]int64(nil), ids...)
	slices.Sort(out)
	return slices.Compact(out)
}
