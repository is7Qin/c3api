// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
//
// evaluate.go holds the non-hot Match entry point used by tests and window
// adjudication. Basic-condition matching is NOT duplicated here: Match compiles
// the when into a compiledRule and calls the single matchBasic (engine.go), so
// the hot path (compiledRule + prebuilt Sets) and the cold path cannot drift.

package rule

import (
	"github.com/is7qin/c3api/internal/domain"
)

// ruleNeedsWindow 规则是否依赖窗口计数（含比例——比例分母来自窗口聚合）。
func ruleNeedsWindow(w domain.RuleWhen) bool {
	return w.Count429GE != nil || w.CountFailureGE != nil || w.CountOKGE != nil ||
		w.CountTotalGE != nil || w.Ratio429GE != nil || w.RatioFailureGE != nil
}

// ruleWindowSeconds 规则统计窗口秒数；未配置取默认。
func ruleWindowSeconds(w domain.RuleWhen) int {
	if w.WindowSeconds != nil && *w.WindowSeconds > 0 {
		return *w.WindowSeconds
	}
	return defaultWindowSeconds
}

// Match 规则 when 与事件（+ 窗口计数）是否匹配：等值/子串/计数阈值/比例。
// 窗口比例 = t429(或 failure) / (ok+failure+t429)，仅当 total ≥ CountTotalGE
// 时参与判定（样本不足不满足，ValidateWhen 已保证比例类必配 CountTotalGE，
// 此处仍防御）。
//
// 非热路径：基础条件复用唯一 matchBasic——把 when 编译成 compiledRule（一次
// 性小分配）后走与热路径相同的判定；窗口条件复用 matchWindow。两处都不再
// 另写一套。
func Match(w domain.RuleWhen, ev Event, wc windowSnapshot) bool {
	r, err := compileRule(domain.Rule{When: w})
	if err != nil || !matchBasic(ev, r) {
		return false
	}
	return matchWindow(w, wc)
}

// ratioPass 比例阈值判定：分母为 total；total < CountTotalGE 时样本不足不满足。
func ratioPass(numerator, total int, totalGE *int, ratio float64) bool {
	if totalGE == nil || total < *totalGE {
		return false
	}
	if total == 0 {
		return false
	}
	return float64(numerator)/float64(total) >= ratio
}
