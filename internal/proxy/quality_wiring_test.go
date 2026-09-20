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
	// proxy ownership via composition.
	p := New(Config{}, nil, nil, nil, nil, nil, nil, nil, nil, Deps{Recorder: r})
	require.Same(t, r, p.QualityRecorder())
	// lifecycle Close wired
	require.NoError(t, p.QualityRecorder().Close())
}

func dispatchBase(id string) AttemptOutcome {
	return AttemptOutcome{
		ID: AttemptID(id), RouteClassID: "rc1", QualityClassID: "qc1", Fingerprint: "fp1", TemplateID: 1, AccountID: 1,
		RequestedModel: "gpt-4o", MappedModel: "gpt-4o", CallerCategory: CallerChat, OperationTag: "chat_completions", Ordinal: 1,
		IdentityRevision: 1, Lane: LanePrimary, Generation: 1,
	}
}

func TestAdaptOutcomeToObservation_Pure(t *testing.T) {
	tt := int64(120)
	o := dispatchBase("id1")
	o.Commit = CommitResponseStarted
	o.Result = ResultSuccess
	o.HTTPStatus = 200
	o.Timing = AttemptTiming{TTFTMS: &tt}
	o.Usage = AttemptUsage{InputTokens: 10, OutputTokens: 5, CallCount: 1}
	o.Terminal = true
	o.BusinessFrameSent = true
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
	o4 := dispatchBase("id4")
	o4.Commit = CommitUpstreamResponded
	o4.Result = ResultFailed
	o4.HTTPStatus = 500
	o4.Terminal = true
	o4.IsMalformed = true
	obs4, ok4 := AdaptOutcomeToObservation(o4)
	require.True(t, ok4)
	require.False(t, obs4.Success)
	require.Equal(t, quality.ErrClass5xx, obs4.ErrClass)
	// status-0 network
	o5 := dispatchBase("id5")
	o5.Commit = CommitNotSent
	o5.Result = ResultFailed
	o5.HTTPStatus = 0
	o5.Terminal = false
	o5.BusinessFrameSent = false
	obs5, ok5 := AdaptOutcomeToObservation(o5)
	require.True(t, ok5)
	require.Equal(t, quality.ErrClassNetwork, obs5.ErrClass)
	// 429 maps to 429
	o6 := dispatchBase("id6")
	o6.Commit = CommitUpstreamResponded
	o6.Result = ResultFailed
	o6.HTTPStatus = 429
	o6.Terminal = false
	o6.BusinessFrameSent = false
	obs6, ok6 := AdaptOutcomeToObservation(o6)
	require.True(t, ok6)
	require.Equal(t, quality.ErrClass429, obs6.ErrClass)
}
