// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

func TestBPSNoWindowQuotaResolverDoesNotFabricateReset(t *testing.T) {
	resolver := BPSNoWindowQuotaResolver{}
	acct := &domain.Account{Template: &domain.Template{CredentialType: credential.TypeBPS}}
	until, ok, err := resolver.ResolveThrottleDeadline(context.Background(), acct)
	require.NoError(t, err)
	require.False(t, ok)
	require.True(t, until.IsZero())
}
