// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package credential

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBPSProviderCapabilities(t *testing.T) {
	require.True(t, TypeBPS.Valid())
	require.True(t, TypeBPS.ValidTemplateExt())
	require.True(t, TypeBPS.ValidAccountExt())
	require.True(t, TypeBPS.IsBPS())
	require.True(t, TypeBPS.IsResponsesProvider())
	require.False(t, TypeBPS.UsesStaticKey())
	require.False(t, TypeBPS.IsCodex())
}

func TestTypeValid(t *testing.T) {
	require.True(t, TypeAPIKey.Valid())
	require.False(t, Type("").Valid(), "空类型不合法")
	require.False(t, Type("codex_oauth").Valid(), "未注册生态类型不合法（占位不生效）")
	require.False(t, Type("bogus").Valid())
}
