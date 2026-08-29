// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDispatchObservation_proxyRequiresCompleteMetadata(t *testing.T) {
	base := validBase()
	// validBase must already be dispatch-complete under new contract
	require.NoError(t, base.Validate())
	require.NotEmpty(t, base.QualityClassID)
	require.NotZero(t, base.TemplateID)
	require.NotZero(t, base.AccountID)
	require.NotEmpty(t, base.RequestedModel)
	require.NotEmpty(t, base.MappedModel)
	require.True(t, base.CallerCategory.Valid())
	require.NotEmpty(t, base.OperationTag)
	require.NotZero(t, base.Ordinal)

	// missing required dispatch fields must be rejected where contract applies
	cases := []AttemptOutcome{
		func() AttemptOutcome { o := base; o.QualityClassID = ""; return o }(),
		func() AttemptOutcome { o := base; o.TemplateID = 0; return o }(),
		func() AttemptOutcome { o := base; o.AccountID = 0; return o }(),
		func() AttemptOutcome { o := base; o.RequestedModel = ""; return o }(),
		func() AttemptOutcome { o := base; o.MappedModel = ""; return o }(),
		func() AttemptOutcome { o := base; o.CallerCategory = ""; return o }(),
		func() AttemptOutcome { o := base; o.OperationTag = ""; return o }(),
		func() AttemptOutcome { o := base; o.Ordinal = 0; return o }(),
	}
	for i, c := range cases {
		require.Error(t, c.Validate(), "case %d should reject missing dispatch metadata", i)
		require.False(t, CanRetry(c.CallerCategory, c), "invalid dispatch must not be retryable")
	}

	// ordinal 1 must have nil previous, ordinal >1 must have previous
	o1 := base
	o1.Ordinal = 1
	o1.PreviousAttemptID = nil
	require.NoError(t, o1.Validate())
	o1Bad := base
	o1Bad.Ordinal = 1
	prev := AttemptID("prev")
	o1Bad.PreviousAttemptID = &prev
	require.Error(t, o1Bad.Validate())

	o2 := base
	o2.Ordinal = 2
	prev2 := AttemptID("a1")
	o2.PreviousAttemptID = &prev2
	require.NoError(t, o2.Validate())
	o2Bad := base
	o2Bad.Ordinal = 2
	o2Bad.PreviousAttemptID = nil
	require.Error(t, o2Bad.Validate())
}

func TestDispatchObservation_proxyDispatchImmutability(t *testing.T) {
	o := validBase()
	require.NoError(t, o.Validate())
	// caller category and operation tag must be valid domain identities
	require.True(t, o.CallerCategory.Valid())
	require.NotEmpty(t, string(o.OperationTag))
	// lane/generation/revision must still be required
	bad := o
	bad.Lane = ""
	require.Error(t, bad.Validate())
	bad = o
	bad.Generation = 0
	require.Error(t, bad.Validate())
	bad = o
	bad.LifecycleRevision = 0
	require.Error(t, bad.Validate())
}
