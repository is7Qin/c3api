// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
)

type HealthController struct {
	health *RuntimeHealth
	latch  *latchStore
	sched  *Scheduler
}

func NewHealthController(h *RuntimeHealth, latch *latchStore) *HealthController {
	if latch == nil {
		latch = newLatchStore()
	}
	return &HealthController{health: h, latch: latch}
}

func NewHealthControllerWithScheduler(h *RuntimeHealth, s *Scheduler) *HealthController {
	if s == nil {
		return NewHealthController(h, nil)
	}
	return &HealthController{health: h, latch: s.latch, sched: s}
}

func (c *HealthController) Throttle(ev rule.Event, th domain.ThrottleAction) {
	var state HealthState
	var ttl time.Duration
	switch th.Mode {
	case domain.ThrottleModeOpen:
		state = StateOPEN
		if th.DurationMs != nil {
			ttl = time.Duration(*th.DurationMs) * time.Millisecond
		} else {
			ttl = 30 * time.Second
		}
	case domain.ThrottleModeRetryAfter:
		state = StateRetryAfter
		if th.UseReset && ev.ResetAt != nil {
			d := time.Until(*ev.ResetAt)
			if d > 0 {
				ttl = d
			} else {
				ttl = time.Second
			}
		} else if th.DurationMs != nil {
			ttl = time.Duration(*th.DurationMs) * time.Millisecond
		} else {
			ttl = 30 * time.Second
		}
	default:
		return
	}
	key := HealthKey{AccountID: ev.AccountID, Revision: ev.ExpectedRevision}
	if th.Scope == domain.ThrottleScopeAccount {
		key.Quality = "*"
	} else if th.Scope == domain.ThrottleScopeAccountRoute {
		if ev.RouteClassID == "" || ev.QualityClassID == "" {
			return
		}
		key.Quality = ev.QualityClassID
	} else {
		return
	}
	if c.health != nil {
		_, _ = c.health.Throttle(context.Background(), key, state, ttl)
	}
}

func (c *HealthController) FailAccount(ev rule.Event) {
	fp := ""
	if c.sched != nil {
		if snap, ok := c.sched.store.byID.Load().(map[int64]*accountSnapshot); ok {
			if as, ok := snap[ev.AccountID]; ok {
				fp = accountFingerprint(&as.static.Load().acc)
			}
		}
	}
	if fp == "" {
		fp = ev.QualityClassID
		if fp == "" {
			fp = ev.RouteClassID
		}
	}
	if c.latch != nil {
		c.latch.TryAcquire(ev.AccountID, fp, ev.ExpectedRevision)
	}
	if c.sched != nil {
		c.sched.FailAccount(ev.AccountID, ev.ErrorMessage)
	}
}

func (c *HealthController) IsLatched(accountID int64, fingerprint string) bool {
	if c.latch == nil {
		return false
	}
	return c.latch.IsLatched(accountID, fingerprint)
}

func (c *HealthController) ClearOnRevision(accountID int64, currentRevision int64) {
	if c.latch != nil {
		c.latch.ClearIfRevisionGreater(accountID, currentRevision)
	}
}

func (c *HealthController) ClearOnFingerprint(accountID int64, currentFingerprint string) {
	if c.latch != nil {
		c.latch.ClearIfFingerprintChanged(accountID, currentFingerprint)
	}
}

func (c *HealthController) LatchStore() *latchStore { return c.latch }
