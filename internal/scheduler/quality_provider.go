// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"time"

	"github.com/is7qin/c3api/internal/domain"
)

// Windowed quality provider (incident-wiring): the single data path
// replacing the live-cell shortcut, serving lanes AND incidents. Settled PG
// minutes merged with live minute buckets; baseline from PG only.
//
// M discipline: the caller (compileOnce) computes now ONCE per fire and the
// provider derives M = truncate(now, minute) once per call, threading it
// through the settled reads, the live filter, and SettledBoundary. Settled ∩
// live = ∅ by minute label: PG reads cover [..., M), live rows older than
// M-5m are excluded from current (they surface via PG once flushed).
//
// Cache: PG is fetched at most once per (M, identityVersion) — refresh on
// M advance only — regardless of the ~5s compile cadence; the live source is
// read fresh on every call and the baseline recomputed only when M advances
// (newly-hot candidates evaluate non-comparable until next M). PG errors
// degrade to live-only (never stale settled: a stale baseline against fresh
// current could false-fire incidents); the failed M still backoffs PG until
// the next minute. Bounded memory: two maps ≤ the active candidate count.
//
// Lane ownership: the returned closure is compile-lane-confined (no mutex),
// like lastQuality/lastDecisionBytes.

// windowQ32Scale converts Q32 fixed-point TTFT log sums to float (same scale
// the recorder writes and the old live-cell adapter read).
const windowQ32Scale = float64(int64(1) << 32)

// windowSettledTimeout caps one M-advance PG refresh so a wedged database
// cannot stall the serial compile lane past one window.
const windowSettledTimeout = 5 * time.Second

// WindowLiveRow is one absolute live minute bucket for a candidate (quality
// classes already merged by the source; out-of-window and version-mismatched
// rows are filtered here, not by the source).
type WindowLiveRow struct {
	Minute            int64
	IdentityVersion   int16
	RouteClassID      domain.RouteClassIDVal
	Fingerprint       domain.CandidateFingerprintVal
	Attempts          int64
	Successes         int64
	TTFTCount         int64
	SumLogQ32         int64
	SumSqQ32          int64
	InputTokens       int64
	OutputTokens      int64
	CacheReadTokens   int64
	CacheCreateTokens int64
}

// WindowLiveSource yields absolute live rows not yet in PG (fresh per call).
type WindowLiveSource interface {
	UnflushedMinutes(now time.Time) []WindowLiveRow
}

// WindowSettledCurrent is one candidate aggregated over [M-5m, M).
type WindowSettledCurrent struct {
	Key               CandidateQualityKey
	Attempts          int64
	Successes         int64
	TTFTN             int64
	SumLogQ32         int64
	SumSqQ32          int64
	InputTokens       int64
	OutputTokens      int64
	CacheReadTokens   int64
	CacheCreateTokens int64
}

// WindowSettledHotKey marks a baseline-eligible candidate (current ≥ 30).
type WindowSettledHotKey struct {
	RouteClassID domain.RouteClassIDVal
	Fingerprint  domain.CandidateFingerprintVal
}

// WindowSettledBaseline is one hot candidate truncated newest-first at ≥ 30.
type WindowSettledBaseline struct {
	Key       CandidateQualityKey
	Attempts  int64
	Successes int64
}

// WindowSettledSource reads the settled PG rollup (refreshed per M only).
type WindowSettledSource interface {
	QueryCurrentWindowStats(ctx context.Context, identityVersion int16, evaluatedMinute time.Time) ([]WindowSettledCurrent, error)
	QueryBaselineTruncated(ctx context.Context, identityVersion int16, evaluatedMinute time.Time, hotKeys []WindowSettledHotKey) ([]WindowSettledBaseline, error)
}

// WindowedQuality is one fire's merged quality inputs.
type WindowedQuality struct {
	Current         map[CandidateQualityKey]CandidateQualityInput
	Baseline        map[CandidateQualityKey]Counts
	SettledBoundary time.Time
}

// NewWindowedQualitySource builds the M-cached provider closure. Nil sources
// are tolerated (empty half) so a source outage can never wedge the compile
// lane; the closure itself is not goroutine-safe (serial lane only).
func NewWindowedQualitySource(settled WindowSettledSource, live WindowLiveSource) func(time.Time) WindowedQuality {
	c := &windowedQualityCache{settled: settled, live: live}
	return c.get
}

type windowedQualityCache struct {
	settled WindowSettledSource
	live    WindowLiveSource

	lastMinute int64
	haveCache  bool
	cur        []WindowSettledCurrent
	base       map[CandidateQualityKey]Counts
}

func (c *windowedQualityCache) get(now time.Time) WindowedQuality {
	m := now.UTC().Truncate(time.Minute)
	mUnix := m.Unix()
	version := int16(domain.RoutingIdentityVersion)

	var liveRows []WindowLiveRow
	if c.live != nil {
		liveRows = c.live.UnflushedMinutes(now)
	}
	if !c.haveCache || mUnix != c.lastMinute {
		c.refresh(m, version, liveRows)
	}
	out := WindowedQuality{SettledBoundary: m}
	cur := make(map[CandidateQualityKey]CandidateQualityInput, len(c.cur)+len(liveRows))
	for _, s := range c.cur {
		cnt := Counts{
			Attempts:  int(s.Attempts),
			Successes: int(s.Successes),
			TTFTCount: int(s.TTFTN),
			SumLog:    float64(s.SumLogQ32) / windowQ32Scale,
			SumSq:     float64(s.SumSqQ32) / windowQ32Scale,
		}
		base, err := Counts{}.Add(cnt)
		if err != nil {
			continue // fail-closed: illegal settled contribution skipped
		}
		cur[s.Key] = CandidateQualityInput{
			Counts:            base,
			InputTokens:       s.InputTokens,
			OutputTokens:      s.OutputTokens,
			CacheReadTokens:   s.CacheReadTokens,
			CacheCreateTokens: s.CacheCreateTokens,
		}
	}
	cutoff := m.Add(-5 * time.Minute).Unix()
	for _, row := range liveRows {
		if row.IdentityVersion != version {
			continue // identity-version mismatch ignored
		}
		if row.Minute < cutoff {
			continue // pre-window live surfaces via PG once flushed
		}
		cnt := Counts{
			Attempts:  int(row.Attempts),
			Successes: int(row.Successes),
			TTFTCount: int(row.TTFTCount),
			SumLog:    float64(row.SumLogQ32) / windowQ32Scale,
			SumSq:     float64(row.SumSqQ32) / windowQ32Scale,
		}
		key := CandidateQualityKey{RouteClassID: row.RouteClassID, Fingerprint: row.Fingerprint}
		prev, seen := cur[key]
		if !seen {
			base, err := Counts{}.Add(cnt)
			if err != nil {
				continue
			}
			cur[key] = CandidateQualityInput{
				Counts:            base,
				InputTokens:       row.InputTokens,
				OutputTokens:      row.OutputTokens,
				CacheReadTokens:   row.CacheReadTokens,
				CacheCreateTokens: row.CacheCreateTokens,
			}
			continue
		}
		merged, err := prev.Counts.Add(cnt)
		if err != nil {
			continue // fail-closed: overflow contribution skipped
		}
		prev.Counts = merged
		prev.InputTokens += row.InputTokens
		prev.OutputTokens += row.OutputTokens
		prev.CacheReadTokens += row.CacheReadTokens
		prev.CacheCreateTokens += row.CacheCreateTokens
		cur[key] = prev
	}
	out.Current = cur
	out.Baseline = c.base
	return out
}

// refresh re-reads the settled PG half for a new M: current first, hot keys
// derived from the merged current (settled + the same call's live rows),
// then the truncated baseline. Any PG error degrades to live-only with the
// failed M still recorded (minute-granularity backoff, never stale settled).
func (c *windowedQualityCache) refresh(m time.Time, version int16, liveRows []WindowLiveRow) {
	c.cur = nil
	c.base = nil
	c.lastMinute = m.Unix()
	c.haveCache = true
	if c.settled == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), windowSettledTimeout)
	defer cancel()
	pgCur, err := c.settled.QueryCurrentWindowStats(ctx, version, m)
	if err != nil {
		return
	}
	c.cur = pgCur
	hot := hotKeysFrom(m, version, pgCur, liveRows)
	pgBase, err := c.settled.QueryBaselineTruncated(ctx, version, m, hot)
	if err != nil {
		c.cur = nil
		return
	}
	base := make(map[CandidateQualityKey]Counts, len(pgBase))
	for _, b := range pgBase {
		cnt := Counts{Attempts: int(b.Attempts), Successes: int(b.Successes)}
		valid, err := Counts{}.Add(cnt)
		if err != nil {
			continue
		}
		base[b.Key] = valid
	}
	c.base = base
}

// hotKeysFrom derives baseline eligibility from the merged current (settled
// plus the same call's live rows): candidates at attempts ≥ 30.
func hotKeysFrom(m time.Time, version int16, pgCur []WindowSettledCurrent, liveRows []WindowLiveRow) []WindowSettledHotKey {
	attempts := make(map[CandidateQualityKey]int64, len(pgCur))
	for _, s := range pgCur {
		attempts[s.Key] += s.Attempts
	}
	cutoff := m.Add(-5 * time.Minute).Unix()
	for _, row := range liveRows {
		if row.IdentityVersion != version || row.Minute < cutoff {
			continue
		}
		key := CandidateQualityKey{RouteClassID: row.RouteClassID, Fingerprint: row.Fingerprint}
		attempts[key] += row.Attempts
	}
	var hot []WindowSettledHotKey
	for key, n := range attempts {
		if n >= 30 {
			hot = append(hot, WindowSettledHotKey{RouteClassID: key.RouteClassID, Fingerprint: key.Fingerprint})
		}
	}
	return hot
}
