// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package rule

import (
	"fmt"
	"time"

	"github.com/is7qin/c3api/internal/domain"
)

// ValidateWhen when 语义校验（字段白名单/未知字段由 service 层 JSON 反序列化
// DisallowUnknownFields 挡下；此处查语义）：
//   - kind 必须为 ok/429/4xx/5xx/network 之一（error 已删除——4xx 独立、
//     连接级独立 network，用户裁决）
//   - kind=ok 与 error_message_contains 不兼容（ok 事件错误信息恒空 → 永不命中）
//   - 计数阈值 ≥ 0、window_seconds ≥ 1
//   - 比例 ∈ [0,1] 且必须配 count_total_ge（比例样本下限）
func ValidateWhen(w domain.RuleWhen) error {
	if w.Model != nil && *w.Model == "" {
		return fmt.Errorf("when.model must be non-empty, got %q", *w.Model)
	}
	if w.Kind != nil && kindFromString(*w.Kind) < 0 {
		return fmt.Errorf("when.kind must be ok/429/4xx/5xx/network, got %q", *w.Kind)
	}
	// 确定性死配置：ok 事件不带错误信息，contains 恒假 → 规则永不命中。
	// 其余 kind 交叉组合（如 kind=ok + count_429_ge）为合法观察者语义，放行。
	if w.Kind != nil && kindFromString(*w.Kind) == KindOK && w.ErrorMessageContains != nil {
		return fmt.Errorf("when.error_message_contains is incompatible with when.kind=ok")
	}
	if w.WindowSeconds != nil && *w.WindowSeconds < 1 {
		return fmt.Errorf("when.window_seconds must be >= 1, got %d", *w.WindowSeconds)
	}
	for name, v := range map[string]*int{
		"when.count_429_ge":     w.Count429GE,
		"when.count_failure_ge": w.CountFailureGE,
		"when.count_ok_ge":      w.CountOKGE,
		"when.count_total_ge":   w.CountTotalGE,
	} {
		if v != nil && *v < 0 {
			return fmt.Errorf("%s must be >= 0, got %d", name, *v)
		}
	}
	if w.Ratio429GE != nil {
		if w.CountTotalGE == nil {
			return fmt.Errorf("when.ratio_429_ge requires when.count_total_ge")
		}
		if *w.Ratio429GE < 0 || *w.Ratio429GE > 1 {
			return fmt.Errorf("when.ratio_429_ge must be in [0,1], got %v", *w.Ratio429GE)
		}
	}
	if w.RatioFailureGE != nil {
		if w.CountTotalGE == nil {
			return fmt.Errorf("when.ratio_failure_ge requires when.count_total_ge")
		}
		if *w.RatioFailureGE < 0 || *w.RatioFailureGE > 1 {
			return fmt.Errorf("when.ratio_failure_ge must be in [0,1], got %v", *w.RatioFailureGE)
		}
	}
	return nil
}

// ValidateThen then 动作校验：至少一个动作（status/cooldown/weight/response_code/custom_message 任一非nil——
// 指针即意图，nil=透传；seed-4xx-400 nil/nil 为种子特例直插 store 不走本校验，用户规则 nil/nil 视为无效）；
// status 合法枚举；cooldown 可 time.ParseDuration 解析且 > 0；weight ∈ [0,100]；
// ResponseCode!=nil 需 400-599；CustomMessage==ptr("") 拒绝。
func ValidateThen(t domain.RuleThen) error {
	if t.Status == nil && t.Cooldown == nil && t.Weight == nil && t.ResponseCode == nil && t.CustomMessage == nil {
		return fmt.Errorf("then must set at least one of status/cooldown/weight/response_code/custom_message")
	}
	if t.Status != nil && !validStatus(*t.Status) {
		return fmt.Errorf("then.status must be active/unhealthy/429/disabled, got %q", *t.Status)
	}
	if t.Cooldown != nil {
		d, err := time.ParseDuration(*t.Cooldown)
		if err != nil {
			return fmt.Errorf("then.cooldown: %w", err)
		}
		if d <= 0 {
			return fmt.Errorf("then.cooldown must be > 0, got %q", *t.Cooldown)
		}
	}
	if t.Weight != nil && (*t.Weight < 0 || *t.Weight > 100) {
		return fmt.Errorf("then.weight must be in [0,100], got %d", *t.Weight)
	}
	if t.ResponseCode != nil && (*t.ResponseCode < 400 || *t.ResponseCode > 599) {
		return fmt.Errorf("then.response_code must be in [400,599], got %d", *t.ResponseCode)
	}
	if t.CustomMessage != nil && *t.CustomMessage == "" {
		return fmt.Errorf("then.custom_message must be non-empty, got %q", *t.CustomMessage)
	}
	return nil
}

func validStatus(s domain.AccountStatus) bool {
	switch s {
	case domain.StatusActive, domain.StatusUnhealthy, domain.Status429, domain.StatusDisabled:
		return true
	}
	return false
}
