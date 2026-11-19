// SPDX-License-Identifier: AGPL-3.0-or-later
package probe

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestFinalizePreview(t *testing.T) {
	require.Empty(t, FinalizePreview(""))
	require.Equal(t, "see [url]", FinalizePreview("see https://example.com/x"))
	out := FinalizePreview(strings.Repeat("y", domain.ProbePreviewMaxLen+10))
	require.LessOrEqual(t, len(out), domain.ProbePreviewMaxLen)
	require.Equal(t, domain.ProbePreviewMaxLen, len(out))
}

func TestErrorCodeDetail(t *testing.T) {
	require.Equal(t, "code=invalid_api_key", ErrorCodeDetail("invalid_api_key"))
	require.Empty(t, ErrorCodeDetail("evil"))
	require.Empty(t, ErrorCodeDetail(""))
	require.Empty(t, ErrorCodeDetail("invalid_api_key\n"))
}
