// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestNewAttemptPlanDefersCandidateResolution(t *testing.T) {
	s := schedulerWithAccounts(t, 5000, domain.ModelMapping{})

	plan, err := s.NewAttemptPlan(
		AttemptPlanIdentity{RequestID: "lazy-plan"},
		RouteRefFor(10, string(domain.FormatOpenAIChat), mappingRequestModel),
	)

	require.NoError(t, err)
	require.NotNil(t, plan.route)
	require.NotZero(t, plan.total)
	require.Zero(t, plan.attemptedCnt, "plan construction must not dispatch")
	require.Zero(t, plan.ordinal)
}
