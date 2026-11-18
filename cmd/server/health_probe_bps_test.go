// SPDX-License-Identifier: AGPL-3.0-or-later
package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

func TestHealthProbeBPSFailsClosedWithoutDedicatedAdapter(t *testing.T) {
	tpl := probeTpl(9, credential.TypeBPS, domain.FormatOpenAIResponses)
	fn := probeFn(t, probeLookup(map[int64]*domain.Account{
		7: probeAcc(7, tpl, 3, ""),
	}), nil)
	err := fn(context.Background(), scheduler.HealthKey{AccountID: 7, Quality: "*", Identity: healthTestIdentity, IdentityRevision: 3})
	require.EqualError(t, err, "health probe: BPS adapter not wired for account 7")
}
