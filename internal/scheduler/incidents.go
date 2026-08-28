// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"math"
	"sort"
)

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

type QualityCandidate struct {
	AccountID int64
	Successes int
	Attempts  int
	SumLog    float64
	SumSq     float64
	TTFTCount int
	Cost      int64
}

type classifiedCandidate struct {
	QualityCandidate
	Success Interval
	TTFT    Interval
	TTFTOk  bool
}

func ClassifyQuality(cands []QualityCandidate) (primary, degraded, explore []QualityCandidate) {
	if len(cands) == 0 {
		return nil, nil, nil
	}
	var all []classifiedCandidate
	for _, c := range cands {
		if IsExplore(c.Attempts) {
			explore = append(explore, c)
			continue
		}
		si := Wilson95(c.Successes, c.Attempts)
		ti, ok := LogTTFTInterval(c.SumLog, c.SumSq, c.TTFTCount)
		all = append(all, classifiedCandidate{c, si, ti, ok})
	}
	if len(all) == 0 {
		sort.Slice(explore, func(i, j int) bool { return explore[i].AccountID < explore[j].AccountID })
		return nil, nil, explore
	}
	bestLCB := all[0].Success.LCB
	for _, a := range all[1:] {
		if a.Success.LCB > bestLCB {
			bestLCB = a.Success.LCB
		}
	}
	var equiv []classifiedCandidate
	for _, a := range all {
		if a.Success.UCB >= bestLCB {
			equiv = append(equiv, a)
		}
	}
	if len(equiv) == 0 {
		return nil, nil, explore
	}
	hasTTFT := false
	fastestUCB := 0.0
	first := true
	for _, a := range equiv {
		if a.TTFTOk {
			if first || a.TTFT.UCB < fastestUCB {
				fastestUCB = a.TTFT.UCB
				first = false
				hasTTFT = true
			}
		}
	}
	for _, a := range equiv {
		ttftEquiv := false
		if !hasTTFT {
			ttftEquiv = !a.TTFTOk
		} else if a.TTFTOk {
			ttftEquiv = a.TTFT.LCB <= fastestUCB
		}
		if ttftEquiv {
			primary = append(primary, a.QualityCandidate)
		} else {
			degraded = append(degraded, a.QualityCandidate)
		}
	}
	sort.Slice(primary, func(i, j int) bool {
		if primary[i].Cost != primary[j].Cost {
			return primary[i].Cost < primary[j].Cost
		}
		si := Wilson95(primary[i].Successes, primary[i].Attempts)
		sj := Wilson95(primary[j].Successes, primary[j].Attempts)
		if si.LCB != sj.LCB {
			return si.LCB > sj.LCB
		}
		ti, oki := LogTTFTInterval(primary[i].SumLog, primary[i].SumSq, primary[i].TTFTCount)
		tj, okj := LogTTFTInterval(primary[j].SumLog, primary[j].SumSq, primary[j].TTFTCount)
		if oki && okj && ti.UCB != tj.UCB {
			return ti.UCB < tj.UCB
		}
		return primary[i].AccountID < primary[j].AccountID
	})
	sort.Slice(degraded, func(i, j int) bool {
		si := Wilson95(degraded[i].Successes, degraded[i].Attempts)
		sj := Wilson95(degraded[j].Successes, degraded[j].Attempts)
		di := math.Max(0, bestLCB-si.UCB)
		dj := math.Max(0, bestLCB-sj.UCB)
		if di != dj {
			return di < dj
		}
		ti, oki := LogTTFTInterval(degraded[i].SumLog, degraded[i].SumSq, degraded[i].TTFTCount)
		tj, okj := LogTTFTInterval(degraded[j].SumLog, degraded[j].SumSq, degraded[j].TTFTCount)
		if hasTTFT {
			var tdi, tdj float64
			if oki {
				tdi = math.Max(0, ti.LCB-fastestUCB)
			} else {
				tdi = math.Max(0, math.Inf(1))
			}
			if okj {
				tdj = math.Max(0, tj.LCB-fastestUCB)
			} else {
				tdj = math.Max(0, math.Inf(1))
			}
			if tdi != tdj {
				return tdi < tdj
			}
		}
		if degraded[i].Cost != degraded[j].Cost {
			return degraded[i].Cost < degraded[j].Cost
		}
		return degraded[i].AccountID < degraded[j].AccountID
	})
	sort.Slice(explore, func(i, j int) bool { return explore[i].AccountID < explore[j].AccountID })
	return primary, degraded, explore
}
