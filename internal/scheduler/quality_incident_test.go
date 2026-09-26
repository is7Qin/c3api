// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"testing"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestIncidentFailureDomain(t *testing.T) {
	// 故障域与候选指纹读的是同一份源：路径、查询串都不把同一 host 拆成两个域。
	first, err := domain.CanonicalOrigin("https://api.example.com/v1/chat")
	require.NoError(t, err)
	require.Equal(t, "https://api.example.com:443", first)
	second, err := domain.CanonicalOrigin("HTTPS://API.EXAMPLE.COM:443/path?x=1")
	require.NoError(t, err)
	require.Equal(t, first, second)
	httpOrigin, err := domain.CanonicalOrigin("http://example.com")
	require.NoError(t, err)
	require.Equal(t, "http://example.com:80", httpOrigin)
	_, err = domain.CanonicalOrigin("")
	require.Error(t, err)
	_, err = domain.CanonicalOrigin("not-a-url")
	require.Error(t, err)
	require.NotEqual(t, first, httpOrigin)
}

func TestIncidentDegraded(t *testing.T) {
	current := Interval{LCB: 0.9, UCB: 0.95}
	baseline := Interval{LCB: 0.7, UCB: 0.8}
	require.True(t, IsDegraded(current, baseline, 30, 30))
	require.False(t, IsDegraded(current, baseline, 29, 30))
	require.False(t, IsDegraded(current, baseline, 30, 29))
	require.False(t, IsDegraded(Interval{LCB: 0.75, UCB: 0.85}, baseline, 30, 30))
}

func TestIncidentDomain(t *testing.T) {
	require.True(t, DomainIncident([]IncidentCandidate{
		{Domain: "a", Comparable: true, Degraded: true},
		{Domain: "a", Comparable: true, Degraded: true},
		{Domain: "a", Comparable: true},
	}))
	require.False(t, DomainIncident([]IncidentCandidate{
		{Domain: "a", Comparable: true, Degraded: true},
		{Domain: "a", Comparable: true},
	}))
	require.False(t, DomainIncident([]IncidentCandidate{{Domain: "a", Comparable: true, Degraded: true}}))
}

func TestIncidentModel(t *testing.T) {
	require.True(t, ModelIncident([]IncidentCandidate{
		{Domain: "a.com", Comparable: true, Degraded: true},
		{Domain: "b.com", Comparable: true, Degraded: true},
		{Domain: "c.com", Comparable: true},
	}))
	require.False(t, ModelIncident([]IncidentCandidate{
		{Domain: "a.com", Comparable: true, Degraded: true},
		{Domain: "b.com", Comparable: true},
	}))
	require.False(t, ModelIncident([]IncidentCandidate{{Domain: "a.com", Comparable: true, Degraded: true}}))
}

func TestIncidentTwoCycleRecovery(t *testing.T) {
	var state IncidentState
	require.True(t, state.Update(true))
	require.True(t, state.Update(false))
	require.Equal(t, 1, state.HealthyStreak)
	require.False(t, state.Update(false))
	require.Equal(t, 0, state.HealthyStreak)
	state.Update(true)
	state.Update(false)
	require.True(t, state.Update(true))
	require.Equal(t, 0, state.HealthyStreak)
}
