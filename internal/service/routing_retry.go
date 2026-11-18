// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// 重试观测面 S1（spec 2026-11-18 §2）：把 routing_flow_fact 的聚合行折成
// 「重试」卡的四指标 + 按原因分解。**纯函数**——零 IO、零时钟、零仓储依赖：
// 输入就是事实读面已经聚合好的 []repository.RoutingFlowStat，输出是可序列化
// 的摘要。查询边界（route 解析、观测窗口守卫、routes/queried_at 事实）在
// routing.go 的 QueryRoutingRetry。
//
// 口径（spec §2.2 v6，逐条照抄）：
//
//	C_k = Σ ChainCount WHERE Ordinal = k
//	S_k = Σ ChainCount WHERE Ordinal = k AND Outcome = success；S = Σ_k S_k
//	X_k = Σ ChainCount WHERE Ordinal = k AND Outcome = client_cancel；X = Σ_k X_k
//	T₁  = Σ ChainCount WHERE Ordinal = 1 AND IsTerminal
//
//	request_count      = C₁                （打到过上游的请求数；本地拒绝不落 flow）
//	attempt_total      = Σ_k C_k           （每条边 = 一次尝试）
//	retry_rate         = (C₁ − T₁) / C₁    （R = C₁ − T₁ = 重试过的请求数）
//	avg_attempts       = Σ_k C_k / C₁      （**不是** Σ k·C_k / C₁：后者三尝试会得 6.0）
//	exhausted_rate     = (C₁ − S − X) / C₁ （必须减 X，否则主动取消被算成重试耗尽）
//	retry_success_rate = Σ_{k≥2} S_k / (C₁ − T₁)（分母 = 重试过的请求数；全失败 = 0 不是 null）
//
// **重试过的请求数不是 C₂**：ordinal 允许空洞（本地拒绝 / 402 预检不产生边），
// 链 `1(error) → 3(success)` 没有任何 ordinal=2 边——拿 C₂ 当「重试过的请求数」
// 会把它当成「没重试过」，retry_rate 得 0、retry_success_rate 分母 0 得 null。
// settle 在 fold 之前把链的**最后一条**边标 is_terminal，且只在没有任何边已标时
// 才标（internal/proxy/pipeline_observer.go:381-398）⇒ 每条完整链恰好一条
// terminal 边；残链（panicked / 无 last）根本不 fold。故 T₁ = 终局落在
// ordinal=1 的链数，R = C₁ − T₁ = 至少重试过一次的链数，对空洞免疫且恒 ≥ 0
// （T₁ 与 C₁ 同域 ordinal=1，T₁ 是 C₁ 的子集和）。IsTerminal 在这里**只判
// 「是不是链最后一条」**，不判成败；也不进 ExhaustedRate / S 的口径。
//
// 比值分母为 0 ⇒ nil（handler 侧序列化成 null），绝不 0/NaN。null 只在
// 对应的分母真的为 0 时出现：R = 0（一个请求都没重试过）⇒ retry_success_rate
// 是 null，而 retry_rate 在 C₁ > 0 时是确定的 0；C₁ = 0 ⇒ 四个比值全 nil。

import (
	"sort"
	"time"

	"github.com/is7qin/c3api/internal/repository"
)

// flowOutcomeSuccess / flowOutcomeClientCancel 是 flow 边结局的两个「非失败」
// 词表值（token 唯一来源 = proxy.flowOutcomeToken）。其余结局（429 / 5xx /
// network / 4xx / error）一律是失败边。client_cancel 是**取消**不是失败：它
// 既不进 exhausted_rate 的分子（否则主动取消会被算成「重试耗尽」），也从
// 失败边里单列出来（运营需要看到「取消了多少」），故 ByReason 的枚举域 =
// 非 success 结局（失败 + 单列的取消）。
const (
	flowOutcomeSuccess      = "success"
	flowOutcomeClientCancel = "client_cancel"
)

// RoutingRetryReason 按结局分解的一列。ChainCount 是**尝试维度**：走过该
// 结局边的请求数之和（同一请求的多条尝试边各计一次）。
type RoutingRetryReason struct {
	Outcome    string
	ChainCount int64
}

// RoutingRetrySummary 重试聚合结果。指标字段全部由 summarizeRoutingRetry
// 从行集算出；Routes / QueriedAt 是查询边界的事实（显式 route = 1、缺省 =
// 窗口内观测到的 route 数；查询时刻），由 QueryRoutingRetry 在纯函数之后
// 填充——纯函数不碰时钟。
//
// 计数在空窗口恒 0（真实值就是 0，不伪造成 null）；四个比值 nil = 分母为 0。
type RoutingRetrySummary struct {
	Routes    int
	QueriedAt time.Time

	RequestCount int64 // C₁
	AttemptTotal int64 // Σ_k C_k

	RetryRate        *float64 // (C₁ − T₁) / C₁；T₁ = 终局停在首发的链数
	AvgAttempts      *float64 // Σ_k C_k / C₁（不是 Σ k·C_k / C₁）
	ExhaustedRate    *float64 // (C₁ − S − X) / C₁
	RetrySuccessRate *float64 // Σ_{k≥2} S_k / (C₁ − T₁)

	ByReason []RoutingRetryReason
}

// summarizeRoutingRetry 是纯聚合：行序无关（分组后按 ChainCount 降序、并列
// outcome 升序重排，输出确定性），不读时钟、不碰 store。ByReason 恒非 nil
// （空窗口 = 空切片 → JSON []）。
func summarizeRoutingRetry(rows []repository.RoutingFlowStat) RoutingRetrySummary {
	var (
		c1, t1        int64 // C₁ / T₁（终局落在 ordinal=1 的链数和）
		attemptTotal  int64 // Σ_k C_k
		successTotal  int64 // S
		cancelTotal   int64 // X
		retrySuccess  int64 // Σ_{k≥2} S_k
		reasonsByName = make(map[string]int64, 4)
	)
	for _, row := range rows {
		attemptTotal += row.ChainCount
		if row.Ordinal == 1 {
			c1 += row.ChainCount
			// 终局停在首发 ⇒ 这条链没有重试过。ordinal 空洞（1 → 3）的首发边
			// 不 terminal，照样落进 R = C₁ − T₁。
			if row.IsTerminal {
				t1 += row.ChainCount
			}
		}
		switch row.Outcome {
		case flowOutcomeSuccess:
			successTotal += row.ChainCount
			if row.Ordinal >= 2 {
				retrySuccess += row.ChainCount
			}
		case flowOutcomeClientCancel:
			// 取消：并入 X（从耗尽率里剔除），并在 ByReason 里单列一列。
			cancelTotal += row.ChainCount
			reasonsByName[row.Outcome] += row.ChainCount
		case "":
			// 空结局不在 flow 词表里（flowOutcomeToken 只产出六种 token），
			// 出现即上游投影退化：不铸出「空串原因」的伪列，也不并入任何比值
			// 之外的口径——它仍计入 attempt_total（那是一条真实尝试边）。
		default:
			reasonsByName[row.Outcome] += row.ChainCount
		}
	}
	// T₁ 是 C₁ 的子集和（同域 ordinal=1 行）⇒ retried = C₁ − T₁ 恒 ≥ 0
	// （不做钳制：负数只能来自违约数据）。
	retried := c1 - t1
	out := RoutingRetrySummary{
		RequestCount:     c1,
		AttemptTotal:     attemptTotal,
		RetryRate:        ratioOrNil(retried, c1),
		AvgAttempts:      ratioOrNil(attemptTotal, c1),
		ExhaustedRate:    ratioOrNil(c1-successTotal-cancelTotal, c1),
		RetrySuccessRate: ratioOrNil(retrySuccess, retried),
		ByReason:         make([]RoutingRetryReason, 0, len(reasonsByName)),
	}
	for outcome, n := range reasonsByName {
		out.ByReason = append(out.ByReason, RoutingRetryReason{Outcome: outcome, ChainCount: n})
	}
	// 横条按占比从大到小读；并列按 outcome 升序，保证同一行集恒得同一序列。
	sort.Slice(out.ByReason, func(i, j int) bool {
		if out.ByReason[i].ChainCount != out.ByReason[j].ChainCount {
			return out.ByReason[i].ChainCount > out.ByReason[j].ChainCount
		}
		return out.ByReason[i].Outcome < out.ByReason[j].Outcome
	})
	return out
}

// ratioOrNil 是「分母为 0 ⇒ null」的唯一实现：分母 0 返回 nil，绝不返回 0
// 或 NaN（0/0 与 x/0 在前端都不可区分于「真的 0%」，那是撒谎）。
func ratioOrNil(num, den int64) *float64 {
	if den == 0 {
		return nil
	}
	v := float64(num) / float64(den)
	return &v
}
