// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// --- T4: small-allocation elimination tests ---

func t4Snapshot(a *domain.Account) (*accountSnapshot, compilerAccountFacts) {
	st := newSnapshotStatic(*a, a.Template, []int64{10})
	snap := newAccountSnapshot(st, &accState{status: domain.StatusActive})
	return snap, deriveCompilerAccountFacts(snap, st)
}

// TestFilterCandidatesClearsTail pins the in-place stable compaction AND the
// tail clear: the compacted-away entries must be zeroed so they no longer hold
// static/leaf references (spec §4 T4 tail clear).
func TestFilterCandidatesClearsTail(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	disabled1 := accWithEnabled(2, tpl, false, 10000)
	enabled := accWithEnabled(1, tpl, true, 10000)
	disabled2 := accWithEnabled(3, tpl, false, 10000)

	rootFacts := map[int64]compilerAccountFacts{}
	cands := make([]*accountSnapshot, 0, 3)
	for _, a := range []*domain.Account{disabled1, enabled, disabled2} {
		snap, facts := t4Snapshot(a)
		rootFacts[a.ID] = facts
		cands = append(cands, snap)
	}
	facts := buildCandidateFacts(cands, rootFacts, routeKey{format: domain.FormatOpenAIChat, model: "m"}, domain.OpChatCompletions)
	require.Len(t, facts, 3)
	require.NotNil(t, facts[2].static, "precondition: tail entry holds a leaf reference")

	filtered := filterCandidates(facts)
	require.Len(t, filtered, 1)
	require.Equal(t, int64(1), filtered[0].accountID)
	require.True(t, &facts[0] == &filtered[0], "must compact in place")

	// The compacted-away tail is zeroed: no lingering static slice/pointer refs.
	for i := 1; i < len(facts); i++ {
		require.Equal(t, compilerCandidateFacts{}, facts[i], "tail entry %d must be cleared", i)
	}
}

// TestFilterCandidatesAllFilteredAndEmpty: an empty input and an all-filtered
// input both return empty without panicking, and an all-filtered slice is fully
// zeroed.
func TestFilterCandidatesAllFilteredAndEmpty(t *testing.T) {
	require.Empty(t, filterCandidates(nil))
	require.Empty(t, filterCandidates([]compilerCandidateFacts{}))

	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	a1 := accWithEnabled(2, tpl, false, 10000)
	a2 := accWithEnabled(3, tpl, false, 10000)
	rootFacts := map[int64]compilerAccountFacts{}
	cands := make([]*accountSnapshot, 0, 2)
	for _, a := range []*domain.Account{a1, a2} {
		snap, facts := t4Snapshot(a)
		rootFacts[a.ID] = facts
		cands = append(cands, snap)
	}
	facts := buildCandidateFacts(cands, rootFacts, routeKey{format: domain.FormatOpenAIChat, model: "m"}, domain.OpChatCompletions)
	require.Len(t, filterCandidates(facts), 0)
	for i := range facts {
		require.Equal(t, compilerCandidateFacts{}, facts[i], "all-filtered slice entry %d must be cleared", i)
	}
}

// TestBuildCandidateFactsMappingAndQuality exercises the (format,model,op)
// quality memo and identity-vs-mapped model reuse: identity mapping reuses the
// raw quality (same string), a real mapping derives the mapped-model class and
// keeps the requested-model class for QualityRaw. Rebuilding with a different
// format/op must not leak the previous memo.
func TestBuildCandidateFactsMappingAndQuality(t *testing.T) {
	req := qualityClassHexForWithOp(domain.FormatOpenAIChat, "requested", domain.OpChatCompletions)
	mapped := qualityClassHexForWithOp(domain.FormatOpenAIChat, "mapped", domain.OpChatCompletions)

	mappingTpl := &domain.Template{
		ID: 7, BaseURL: "https://up.example.com",
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		Models:           []string{"requested"},
		ModelMapping:     map[string]domain.ModelMappingEntry{"requested": {MappedModel: "mapped", Mode: domain.ModelMappingModeExplicit}},
	}
	identityTpl := tplWith(domain.FormatOpenAIChat, []string{"requested"})

	rootFacts := map[int64]compilerAccountFacts{}
	cands := make([]*accountSnapshot, 0, 2)
	for _, a := range []*domain.Account{accWithEnabled(9, mappingTpl, true, 10000), accWithEnabled(10, identityTpl, true, 10000)} {
		snap, facts := t4Snapshot(a)
		rootFacts[a.ID] = facts
		cands = append(cands, snap)
	}
	// order: [mapped, identity]
	facts := buildCandidateFacts(cands, rootFacts, routeKey{format: domain.FormatOpenAIChat, model: "requested"}, domain.OpChatCompletions)
	require.Len(t, facts, 2)
	mappedFact, identityFact := facts[0], facts[1]

	require.Equal(t, "mapped", mappedFact.mappedModel)
	require.Equal(t, domain.ModelMappingModeExplicit, mappedFact.mappingMode)
	require.Equal(t, mapped, mappedFact.quality, "mapped-model class")
	require.Equal(t, req, mappedFact.qualityRaw, "requested-model class for a real mapping")

	require.Equal(t, "requested", identityFact.mappedModel, "identity mapping keeps the requested model")
	require.Equal(t, req, identityFact.quality)
	require.Equal(t, req, identityFact.qualityRaw, "identity mapping reuses the raw quality")

	// A different op/format must be compiled with its own class (no stale memo).
	factsOther := buildCandidateFacts(cands, rootFacts, routeKey{format: domain.FormatOpenAIResponses, model: "requested"}, domain.OpChatCompletions)
	require.Equal(t, qualityClassHexForWithOp(domain.FormatOpenAIResponses, "requested", domain.OpChatCompletions), factsOther[1].quality)
	require.NotEqual(t, factsOther[1].quality, identityFact.quality, "format change must change the quality class")
}

// TestCompileCacheDomainPlanDedupOrder: duplicate account IDs are de-duplicated
// and the resulting CacheDomainAccounts are sorted by account ID regardless of
// input order.
func TestCompileCacheDomainPlanDedupOrder(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m"})
	mk := func(id int64) compilerCandidateFacts {
		snap, facts := t4Snapshot(acc(id, tpl, 4))
		_ = snap
		return compilerCandidateFacts{compilerAccountFacts: facts}
	}
	_, accounts, err := compileCacheDomainPlan([]compilerCandidateFacts{mk(3), mk(1), mk(3), mk(2), mk(1)})
	require.NoError(t, err)
	require.Len(t, accounts, 3, "duplicate account IDs must be de-duplicated")
	require.Equal(t, int64(1), accounts[0].AccountID)
	require.Equal(t, int64(2), accounts[1].AccountID)
	require.Equal(t, int64(3), accounts[2].AccountID)
}
