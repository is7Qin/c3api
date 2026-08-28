// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"context"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
)

// newTestRuleEngine 空规则引擎（bench 不依赖规则路径，满足 New 的非 nil 要求）。
func newTestRuleEngine(tb testing.TB) *rule.RuleEngine {
	tb.Helper()
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
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
	wantResponse string
}

func mappingSelectionCases() []mappingSelectionCase {
	return []mappingSelectionCase{
		{name: "unmapped", wantModel: mappingRequestModel},
		{
			name:      "explicit_target",
			mapping:   domain.ModelMapping{mappingRequestModel: {MappedModel: mappingTargetModel, Mode: domain.ModelMappingModeExplicit}},
			wantModel: mappingTargetModel,
			wantMode:  domain.ModelMappingModeExplicit,
			wantUsage: mappingTargetModel,
		},
		{
			name:      "explicit_identity",
			mapping:   domain.ModelMapping{mappingRequestModel: {MappedModel: mappingRequestModel, Mode: domain.ModelMappingModeExplicit}},
			wantModel: mappingRequestModel,
			wantMode:  domain.ModelMappingModeExplicit,
		},
		{
			name:         "implicit_target",
			mapping:      domain.ModelMapping{mappingRequestModel: {MappedModel: mappingTargetModel, Mode: domain.ModelMappingModeImplicit}},
			wantModel:    mappingTargetModel,
			wantMode:     domain.ModelMappingModeImplicit,
			wantUsage:    mappingRequestModel,
			wantResponse: mappingRequestModel,
		},
		{
			name:         "implicit_identity",
			mapping:      domain.ModelMapping{mappingRequestModel: {MappedModel: mappingRequestModel, Mode: domain.ModelMappingModeImplicit}},
			wantModel:    mappingRequestModel,
			wantMode:     domain.ModelMappingModeImplicit,
			wantUsage:    mappingRequestModel,
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
	require.Equal(t, uintptr(112), unsafe.Sizeof(Selection{}))
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
		var usageModel, responseModel string
		allocs := testing.AllocsPerRun(1000, func() {
			sel, err := fixtures[i].Select(10, domain.FormatOpenAIChat, mappingRequestModel)
			if err != nil {
				selectErr = err
				return
			}
			usageModel = sel.LogMappedModel(mappingRequestModel)
			responseModel = sel.ClientResponseModel(mappingRequestModel)
			fixtures[i].Release(sel.AccountID)
		})
		require.NoError(t, selectErr)
		require.Equal(t, tc.wantUsage, usageModel)
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
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		Models:           []string{mappingRequestModel},
		ModelMapping:     mapping,
	}
	accs := make(map[int64][]*domain.Account)
	for i := int64(1); i <= int64(n); i++ {
		accs[10] = append(accs[10], &domain.Account{
			ID: i, TemplateID: 1, Template: tpl, UpstreamKey: "k",
			Status: domain.StatusActive, Weight: 100, MaxConcurrency: 100000,
		})
	}
	s := New(Config{DefaultMaxConcurrency: 100000, SyncInterval: time.Hour}, newMemLoader(accs), newTestRuleEngine(tb), nil)
	if err := s.InvalidateAllSync(); err != nil {
		tb.Fatal(err)
	}
	return s
}
