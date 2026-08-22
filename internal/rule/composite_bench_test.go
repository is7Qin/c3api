// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package rule

// R-6（2026-08-22-rule-verify-remediation-design）：smallSetThreshold=4 实测背书。
// 阈值切换的是 _in 数组基数维度（set.go：len<=4 线性扫 vals，>4 建 map 查表），
// 覆盖 intSet（http_status_in）与 stringSet（model_in）两族；substringSet 恒线性
// （设计如此，无阈值）。扫 set 基数而非规则条数——阈值按集合大小分叉，非规则数。

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// benchSink 防止编译器死代码消除 Classify 结果（包级变量，计时区内只写不读）。
var (
	benchSinkThen   domain.RuleThen
	benchSinkPunish bool
)

func benchEngine(b *testing.B, rules ...domain.Rule) *RuleEngine {
	b.Helper()
	st := newFakeRuleStore()
	for _, r := range rules {
		if _, err := st.CreateRule(context.Background(), r); err != nil {
			b.Fatal(err)
		}
	}
	e := New(Config{}, st, nil)
	if err := e.Reload(context.Background()); err != nil {
		b.Fatal(err)
	}
	return e
}

// benchModels n 个互异、长度真实感（~17B）的模型名；探测取末位=线性扫最坏情形。
func benchModels(n int) []string {
	models := make([]string, n)
	for i := range models {
		models[i] = fmt.Sprintf("gpt-4o-mini-%04d", i)
	}
	return models
}

// benchStatuses n 个互异状态码 ⊂ [400,599]（ValidateWhen 合法域），500..500+n-1。
func benchStatuses(n int) []int {
	codes := make([]int, n)
	for i := range codes {
		codes[i] = 500 + i
	}
	return codes
}

// benchSubstrings n 个互异错误子串；事件消息含末位子串（命中）/全不含（未命中）。
func benchSubstrings(n int) []string {
	subs := make([]string, n)
	for i := range subs {
		subs[i] = fmt.Sprintf("overloaded-backend-%04d", i)
	}
	return subs
}

// assertHit/assertMiss 计时区外锚定行为：hit 必中且 punish、miss 必落默认归一 502。
func assertHit(b *testing.B, e *RuleEngine, ev Event) {
	b.Helper()
	then, punish := e.Classify(ev)
	require.NotNil(b, then.Status)
	require.True(b, punish)
}

func assertMiss(b *testing.B, e *RuleEngine, ev Event) {
	b.Helper()
	then, punish := e.Classify(ev)
	require.NotNil(b, then.ResponseCode)
	require.Equal(b, 502, *then.ResponseCode)
	require.False(b, punish)
}

// BenchmarkClassifyComposite 复合 IN 规则 Classify 全路径：
// 每 set 基数一个子基准，HIT 探测取集合末位（线性扫最坏情形）、MISS 探测必不命中
// （走默认归一分支——该分支有意分配 intPtr/strPtr，allocs>0 属预期，零分配主张
// 只覆盖 matchBasic 匹配路径）。multi_* 为 kind+三 IN 维度复合（跨字段 AND）。
func BenchmarkClassifyComposite(b *testing.B) {
	sizes := []int{1, 2, 3, 4, 5, 6, 8, 16, 64}

	for _, n := range sizes {
		models := benchModels(n)
		statuses := benchStatuses(n)
		hitModel := models[n-1]
		missModel := "no-such-model-anywhere"
		hitCode := statuses[n-1]
		missCode := 599 // 集合最大 500+n-1 ≤ 563 < 599

		b.Run(fmt.Sprintf("model_hit_%02d", n), func(b *testing.B) {
			e := benchEngine(b, domain.Rule{
				Name: "bench", Enabled: true, Priority: 10,
				When: domain.RuleWhen{Kind: strPtr("5xx"), ModelIn: models},
				Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy)},
			})
			ev := Event{Kind: Kind5xx, Model: hitModel}
			assertHit(b, e, ev)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				then, punish := e.Classify(ev)
				benchSinkThen, benchSinkPunish = then, punish
			}
		})

		b.Run(fmt.Sprintf("model_miss_%02d", n), func(b *testing.B) {
			e := benchEngine(b, domain.Rule{
				Name: "bench", Enabled: true, Priority: 10,
				When: domain.RuleWhen{Kind: strPtr("5xx"), ModelIn: models},
				Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy)},
			})
			ev := Event{Kind: Kind5xx, Model: missModel}
			assertMiss(b, e, ev)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				then, punish := e.Classify(ev)
				benchSinkThen, benchSinkPunish = then, punish
			}
		})

		b.Run(fmt.Sprintf("status_hit_%02d", n), func(b *testing.B) {
			e := benchEngine(b, domain.Rule{
				Name: "bench", Enabled: true, Priority: 10,
				When: domain.RuleWhen{Kind: strPtr("5xx"), HTTPStatusIn: statuses},
				Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy)},
			})
			ev := Event{Kind: Kind5xx, HTTPStatus: &hitCode}
			assertHit(b, e, ev)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				then, punish := e.Classify(ev)
				benchSinkThen, benchSinkPunish = then, punish
			}
		})

		b.Run(fmt.Sprintf("status_miss_%02d", n), func(b *testing.B) {
			e := benchEngine(b, domain.Rule{
				Name: "bench", Enabled: true, Priority: 10,
				When: domain.RuleWhen{Kind: strPtr("5xx"), HTTPStatusIn: statuses},
				Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy)},
			})
			ev := Event{Kind: Kind5xx, HTTPStatus: &missCode}
			assertMiss(b, e, ev)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				then, punish := e.Classify(ev)
				benchSinkThen, benchSinkPunish = then, punish
			}
		})
	}

	// 多维复合（kind + http_status_in + model_in + error_message_contains_in）：
	// 代表性基数 4（=阈值，线性）与 8（>阈值，map）各一组 HIT/MISS。
	for _, n := range []int{4, 8} {
		statuses := benchStatuses(n)
		models := benchModels(n)
		subs := benchSubstrings(n)
		hitCode := statuses[n-1]

		b.Run(fmt.Sprintf("multi_hit_%02d", n), func(b *testing.B) {
			e := benchEngine(b, domain.Rule{
				Name: "bench", Enabled: true, Priority: 10,
				When: domain.RuleWhen{
					Kind:                   strPtr("5xx"),
					HTTPStatusIn:           statuses,
					ModelIn:                models,
					ErrorMessageContainsIn: subs,
				},
				Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy)},
			})
			msg := fmt.Sprintf("upstream failed: %s (retryable)", subs[n-1])
			ev := Event{Kind: Kind5xx, HTTPStatus: &hitCode, Model: models[n-1], ErrorMessage: msg}
			assertHit(b, e, ev)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				then, punish := e.Classify(ev)
				benchSinkThen, benchSinkPunish = then, punish
			}
		})

		b.Run(fmt.Sprintf("multi_miss_%02d", n), func(b *testing.B) {
			e := benchEngine(b, domain.Rule{
				Name: "bench", Enabled: true, Priority: 10,
				When: domain.RuleWhen{
					Kind:                   strPtr("5xx"),
					HTTPStatusIn:           statuses,
					ModelIn:                models,
					ErrorMessageContainsIn: subs,
				},
				Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy)},
			})
			missCode := 599 // 状态维不命中 → 最先早退
			ev := Event{Kind: Kind5xx, HTTPStatus: &missCode, Model: models[n-1], ErrorMessage: "clean timeout"}
			assertMiss(b, e, ev)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				then, punish := e.Classify(ev)
				benchSinkThen, benchSinkPunish = then, punish
			}
		})
	}
}
