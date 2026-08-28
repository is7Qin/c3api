// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

func IsDegraded(current, baseline Interval, nCurrent, nBaseline int) bool {
	if nCurrent < 30 || nBaseline < 30 {
		return false
	}
	if current.UCB < baseline.LCB || baseline.UCB < current.LCB {
		return true
	}
	return false
}

type IncidentCandidate struct {
	Domain     string
	Comparable bool
	Degraded   bool
}

func DomainIncident(cands []IncidentCandidate) bool {
	comparable, degraded := 0, 0
	for _, c := range cands {
		if c.Comparable {
			comparable++
			if c.Degraded {
				degraded++
			}
		}
	}
	if comparable < 2 {
		return false
	}
	return degraded*2 > comparable
}

func ModelIncident(cands []IncidentCandidate) bool {
	domains := map[string]struct{}{}
	incidentDomains := map[string]struct{}{}
	for _, c := range cands {
		if c.Comparable {
			domains[c.Domain] = struct{}{}
			if c.Degraded {
				incidentDomains[c.Domain] = struct{}{}
			}
		}
	}
	if len(domains) < 2 {
		return false
	}
	return len(incidentDomains)*2 > len(domains)
}

type IncidentState struct {
	Active        bool
	HealthyStreak int
}

func (s *IncidentState) Update(degraded bool) bool {
	if degraded {
		s.Active = true
		s.HealthyStreak = 0
		return true
	}
	if s.Active {
		s.HealthyStreak++
		if s.HealthyStreak >= 2 {
			s.Active = false
			s.HealthyStreak = 0
		}
	}
	return s.Active
}
