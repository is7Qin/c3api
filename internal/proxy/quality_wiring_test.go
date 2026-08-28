// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/config"
	"github.com/is7qin/c3api/internal/quality"
)

func TestQualityRecorder_ProductionOwnership_Wiring(t *testing.T) {
	eff, err := config.EffectiveMaxInflight(0)
	require.NoError(t, err)
	r, err := quality.NewRecorder(eff)
	require.NoError(t, err)
	// proxy ownership via composition (dormant for Task13)
	p := &Proxy{}
	p.SetQualityRecorder(r)
	require.Same(t, r, p.QualityRecorder())
	// lifecycle Close wired
	require.NoError(t, p.QualityRecorder().Close())
}

func TestAdaptOutcomeToObservation_Pure(t *testing.T) {
	tt := int64(120)
	o := AttemptOutcome{
		ID:                "id1",
		RouteClassID:      "rc1",
		Fingerprint:       "fp1",
		LifecycleRevision: 1,
		Lane:              LanePrimary,
		Generation:        1,
		Commit:            CommitUpstreamResponded,
		Result:            ResultSuccess,
		HTTPStatus:        200,
		Timing:            AttemptTiming{TTFTMS: &tt},
		Usage:             AttemptUsage{InputTokens: 10, OutputTokens: 5, CallCount: 1},
		Terminal:          true,
		BusinessFrameSent: true,
	}
	obs, ok := AdaptOutcomeToObservation(o)
	require.True(t, ok)
	require.True(t, obs.Success)
	require.Equal(t, int64(15), obs.Tokens)
	require.Equal(t, int64(1), obs.Calls)
	// cancel exclude
	o2 := AttemptOutcome{Result: ResultClientCancel, Commit: CommitNotSent}
	_, ok2 := AdaptOutcomeToObservation(o2)
	require.False(t, ok2)
	// local exclude
	o3 := AttemptOutcome{Result: ResultLocalReject, Commit: CommitNotSent}
	_, ok3 := AdaptOutcomeToObservation(o3)
	require.False(t, ok3)
}
