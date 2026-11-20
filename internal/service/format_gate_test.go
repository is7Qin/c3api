// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"testing"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/stretchr/testify/require"
)

// TestValidateFormatsForTypeUnknownType 表外类型必须**显式拒绝**：FormatsAllowedFor
// 返回 nil 时，若丢掉 `!ok` 早退，下面的循环零次迭代 = 放行任意格式。空切片那条
// 专门咬这个形态——nil 交给循环同样零次。
func TestValidateFormatsForTypeUnknownType(t *testing.T) {
	for _, typ := range []credential.Type{"", "bogus", "codex_oauth"} {
		require.ErrorIs(t, validateFormatsForType(typ, []domain.RequestFormat{domain.FormatAnthropic}), ErrInvalidInput,
			"%q + 任意格式必须拒绝", typ)
		require.ErrorIs(t, validateFormatsForType(typ, []domain.RequestFormat{}), ErrInvalidInput,
			"%q + 空切片也必须拒绝（nil 循环零次 ≠ 放行）", typ)
		require.ErrorIs(t, validateFormatsForType(typ, nil), ErrInvalidInput, "%q + nil 也必须拒绝", typ)
	}
}

// TestValidateFormatsForTypeFollowsPolicy 站点 1/2 共用的 seam：成员判定取权威表
// 的对应行，不是「BPS 特例 + 其余 4 格式」两段硬编码。
func TestValidateFormatsForTypeFollowsPolicy(t *testing.T) {
	require.NoError(t, validateFormatsForType(credential.TypeBPS, []domain.RequestFormat{
		domain.FormatOpenAIResponses, domain.FormatOpenAIResponsesWS,
	}))
	require.ErrorIs(t, validateFormatsForType(credential.TypeBPS, []domain.RequestFormat{domain.FormatOpenAIImages}), ErrInvalidInput)
	require.ErrorIs(t, validateFormatsForType(credential.TypeBPS, []domain.RequestFormat{domain.FormatAnthropic}), ErrInvalidInput)

	require.NoError(t, validateFormatsForType(credential.TypeAPIKey, []domain.RequestFormat{
		domain.FormatOpenAIChat, domain.FormatOpenAIResponses, domain.FormatOpenAIResponsesWS,
		domain.FormatOpenAIImages, domain.FormatOpenAISearch, domain.FormatAnthropic,
	}))
	require.NoError(t, validateFormatsForType(credential.TypeCustom, []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatAnthropic}))

	require.NoError(t, validateFormatsForType(credential.TypeResponsesSpecial, []domain.RequestFormat{domain.FormatOpenAIImages}))
	require.ErrorIs(t, validateFormatsForType(credential.TypeResponsesSpecial, []domain.RequestFormat{domain.FormatOpenAIChat}), ErrInvalidInput)
	require.ErrorIs(t, validateFormatsForType(credential.TypeCodexOAuth, []domain.RequestFormat{domain.FormatAnthropic}), ErrInvalidInput)
	require.ErrorIs(t, validateFormatsForType(credential.TypeCodexPAT, []domain.RequestFormat{domain.FormatOpenAIChat}), ErrInvalidInput)
}

// TestBPSTemplateRejectsNonResponsesFormat BPS 模板只能声明 responses / responses-ws
// 格式。这条行为在收敛前完全不存在（BPS 校验只活在 repo/scheduler 的运行时门里），
// 正是运行时门可以被静默拆掉的原因。
func TestBPSTemplateRejectsNonResponsesFormat(t *testing.T) {
	tpl := &domain.Template{
		Name:             "bps",
		CredentialType:   credential.TypeBPS,
		SupportedFormats: []domain.RequestFormat{domain.FormatAnthropic},
	}
	require.ErrorIs(t, validateTemplate(tpl), ErrInvalidInput)

	tplOK := &domain.Template{
		Name:             "bps-ok",
		CredentialType:   credential.TypeBPS,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses, domain.FormatOpenAIResponsesWS},
	}
	require.NoError(t, validateTemplate(tplOK))
}
