// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"encoding/binary"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

func TestRoutingPlan_EmptyViewIsNonNilEmptyPlan(t *testing.T) {
	s := &Scheduler{}
	plan := s.CurrentRoutingPlan()
	require.NotNil(t, plan)
	require.Zero(t, plan.Generation)
	require.NotNil(t, plan.Routes)
	require.Empty(t, plan.Routes)
}

func TestRoutingPlan_GenerationMatchesPublishedRoot(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{accWithEnabled(1, tplx, true, 10000)})
	plan := s.CurrentRoutingPlan()
	require.Equal(t, s.View().Generation(), plan.Generation)
	require.Empty(t, plan.Routes, "static-only view has no compiled routes")

	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	s.PublishDecisionForTest(route, &RouteDecision{Primary: []int64{1}})
	plan = s.CurrentRoutingPlan()
	require.Equal(t, s.View().Generation(), plan.Generation)
	require.Len(t, plan.Routes, 1)
	require.Equal(t, route, plan.Routes[0].Ref)
	require.NotEmpty(t, route.RouteClassID, "RouteRefFor must carry canonical route class hex")
}

func TestRoutingPlan_RouteOrderDeterministicAndLaneOrderPreserved(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{
		accWithEnabled(1, tplx, true, 10000),
		accWithEnabled(2, tplx, true, 10000),
		accWithEnabled(3, tplx, true, 10000),
	})
	chatRoute := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	antRoute := RouteRefFor(10, string(domain.FormatAnthropic), "m")
	grpRoute := RouteRefFor(20, string(domain.FormatOpenAIChat), "m")
	// Publish in scrambled order; projection must sort by full RouteRef identity.
	s.PublishDecisionForTest(grpRoute, &RouteDecision{Primary: []int64{1}})
	s.PublishDecisionForTest(antRoute, &RouteDecision{Primary: []int64{2}})
	s.PublishDecisionForTest(chatRoute, &RouteDecision{
		Primary:  []int64{3, 1}, // lane order is semantic — never re-sorted
		Degraded: []int64{2},
		Explore: ExploreDecision{
			IDs:        []int64{2, 3},
			Weights:    map[int64]int{2: 40, 3: 60},
			Cumulative: []uint64{40, 100},
			Total:      100,
			Fallback:   []int64{3, 2},
		},
	})

	plan := s.CurrentRoutingPlan()
	require.Len(t, plan.Routes, 3)
	require.Equal(t, []RouteRef{antRoute, chatRoute, grpRoute},
		[]RouteRef{plan.Routes[0].Ref, plan.Routes[1].Ref, plan.Routes[2].Ref},
		"routes sorted by (group, format, model, op, route-class)")

	chat := plan.Routes[1]
	require.Equal(t, []int64{3, 1}, chat.Primary, "published primary order preserved verbatim")
	require.Equal(t, []int64{2}, chat.Degraded)
	require.Equal(t, []int64{2, 3}, chat.Explore.IDs, "explore order preserved verbatim")
	require.Equal(t, map[int64]int{2: 40, 3: 60}, chat.Explore.Weights)
	require.Equal(t, []uint64{40, 100}, chat.Explore.Cumulative)
	require.Equal(t, uint64(100), chat.Explore.Total)
	require.Equal(t, []int64{3, 2}, chat.Explore.Fallback)
	// Candidate union = lanes deduped, ascending account ID.
	require.Equal(t, []int64{1, 2, 3}, []int64{chat.Candidates[0].AccountID, chat.Candidates[1].AccountID, chat.Candidates[2].AccountID})
}

func TestRoutingPlan_DefensiveCopyNoAliasIntoPublishedView(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{accWithEnabled(1, tplx, true, 10000), accWithEnabled(2, tplx, true, 10000)})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	s.PublishDecisionForTest(route, &RouteDecision{
		Primary: []int64{1, 2},
		Explore: ExploreDecision{IDs: []int64{2}, Weights: map[int64]int{2: 7}, Cumulative: []uint64{7}, Total: 7},
	})

	first := s.CurrentRoutingPlan()
	require.Len(t, first.Routes, 1)
	// Mutate every mutable surface of the projection.
	first.Routes[0].Primary[0] = 999
	first.Routes[0].Explore.Weights[2] = 999
	first.Routes[0].Explore.IDs[0] = 999

	second := s.CurrentRoutingPlan()
	require.Equal(t, []int64{1, 2}, second.Routes[0].Primary, "published lanes never alias into the projection")
	require.Equal(t, map[int64]int{2: 7}, second.Routes[0].Explore.Weights)
	require.Equal(t, []int64{2}, second.Routes[0].Explore.IDs)

	// The published decision itself is untouched.
	rd, ok := s.View().DecisionView().Route(10, string(domain.FormatOpenAIChat), "m")
	require.True(t, ok)
	require.Equal(t, []int64{1, 2}, rd.Primary)
}

func TestRoutingPlan_CandidateMetadataMappingFingerprintQualityClass(t *testing.T) {
	tplx := &domain.Template{
		ID: 7, BaseURL: "https://up.example.com", CredentialType: credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		Models:           []string{"req"},
		ModelMapping:     map[string]domain.ModelMappingEntry{"req": {MappedModel: "resolved", Mode: domain.ModelMappingModeExplicit}},
	}
	a := accWithEnabled(1, tplx, true, 12345)
	a.TemplateID = 7
	s := newTestScheduler(t, []*domain.Account{a})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "req")
	s.PublishDecisionForTest(route, &RouteDecision{Primary: []int64{1}})

	plan := s.CurrentRoutingPlan()
	cand := plan.Routes[0].Candidates[0]
	require.Equal(t, int64(1), cand.AccountID)
	require.Equal(t, int64(7), cand.TemplateID)
	require.Equal(t, int64(1), cand.LifecycleRevision)
	require.Equal(t, 12345, cand.UpstreamCostMultiplierBp)
	require.Equal(t, "resolved", cand.MappedModel)

	wantFP, err := CandidateFingerprint(a)
	require.NoError(t, err)
	require.Equal(t, wantFP, cand.Fingerprint)
	require.Equal(t, wantFP, cand.IdentityFingerprint, "derivable fingerprint IS the join identity")

	wantQC := qualityClassHexForWithOp(domain.FormatOpenAIChat, "resolved", domain.OpChatCompletions)
	require.NotEmpty(t, wantQC)
	require.Equal(t, wantQC, cand.QualityClassID)
}

func TestRoutingPlan_IdentityFingerprintSynthesisWhenUnderivable(t *testing.T) {
	// tplWith has no BaseURL/credential → real fingerprint underivable; identity
	// falls back to big-endian account ID bytes (compiler synthesis rule).
	tplx := tplWith(domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{accWithEnabled(5, tplx, true, 10000)})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	s.PublishDecisionForTest(route, &RouteDecision{Primary: []int64{5}})

	plan := s.CurrentRoutingPlan()
	cand := plan.Routes[0].Candidates[0]
	require.Empty(t, cand.Fingerprint)
	var b [32]byte
	binary.BigEndian.PutUint64(b[:8], 5)
	require.Equal(t, hex.EncodeToString(b[:]), cand.IdentityFingerprint)
}

func TestRoutingPlan_MissingStaticLeafKeepsReference(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	s := newTestScheduler(t, []*domain.Account{accWithEnabled(1, tplx, true, 10000)})
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	// Lane references an account absent from the static leaves (stale decision
	// rebased over a removal): the projection keeps the reference, identity-only.
	s.PublishDecisionForTest(route, &RouteDecision{Primary: []int64{1, 404}})

	plan := s.CurrentRoutingPlan()
	require.Len(t, plan.Routes[0].Candidates, 2)
	ghost := plan.Routes[0].Candidates[1]
	require.Equal(t, int64(404), ghost.AccountID)
	require.Zero(t, ghost.TemplateID)
	require.Empty(t, ghost.Fingerprint)
	var b [32]byte
	binary.BigEndian.PutUint64(b[:8], 404)
	require.Equal(t, hex.EncodeToString(b[:]), ghost.IdentityFingerprint)
}
