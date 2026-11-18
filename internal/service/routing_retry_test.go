// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// 重试聚合纯函数 + /routing/retry 服务边界（spec 2026-11-18 §2 / §6 S1）。
// 纯函数夹具直接构造 []repository.RoutingFlowStat（不触 PG、不触时钟）；
// 端点用例经 fakeStore 供给行集与 route 枚举（仍不触 PG）。

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/scheduler"
)

// ptrFloat 比值期望值的指针形式（nil 与 0 必须可区分）。
func ptrFloat(v float64) *float64 { return &v }

// fmtRatio 比值日志形态：nil 打印 null（与线上 JSON 同语义，报告可直接引用）。
func fmtRatio(v *float64) string {
	if v == nil {
		return "null"
	}
	return fmt.Sprintf("%.4f", *v)
}

// retryRow 夹具：一条聚合边（只填与重试口径相关的字段）。isTerminal 必须显式
// 给出：真实数据里每条完整链**恰好一条** terminal 边（settle 在 fold 前强制标尾，
// spec §2.2 v6 的 T₁ 依据），夹具若把 terminal 当默认值填，T₁ 会悄悄失真。
func retryRow(ordinal int16, outcome string, chainCount int64, isTerminal bool) repository.RoutingFlowStat {
	return repository.RoutingFlowStat{
		Ordinal: ordinal, Outcome: outcome, ChainCount: chainCount, IsTerminal: isTerminal,
	}
}

// retryWant 一条夹具的完整期望：六个指标 + by_reason 的分组与顺序。
type retryWant struct {
	requestCount     int64
	attemptTotal     int64
	retryRate        *float64
	avgAttempts      *float64
	exhaustedRate    *float64
	retrySuccessRate *float64
	byReason         map[string]int64
	byReasonOrder    []string
}

// retryReasons 把 by_reason 折成 map 便于断言（顺序另有专门用例）。
func retryReasons(rows []RoutingRetryReason) map[string]int64 {
	out := make(map[string]int64, len(rows))
	for _, r := range rows {
		out[r.Outcome] = r.ChainCount
	}
	return out
}

// TestSummarizeRoutingRetry 是 spec §6 S1 的夹具表：v3 口径的四条用例 +
// v6 的「ordinal 空洞」回归护栏（纯空洞、混合、空洞全失败）+ 两个「未重试」补充
// + 一条分母对照（三次尝试第 3 次成功：R=1 而不是 Σ_{k≥2}C_k=2）。
//
// 重点断言：
//   - 3 次全失败 ⇒ avg_attempts = 3.0（不是 6.0：Σ k·C_k / C₁ 会得 6.0）；
//   - 3 次全失败 ⇒ retry_rate = 1.0（不是 2.0：Σ_{k≥2} C_k / C₁ 会得 2.0）；
//   - 3 次全失败 ⇒ retry_success_rate = 0（不是 null：0/R 是确定的 0）；
//   - 第 2 次 client_cancel ⇒ exhausted_rate = 0（不是 1.0），by_reason 单列；
//   - 空洞链 1(error) → 3(success) ⇒ retry_rate = 1.0、retry_success_rate = 1.0
//     （旧的 C₂ 口径会得 0 / null，这就是回归护栏）。
func TestSummarizeRoutingRetry(t *testing.T) {
	tests := []struct {
		name string
		rows []repository.RoutingFlowStat
		want retryWant
	}{
		{
			name: "1 请求 2 次尝试，第 2 次成功",
			rows: []repository.RoutingFlowStat{
				retryRow(1, "429", 1, false),
				retryRow(2, "success", 1, true),
			},
			want: retryWant{
				requestCount: 1, attemptTotal: 2,
				retryRate: ptrFloat(1), avgAttempts: ptrFloat(2),
				exhaustedRate: ptrFloat(0), retrySuccessRate: ptrFloat(1),
				byReason:      map[string]int64{"429": 1},
				byReasonOrder: []string{"429"},
			},
		},
		{
			name: "1 请求 3 次尝试，全失败",
			rows: []repository.RoutingFlowStat{
				retryRow(1, "429", 1, false),
				retryRow(2, "5xx", 1, false),
				retryRow(3, "network", 1, true),
			},
			want: retryWant{
				requestCount: 1, attemptTotal: 3,
				retryRate: ptrFloat(1), avgAttempts: ptrFloat(3),
				exhaustedRate: ptrFloat(1), retrySuccessRate: ptrFloat(0),
				byReason:      map[string]int64{"429": 1, "5xx": 1, "network": 1},
				byReasonOrder: []string{"429", "5xx", "network"},
			},
		},
		{
			name: "1 请求 2 次尝试，第 2 次客户端取消",
			rows: []repository.RoutingFlowStat{
				retryRow(1, "429", 1, false),
				retryRow(2, flowOutcomeClientCancel, 1, true),
			},
			want: retryWant{
				requestCount: 1, attemptTotal: 2,
				retryRate: ptrFloat(1), avgAttempts: ptrFloat(2),
				// 取消不是耗尽：X 从分子里减掉 ⇒ 0，而非 1.0。
				exhaustedRate: ptrFloat(0), retrySuccessRate: ptrFloat(0),
				byReason:      map[string]int64{"429": 1, flowOutcomeClientCancel: 1},
				byReasonOrder: []string{"429", flowOutcomeClientCancel},
			},
		},
		{
			name: "只有本地拒绝（无 flow 行）",
			rows: nil,
			want: retryWant{
				requestCount: 0, attemptTotal: 0,
				retryRate: nil, avgAttempts: nil, exhaustedRate: nil, retrySuccessRate: nil,
				byReason:      map[string]int64{},
				byReasonOrder: []string{},
			},
		},
		{
			name: "首发即成功（从未重试，多链）",
			rows: []repository.RoutingFlowStat{
				retryRow(1, "success", 4, true),
			},
			want: retryWant{
				requestCount: 4, attemptTotal: 4,
				retryRate: ptrFloat(0), avgAttempts: ptrFloat(1),
				exhaustedRate: ptrFloat(0),
				// R = C₁ − T₁ = 0 ⇒ 唯一合法的 null（一个请求都没重试过）。
				retrySuccessRate: nil,
				byReason:         map[string]int64{},
				byReasonOrder:    []string{},
			},
		},
		{
			name: "单链首发即成功（从未重试）",
			rows: []repository.RoutingFlowStat{
				retryRow(1, "success", 1, true),
			},
			want: retryWant{
				requestCount: 1, attemptTotal: 1,
				retryRate: ptrFloat(0), avgAttempts: ptrFloat(1),
				exhaustedRate:    ptrFloat(0),
				retrySuccessRate: nil, // 分母 R = 0 ⇒ null，不是 0
				byReason:         map[string]int64{},
				byReasonOrder:    []string{},
			},
		},
		{
			// 分母必须是 R = C₁ − T₁，不是 Σ_{k≥2} C_k：这里后者的值是 2，会把
			// 1/2 = 0.5 当成「重试后成功率」，而真实答案是一次重试请求一次成功 = 1。
			name: "1 请求 3 次尝试，第 3 次成功",
			rows: []repository.RoutingFlowStat{
				retryRow(1, "429", 1, false),
				retryRow(2, "5xx", 1, false),
				retryRow(3, "success", 1, true),
			},
			want: retryWant{
				requestCount: 1, attemptTotal: 3,
				retryRate: ptrFloat(1), avgAttempts: ptrFloat(3),
				exhaustedRate: ptrFloat(0), retrySuccessRate: ptrFloat(1),
				byReason:      map[string]int64{"429": 1, "5xx": 1},
				byReasonOrder: []string{"429", "5xx"},
			},
		},
		{
			// 空结局 token：flowOutcomeToken 只产出六种 token，空串是投影退化。
			// 它仍是一条真实尝试边（计入 attempt_total），但不得铸出「空串原因」
			// 的伪列，也不进 success / cancel 口径——于是按既有口径被算成「耗尽」。
			name: "空结局 token：计入尝试总数但不铸原因列",
			rows: []repository.RoutingFlowStat{
				retryRow(1, "", 4, false),
				retryRow(2, "", 4, true),
			},
			want: retryWant{
				requestCount: 4, attemptTotal: 8,
				retryRate: ptrFloat(1), avgAttempts: ptrFloat(2),
				exhaustedRate: ptrFloat(1), retrySuccessRate: ptrFloat(0),
				byReason:      map[string]int64{},
				byReasonOrder: []string{},
			},
		},
		{
			// **v6 回归护栏**：ordinal 空洞（本地拒绝 / 402 预检不产生边）。
			// 旧 C₂ 口径下 C₂ = 0 ⇒ retry_rate 0、retry_success_rate null。
			name: "ordinal 空洞：1(error) → 3(success)",
			rows: []repository.RoutingFlowStat{
				retryRow(1, "error", 1, false),
				retryRow(3, "success", 1, true),
			},
			want: retryWant{
				requestCount: 1, attemptTotal: 2,
				retryRate: ptrFloat(1), avgAttempts: ptrFloat(2),
				exhaustedRate: ptrFloat(0), retrySuccessRate: ptrFloat(1),
				byReason:      map[string]int64{"error": 1},
				byReasonOrder: []string{"error"},
			},
		},
		{
			// 混合形状：连续链（1 → 2）与空洞链（1 → 3）并存。C₂ = 1 但 R = 2
			// ⇒ 旧的 C₂ 口径得 retry_rate = 0.5。
			name: "混合：连续链与空洞链并存",
			rows: []repository.RoutingFlowStat{
				retryRow(1, "429", 1, false),
				retryRow(2, "success", 1, true),
				retryRow(1, "error", 1, false),
				retryRow(3, "success", 1, true),
			},
			want: retryWant{
				requestCount: 2, attemptTotal: 4,
				retryRate: ptrFloat(1), avgAttempts: ptrFloat(2),
				exhaustedRate: ptrFloat(0), retrySuccessRate: ptrFloat(1),
				byReason:      map[string]int64{"429": 1, "error": 1},
				byReasonOrder: []string{"429", "error"},
			},
		},
		{
			// 空洞链全程失败：R = 1 ⇒ retry_success_rate 是确定的 0；C₂ = 0 会把它
			// 误判成「没重试过」而返回 null。
			name: "ordinal 空洞全程失败：1(error) → 3(error)",
			rows: []repository.RoutingFlowStat{
				retryRow(1, "error", 1, false),
				retryRow(3, "error", 1, true),
			},
			want: retryWant{
				requestCount: 1, attemptTotal: 2,
				retryRate: ptrFloat(1), avgAttempts: ptrFloat(2),
				exhaustedRate: ptrFloat(1), retrySuccessRate: ptrFloat(0),
				byReason:      map[string]int64{"error": 2},
				byReasonOrder: []string{"error"},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := summarizeRoutingRetry(tt.rows)
			t.Logf("实测：request_count=%d attempt_total=%d retry_rate=%s avg_attempts=%s exhausted_rate=%s retry_success_rate=%s by_reason=%v",
				got.RequestCount, got.AttemptTotal, fmtRatio(got.RetryRate), fmtRatio(got.AvgAttempts),
				fmtRatio(got.ExhaustedRate), fmtRatio(got.RetrySuccessRate), got.ByReason)
			require.Equal(t, tt.want.requestCount, got.RequestCount, "request_count = C₁")
			require.Equal(t, tt.want.attemptTotal, got.AttemptTotal, "attempt_total = Σ_k C_k")
			require.Equal(t, tt.want.retryRate, got.RetryRate, "retry_rate = (C₁ − T₁) / C₁")
			require.Equal(t, tt.want.avgAttempts, got.AvgAttempts, "avg_attempts = Σ C_k / C₁")
			require.Equal(t, tt.want.exhaustedRate, got.ExhaustedRate, "exhausted_rate = (C₁ − S − X) / C₁")
			require.Equal(t, tt.want.retrySuccessRate, got.RetrySuccessRate, "retry_success_rate = Σ_{k≥2} S_k / (C₁ − T₁)")
			require.Equal(t, tt.want.byReason, retryReasons(got.ByReason), "by_reason 分组")
			order := make([]string, 0, len(got.ByReason))
			for _, r := range got.ByReason {
				require.NotEmpty(t, r.Outcome, "空结局不得铸出空串原因列")
				order = append(order, r.Outcome)
			}
			require.Equal(t, tt.want.byReasonOrder, order, "by_reason 顺序（chain_count 降序、并列 outcome 升序）")
			// 纯函数不碰边界事实：Routes / QueriedAt 由 QueryRoutingRetry 填。
			require.Zero(t, got.Routes)
			require.True(t, got.QueriedAt.IsZero())
			require.NotNil(t, got.ByReason, "ByReason 恒非 nil（空窗口序列化成 []，不是 null）")
		})
	}
}

// TestSummarizeRoutingRetryNilOnlyWhenDenominatorZero 钉住「null 的语义」：
// 0 是确定的比值，null 只在分母为 0 时出现——两者绝不可互换。
func TestSummarizeRoutingRetryNilOnlyWhenDenominatorZero(t *testing.T) {
	// 重试过但重试后全失败：retry_success_rate 必须是 0，不是 null。
	allFailed := summarizeRoutingRetry([]repository.RoutingFlowStat{
		retryRow(1, "5xx", 2, false),
		retryRow(2, "5xx", 2, true),
	})
	require.Equal(t, ptrFloat(1), allFailed.RetryRate)
	require.Equal(t, ptrFloat(2), allFailed.AvgAttempts)
	require.NotNil(t, allFailed.RetrySuccessRate)
	require.Equal(t, ptrFloat(0), allFailed.RetrySuccessRate)
	require.Equal(t, ptrFloat(1), allFailed.ExhaustedRate)

	// 窗口里只有非 ordinal=1 的残行（不应出现，但不得产出 NaN）：C₁=0 ⇒ R=0 ⇒ 全 null。
	degraded := summarizeRoutingRetry([]repository.RoutingFlowStat{
		retryRow(2, "429", 3, true),
	})
	require.Zero(t, degraded.RequestCount)
	require.Nil(t, degraded.RetryRate)
	require.Nil(t, degraded.AvgAttempts)
	require.Nil(t, degraded.ExhaustedRate)
	require.Nil(t, degraded.RetrySuccessRate)

	// 反过来：C₁ > 0 而 R = 0（一次都没重试）⇒ retry_rate 是确定的 0，
	// retry_success_rate 才是 null。两者同一行集内必须可区分。
	noRetry := summarizeRoutingRetry([]repository.RoutingFlowStat{
		retryRow(1, "success", 3, true),
	})
	require.NotNil(t, noRetry.RetryRate)
	require.Equal(t, ptrFloat(0), noRetry.RetryRate)
	require.Nil(t, noRetry.RetrySuccessRate)
}

// TestQueryRoutingRetryRouteLess 无 route 的默认视图：一次 QueryObservedRouteClasses
// 枚举 + 一次 QueryFlowFactStatsMulti 聚合（不是 N 次单 route 查询）。
func TestQueryRoutingRetryRouteLess(t *testing.T) {
	fs := newFakeStore()
	rc1, err := domain.RouteClassID(10, domain.FormatOpenAIChat, "m", domain.OpChatCompletions)
	require.NoError(t, err)
	rc2, err := domain.RouteClassID(11, domain.FormatOpenAIChat, "m", domain.OpChatCompletions)
	require.NoError(t, err)
	fs.routingObservedRows = []domain.RouteClassIDVal{rc1, rc2}
	fs.routingFlowRows = []repository.RoutingFlowStat{
		retryRow(1, "429", 1, false), retryRow(2, "success", 1, true),
	}
	fixed := time.Date(2026, 9, 20, 12, 34, 56, 0, time.UTC)
	svc := routingSvc(t, fs, nil)
	svc.statsNow = func() time.Time { return fixed }
	from, to := routingBase, routingBase.Add(time.Hour)

	got, err := svc.QueryRoutingRetry(context.Background(), RoutingRetryQuery{From: from, To: to})
	require.NoError(t, err)
	require.Equal(t, 2, got.Routes, "routes = 窗口内观测到的 route 数")
	require.Equal(t, 1, fs.routingObservedCalls)
	require.Equal(t, 1, fs.routingMultiCalls, "默认视图一次查询覆盖全部 route")
	require.Equal(t, []domain.RouteClassIDVal{rc1, rc2}, fs.routingMultiIDs)
	require.Equal(t, int64(1), got.RequestCount)
	require.Equal(t, ptrFloat(1), got.RetryRate)
	require.Equal(t, fixed.UTC(), got.QueriedAt, "queried_at 走注入时钟（可测且与保留守卫同源）")
}

// TestQueryRoutingRetrySingleRoute 有 route：单次 QueryFlowFactStats，Routes=1；
// 且**不走当前计划目录**——历史 route（未出现在计划里）照样能查，不 404。
func TestQueryRoutingRetrySingleRoute(t *testing.T) {
	fs := newFakeStore()
	fs.routingFlowRows = []repository.RoutingFlowStat{
		retryRow(1, "429", 1, false), retryRow(2, "success", 1, true),
	}
	// 计划为空（当前发布计划不认任何 route）——历史 route 仍必须可查。
	svc := routingSvc(t, fs, &scheduler.RoutingPlan{})
	historical := fpHex(0x7e)
	got, err := svc.QueryRoutingRetry(context.Background(), RoutingRetryQuery{
		RouteID: historical, From: routingBase, To: routingBase.Add(time.Hour),
	})
	require.NoError(t, err)
	require.Equal(t, 1, got.Routes)
	require.Zero(t, fs.routingObservedCalls, "显式 route 不枚举 route class")
	require.Zero(t, fs.routingMultiCalls, "单 route 走既有 QueryFlowFactStats（1 次查询）")
	raw, hexErr := domain.HexToID(historical)
	require.NoError(t, hexErr)
	require.Equal(t, domain.RouteClassIDVal(raw), fs.routingFactCall.routeClass, "route 由 HexToID 解析（不经计划目录）")
	require.Equal(t, int64(1), got.RequestCount)
	require.Equal(t, ptrFloat(2), got.AvgAttempts)

	// 非法 hex ⇒ 400（ErrInvalidInput），不是 404。
	_, err = svc.QueryRoutingRetry(context.Background(), RoutingRetryQuery{
		RouteID: "nothex", From: routingBase, To: routingBase.Add(time.Hour),
	})
	require.ErrorIs(t, err, ErrInvalidInput)
}

// TestQueryRoutingRetryRouteCap 无 route 的默认视图上限：**恰好 cap 必须成功**，
// cap+1 才要求显式 route。两侧都钉住，否则 `routes > cap` 被改成 `>= cap` 时
// 只有「超限报错」一侧的用例仍然绿。
func TestQueryRoutingRetryRouteCap(t *testing.T) {
	observedRoutes := func(n int) []domain.RouteClassIDVal {
		out := make([]domain.RouteClassIDVal, n)
		for i := range out {
			out[i][0] = byte(i)
			out[i][1] = byte(i >> 8)
		}
		return out
	}

	t.Run("恰好 cap 必须成功", func(t *testing.T) {
		fs := newFakeStore()
		fs.routingObservedRows = observedRoutes(routingRetryRouteMax)
		fs.routingFlowRows = []repository.RoutingFlowStat{retryRow(1, "success", 1, true)}
		svc := routingSvc(t, fs, nil)

		got, err := svc.QueryRoutingRetry(context.Background(), RoutingRetryQuery{
			From: routingBase, To: routingBase.Add(time.Hour),
		})
		require.NoError(t, err, "routes == cap 是合法默认视图（off-by-one 会把它误判成 400）")
		require.Equal(t, routingRetryRouteMax, got.Routes)
		require.Equal(t, 1, fs.routingMultiCalls, "恰好 cap 走一次 ANY($1) 聚合")
	})

	t.Run("cap+1 必须报错", func(t *testing.T) {
		fs := newFakeStore()
		fs.routingObservedRows = observedRoutes(routingRetryRouteMax + 1)
		svc := routingSvc(t, fs, nil)

		_, err := svc.QueryRoutingRetry(context.Background(), RoutingRetryQuery{
			From: routingBase, To: routingBase.Add(time.Hour),
		})
		require.Error(t, err)
		require.ErrorIs(t, err, ErrInvalidInput, "超限 = 客户端可纠正的 400")
		require.Contains(t, err.Error(), "exceed")
		require.Contains(t, err.Error(), "pass route")
		require.Zero(t, fs.routingMultiCalls, "超限不得发聚合查询")
	})
}

// TestSummarizeRoutingRetryByReasonTieOrder 并列序的确定性：chain_count 全相等
// 时按 outcome 升序，且与输入行序无关（map 遍历顺序不得泄漏到输出）。
// 6 个并列项让「删掉 tie-break」的变异几乎不可能碰巧通过（1/6!），
// 再用固定种子的 20 次置换做稳定性断言。
func TestSummarizeRoutingRetryByReasonTieOrder(t *testing.T) {
	outcomes := []string{"429", "4xx", "5xx", "client_cancel", "error", "network"}
	want := append([]string(nil), outcomes...) // 已是 ASCII 升序
	rows := make([]repository.RoutingFlowStat, 0, len(outcomes))
	for _, outcome := range outcomes {
		rows = append(rows, retryRow(2, outcome, 3, true))
	}

	order := func() []string {
		out := make([]string, 0, len(outcomes))
		for _, r := range summarizeRoutingRetry(rows).ByReason {
			require.Equal(t, int64(3), r.ChainCount, "夹具里所有并列项计数相等")
			out = append(out, r.Outcome)
		}
		return out
	}

	require.Equal(t, want, order(), "并列必须按 outcome 升序")
	rng := rand.New(rand.NewSource(1))
	for i := range 20 {
		rng.Shuffle(len(rows), func(a, b int) { rows[a], rows[b] = rows[b], rows[a] })
		require.Equal(t, want, order(), "第 %d 次置换后序列必须逐字不变", i+1)
	}
}

// TestQueryRoutingRetryEmptyWindow 空窗口：0 计数 + null 比值 + 空 by_reason，
// 且**不报错**（观测面不因「没有流量」404/500）。
func TestQueryRoutingRetryEmptyWindow(t *testing.T) {
	fs := newFakeStore()
	fs.routingObservedRows = nil
	svc := routingSvc(t, fs, nil)

	got, err := svc.QueryRoutingRetry(context.Background(), RoutingRetryQuery{
		From: routingBase, To: routingBase.Add(time.Hour),
	})
	require.NoError(t, err)
	require.Zero(t, got.Routes)
	require.Zero(t, got.RequestCount)
	require.Zero(t, got.AttemptTotal)
	require.Nil(t, got.RetryRate)
	require.Nil(t, got.AvgAttempts)
	require.Nil(t, got.ExhaustedRate)
	require.Nil(t, got.RetrySuccessRate)
	require.NotNil(t, got.ByReason)
	require.Empty(t, got.ByReason)
}

// TestQueryRoutingRetryWindowGuards 窗口/保留守卫与 /routing/flow **同一份**
// 实现（≥90d、保留期裁、必填/倒置），不是第二份判定。
func TestQueryRoutingRetryWindowGuards(t *testing.T) {
	fs := newFakeStore()
	fs.routingObservedRows = nil
	fixed := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	svc := routingSvc(t, fs, nil)
	svc.routingRetentionDays = 7
	svc.statsNow = func() time.Time { return fixed }

	ok := RoutingRetryQuery{From: fixed.Add(-24 * time.Hour), To: fixed}
	_, err := svc.QueryRoutingRetry(context.Background(), ok)
	require.NoError(t, err)

	for name, q := range map[string]RoutingRetryQuery{
		"missing from":  {To: fixed},
		"inverted":      {From: fixed, To: fixed},
		"over 90d":      {From: fixed.Add(-90*24*time.Hour - time.Hour), To: fixed},
		"before cutoff": {From: fixed.Add(-8 * 24 * time.Hour), To: fixed},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.QueryRoutingRetry(context.Background(), q)
			require.ErrorIs(t, err, ErrInvalidInput)
		})
	}
}

// TestQueryRoutingRetryNotWired 事实读能力未装配 ⇒ 明确错误（不是空结果）。
func TestQueryRoutingRetryNotWired(t *testing.T) {
	svc := New(Deps{Store: fakeStoreNoFacts{}, Scheduler: &fakeRoutingSched{}, Invalidate: NopInvalidator{}, EmailCodeStore: testEmailCodes})
	_, err := svc.QueryRoutingRetry(context.Background(), RoutingRetryQuery{
		From: routingBase, To: routingBase.Add(time.Hour),
	})
	require.True(t, errors.Is(err, errRoutingNotWired), "unexpected error: %v", err)
}
