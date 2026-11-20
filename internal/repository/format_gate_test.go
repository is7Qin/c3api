// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"testing"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/stretchr/testify/require"
)

// TestValidateBPSTemplateFormats 站点 3 的纯函数单测（无需 PG）：BPS 模板只接受
// responses / responses-ws；其余类型不在此门收紧（历史 codex 模板带 anthropic
// 仍放行——本门只做 BPS 子集视图，不改成全量白名单）。
func TestValidateBPSTemplateFormats(t *testing.T) {
	cases := []struct {
		name    string
		typ     credential.Type
		formats []domain.RequestFormat
		wantErr bool
	}{
		{"bps+anthropic", credential.TypeBPS, []domain.RequestFormat{domain.FormatAnthropic}, true},
		{"bps+chat", credential.TypeBPS, []domain.RequestFormat{domain.FormatOpenAIChat}, true},
		{"bps+responses", credential.TypeBPS, []domain.RequestFormat{domain.FormatOpenAIResponses}, false},
		{"bps+responses-ws", credential.TypeBPS, []domain.RequestFormat{domain.FormatOpenAIResponsesWS}, false},
		{"bps+both", credential.TypeBPS, []domain.RequestFormat{domain.FormatOpenAIResponses, domain.FormatOpenAIResponsesWS}, false},
		{"api_key+anthropic", credential.TypeAPIKey, []domain.RequestFormat{domain.FormatAnthropic}, false},
		{"codex-oauth+anthropic", credential.TypeCodexOAuth, []domain.RequestFormat{domain.FormatAnthropic}, false},
	}
	for _, tc := range cases {
		tpl := &domain.Template{CredentialType: tc.typ, SupportedFormats: tc.formats}
		err := validateBPSTemplateFormats(tpl)
		if tc.wantErr {
			require.ErrorIs(t, err, ErrInvalidInput, tc.name)
			continue
		}
		require.NoError(t, err, tc.name)
	}
}
