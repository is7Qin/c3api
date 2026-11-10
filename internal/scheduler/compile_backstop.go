// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// Staleness backstop probe (v5): the demoted 30s tick FIRST
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
// transactions sharing no snapshot — a baseline read that precedes the
// load can only ever be stale-or-equal to what the load sees, so a commit
// landing between the two causes at most one redundant reload, never a miss.
// Refreshing after the load would open a miss window (baseline ahead of the
// staged root). Nil probe (unwired): baseline untouched. Query error: baseline
// untouched AND the sticky reloadRequired obligation is set (r8 §:147) so a
// later healthy tick retries even though the probe-hit gate cannot see the
// failure. The caller must hold publisher.mu (reloadRequired discipline).
func (s *Scheduler) refreshProbeBaseline(ctx context.Context) {
	if s.stalenessProbe == nil {
		return
	}
	c, err := s.stalenessProbe(ctx)
	if err != nil {
		s.reloadRequired = true
		if s.log != nil {
			s.log.Warn("compile staleness baseline refresh failed; keeping previous baseline", logx.Error(err))
		}
		return
	}
	s.lastProbe.Store(&c)
}

// backstopTick is the demoted 30s tick: FIRST the O(1) probe, full
// work ONLY on mismatch — plus a forced full path while the published view is
// partial (probe-hit skips apply to whole views only, v5 §5.2 wholeness bit).
// Probe hit on a whole view = return with zero rebuild, zero compile,
// zero serialization. The unconditional reload-on-every-tick default path is
// DELETED outright (no flag, no dual-track). Staleness SLO: the
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
		// Fail-safe full reload under publisher.mu; reloadLocked itself sets the
		// sticky obligation on failure (single error source), so a subsequent
		// healthy tick retries (probe errors leave the baseline untouched, so
		// the retry is not probe-gated).
		s.withPublisherLock(func(reclaim *poolReclaim) {
			if rerr := s.reloadLocked(ctx, reclaim); rerr != nil && s.log != nil {
				s.log.Warn("scheduler sync failed", logx.Error(rerr))
			}
		})
		return
	}
	// Decide-and-rebuild under ONE publisher.mu hold: the skip check must
	// observe reloadRequired atomically with the published root, else a failed
	// load that is probe-invisible (equal-count membership swap) could be
	// skipped forever. probe-before-load is preserved (refresh-first baseline
	// never runs ahead of a successful load).
	s.withPublisherLock(func(reclaim *poolReclaim) {
		if last := s.lastProbe.Load(); last != nil && *last == c && !s.reloadRequired && s.publishedViewWhole() {
			// Probe-hit skip applies ONLY to whole views. A partial (scoped-carry)
			// view may carry forward holes from its carry, and DB-quiet would
			// otherwise freeze them forever — fall through to the full path
			// instead. This is NOT a periodic full recompile: whole views skip.
			//
			// 跳过的只是**配置重载**，不是资源回收义务：请求侧可能在上一批
			// `Accounts()` 快照之后、批次应用之前为已移除账号建出条目，而删除指令
			// 只能由全集差分产生。静默期因此仍需一次"点名机会"：用内存里**已有的**
			// 账号全集（不重扫 DB、不做 full reload）+ 资源表**新鲜**的 `Accounts()`
			// 快照做差分——表仍只执行指令，差分与策略仍在本侧。
			//
			// **没有差异就不取号、不下发**：静默是常态（30s 一 tick），无条件取号
			// 会让代号每 tick 空转一次、并反复下发内容相同的保留指令。每次都重新
			// 读 `Accounts()` 再判，所以迟到资源不会被永久漏掉（不记"回收过就不查"）。
			//
			// 取号在本锁内（保持 Gen 单调），应用在锁外（reclaim.run，与既有回收
			// 路径同一批次、同一顺序）。未装配资源表 ⇒ 无资源可回收，不取号。
			if held := s.clientAccounts(); s.accounts.hasDropped(held) {
				cfg := s.accounts.issue(s.accounts.universe, held)
				reclaim.config = &cfg
			}
			return
		}
		if rerr := s.reloadLocked(ctx, reclaim); rerr != nil && s.log != nil {
			s.log.Warn("scheduler sync failed", logx.Error(rerr))
		}
	})
}
