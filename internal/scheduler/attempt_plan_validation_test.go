// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewAttemptPlan_rejectsDuplicateAfterFixedStorageCapacity(t *testing.T) {
	// Given
	decision := &RouteDecision{
		Primary:  ccPrimary(1, 2, 3, 4, 5, 6, 7, 8, 9),
		Degraded: ccDegraded(9),
	}

	// When
	_, err := NewAttemptPlan(AttemptPlanIdentity{}, decision)

	// Then
	var invalid *InvalidRouteDecisionError
	require.ErrorAs(t, err, &invalid)
	require.Equal(t, "degraded[0]", invalid.Field)
	require.Equal(t, int64(9), invalid.AccountID)
}

func TestNewAttemptPlan_rejectsDuplicateAndOutOfRangeFallbackIndexes(t *testing.T) {
	tests := []struct {
		name         string
		fallback     []uint16
		wantField    string
		wantIndex    int
		wantFallback uint16
	}{
		{name: "duplicate", fallback: []uint16{1, 1}, wantField: "explore.fallback[1]", wantIndex: 1, wantFallback: 1},
		{name: "out of range", fallback: []uint16{2}, wantField: "explore.fallback[0]", wantIndex: 0, wantFallback: 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Given
			decision := &RouteDecision{Explore: ExploreDecision{
				Ordered: ccExplore(10, 11),
				Weights: map[int64]int{10: 1, 11: 1}, Cumulative: []uint64{1, 2}, Total: 2,
				Fallback: tt.fallback,
			}}

			// When
			_, err := NewAttemptPlan(AttemptPlanIdentity{}, decision)

			// Then
			var invalid *InvalidRouteDecisionError
			require.ErrorAs(t, err, &invalid)
			require.Equal(t, tt.wantField, invalid.Field)
			require.Equal(t, tt.wantIndex, invalid.Index)
			require.Equal(t, tt.wantFallback, invalid.FallbackIndex)
			require.ErrorIs(t, err, ErrInvalidRouteDecision)
		})
	}
}

func TestNewAttemptPlan_rejectsMalformedExploreMetadata(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*ExploreDecision)
		wantField string
	}{
		{name: "cumulative length", mutate: func(e *ExploreDecision) { e.Cumulative = e.Cumulative[:1] }, wantField: "explore.cumulative"},
		{name: "non-monotonic cumulative", mutate: func(e *ExploreDecision) { e.Cumulative[1] = e.Cumulative[0] }, wantField: "explore.cumulative[1]"},
		{name: "total differs from final cumulative", mutate: func(e *ExploreDecision) { e.Total++ }, wantField: "explore.total"},
		{name: "missing weight", mutate: func(e *ExploreDecision) { delete(e.Weights, 11) }, wantField: "explore.weights[11]"},
		{name: "negative weight", mutate: func(e *ExploreDecision) { e.Weights[11] = -1 }, wantField: "explore.weights[11]"},
		{name: "zero weight", mutate: func(e *ExploreDecision) { e.Weights[11] = 0 }, wantField: "explore.weights[11]"},
		{name: "weight differs from cumulative delta", mutate: func(e *ExploreDecision) { e.Weights[11] = 21 }, wantField: "explore.cumulative[1]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Given
			explore := ExploreDecision{
				Ordered: ccExplore(10, 11), Weights: map[int64]int{10: 10, 11: 20},
				Cumulative: []uint64{10, 30}, Total: 30, Fallback: []uint16{0, 1},
			}
			tt.mutate(&explore)

			// When
			_, err := NewAttemptPlan(AttemptPlanIdentity{}, &RouteDecision{Explore: explore})

			// Then
			var invalid *InvalidRouteDecisionError
			require.ErrorAs(t, err, &invalid)
			require.ErrorIs(t, err, ErrInvalidRouteDecision)
			require.Equal(t, tt.wantField, invalid.Field)
		})
	}
}

func TestNewAttemptPlan_acceptsEmptyExploreMetadata(t *testing.T) {
	// When
	plan, err := NewAttemptPlan(AttemptPlanIdentity{}, &RouteDecision{})

	// Then
	require.NoError(t, err)
	require.Equal(t, 0, plan.total)
}

func TestNewAttemptPlan_rejectsNilDecisionWithoutPanic(t *testing.T) {
	// Given
	var decision *RouteDecision

	// When
	_, err := NewAttemptPlan(AttemptPlanIdentity{}, decision)

	// Then
	var invalid *InvalidRouteDecisionError
	require.True(t, errors.As(err, &invalid))
	require.Equal(t, "decision", invalid.Field)
}

func TestNewAttemptPlan_keepsPublishedDecisionPointer(t *testing.T) {
	// Given
	decision := &RouteDecision{Primary: ccPrimary(1)}

	// When
	plan, err := NewAttemptPlan(AttemptPlanIdentity{}, decision)

	// Then
	require.NoError(t, err)
	require.Same(t, decision, plan.route)
}
