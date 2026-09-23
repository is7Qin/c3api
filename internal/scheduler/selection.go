// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"github.com/is7qin/c3api/internal/domain"
)

// Select 选号并占用并发槽：执行预编译 DecisionView 计划
// （NewAttemptPlan + ReserveAttempt：lane 顺序、完整唯一 overflow 尾、
// reservation reject 不耗 attempt）。未编译路由（编译车道尚未产出该桶）
// 直接 ErrFormatUnavailable。
// 调用方完成请求后必须 Release + MarkResult。
//
// the session is a stack value; the scheduler call takes a stack
// pointer that never escapes (nothing retained across calls).
func (s *Scheduler) Select(groupID int64, format domain.RequestFormat, model string) (*Selection, error) {
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{ApplyModelMapping: true}, RouteRefFor(groupID, string(format), model))
	if err != nil {
		return nil, err
	}
	sel, _, rerr := s.ReserveAttempt(&plan)
	if rerr != nil {
		return nil, rerr
	}
	return sel, nil
}

// SelectOpaque selects from a compiled route without applying model mapping.
func (s *Scheduler) SelectOpaque(groupID int64, format domain.RequestFormat, model string) (*Selection, error) {
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{ApplyModelMapping: false}, RouteRefFor(groupID, string(format), model))
	if err != nil {
		return nil, err
	}
	sel, _, reserveErr := s.ReserveAttempt(&plan)
	return sel, reserveErr
}

// LogMappedModel returns the optional mapped_model audit value. Implicit
// mappings intentionally look unmapped in usage logs.
func (s *Selection) LogMappedModel(reqModel string) string {
	if s.ModelMappingMode == domain.ModelMappingModeExplicit && s.Model != reqModel {
		return s.Model
	}
	return ""
}

// PriceModel returns the model whose pricing applies to this selection.
func (s *Selection) PriceModel(reqModel string) string {
	if s.ModelMappingMode == domain.ModelMappingModeImplicit {
		return reqModel
	}
	return s.Model
}

func (s *Selection) ClientResponseModel(reqModel string) string {
	if s.ModelMappingMode == domain.ModelMappingModeImplicit {
		return reqModel
	}
	return ""
}
