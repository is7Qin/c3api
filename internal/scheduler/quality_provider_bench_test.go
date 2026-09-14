// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"testing"
	"time"

	"github.com/is7qin/c3api/internal/domain"
)

// BenchmarkWindowedQualityProvider5000 pins the provider refresh latency at
// the 5000-candidate budget scale (fake settled source: no PG in this unit).
// Refresh advances M every call (Q1+Q2 fetch + full merge); Cached reuses one
// M (live merge only). Budget: PG ≤1/min/instance is structural (M-keyed
// cache, counted in TestWindowedQualityProvider); the real PG p99 is pinned
// by TestRoutingQualityWindowP99PG in the repository package.
func BenchmarkWindowedQualityProvider5000(b *testing.B) {
	const ncand, nhot = 5000, 400
	var rc domain.RouteClassIDVal
	rc[0] = 0xA1
	cur := make([]WindowSettledCurrent, 0, ncand)
	base := make(map[CandidateQualityKey]WindowSettledBaseline, nhot)
	liveRows := make([]WindowLiveRow, 0, ncand)
	for i := 0; i < ncand; i++ {
		var fp domain.CandidateFingerprintVal
		fp[0] = byte(i)
		fp[1] = byte(i >> 8)
		fp[2] = 0xC3
		k := CandidateQualityKey{RouteClassID: rc, Fingerprint: fp}
		cur = append(cur, WindowSettledCurrent{Key: k, Attempts: 30, Successes: 21, TTFTN: 20})
		if i < nhot {
			base[k] = WindowSettledBaseline{Key: k, Attempts: 60, Successes: 55}
		}
		liveRows = append(liveRows, WindowLiveRow{
			Minute:          benchMinute.Unix(),
			IdentityVersion: int16(domain.RoutingIdentityVersion),
			RouteClassID:    rc, Fingerprint: fp,
			Attempts: 5, Successes: 4, TTFTCount: 4,
		})
	}
	settled := &fakeSettledSource{cur: cur, base: base}
	live := &fakeLiveSource{rows: liveRows}

	b.Run("Refresh", func(b *testing.B) {
		provider := NewWindowedQualitySource(settled, live)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			wq := provider(benchMinute.Add(time.Duration(i) * time.Minute))
			if len(wq.Current) != ncand {
				b.Fatalf("current rows = %d, want %d", len(wq.Current), ncand)
			}
		}
	})
	b.Run("Cached", func(b *testing.B) {
		provider := NewWindowedQualitySource(settled, live)
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			wq := provider(benchMinute)
			if len(wq.Current) != ncand {
				b.Fatalf("current rows = %d, want %d", len(wq.Current), ncand)
			}
		}
	})
}

var benchMinute = time.Date(2026, time.August, 29, 12, 0, 0, 0, time.UTC)
