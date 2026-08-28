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
		Commit:            CommitResponseStarted,
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
	require.Equal(t, quality.ErrClassNone, obs.ErrClass)
	// cancel exclude
	o2 := AttemptOutcome{ID: "id2", Result: ResultClientCancel, Commit: CommitNotSent, Terminal: true}
	_, ok2 := AdaptOutcomeToObservation(o2)
	require.False(t, ok2)
	// local exclude
	o3 := AttemptOutcome{ID: "id3", Result: ResultLocalReject, Commit: CommitNotSent}
	_, ok3 := AdaptOutcomeToObservation(o3)
	require.False(t, ok3)
	// malformed post-commit failure counts by status
	o4 := AttemptOutcome{
		ID: "id4", RouteClassID: "rc1", Fingerprint: "fp1", LifecycleRevision: 1, Lane: LanePrimary, Generation: 1,
		Commit: CommitUpstreamResponded, Result: ResultFailed, HTTPStatus: 500, Terminal: true, IsMalformed: true,
	}
	obs4, ok4 := AdaptOutcomeToObservation(o4)
	require.True(t, ok4)
	require.False(t, obs4.Success)
	require.Equal(t, quality.ErrClass5xx, obs4.ErrClass)
	// status-0 network
	o5 := AttemptOutcome{
		ID: "id5", RouteClassID: "rc1", Fingerprint: "fp1", LifecycleRevision: 1, Lane: LanePrimary, Generation: 1,
		Commit: CommitNotSent, Result: ResultFailed, HTTPStatus: 0, Terminal: false,
	}
	obs5, ok5 := AdaptOutcomeToObservation(o5)
	require.True(t, ok5)
	require.Equal(t, quality.ErrClassNetwork, obs5.ErrClass)
	// 429 maps to 429
	o6 := AttemptOutcome{
		ID: "id6", RouteClassID: "rc1", Fingerprint: "fp1", LifecycleRevision: 1, Lane: LanePrimary, Generation: 1,
		Commit: CommitUpstreamResponded, Result: ResultFailed, HTTPStatus: 429, Terminal: false,
	}
	obs6, ok6 := AdaptOutcomeToObservation(o6)
	require.True(t, ok6)
	require.Equal(t, quality.ErrClass429, obs6.ErrClass)
}
