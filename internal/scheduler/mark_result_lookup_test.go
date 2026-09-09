// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
)

func TestMarkResult_LookupAllocsIndependentOfFleetSize(t *testing.T) {
	small := schedulerWithAccounts(t, 50, domain.ModelMapping{})
	large := schedulerWithAccounts(t, 5000, domain.ModelMapping{})

	smallAllocs := testing.AllocsPerRun(200, func() {
		small.MarkResult(1, rule.KindOK, nil, 200, "", mappingRequestModel)
	})
	largeAllocs := testing.AllocsPerRun(200, func() {
		large.MarkResult(1, rule.KindOK, nil, 200, "", mappingRequestModel)
	})

	require.Zero(t, testing.AllocsPerRun(200, func() {
		_, _ = large.View().Account(1)
	}), "Account must be a zero-alloc published-map lookup")
	require.Less(t, largeAllocs, float64(8),
		"single-account MarkResult must not clone the published byID map")
	require.InDelta(t, smallAllocs, largeAllocs, 2,
		"lookup cost must stay flat as the account fleet grows")
}
