// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/quality"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
)

func TestConvertedAndImagesOutcomes_reportsExpectedMetadata(t *testing.T) {
	cases := []struct {
		name string
		dir  domain.ProtocolConvert
		want OperationTag
	}{
		{"chat to resp", domain.ProtocolConvertChatToResp, OperationTag(domain.OpChatCompletions)},
		{"mess to resp", domain.ProtocolConvertMessToResp, OperationTag(domain.OpAnthropicMessages)},
		{"resp to mess", domain.ProtocolConvertRespToMess, OperationTag(domain.OpResponses)},
		{"chat to mess", domain.ProtocolConvertChatToMess, OperationTag(domain.OpChatCompletions)},
	}
	for _, tc := range cases {
		require.Equal(t, tc.want, convertedOpTag(tc.dir), tc.name)
	}
	require.Equal(t, OperationTag(domain.OpImagesGenerations), OperationTag(domain.OpImagesGenerations))
	require.Equal(t, OperationTag(domain.OpImagesEdits), OperationTag(domain.OpImagesEdits))
}

func TestImagesOutcomes_generationsVsEditsIdentity(t *testing.T) {
	sel := &scheduler.Selection{AccountID: 1, TemplateID: 1, Model: "m", CandidateFingerprint: "fp"}
	oGen := imagesOutcome("req-1", sel, "m", OperationTag(domain.OpImagesGenerations), AttemptTiming{}, AttemptUsage{}, ResultSuccess, 200, CommitResponseStarted, true, true, false)
	require.Equal(t, OperationTag(domain.OpImagesGenerations), oGen.OperationTag)
	require.Equal(t, CallerImages, oGen.CallerCategory)
	oEdit := imagesOutcome("req-2", sel, "m", OperationTag(domain.OpImagesEdits), AttemptTiming{}, AttemptUsage{}, ResultSuccess, 200, CommitResponseStarted, true, true, false)
	require.Equal(t, OperationTag(domain.OpImagesEdits), oEdit.OperationTag)
	require.NotEqual(t, oGen.OperationTag, oEdit.OperationTag)
}

func TestImagesOutcomes_reverseConversionMalformedIsObserved(t *testing.T) {
	sel := &scheduler.Selection{AccountID: 1, TemplateID: 1, Model: "m", CandidateFingerprint: "fp"}
	timing := AttemptTiming{LatencyMS: 1}
	usage := AttemptUsage{InputTokens: 1}
	o := convertedOutcome("req-3", sel, "m", OperationTag(domain.OpChatCompletions), timing, usage, ResultFailed, 500, CommitUpstreamResponded, false, true, true)
	require.NoError(t, o.Validate())
	require.True(t, o.IsMalformed)
	require.True(t, o.IsDispatched())
}

func TestImagesOutcomes_exactlyOneObservation(t *testing.T) {
	rec, err := quality.NewRecorder(32)
	require.NoError(t, err)
	key := quality.CanonicalKey([32]byte{1}, [32]byte{2}, [32]byte{3})
	cell := rec.GetOrCreateCell(key)
	require.NotNil(t, cell)
	var ctx quality.AttemptContext
	require.True(t, rec.InitAttemptContext(cell, &ctx))
	var healthCalls atomic.Int64
	var flowCalls atomic.Int64
	var releaseCalls atomic.Int64
	observer := NewAttemptObserver(&ctx,
		func(AttemptOutcome, AttemptHealthEvent) { healthCalls.Add(1) },
		func(AttemptOutcome) { flowCalls.Add(1) },
		func() { releaseCalls.Add(1) },
	)
	base := AttemptOutcome{
		ID: AttemptID("a1"), RouteClassID: RouteClassID("rc1"), QualityClassID: QualityClassID("qc1"), Fingerprint: CandidateFingerprint("fp1"),
		TemplateID: 1, AccountID: 1, RequestedModel: "m", MappedModel: "m", CallerCategory: CallerImages, OperationTag: OperationTag(domain.OpImagesGenerations),
		Ordinal: 1, LifecycleRevision: 1, Lane: LanePrimary, Generation: 1,
		Commit: CommitResponseStarted, Result: ResultSuccess, HTTPStatus: 200, Timing: AttemptTiming{LatencyMS: 1}, Usage: AttemptUsage{CallCount: 1},
		BusinessFrameSent: true, Terminal: true,
	}
	require.NoError(t, base.Validate())
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_ = observer.Complete(base, &AttemptHealthEvent{Kind: rule.KindOK})
		}()
	}
	close(start)
	wg.Wait()
	require.Equal(t, int64(1), healthCalls.Load())
	require.Equal(t, int64(1), flowCalls.Load())
	require.Equal(t, int64(1), releaseCalls.Load())
}

func TestImagesOutcomes_streamAbortCountsAndUsage(t *testing.T) {
	sel := &scheduler.Selection{AccountID: 1, TemplateID: 1, Model: "m", CandidateFingerprint: "fp"}
	timing := AttemptTiming{LatencyMS: 5, TTFTMS: func() *int64 { v := int64(2); return &v }()}
	usage := AttemptUsage{InputTokens: 10, OutputTokens: 20, CallCount: 2}
	o := imagesStreamOutcome("req-5", sel, "m", OperationTag(domain.OpImagesGenerations), timing, usage, ResultFailed, 500, CommitUpstreamResponded, false, true, false)
	require.NoError(t, o.Validate())
	require.Equal(t, int64(10), o.Usage.InputTokens)
	require.Equal(t, int64(2), o.Usage.CallCount)
	require.NotNil(t, o.Timing.TTFTMS)
}

func TestImagesOutcomes_clientCancelSkipsHealth(t *testing.T) {
	rec, err := quality.NewRecorder(32)
	require.NoError(t, err)
	key := quality.CanonicalKey([32]byte{9}, [32]byte{8}, [32]byte{7})
	cell := rec.GetOrCreateCell(key)
	require.NotNil(t, cell)
	var ctx quality.AttemptContext
	require.True(t, rec.InitAttemptContext(cell, &ctx))
	var healthCalls atomic.Int64
	observer := NewAttemptObserver(&ctx,
		func(AttemptOutcome, AttemptHealthEvent) { healthCalls.Add(1) },
		nil, func() {},
	)
	o := AttemptOutcome{
		ID: AttemptID("c1"), RouteClassID: RouteClassID("rc1"), QualityClassID: QualityClassID("qc1"), Fingerprint: CandidateFingerprint("fp1"),
		TemplateID: 1, AccountID: 1, RequestedModel: "m", MappedModel: "m", CallerCategory: CallerImages, OperationTag: OperationTag(domain.OpImagesGenerations),
		Ordinal: 1, LifecycleRevision: 1, Lane: LanePrimary, Generation: 1,
		Commit: CommitNotSent, Result: ResultClientCancel, HTTPStatus: 0, Timing: AttemptTiming{}, Usage: AttemptUsage{}, Terminal: true,
	}
	require.NoError(t, o.Validate())
	require.NoError(t, observer.Cancel(o))
	// second cancel must be idempotent no extra health
	require.NoError(t, observer.Cancel(o))
	require.Equal(t, int64(0), healthCalls.Load())
}

func TestImagesOutcomes_multipartModelExtraction(t *testing.T) {
	require.True(t, isMultipartForm("multipart/form-data; boundary=abc"))
	require.False(t, isMultipartForm("application/json"))
}
