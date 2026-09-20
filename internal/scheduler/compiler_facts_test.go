// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"reflect"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func ownershipFixture(t *testing.T) *Scheduler {
	t.Helper()
	t1 := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o", "other-model"})
	a1 := acc(1, t1, 100000)
	a1.UpstreamCostMultiplierBp = 12000
	t2 := tpl(2, domain.FormatOpenAIChat, []string{"gpt-4o"})
	a2 := acc(2, t2, 100000)
	a2.BaseURL = strPtr("https://override/v1")
	// 候选内容代际是 K（identity_revision），不是客户端 CAS 令牌 C
	// （routing_compiler_candidates.go：f.revision = st.acc.IdentityRevision）。
	// 本行原本设 LifecycleRevision，在 P5 改名后已与 f.revision 脱钩——留着
	// 只会让「代际流动」这条断言退化为对 0 的比较。
	a2.IdentityRevision = 2
	a2.UpstreamCostMultiplierBp = 8000
	t3 := tpl(3, domain.FormatOpenAIChat, []string{"gpt-4o"})
	t3.ModelMapping = domain.ModelMapping{
		"gpt-4o": {MappedModel: "upstream-gpt-4o", Mode: domain.ModelMappingModeExplicit},
	}
	a3 := acc(3, t3, 100000)
	a3.UpstreamCostMultiplierBp = 15000
	return newTestScheduler(t, []*domain.Account{a1, a2, a3})
}

func sortedFactIDs(facts map[int64]compilerAccountFacts) []int64 {
	ids := make([]int64, 0, len(facts))
	for id := range facts {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

func TestCompiledCandidateOwnershipDoesNotCrossRoutesOrStaticRoots(t *testing.T) {
	s := ownershipFixture(t)
	sv := s.View().StaticView()
	require.NotNil(t, sv)
	facts := sv.facts
	require.NotNil(t, facts)
	require.Len(t, facts, 3)

	// The facts type carries exactly the planned immutable fields; no map or
	// slice may alias mutable account/template backing data. Only the two
	// immutable leaf/static pointers are shared, plus scalars and the
	// fixed-size identity array.
	ft := reflect.TypeOf(compilerAccountFacts{})
	var names []string
	for i := 0; i < ft.NumField(); i++ {
		fl := ft.Field(i)
		names = append(names, fl.Name)
		switch fl.Type.Kind() {
		case reflect.Int, reflect.Int64, reflect.String, reflect.Array:
		case reflect.Ptr:
			require.Contains(t, []string{"*scheduler.accountSnapshot", "*scheduler.snapshotStatic"}, fl.Type.String(), fl.Name)
		default:
			t.Fatalf("mutable aliasing risk: field %s kind %s", fl.Name, fl.Type.Kind())
		}
	}
	require.Equal(t, []string{"accountID", "templateID", "baseURL", "fingerprint", "identityFingerprint", "revision", "account", "static", "upstreamCostMultiplierBp"}, names)

	byID := sv.ByID()
	for _, id := range sortedFactIDs(facts) {
		f := facts[id]
		leaf, ok := byID[id]
		require.True(t, ok)
		require.Same(t, leaf, f.account)
		require.Same(t, leaf.static.Load(), f.static)
		st := leaf.static.Load()
		require.Equal(t, id, f.accountID)
		require.Equal(t, st.acc.TemplateID, f.templateID)
		require.Equal(t, st.acc.IdentityRevision, f.revision)
		require.Equal(t, st.acc.UpstreamCostMultiplierBp, f.upstreamCostMultiplierBp)
		wantBase := st.tpl.BaseURL
		if st.acc.BaseURL != nil && *st.acc.BaseURL != "" {
			wantBase = *st.acc.BaseURL
		}
		require.Equal(t, wantBase, f.baseURL)
		wantFP, err := candidateFingerprint(&st.acc)
		require.NoError(t, err)
		require.Equal(t, wantFP, f.fingerprint)
		require.Equal(t, candidateIdentityFingerprint(wantFP, id), f.identityFingerprint)
	}
	// Base URL precedence: account override wins, otherwise template URL.
	require.Equal(t, "https://override/v1", facts[2].baseURL)
	require.Equal(t, "https://u/v1", facts[1].baseURL)

	// Identical input derives identical facts: same scalars, same identity,
	// same immutable leaf/static pointers across routes.
	leaves := []*accountSnapshot{byID[1], byID[2], byID[3]}
	rk := routeKey{format: domain.FormatOpenAIChat, model: "gpt-4o"}
	op := domain.OpChatCompletions
	run1 := buildCandidateFacts(leaves, sv.facts, rk, op)
	run2 := buildCandidateFacts(leaves, sv.facts, rk, op)
	require.Len(t, run1, 3)
	for i := range run1 {
		require.Equal(t, run1[i].fingerprint, run2[i].fingerprint)
		require.Equal(t, run1[i].baseURL, run2[i].baseURL)
		require.Equal(t, run1[i].revision, run2[i].revision)
		require.Equal(t, run1[i].identityFingerprint, run2[i].identityFingerprint)
		require.Same(t, run1[i].account, run2[i].account)
		require.Same(t, run1[i].static, run2[i].static)
		require.Same(t, leaves[i].static.Load(), run1[i].static)
	}
	// Full compiles of the same root are byte-identical.
	c := NewRoutingCompiler()
	dv1, err := c.Compile(CompilerInputs{Static: sv})
	require.NoError(t, err)
	dv2, err := c.Compile(CompilerInputs{Static: sv})
	require.NoError(t, err)
	require.Equal(t, decisionViewBytes(dv1), decisionViewBytes(dv2))

	// Route-specific values stay distinct: alternate model, alternate
	// operation, and the explicit mapping only affect their own route.
	otherModel := buildCandidateFacts(leaves, sv.facts, routeKey{format: domain.FormatOpenAIChat, model: "other-model"}, op)
	require.NotEqual(t, run1[0].mappedModel, otherModel[0].mappedModel)
	require.NotEqual(t, run1[0].quality, otherModel[0].quality)
	otherOp := buildCandidateFacts(leaves, sv.facts, rk, domain.OpResponses)
	require.NotEqual(t, run1[0].quality, otherOp[0].quality)
	require.Equal(t, run1[0].identityFingerprint, otherOp[0].identityFingerprint)
	require.Equal(t, "upstream-gpt-4o", run1[2].mappedModel)
	require.Equal(t, domain.ModelMappingModeExplicit, run1[2].mappingMode)

	// A new static root owns a fresh facts map; the old root is untouched.
	oldPtr := reflect.ValueOf(sv.facts).Pointer()
	require.NoError(t, s.InvalidateAllSync())
	s.compileOnce()
	sv2 := s.View().StaticView()
	require.NotNil(t, sv2.facts)
	require.Len(t, sv2.facts, 3)
	require.NotEqual(t, oldPtr, reflect.ValueOf(sv2.facts).Pointer())
	require.Len(t, facts, 3)

	// The same-root wrapper preserves the identical facts map and leaves.
	s2 := newTestSchedulerStatic(t, []*domain.Account{acc(9, tpl(9, domain.FormatOpenAIChat, []string{"gpt-4o"}), 4)})
	staged := s2.View().StaticView()
	require.NotNil(t, staged.facts)
	s2.publisher.mu.Lock()
	stagedDV, err := NewRoutingCompiler().Compile(CompilerInputs{Static: staged})
	require.NoError(t, err)
	wrapped := s2.publisher.publishPairLocked(staged, stagedDV)
	s2.publisher.mu.Unlock()
	require.NotSame(t, staged, wrapped)
	require.Equal(t, reflect.ValueOf(staged.facts).Pointer(), reflect.ValueOf(wrapped.facts).Pointer())
	for id, leaf := range staged.byID {
		require.Same(t, leaf, wrapped.byID[id])
	}
}
