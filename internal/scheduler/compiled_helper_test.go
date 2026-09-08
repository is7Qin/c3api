// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func mustNewAttemptPlan(t *testing.T, identity AttemptPlanIdentity, decision *RouteDecision) *AttemptPlan {
	t.Helper()
	plan, err := NewAttemptPlan(identity, decision)
	require.NoError(t, err)
	return plan
}

// testCC builds stub compiled candidates for pure-plan unit tests (no
// scheduler). Metadata is deterministic and validation-free; Lane is carried
// so lane-order assertions keep working.
func testCC(lane AttemptLane, ids ...int64) []CompiledCandidate {
	out := make([]CompiledCandidate, 0, len(ids))
	for _, id := range ids {
		out = append(out, CompiledCandidate{
			AccountID: id, Lane: lane, TemplateID: 1, BaseURL: "https://u/v1",
			Fingerprint: fmt.Sprintf("fp-%d", id), RequestedModel: "m", MappedModel: "m",
			Quality: "q", QualityRaw: "q", LifecycleRevision: 1,
		})
	}
	return out
}

func ccPrimary(ids ...int64) []CompiledCandidate  { return testCC(AttemptLanePrimary, ids...) }
func ccExplore(ids ...int64) []CompiledCandidate  { return testCC(AttemptLaneExplore, ids...) }
func ccDegraded(ids ...int64) []CompiledCandidate { return testCC(AttemptLaneDegraded, ids...) }

func fallbackIndexes(indexes ...uint16) []uint16 { return indexes }

func compiledAccountIDs(cs []CompiledCandidate) []int64 {
	if cs == nil {
		return nil
	}
	ids := make([]int64, len(cs))
	for i, c := range cs {
		ids[i] = c.AccountID
	}
	return ids
}

// enrichDecision fills stub compiled candidates with real metadata from the
// scheduler's current static view (cold test path only). Preserves lane order;
// unknown IDs keep their stubs.
func enrichDecision(s *Scheduler, route RouteRef, d *RouteDecision) *RouteDecision {
	if d == nil {
		return nil
	}
	v := s.view.Load()
	if v == nil || v.static == nil {
		return d
	}
	caller := string(callerKindForFormat(domain.RequestFormat(route.Format)))
	op := domain.OperationTag(route.OperationTag)
	fill := func(cs []CompiledCandidate, lane AttemptLane) []CompiledCandidate {
		out := make([]CompiledCandidate, 0, len(cs))
		for _, c := range cs {
			if snap, ok := v.static.byID[c.AccountID]; ok && snap != nil {
				facts := buildCandidateFacts([]*accountSnapshot{snap}, v.static.facts, routeKey{format: domain.RequestFormat(route.Format), model: route.Model}, op)
				cc := compileCandidate(facts[0], lane)
				if cc.Fingerprint == "" {
					cc.Fingerprint = c.Fingerprint
				}
				out = append(out, cc)
			} else {
				out = append(out, c)
			}
		}
		return out
	}
	d.Format = route.Format
	d.RequestedModel = route.Model
	d.RouteClassID = route.RouteClassID
	d.CallerCategory = caller
	d.OperationTag = route.OperationTag
	d.Primary = fill(d.Primary, AttemptLanePrimary)
	d.Degraded = fill(d.Degraded, AttemptLaneDegraded)
	d.Explore.Ordered = fill(d.Explore.Ordered, AttemptLaneExplore)
	return d
}
