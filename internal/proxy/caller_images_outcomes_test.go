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
	// off / 未知方向回退 chat_completions（convertedOpTag 的默认分支）。
	require.Equal(t, OperationTag(domain.OpChatCompletions), convertedOpTag(domain.ProtocolConvertOff))
	require.Equal(t, OperationTag(domain.OpChatCompletions), convertedOpTag(domain.ProtocolConvert("bogus")))
}

// TestBuildOutcome_CallerVariantsShareCanonicalFields 钉住 B2 收敛：四类「合成
// id」构造器（images/codex-images/images-stream/converted）现经单一 buildOutcome
// 生产，除 CallerCategory 外逐字段一致，且派发元数据取自 canonical 常量。
func TestBuildOutcome_CallerVariantsShareCanonicalFields(t *testing.T) {
	sel := &scheduler.Selection{AccountID: 7, TemplateID: 8, Model: "upstream", CandidateFingerprint: "fp"}
	op := OperationTag(domain.OpImagesGenerations)
	usage := AttemptUsage{InputTokens: 1}
	cases := []struct {
		name   string
		caller CallerCategory
		got    AttemptOutcome
	}{
		{"images", CallerImages, imagesOutcome("r", sel, "req", op, AttemptTiming{}, usage, ResultSuccess, 200, CommitResponseStarted, true, true, false)},
		{"codex_images", CallerImagesCodex, codexImagesOutcome("r", sel, "req", op, AttemptTiming{}, usage, ResultSuccess, 200, CommitResponseStarted, true, true, false)},
		{"images_stream", CallerImagesCodex, imagesStreamOutcome("r", sel, "req", op, AttemptTiming{}, usage, ResultSuccess, 200, CommitResponseStarted, true, true, false)},
		{"converted", CallerConverted, convertedOutcome("r", sel, "req", op, AttemptTiming{}, usage, ResultSuccess, 200, CommitResponseStarted, true, true, false)},
	}
	for _, tc := range cases {
		o := tc.got
		require.Equal(t, tc.caller, o.CallerCategory, tc.name)
		require.Equal(t, op, o.OperationTag, tc.name)
		require.Equal(t, AttemptID("r"), o.ID, tc.name)
		require.Equal(t, RouteClassID("rc-r"), o.RouteClassID, tc.name)
		require.Equal(t, QualityClassID("qc-r"), o.QualityClassID, tc.name)
		require.Equal(t, CandidateFingerprint("fp"), o.Fingerprint, tc.name)
		require.Equal(t, int64(8), o.TemplateID, tc.name)
		require.Equal(t, int64(7), o.AccountID, tc.name)
		require.Equal(t, "req", o.RequestedModel, tc.name)
		require.Equal(t, "upstream", o.MappedModel, tc.name)
		require.EqualValues(t, 1, o.Ordinal, tc.name)
		require.EqualValues(t, 1, o.IdentityRevision, tc.name)
		require.Equal(t, LanePrimary, o.Lane, tc.name)
		require.EqualValues(t, 1, o.Generation, tc.name)
	}

	// 候选指纹缺失 → 四类合成 id 构造器统一回退「fp-<reqID>」（syntheticFingerprint
	// 的回退分支）；另建独立 sel（CandidateFingerprint == ""），不复用上方共享候选。
	t.Run("empty_candidate_fingerprint_falls_back", func(t *testing.T) {
		emptyFP := &scheduler.Selection{AccountID: 7, TemplateID: 8, Model: "upstream"}
		fallbacks := []struct {
			name string
			got  AttemptOutcome
		}{
			{"images", imagesOutcome("req", emptyFP, "upstream", op, AttemptTiming{}, usage, ResultSuccess, 200, CommitResponseStarted, true, true, false)},
			{"codex_images", codexImagesOutcome("req", emptyFP, "upstream", op, AttemptTiming{}, usage, ResultSuccess, 200, CommitResponseStarted, true, true, false)},
			{"images_stream", imagesStreamOutcome("req", emptyFP, "upstream", op, AttemptTiming{}, usage, ResultSuccess, 200, CommitResponseStarted, true, true, false)},
			{"converted", convertedOutcome("req", emptyFP, "upstream", op, AttemptTiming{}, usage, ResultSuccess, 200, CommitResponseStarted, true, true, false)},
		}
		for _, tc := range fallbacks {
			require.Equal(t, CandidateFingerprint("fp-req"), tc.got.Fingerprint, tc.name)
		}
	})
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
		Ordinal: 1, IdentityRevision: 1, Lane: LanePrimary, Generation: 1,
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
		Ordinal: 1, IdentityRevision: 1, Lane: LanePrimary, Generation: 1,
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

// TestProtocolConvertDirections_ConsistentAcrossHelpers 钉住 B4：方向方法
// ProtocolConvert.Client()/Target() 与 convertedOpTag/clientAndTargetOf/
// convertedRoute 三个消费端口径逐方向一致（X2 等价性证据）。
func TestProtocolConvertDirections_ConsistentAcrossHelpers(t *testing.T) {
	cases := []struct {
		dir    domain.ProtocolConvert
		client domain.RequestFormat
		target domain.RequestFormat
		op     OperationTag
	}{
		{domain.ProtocolConvertChatToResp, domain.FormatOpenAIChat, domain.FormatOpenAIResponses, OperationTag(domain.OpChatCompletions)},
		{domain.ProtocolConvertMessToResp, domain.FormatAnthropic, domain.FormatOpenAIResponses, OperationTag(domain.OpAnthropicMessages)},
		{domain.ProtocolConvertRespToMess, domain.FormatOpenAIResponses, domain.FormatAnthropic, OperationTag(domain.OpResponses)},
		{domain.ProtocolConvertChatToMess, domain.FormatOpenAIChat, domain.FormatAnthropic, OperationTag(domain.OpChatCompletions)},
	}
	for _, tc := range cases {
		require.Equal(t, tc.client, tc.dir.Client(), tc.dir)
		require.Equal(t, tc.target, tc.dir.Target(), tc.dir)
		require.Equal(t, tc.op, convertedOpTag(tc.dir), tc.dir)
		gotClient, gotTarget := clientAndTargetOf(tc.dir)
		require.Equal(t, tc.client, gotClient, tc.dir)
		require.Equal(t, tc.target, gotTarget, tc.dir)
		gotTarget2, gotDir, ok := convertedRoute([]domain.ProtocolConvert{tc.dir}, tc.client)
		require.True(t, ok, tc.dir)
		require.Equal(t, tc.target, gotTarget2, tc.dir)
		require.Equal(t, tc.dir, gotDir, tc.dir)
		for _, other := range []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatAnthropic, domain.FormatOpenAIResponses} {
			if other == tc.client {
				continue
			}
			_, _, miss := convertedRoute([]domain.ProtocolConvert{tc.dir}, other)
			require.False(t, miss, "%s must not match client %s", tc.dir, other)
		}
	}
	_, _, ok := convertedRoute(nil, domain.FormatOpenAIChat)
	require.False(t, ok, "empty converts must not match")
	_, _, ok = convertedRoute([]domain.ProtocolConvert{"bogus"}, domain.FormatOpenAIChat)
	require.False(t, ok, "unknown direction must not match")
}
