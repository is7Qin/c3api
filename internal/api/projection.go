// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package api

import "github.com/is7qin/c3api/internal/domain"

// LogMappedModel 返回可选的 mapped_model 审计值。隐式映射在用量日志里**故意**
// 看起来没映射（与 ClientResponseModel 的"客户端看到的模型"是相反问题）。
func (s *Selection) LogMappedModel(reqModel string) string {
	if s.ModelMappingMode == domain.ModelMappingModeExplicit && s.Model != reqModel {
		return s.Model
	}
	return ""
}

// PriceModel 返回本 selection 的计价模型。
func (s *Selection) PriceModel(reqModel string) string {
	if s.ModelMappingMode == domain.ModelMappingModeImplicit {
		return reqModel
	}
	return s.Model
}

// ClientResponseModel 返回响应头里要回写客户端的模型（仅 implicit 映射有值）。
func (s *Selection) ClientResponseModel(reqModel string) string {
	if s.ModelMappingMode == domain.ModelMappingModeImplicit {
		return reqModel
	}
	return ""
}
