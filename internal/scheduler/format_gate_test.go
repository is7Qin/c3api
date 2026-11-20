// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"testing"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/stretchr/testify/require"
)

// TestTplSupportsFormatBPSGate 站点 5 的纯函数单测（无需 PG）：BPS 模板对非
// responses/ws 格式恒 false（即便 supported_formats 里写了它）；其余类型不受此门
// 收紧，只看 supported_formats 是否包含。
func TestTplSupportsFormatBPSGate(t *testing.T) {
	bps := &domain.Template{
		CredentialType:   credential.TypeBPS,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses, domain.FormatAnthropic},
	}
	require.False(t, tplSupportsFormat(bps, domain.FormatAnthropic), "BPS 模板 + anthropic：即便在 supported_formats 里也必须 false")
	require.True(t, tplSupportsFormat(bps, domain.FormatOpenAIResponses))
	require.False(t, tplSupportsFormat(bps, domain.FormatOpenAIChat), "不在 supported_formats → false")

	apiKey := &domain.Template{
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatAnthropic},
	}
	require.True(t, tplSupportsFormat(apiKey, domain.FormatAnthropic), "非 BPS 类型不受 BPS 门收紧")
}
