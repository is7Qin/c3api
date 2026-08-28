// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"crypto/sha256"
	"errors"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"
)

const WilsonZ = 1.959963984540054
const ttftZ = 1.96

type Interval struct{ LCB, UCB float64 }

func Wilson95(successes, attempts int) Interval {
	if attempts <= 0 {
		return Interval{}
	}
	if successes < 0 {
		successes = 0
	}
	if successes > attempts {
		successes = attempts
	}
	n := float64(attempts)
	p := float64(successes) / n
	z2 := WilsonZ * WilsonZ
	denom := 1 + z2/n
	center := p + z2/(2*n)
	variance := p*(1-p)/n + z2/(4*n*n)
	if variance < 0 {
		variance = 0
	}
	margin := WilsonZ * math.Sqrt(variance)
	lcb := (center - margin) / denom
	ucb := (center + margin) / denom
	if lcb < 0 {
		lcb = 0
	}
	if ucb > 1 {
		ucb = 1
	}
	return Interval{lcb, ucb}
}

func LogTTFTInterval(sumLog, sumSq float64, n int) (Interval, bool) {
	if n < 30 {
		return Interval{}, false
	}
	mean := sumLog / float64(n)
	variance := 0.0
	if n > 1 {
		variance = (sumSq - float64(n)*mean*mean) / float64(n-1)
		if variance < 0 {
			variance = 0
		}
	}
	half := ttftZ * math.Sqrt(variance) / math.Sqrt(float64(n))
	return Interval{math.Exp(mean - half), math.Exp(mean + half)}, true
}

func LogTTFTFromSamples(ttftMs []int64) (Interval, bool) {
	if len(ttftMs) < 30 {
		return Interval{}, false
	}
	var sum, sumSq float64
	for _, v := range ttftMs {
		if v < 1 {
			v = 1
		}
		y := math.Log(float64(v))
		sum += y
		sumSq += y * y
	}
	return LogTTFTInterval(sum, sumSq, len(ttftMs))
}

func CurrentWindow(now time.Time) (time.Time, time.Time, time.Time, time.Time) {
	m := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), now.UTC().Hour(), now.UTC().Minute(), 0, 0, time.UTC)
	return m.Add(-5 * time.Minute), m, m, now.UTC()
}

func BaselineWindow(now time.Time) (time.Time, time.Time) {
	m := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), now.UTC().Hour(), now.UTC().Minute(), 0, 0, time.UTC)
	return m.Add(-24 * time.Hour), m.Add(-5 * time.Minute)
}

type Counts struct {
	Attempts  int
	Successes int
	SumLog    float64
	SumSq     float64
	TTFTCount int
}

func (c Counts) Add(o Counts) Counts {
	return Counts{c.Attempts + o.Attempts, c.Successes + o.Successes, c.SumLog + o.SumLog, c.SumSq + o.SumSq, c.TTFTCount + o.TTFTCount}
}

type MinuteBucket struct {
	Start  time.Time
	Counts Counts
}

func AccumulateCurrent(buckets []MinuteBucket, live Counts, now time.Time) Counts {
	m := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), now.UTC().Hour(), now.UTC().Minute(), 0, 0, time.UTC)
	cut := m.Add(-5 * time.Minute)
	var out Counts
	for _, b := range buckets {
		if !b.Start.Before(cut) && b.Start.Before(m) {
			out = out.Add(b.Counts)
		}
	}
	return out.Add(live)
}

func AccumulateBaseline(buckets []MinuteBucket, now time.Time) Counts {
	m := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), now.UTC().Hour(), now.UTC().Minute(), 0, 0, time.UTC)
	bs := m.Add(-24 * time.Hour)
	be := m.Add(-5 * time.Minute)
	var filtered []MinuteBucket
	for _, b := range buckets {
		if !b.Start.Before(bs) && b.Start.Before(be) {
			filtered = append(filtered, b)
		}
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].Start.After(filtered[j].Start) })
	var acc Counts
	for _, b := range filtered {
		acc = acc.Add(b.Counts)
		if acc.Attempts >= 30 {
			break
		}
	}
	return acc
}

func IsExplore(attempts int) bool { return attempts < 30 }

func ExploreBP(eligible, unknown, primaryCount int) int {
	if eligible <= 0 {
		return 0
	}
	if primaryCount == 0 {
		return 10000
	}
	if unknown <= 0 {
		return 100
	}
	v := math.Ceil(400 * float64(unknown) / float64(eligible))
	if v > 400 {
		v = 400
	}
	return 100 + int(v)
}

func ExploreWeight(successes, attempts int) int {
	if successes < 0 {
		successes = 0
	}
	if successes > attempts {
		successes = attempts
	}
	v := 10000 * float64(successes+1) / float64(attempts+2)
	w := int(math.Floor(v + 0.5))
	if w < 100 {
		w = 100
	}
	return w
}

type ExploreCandidate struct {
	AccountID int64
	Weight    int
}

func CumulativeWeights(cands []ExploreCandidate) ([]uint64, uint64, error) {
	if len(cands) == 0 {
		return nil, 0, nil
	}
	sorted := make([]ExploreCandidate, len(cands))
	copy(sorted, cands)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].AccountID < sorted[j].AccountID })
	cum := make([]uint64, len(sorted))
	var total uint64
	for i, c := range sorted {
		if c.Weight < 0 {
			return nil, 0, errors.New("negative weight")
		}
		w := uint64(c.Weight)
		if total > math.MaxUint64-w {
			return nil, 0, errors.New("uint64 overflow")
		}
		total += w
		cum[i] = total
	}
	return cum, total, nil
}

type FallbackCandidate struct {
	AccountID int64
	Successes int
	Attempts  int
}

func FallbackOrder(cands []FallbackCandidate) []FallbackCandidate {
	out := make([]FallbackCandidate, len(cands))
	copy(out, cands)
	sort.Slice(out, func(i, j int) bool {
		pi := float64(out[i].Successes+1) / float64(out[i].Attempts+2)
		pj := float64(out[j].Successes+1) / float64(out[j].Attempts+2)
		if pi != pj {
			return pi > pj
		}
		if out[i].Attempts != out[j].Attempts {
			return out[i].Attempts < out[j].Attempts
		}
		return out[i].AccountID < out[j].AccountID
	})
	return out
}

func CanonicalOrigin(raw string) (string, error) {
	if raw == "" {
		return "", errors.New("empty origin")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	scheme := strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	if scheme == "" || host == "" {
		return "", errors.New("missing scheme or host")
	}
	port := u.Port()
	if port == "" {
		if scheme == "https" {
			port = "443"
		} else if scheme == "http" {
			port = "80"
		}
	}
	if port != "" {
		host = host + ":" + port
	}
	return scheme + "://" + host, nil
}

func FailureDomainID(origin string) [32]byte {
	canonical, err := CanonicalOrigin(origin)
	if err != nil {
		canonical = strings.ToLower(strings.TrimSpace(origin))
	}
	return sha256.Sum256([]byte(canonical))
}
