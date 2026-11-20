// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"slices"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// validateFormatsForType 校验格式集合全部落在该凭据类型允许的格式内（唯一真值是
// domain.allowedFormats，经 FormatsAllowedFor 读副本）。创建/更新模板与批量更新
// 模板共用这一处，避免同一策略两份字面量。
//
// !ok 必须**显式早退**：FormatsAllowedFor 对表外类型返回 nil，若把 nil 直接交给
// 下面的循环，零次迭代 = 放行任意格式（对抗评审的 Critical）。站点 1 的模板来自
// DB，表外历史 credential_type 因此 fail-closed——这是刻意的行为选择。
func validateFormatsForType(t credential.Type, fs []domain.RequestFormat) error {
	allowed, ok := domain.FormatsAllowedFor(t)
	if !ok {
		return ErrInvalidInput
	}
	for _, f := range fs {
		if !slices.Contains(allowed, f) {
			return ErrInvalidInput
		}
	}
	return nil
}
