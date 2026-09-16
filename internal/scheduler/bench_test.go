// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"runtime"
	"sort"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
)

// newTestRuleEngine 空规则引擎（bench 不依赖规则路径，满足 New 的非 nil 要求）。
func newTestRuleEngine(tb testing.TB) *rule.RuleEngine {
	tb.Helper()
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil, nil, nil)
	if err := re.Reload(context.Background()); err != nil {
		tb.Fatal(err)
	}
	return re
}

const (
	mappingRequestModel = "gpt-4o"
	mappingTargetModel  = "upstream-gpt-4o"
)

type mappingSelectionCase struct {
	name         string
	mapping      domain.ModelMapping
	wantModel    string
	wantMode     domain.ModelMappingMode
	wantUsage    string
	wantPrice    string
	wantResponse string
}

func mappingSelectionCases() []mappingSelectionCase {
	return []mappingSelectionCase{
		{name: "unmapped", wantModel: mappingRequestModel, wantPrice: mappingRequestModel},
		{
			name:      "explicit_target",
			mapping:   domain.ModelMapping{mappingRequestModel: {MappedModel: mappingTargetModel, Mode: domain.ModelMappingModeExplicit}},
			wantModel: mappingTargetModel,
			wantMode:  domain.ModelMappingModeExplicit,
			wantUsage: mappingTargetModel,
			wantPrice: mappingTargetModel,
		},
		{
			name:      "explicit_identity",
			mapping:   domain.ModelMapping{mappingRequestModel: {MappedModel: mappingRequestModel, Mode: domain.ModelMappingModeExplicit}},
			wantModel: mappingRequestModel,
			wantMode:  domain.ModelMappingModeExplicit,
			wantPrice: mappingRequestModel,
		},
		{
			name:         "implicit_target",
			mapping:      domain.ModelMapping{mappingRequestModel: {MappedModel: mappingTargetModel, Mode: domain.ModelMappingModeImplicit}},
			wantModel:    mappingTargetModel,
			wantMode:     domain.ModelMappingModeImplicit,
			wantPrice:    mappingRequestModel,
			wantResponse: mappingRequestModel,
		},
		{
			name:         "implicit_identity",
			mapping:      domain.ModelMapping{mappingRequestModel: {MappedModel: mappingRequestModel, Mode: domain.ModelMappingModeImplicit}},
			wantModel:    mappingRequestModel,
			wantMode:     domain.ModelMappingModeImplicit,
			wantPrice:    mappingRequestModel,
			wantResponse: mappingRequestModel,
		},
	}
}

func TestSelectMappingIdentities(t *testing.T) {
	for _, tc := range mappingSelectionCases() {
		t.Run(tc.name, func(t *testing.T) {
			s := schedulerWithAccounts(t, 1, tc.mapping)

			sel, err := s.Select(10, domain.FormatOpenAIChat, mappingRequestModel)

			require.NoError(t, err)
			require.Equal(t, tc.wantModel, sel.Model)
			require.Equal(t, tc.wantMode, sel.ModelMappingMode)
			require.Equal(t, tc.wantUsage, sel.LogMappedModel(mappingRequestModel))
			require.Equal(t, tc.wantPrice, sel.PriceModel(mappingRequestModel))
			require.Equal(t, tc.wantResponse, sel.ClientResponseModel(mappingRequestModel))
			s.Release(sel.AccountID)
		})
	}
}

func TestSelectMappingModeIsFreshForFallbackCandidate(t *testing.T) {
	cases := mappingSelectionCases()
	explicit := tpl(1, domain.FormatOpenAIChat, []string{mappingRequestModel})
	explicit.ModelMapping = cases[1].mapping
	implicit := tpl(2, domain.FormatOpenAIChat, []string{mappingRequestModel})
	implicit.ModelMapping = cases[3].mapping
	s := newTestScheduler(t, []*domain.Account{acc(1, explicit, 1), acc(2, implicit, 1)})
	want := map[int64]mappingSelectionCase{1: cases[1], 2: cases[3]}

	first, err := s.Select(10, domain.FormatOpenAIChat, mappingRequestModel)
	require.NoError(t, err)
	second, err := s.Select(10, domain.FormatOpenAIChat, mappingRequestModel)
	require.NoError(t, err)
	require.NotEqual(t, first.AccountID, second.AccountID)

	for _, sel := range []*Selection{first, second} {
		tc, ok := want[sel.AccountID]
		require.True(t, ok)
		require.Equal(t, tc.wantModel, sel.Model)
		require.Equal(t, tc.wantMode, sel.ModelMappingMode)
		require.Equal(t, tc.wantUsage, sel.LogMappedModel(mappingRequestModel))
		require.Equal(t, tc.wantPrice, sel.PriceModel(mappingRequestModel))
		require.Equal(t, tc.wantResponse, sel.ClientResponseModel(mappingRequestModel))
		s.Release(sel.AccountID)
	}
}

func TestGroupModelsKeepsMappingAlias(t *testing.T) {
	tplMap := tpl(1, domain.FormatOpenAIChat, []string{"listed-model"})
	tplMap.ModelMapping = domain.ModelMapping{
		mappingRequestModel: {MappedModel: mappingTargetModel, Mode: domain.ModelMappingModeImplicit},
	}
	s := newTestScheduler(t, []*domain.Account{acc(1, tplMap, 1)})

	models, ok := s.GroupModels(10)

	require.True(t, ok)
	require.Equal(t, []string{mappingRequestModel, "listed-model"}, models)
	require.NotContains(t, models, mappingTargetModel)
}

func TestSelectionSizeAMD64(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("amd64 layout gate")
	}
	require.Equal(t, uintptr(144), unsafe.Sizeof(Selection{}))
}

func TestSelectMappingIdentitiesHaveEqualAllocations(t *testing.T) {
	cases := mappingSelectionCases()
	fixtures := make([]*Scheduler, len(cases))
	for i, tc := range cases {
		fixtures[i] = schedulerWithAccounts(t, 5000, tc.mapping)
	}

	var control float64
	for i, tc := range cases {
		var selectErr error
		var usageModel, priceModel, responseModel string
		allocs := testing.AllocsPerRun(1000, func() {
			sel, err := fixtures[i].Select(10, domain.FormatOpenAIChat, mappingRequestModel)
			if err != nil {
				selectErr = err
				return
			}
			usageModel = sel.LogMappedModel(mappingRequestModel)
			priceModel = sel.PriceModel(mappingRequestModel)
			responseModel = sel.ClientResponseModel(mappingRequestModel)
			fixtures[i].Release(sel.AccountID)
		})
		require.NoError(t, selectErr)
		require.Equal(t, tc.wantUsage, usageModel)
		require.Equal(t, tc.wantPrice, priceModel)
		require.Equal(t, tc.wantResponse, responseModel)
		if i == 0 {
			control = allocs
			continue
		}
		require.Equal(t, control, allocs, tc.name)
	}
}

var (
	benchmarkUsageModel    string
	benchmarkResponseModel string
	// benchmarkCompiledRoutes sinks the compiled route count so the
	// 5000-account compiler benchmark cannot be dead-code eliminated.
	benchmarkCompiledRoutes int
)

// 5000 账号快照（压测场景复现）：Select 单次耗时对照（O(1) 序列取用）。
func BenchmarkSelect5000Accounts(b *testing.B) {
	cases := mappingSelectionCases()
	fixtures := make([]*Scheduler, len(cases))
	for i, tc := range cases {
		fixtures[i] = schedulerWithAccounts(b, 5000, tc.mapping)
	}

	for i, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			s := fixtures[i]
			b.ReportAllocs()
			b.ResetTimer()
			for j := 0; j < b.N; j++ {
				sel, err := s.Select(10, domain.FormatOpenAIChat, mappingRequestModel)
				if err != nil {
					b.Fatal(err)
				}
				benchmarkUsageModel = sel.LogMappedModel(mappingRequestModel)
				benchmarkResponseModel = sel.ClientResponseModel(mappingRequestModel)
				s.Release(sel.AccountID)
			}
		})
	}
}

func schedulerWithAccounts(tb testing.TB, n int, mapping domain.ModelMapping) *Scheduler {
	tb.Helper()
	tpl := &domain.Template{
		ID:               1,
		BaseURL:          "https://u/v1",
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		Models:           []string{mappingRequestModel},
		ModelMapping:     mapping,
	}
	accs := make(map[int64][]*domain.Account)
	for i := int64(1); i <= int64(n); i++ {
		accs[10] = append(accs[10], &domain.Account{
			ID: i, TemplateID: 1, Template: tpl, UpstreamKey: "k",
			Enabled: true, MaxConcurrency: 100000,
		})
	}
	s := New(Config{DefaultMaxConcurrency: 100000, SyncInterval: time.Hour}, newMemLoader(accs), newTestRuleEngine(tb), nil, nil, nil, nil)
	if err := s.InvalidateAllSync(); err != nil {
		tb.Fatal(err)
	}
	wireSources(s, nil, nil)
	s.compileOnce()
	return s
}

// BenchmarkCompile5000Accounts compiles the fixed 5000-account fixture
// (one group, one template, fixed model, nil quality/health/latch/prices)
// against the same immutable static root every iteration. Setup stays
// outside the timer; only RoutingCompiler.Compile is measured.
func BenchmarkCompile5000Accounts(b *testing.B) {
	s := schedulerWithAccounts(b, 5000, domain.ModelMapping{})
	sv := s.View().StaticView()
	if sv == nil {
		b.Fatal("missing static view")
	}
	c := NewRoutingCompiler()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		dv, err := c.Compile(CompilerInputs{Static: sv})
		if err != nil {
			b.Fatal(err)
		}
		benchmarkCompiledRoutes = len(dv.routes)
	}
}

// hashCompilerFixture hashes the fixed compiler fixture: sorted account ID,
// template ID, template base URL, credential type, lifecycle revision,
// enabled byte 0|1, model count plus each model length+bytes, format count
// plus each format length+bytes. Integers use binary.AppendUvarint; strings
// use uvarint byte length; nil strings use zero length.
func hashCompilerFixture(s *Scheduler) (string, int) {
	sv := s.View().StaticView()
	if sv == nil {
		return "", 0
	}
	ids := make([]int64, 0, len(sv.byID))
	for id := range sv.byID {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var buf []byte
	appendHashStr := func(str string) {
		buf = binary.AppendUvarint(buf, uint64(len(str)))
		buf = append(buf, str...)
	}
	for _, id := range ids {
		as := sv.byID[id]
		if as == nil {
			continue
		}
		st := as.static.Load()
		if st == nil {
			continue
		}
		acc := st.acc
		buf = binary.AppendUvarint(buf, uint64(acc.ID))
		buf = binary.AppendUvarint(buf, uint64(acc.TemplateID))
		if st.tpl != nil {
			appendHashStr(st.tpl.BaseURL)
			appendHashStr(string(st.tpl.CredentialType))
		} else {
			appendHashStr("")
			appendHashStr("")
		}
		buf = binary.AppendUvarint(buf, uint64(acc.LifecycleRevision))
		if acc.Enabled {
			buf = append(buf, 1)
		} else {
			buf = append(buf, 0)
		}
		if st.tpl != nil {
			buf = binary.AppendUvarint(buf, uint64(len(st.tpl.Models)))
			for _, m := range st.tpl.Models {
				appendHashStr(m)
			}
			buf = binary.AppendUvarint(buf, uint64(len(st.tpl.SupportedFormats)))
			for _, f := range st.tpl.SupportedFormats {
				appendHashStr(string(f))
			}
		} else {
			buf = binary.AppendUvarint(buf, 0)
			buf = binary.AppendUvarint(buf, 0)
		}
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:]), len(ids)
}

func TestCompilerFixtureHashStable(t *testing.T) {
	first := schedulerWithAccounts(t, 5000, domain.ModelMapping{})
	second := schedulerWithAccounts(t, 5000, domain.ModelMapping{})

	firstSum, firstCount := hashCompilerFixture(first)
	secondSum, secondCount := hashCompilerFixture(second)

	require.Equal(t, 5000, firstCount)
	require.Equal(t, 5000, secondCount)
	require.Equal(t, firstSum, secondSum)
	t.Logf("compiler_fixture sha256=%s accounts=%d", firstSum, firstCount)
}

func TestCompilerFixtureHashRejectsMutation(t *testing.T) {
	s := schedulerWithAccounts(t, 5000, domain.ModelMapping{})
	before, _ := hashCompilerFixture(s)

	snap, ok := s.View().Account(1)
	require.True(t, ok)
	st := snap.static.Load()
	require.NotNil(t, st)
	mutated := *st
	mutated.acc.LifecycleRevision++
	snap.static.Store(&mutated)

	after, _ := hashCompilerFixture(s)
	require.NotEqual(t, before, after)
	t.Logf("fixture_sha256_mismatch before=%s after=%s", before, after)
}

// BenchmarkDecisionViewBytes encodes the 5000-account fixture's decision view
// (the per-fire publish byte-guard payload). 5000 candidates × ~10 fields
// exercises the varint/string encoder at full width.
func BenchmarkDecisionViewBytes(b *testing.B) {
	s := schedulerWithAccounts(b, 5000, domain.ModelMapping{})
	dv := s.View().DecisionView()
	if dv == nil {
		b.Fatal("missing decision view")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkDecisionBytes = len(decisionViewBytes(dv))
	}
}

var benchmarkDecisionBytes int

// BenchmarkDecisionViewEncodeReused measures the production path: the
// lane-owned encoder reuses its output buffer and refs/ids scratch across
// fires (steady-state zero allocation).
func BenchmarkDecisionViewEncodeReused(b *testing.B) {
	s := schedulerWithAccounts(b, 5000, domain.ModelMapping{})
	dv := s.View().DecisionView()
	if dv == nil {
		b.Fatal("missing decision view")
	}
	var e decisionEncoder
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkDecisionBytes = len(e.encode(dv))
	}
}
