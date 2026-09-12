// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// Staleness backstop probe (v5 C1-backstop/C3): the demoted 30s tick FIRST
// runs this O(1) aggregate compare and does full work ONLY on mismatch. Owner:
// compile lane (baseline) / fire (result). Lifecycle: baseline persists
// across ticks (atomic); each probe result dies at return.

// compileProbeCounts is the §4 counter tuple: accounts COUNT +
// MAX(lifecycle_revision) + MAX(updated_at), groups/templates COUNT +
// MAX(updated_at), membership-count-only/exts COUNT. Timestamps ride as
// UnixNano int64 (exact == comparison). The lane never queries for this — the
// repository owns the single §9-A1 aggregate pass (GroupRepo.
// CompileStalenessSnapshot) and the lane maps it 1:1 below.
type compileProbeCounts struct {
	accounts    int64
	maxRev      int64
	accUpdated  int64
	groups      int64
	grpUpdated  int64
	templates   int64
	tplUpdated  int64
	memberships int64
	exts        int64
}

// stalenessQuerier supplies the §9-A1 aggregate tuple. Production wiring passes
// the repository GroupRepo; tests pass a fake. No SQL string and no DB handle
// ever cross this seam — the query lives beside LoadGroupsAccounts.
type stalenessQuerier interface {
	CompileStalenessSnapshot(ctx context.Context) (domain.CompileStaleness, error)
}

// snapshotToProbeCounts maps the repository tuple onto the lane tuple 1:1
// (field-exact, no derivation — the backstop compares, never computes).
func snapshotToProbeCounts(s domain.CompileStaleness) compileProbeCounts {
	return compileProbeCounts{
		accounts:    s.Accounts,
		maxRev:      s.MaxLifecycleRevision,
		accUpdated:  s.AccountsUpdatedAtNano,
		groups:      s.Groups,
		grpUpdated:  s.GroupsUpdatedAtNano,
		templates:   s.Templates,
		tplUpdated:  s.TemplatesUpdatedAtNano,
		memberships: s.Memberships,
		exts:        s.Exts,
	}
}

// SetStalenessProbe wires the C1 backstop probe to a repository-backed tuple
// supplier (production: repos.Groups, passed once at the cmd/server
// construction site). Nil supplier = no-op (scheduler stays unwired and the
// backstop keeps its fail-safe full reload); this is the single exported seam
// symbol authorized by §9-A2. Assembly-time only (before Start).
func (s *Scheduler) SetStalenessProbe(q stalenessQuerier) {
	if q == nil {
		return
	}
	s.stalenessProbe = func(ctx context.Context) (compileProbeCounts, error) {
		snap, err := q.CompileStalenessSnapshot(ctx)
		if err != nil {
			return compileProbeCounts{}, err
		}
		return snapshotToProbeCounts(snap), nil
	}
}

// publishedViewWhole reports whether the published decision is whole (every
// route recomputed by a full fire). A missing view or missing decision counts
// as not-whole. Lock-free: DecisionView is immutable after publish. Owner:
// compile lane (publisher pair); lifecycle: born at publish, superseded by
// the next publish.
func (s *Scheduler) publishedViewWhole() bool {
	v := s.view.Load()
	return v != nil && v.decision != nil && v.decision.whole
}

// refreshProbeBaseline advances the tick baseline BEFORE the stage load
// (refresh-first ordering). Rationale: probe and reload are separate
// transactions sharing no snapshot (v5-C3) — a baseline read that precedes the
// load can only ever be stale-or-equal to what the load sees, so a commit
// landing between the two causes at most one redundant reload, never a miss.
// Refreshing after the load would open a miss window (baseline ahead of the
// staged root). Nil probe (unwired) or query error: baseline untouched,
// fail-safe Warn + old-view retention on every error path.
func (s *Scheduler) refreshProbeBaseline(ctx context.Context) {
	if s.stalenessProbe == nil {
		return
	}
	c, err := s.stalenessProbe(ctx)
	if err != nil {
		if s.log != nil {
			s.log.Warn("compile staleness baseline refresh failed; keeping previous baseline", logx.Error(err))
		}
		return
	}
	s.lastProbe.Store(&c)
}

// backstopTick is the demoted 30s tick (v5-C1): FIRST the O(1) probe, full
// work ONLY on mismatch — plus a forced full path while the published view is
// partial (probe-hit skips apply to whole views only, v5 §5.2 wholeness bit).
// Probe hit on a whole view = return with zero rebuild, zero compile,
// zero serialization. The unconditional reload-on-every-tick default path is
// DELETED outright (no flag, no dual-track). Staleness SLO (v5-C3): the
// published view is never older than 2×SyncInterval + NOTIFY latency even if
// all events are lost — probe and reload share no snapshot, so a commit
// landing between them delays detection a full period: worst case two periods,
// not one (TOCTOU closed by bound, not mechanism). Reconnect recovery stays
// the existing FullRefresh (dispatcher → registry ReloadAll), preserved, not
// reimplemented. request/tick boundary: the tick never touches request state.
func (s *Scheduler) backstopTick(ctx context.Context) {
	if s.stalenessProbe == nil {
		// Probe unwired: fail-safe full reload preserves the SLO by
		// construction (fresh every tick ≤ 2×SyncInterval). Production wires
		// the repository-backed probe at the construction site (§9-A2); the
		// event path already carries scope without it.
		if err := s.reload(ctx); err != nil && s.log != nil {
			s.log.Warn("scheduler sync failed", logx.Error(err))
		}
		return
	}
	c, err := s.stalenessProbe(ctx)
	if err != nil {
		if s.log != nil {
			s.log.Warn("compile staleness probe failed; full reload fail-safe", logx.Error(err))
		}
		s.recordCompileFallback("probe-error", 0, 0, 0)
		if rerr := s.reload(ctx); rerr != nil && s.log != nil {
			s.log.Warn("scheduler sync failed", logx.Error(rerr))
		}
		return
	}
	if last := s.lastProbe.Load(); last != nil && *last == c && s.publishedViewWhole() {
		// Probe-hit skip applies ONLY to whole views. A partial (scoped-carry)
		// view may carry forward holes from its carry, and DB-quiet would
		// otherwise freeze them forever — fall through to the full path
		// instead. This is NOT a periodic full recompile: whole views skip.
		return
	}
	if err := s.reload(ctx); err != nil && s.log != nil {
		s.log.Warn("scheduler sync failed", logx.Error(err))
	}
}
