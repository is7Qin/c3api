// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// 路由 flow 的桑基图边集构造：每 (ordinal, lane) 层按 chain_count 保留 top-N
// 账号，其余折叠为 account_id=0 的「其他」节点。
//
// 两条不变量：
//  1. 折叠只合并账号，层内 chain_count 求和不变——守恒不破，图形不因折叠失真。
//  2. sankey 恒在**完整边集**上折叠，与边表分页（offset/limit）严格解耦——
//     翻页不改变图形，图形也不随翻页变化。
//
// 折叠是纯拓扑操作，不重算任何服务端统计量（Wilson/frontier/守恒判定都不在此）。

import "sort"

// RoutingFlowGraphEdge 一条桑基边（已按 (ordinal, lane, account, outcome,
// is_terminal) 聚合）。Folded=true 表示「其他」聚合节点（AccountID 恒 0）。
type RoutingFlowGraphEdge struct {
	Ordinal    int16
	Lane       string
	AccountID  int64
	Folded     bool
	Outcome    string
	IsTerminal bool
	ChainCount int64
	// 到达链去重集（升序）。折叠节点不收集 PreviousAccounts——折叠合并了多个
	// 账号，前驱集合会膨胀且无展示价值；reasons/outcomes 是小枚举，照常收集。
	TransitionReasons []string
	PreviousOutcomes  []string
	PreviousAccounts  []int64
}

// RoutingFlowGraph 桑基图边集 + 折叠元数据。
type RoutingFlowGraph struct {
	Edges          []RoutingFlowGraphEdge
	AccountLimit   int
	FoldedAccounts int64
	Folded         bool
}

// sankeyLayerKey 折叠分层的身份：(ordinal, lane)。
type sankeyLayerKey struct {
	ordinal int16
	lane    string
}

// sankeyLayerAccount 层内账号身份。
type sankeyLayerAccount struct {
	layer   sankeyLayerKey
	account int64
}

// sankeyEdgeKey 聚合身份（不含 generation/fingerprint——桑基不消费这两个维度，
// 它们正是边数膨胀的来源，聚合掉才能让图形有界）。
type sankeyEdgeKey struct {
	ordinal    int16
	lane       string
	accountID  int64
	outcome    string
	isTerminal bool
}

// buildFlowSankey 把完整边集折叠成有界桑基边集。
//
// accountLimit ≤0 视为不折叠（保留全部账号）；由调用方按契约归一后再传入。
// 输出对同一输入字节级确定：层内账号按 (chain_count 降序, account_id 升序)
// 排名，边按 (ordinal, lane, account_id, outcome, is_terminal) 升序。
func buildFlowSankey(edges []RoutingFlowEdge, accountLimit int) RoutingFlowGraph {
	// 1. 层内账号的 chain_count 合计（排名依据）。
	layerTotals := make(map[sankeyLayerAccount]int64)
	layerAccounts := make(map[sankeyLayerKey]map[int64]struct{})
	for _, e := range edges {
		lk := sankeyLayerKey{e.Ordinal, e.Lane}
		la := sankeyLayerAccount{layer: lk, account: e.AccountID}
		layerTotals[la] += e.ChainCount
		if layerAccounts[lk] == nil {
			layerAccounts[lk] = make(map[int64]struct{})
		}
		layerAccounts[lk][e.AccountID] = struct{}{}
	}

	// 2. 每层独立排名，取 top-N 保留。
	kept := make(map[sankeyLayerAccount]bool)
	var foldedAccounts int64
	layerKeys := make([]sankeyLayerKey, 0, len(layerAccounts))
	for lk := range layerAccounts {
		layerKeys = append(layerKeys, lk)
	}
	sort.Slice(layerKeys, func(i, j int) bool {
		if layerKeys[i].ordinal != layerKeys[j].ordinal {
			return layerKeys[i].ordinal < layerKeys[j].ordinal
		}
		return layerKeys[i].lane < layerKeys[j].lane
	})
	for _, lk := range layerKeys {
		ids := make([]int64, 0, len(layerAccounts[lk]))
		for id := range layerAccounts[lk] {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool {
			ai := layerTotals[sankeyLayerAccount{layer: lk, account: ids[i]}]
			aj := layerTotals[sankeyLayerAccount{layer: lk, account: ids[j]}]
			if ai != aj {
				return ai > aj // 链数多的优先
			}
			return ids[i] < ids[j] // 同链数按账号升序（确定性）
		})
		for i, id := range ids {
			if accountLimit > 0 && i >= accountLimit {
				foldedAccounts++
				continue
			}
			kept[sankeyLayerAccount{layer: lk, account: id}] = true
		}
	}

	// 3. 按 (ordinal, lane, account, outcome, is_terminal) 聚合；未保留账号归入占位
	// id 并置 folded=true。folded 是权威判别位（I6）：不得以 account_id == 0
	// 推断——占位 id 恰为 0，消费方必须以 folded 为准。
	type agg struct {
		chainCount int64
		folded     bool
		reasons    map[string]struct{}
		prevOut    map[string]struct{}
		prevAccts  map[int64]struct{}
	}
	aggs := make(map[sankeyEdgeKey]*agg)
	for _, e := range edges {
		lk := sankeyLayerKey{e.Ordinal, e.Lane}
		isKept := kept[sankeyLayerAccount{layer: lk, account: e.AccountID}]
		acct := e.AccountID
		if !isKept {
			acct = 0
		}
		k := sankeyEdgeKey{e.Ordinal, e.Lane, acct, e.Outcome, e.IsTerminal}
		a := aggs[k]
		if a == nil {
			a = &agg{
				folded:    !isKept,
				reasons:   make(map[string]struct{}),
				prevOut:   make(map[string]struct{}),
				prevAccts: make(map[int64]struct{}),
			}
			aggs[k] = a
		}
		a.chainCount += e.ChainCount
		if e.TransitionReason != "" {
			a.reasons[e.TransitionReason] = struct{}{}
		}
		if e.PreviousOutcome != "" {
			a.prevOut[e.PreviousOutcome] = struct{}{}
		}
		if isKept && e.PreviousAccountID != nil {
			a.prevAccts[*e.PreviousAccountID] = struct{}{}
		}
	}

	// 4. 确定性输出序。
	keys := make([]sankeyEdgeKey, 0, len(aggs))
	for k := range aggs {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.ordinal != b.ordinal {
			return a.ordinal < b.ordinal
		}
		if a.lane != b.lane {
			return a.lane < b.lane
		}
		if a.accountID != b.accountID {
			return a.accountID < b.accountID
		}
		if a.outcome != b.outcome {
			return a.outcome < b.outcome
		}
		return !a.isTerminal && b.isTerminal
	})

	out := make([]RoutingFlowGraphEdge, 0, len(keys))
	for _, k := range keys {
		a := aggs[k]
		out = append(out, RoutingFlowGraphEdge{
			Ordinal:           k.ordinal,
			Lane:              k.lane,
			AccountID:         k.accountID,
			Folded:            a.folded,
			Outcome:           k.outcome,
			IsTerminal:        k.isTerminal,
			ChainCount:        a.chainCount,
			TransitionReasons: sortedStringSet(a.reasons),
			PreviousOutcomes:  sortedStringSet(a.prevOut),
			PreviousAccounts:  sortedInt64Set(a.prevAccts),
		})
	}
	return RoutingFlowGraph{
		Edges:          out,
		AccountLimit:   accountLimit,
		FoldedAccounts: foldedAccounts,
		Folded:         foldedAccounts > 0,
	}
}

func sortedStringSet(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedInt64Set(m map[int64]struct{}) []int64 {
	out := make([]int64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
