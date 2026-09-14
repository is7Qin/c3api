// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7qin.

package main

import (
	"context"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/quality"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/scheduler"
)

// windowSettledBackend is the narrow PG face the windowed provider needs
// (satisfied by *repository.Partitions; fakes in tests).
type windowSettledBackend interface {
	QueryCurrentWindowStats(ctx context.Context, identityVersion int16, evaluatedMinute time.Time) ([]repository.WindowCurrentStat, error)
	QueryBaselineTruncated(ctx context.Context, identityVersion int16, evaluatedMinute time.Time, hotKeys []repository.WindowHotKey) ([]repository.WindowBaselineStat, error)
}

// partitionWindowSettled adapts the repository window rows to the scheduler
// provider DTOs (no rescaling here — Q32→float lives in the provider core).
type partitionWindowSettled struct {
	parts windowSettledBackend
}

func (p *partitionWindowSettled) QueryCurrentWindowStats(ctx context.Context, v int16, m time.Time) ([]scheduler.WindowSettledCurrent, error) {
	rows, err := p.parts.QueryCurrentWindowStats(ctx, v, m)
	if err != nil {
		return nil, err
	}
	out := make([]scheduler.WindowSettledCurrent, 0, len(rows))
	for _, r := range rows {
		out = append(out, scheduler.WindowSettledCurrent{
			Key:               scheduler.CandidateQualityKey{RouteClassID: r.RouteClassID, Fingerprint: r.Fingerprint},
			Attempts:          r.Attempts,
			Successes:         r.Successes,
			TTFTN:             r.TTFTN,
			SumLogQ32:         r.TTFTSumLogQ32,
			SumSqQ32:          r.TTFTSumSqLogQ32,
			InputTokens:       r.InputTokens,
			OutputTokens:      r.OutputTokens,
			CacheReadTokens:   r.CacheReadTokens,
			CacheCreateTokens: r.CacheCreateTokens,
		})
	}
	return out, nil
}

func (p *partitionWindowSettled) QueryBaselineTruncated(ctx context.Context, v int16, m time.Time, hot []scheduler.WindowSettledHotKey) ([]scheduler.WindowSettledBaseline, error) {
	keys := make([]repository.WindowHotKey, 0, len(hot))
	for _, h := range hot {
		keys = append(keys, repository.WindowHotKey{RouteClassID: h.RouteClassID, Fingerprint: h.Fingerprint})
	}
	rows, err := p.parts.QueryBaselineTruncated(ctx, v, m, keys)
	if err != nil {
		return nil, err
	}
	out := make([]scheduler.WindowSettledBaseline, 0, len(rows))
	for _, r := range rows {
		out = append(out, scheduler.WindowSettledBaseline{
			Key:       scheduler.CandidateQualityKey{RouteClassID: r.RouteClassID, Fingerprint: r.Fingerprint},
			Attempts:  r.Attempts,
			Successes: r.Successes,
		})
	}
	return out, nil
}

// recorderWindowLive adapts the recorder's unflushed absolute rows to the
// provider's flat live DTOs (quality classes merge downstream by (route, fp),
// the same aggregation the deleted live-cell shortcut used).
type recorderWindowLive struct {
	rec *quality.Recorder
}

func (l *recorderWindowLive) UnflushedMinutes(now time.Time) []scheduler.WindowLiveRow {
	if l.rec == nil {
		return nil
	}
	byMinute := l.rec.UnflushedMinutes(now)
	var out []scheduler.WindowLiveRow
	for minute, rows := range byMinute {
		for k, qm := range rows {
			out = append(out, scheduler.WindowLiveRow{
				Minute:            minute,
				IdentityVersion:   k.IdentityVersion,
				RouteClassID:      domain.RouteClassIDVal(k.RouteClassID),
				Fingerprint:       domain.CandidateFingerprintVal(k.Fingerprint),
				Attempts:          qm.Attempts(),
				Successes:         qm.Successes(),
				TTFTCount:         qm.TTFTCount(),
				SumLogQ32:         qm.SumQ32(),
				SumSqQ32:          qm.SumSqQ32(),
				InputTokens:       qm.InputTokens(),
				OutputTokens:      qm.OutputTokens(),
				CacheReadTokens:   qm.CacheReadTokens(),
				CacheCreateTokens: qm.CacheCreateTokens(),
			})
		}
	}
	return out
}

// NewWindowedQualityProvider wires the recorder live seam and the PG settled
// reads into the scheduler's M-cached windowed quality provider (single-M
// discipline, PG ≤1/min/instance). Replaces the deleted live-cell shortcut:
// current = settled PG minutes merged with live buckets, baseline = PG only.
func NewWindowedQualityProvider(rec *quality.Recorder, parts windowSettledBackend) func(time.Time) scheduler.WindowedQuality {
	return scheduler.NewWindowedQualitySource(
		&partitionWindowSettled{parts: parts},
		&recorderWindowLive{rec: rec},
	)
}
