// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package rule

import (
	"strings"
	"time"

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

// 非热路径：Match 供测试/窗口判定线性扫；热路径 Classify 走 engine.compiledRule Set（零分配早退）。
func matchBasic(w domain.RuleWhen, ev Event) bool {
	if w.Kind != nil && kindFromString(*w.Kind) != ev.Kind {
		return false
	}
	if w.HTTPStatus != nil && (ev.HTTPStatus == nil || *w.HTTPStatus != *ev.HTTPStatus) {
		return false
	}
	if len(w.HTTPStatusIn) > 0 {
		if ev.HTTPStatus == nil {
			return false
		}
		found := false
		for _, v := range w.HTTPStatusIn {
			if *ev.HTTPStatus == v {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if w.ErrorMessageContains != nil && !strings.Contains(ev.ErrorMessage, *w.ErrorMessageContains) {
		return false
	}
	if len(w.ErrorMessageContainsIn) > 0 {
		found := false
		for _, sub := range w.ErrorMessageContainsIn {
			if strings.Contains(ev.ErrorMessage, sub) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if w.AccountID != nil && ev.AccountID != *w.AccountID {
		return false
	}
	if w.TemplateID != nil && ev.TemplateID != *w.TemplateID {
		return false
	}
	if w.GroupID != nil && (ev.GroupID == nil || *ev.GroupID != *w.GroupID) {
		return false
	}
	if w.Model != nil && *w.Model != ev.Model {
		return false
	}
	if len(w.ModelIn) > 0 {
		found := false
		for _, v := range w.ModelIn {
			if ev.Model == v {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// Match 规则 when 与事件（+ 窗口计数）是否匹配：等值/子串/计数阈值/比例。
// 窗口比例 = t429(或 failure) / (ok+failure+t429)，仅当 total ≥ CountTotalGE
// 时参与判定（样本不足不满足，ValidateWhen 已保证比例类必配 CountTotalGE，
// 此处仍防御）。
func Match(w domain.RuleWhen, ev Event, wc windowSnapshot) bool {
	if !matchBasic(w, ev) {
		return false
	}
	if w.Count429GE != nil && wc.t429 < *w.Count429GE {
		return false
	}
	if w.CountFailureGE != nil && wc.failure < *w.CountFailureGE {
		return false
	}
	if w.CountOKGE != nil && wc.ok < *w.CountOKGE {
		return false
	}
	if w.CountTotalGE != nil && wc.total() < *w.CountTotalGE {
		return false
	}
	if w.Ratio429GE != nil && !ratioPass(wc.t429, wc.total(), w.CountTotalGE, *w.Ratio429GE) {
		return false
	}
	if w.RatioFailureGE != nil && !ratioPass(wc.failure, wc.total(), w.CountTotalGE, *w.RatioFailureGE) {
		return false
	}
	return true
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

// Apply 解析 then 动作：cooldownUntil = OccurredAt + 解析后的 cooldown；
// cooldown 未配但事件带 ResetAt 时用 ResetAt（M2 残留：resetAt 语义保留）。
// Status 为 nil 返回 nil 状态 = 只改权重（或只改冷却）。
func Apply(t domain.RuleThen, ev Event) (*domain.AccountStatus, *time.Time, *int) {
	var cd *time.Time
	if t.Cooldown != nil {
		if d, err := time.ParseDuration(*t.Cooldown); err == nil && d > 0 {
			c := ev.OccurredAt.Add(d)
			cd = &c
		}
	} else if ev.ResetAt != nil {
		cd = ev.ResetAt
	}
	return t.Status, cd, t.Weight
}