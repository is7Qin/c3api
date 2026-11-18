// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

// 重试聚合读面（spec 2026-11-18 §2.3 / §6 S1-2）：多 route 一次查询的结果
// 必须**逐行等于**逐 route 查询结果的并集（同一 identity 的 chain_count 求和
// 恒等），空集合不发查询且返回非 nil 空切片。EXPLAIN 断言另见同包
// routing_retry_read_explain_test.go（内部包，引用生产 SQL 常量）。

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

func retryFactKeys(rows []repository.RoutingFlowStat) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, fmt.Sprintf("%x|%d|%s|%d|%s|%s|%d|%t|%d",
			r.RouteClassID[:], r.Ordinal, r.Lane, r.AccountID, r.Outcome,
			r.TransitionReason, r.ChainCount, r.IsTerminal, r.MinGeneration))
	}
	sort.Strings(out)
	return out
}

func TestRoutingFlowStatsMultiMatchesPerRoutePG(t *testing.T) {
	repos, _ := newRoutingRepos(t)
	ctx := context.Background()
	base := time.Date(2026, 9, 21, 10, 0, 0, 0, time.UTC)
	require.NoError(t, repos.Partitions.EnsureRoutingPartitions(ctx, base))

	rcA := mustRouteClassVal(t, 1, domain.FormatOpenAIChat, "m-a", domain.OpChatCompletions)
	rcB := mustRouteClassVal(t, 2, domain.FormatOpenAIChat, "m-b", domain.OpChatCompletions)
	rcC := mustRouteClassVal(t, 3, domain.FormatOpenAIChat, "m-c", domain.OpChatCompletions)
	// rcD 有数据但不在查询集合里：ANY() 必须把它排除（不是「窗口内全部」）。
	rcD := mustRouteClassVal(t, 4, domain.FormatOpenAIChat, "m-d", domain.OpChatCompletions)
	// rcE 是 v6 的 ordinal 空洞形状（本地拒绝 / 402 预检不产生 ordinal=2 边）：
	// 1(error, 非链尾) → 3(success, 链尾)。T₁ 的数据源必须真能读出 ordinal=1 的 false。
	rcE := mustRouteClassVal(t, 5, domain.FormatOpenAIChat, "m-e", domain.OpChatCompletions)

	row := func(rc domain.RouteClassIDVal, ordinal int16, outcome string, chain int64) repository.RoutingFlowRow {
		return repository.RoutingFlowRow{
			IdentityVersion: 1, RouteClassID: rc, TerminalMinute: base, Ordinal: ordinal,
			Lane: "primary", AccountID: int64(ordinal) * 10, PreviousOutcome: "",
			TransitionReason: "init", Outcome: outcome, IsTerminal: outcome != "429",
			Generation: 1, InstanceSrc: "src-A", ChainCount: chain,
		}
	}
	seedA := []repository.RoutingFlowRow{
		row(rcA, 1, "429", 3),
		row(rcA, 2, "success", 3),
	}
	seedB := []repository.RoutingFlowRow{
		row(rcB, 1, "5xx", 2),
		row(rcB, 2, "5xx", 2),
		row(rcB, 3, "success", 2),
	}
	seedC := []repository.RoutingFlowRow{
		row(rcC, 1, "success", 5),
	}
	seedD := []repository.RoutingFlowRow{
		row(rcD, 1, "network", 7),
	}
	// 当 row() 的 outcome 推导（!= "429"）不适用于空洞链的首发边：它必须是非终局，
	// 否则 T₁ 会被误算成 1，R = C₁ − T₁ 就退回旧口径的错误答案。
	voidFirst := row(rcE, 1, "error", 1)
	voidFirst.IsTerminal = false
	seedVoid := []repository.RoutingFlowRow{
		voidFirst,
		row(rcE, 3, "success", 1),
	}
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-A", base, 1, 1, seedA))
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-B", base, 1, 1, seedB))
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-C", base, 1, 1, seedC))
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-D", base, 1, 1, seedD))
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-E", base, 1, 1, seedVoid))
	// 第二条分片对 rcA 的同一 identity 再累加 4 链：多 route 与单 route 都必须
	// 做同样的跨分片 SUM。
	require.NoError(t, repos.Partitions.UpsertFlowSnapshot(ctx, "src-F", base, 1, 1,
		[]repository.RoutingFlowRow{func() repository.RoutingFlowRow {
			r := row(rcA, 2, "success", 4)
			r.InstanceSrc = "src-F"
			return r
		}()}))

	from, to := base, base.Add(time.Minute)
	queried := []domain.RouteClassIDVal{rcA, rcB, rcC, rcE}
	multi, err := repos.Partitions.QueryFlowFactStatsMulti(ctx, queried, from, to)
	require.NoError(t, err)
	require.NotEmpty(t, multi)

	var union []repository.RoutingFlowStat
	for _, rc := range queried {
		rows, err := repos.Partitions.QueryFlowFactStats(ctx, rc, from, to)
		require.NoError(t, err)
		union = append(union, rows...)
	}
	require.Equal(t, retryFactKeys(union), retryFactKeys(multi),
		"multi 查询必须逐行等于逐 route 查询的并集")
	// rcD 不在集合内 ⇒ 任何一行都不得属于它。
	for _, r := range multi {
		require.NotEqual(t, rcD, r.RouteClassID, "ANY() 必须排除未请求的 route")
	}
	// 跨分片 SUM 生效：rcA ordinal=2 的 chain_count = 3 + 4。
	var rcAOrd2 int64
	for _, r := range multi {
		if r.RouteClassID == rcA && r.Ordinal == 2 {
			rcAOrd2 += r.ChainCount
		}
	}
	require.Equal(t, int64(7), rcAOrd2)

	// IsTerminal 必须原样穿过聚合层（v6 的 T₁ 只靠它）：逐 identity 与写入值相等。
	wantTerminal := make(map[string]bool, len(seedA)+len(seedB)+len(seedC)+len(seedVoid)+1)
	for _, seed := range [][]repository.RoutingFlowRow{seedA, seedB, seedC, seedVoid} {
		for _, r := range seed {
			wantTerminal[fmt.Sprintf("%x|%d|%s", r.RouteClassID[:], r.Ordinal, r.Outcome)] = r.IsTerminal
		}
	}
	// src-F 的累加行与 seedA 的 ordinal=2 是同一 identity（IsTerminal 同值）。
	gotTerminal := make(map[string]bool, len(wantTerminal))
	for _, r := range multi {
		key := fmt.Sprintf("%x|%d|%s", r.RouteClassID[:], r.Ordinal, r.Outcome)
		require.NotContains(t, gotTerminal, key, "同一 identity 只能聚合出一行：%s", key)
		gotTerminal[key] = r.IsTerminal
	}
	require.Equal(t, wantTerminal, gotTerminal, "IsTerminal 必须逐 identity 与写入一致")

	// 空洞链的门禁：回到读面的形状恰好是 1(false) → 3(true)，没有 ordinal=2；
	// 即服务层算得出 C₁ = 1、T₁ = 0（二者若错，retry_rate 就不是 1）。
	var rcEC1, rcET1 int64
	var rcEOrdinals []int16
	for _, r := range multi {
		if r.RouteClassID != rcE {
			continue
		}
		rcEOrdinals = append(rcEOrdinals, r.Ordinal)
		if r.Ordinal == 1 {
			rcEC1 += r.ChainCount
			if r.IsTerminal {
				rcET1 += r.ChainCount
			}
		}
		if r.Ordinal == 1 {
			require.False(t, r.IsTerminal, "空洞链的 ordinal=1 边不得是链尾")
		} else {
			require.True(t, r.IsTerminal, "空洞链的 ordinal=3 边必须是链尾")
		}
	}
	require.ElementsMatch(t, []int16{1, 3}, rcEOrdinals, "空洞链不得凭空长出 ordinal=2 边")
	require.Equal(t, int64(1), rcEC1)
	require.Zero(t, rcET1, "终局不在首发 ⇒ T₁ = 0（这是 R = C₁ − T₁ 的数据源）")

	// 空集合：不发查询、返回非 nil 空切片（handler 侧序列化成 []）。
	empty, err := repos.Partitions.QueryFlowFactStatsMulti(ctx, nil, from, to)
	require.NoError(t, err)
	require.NotNil(t, empty)
	require.Empty(t, empty)
}
