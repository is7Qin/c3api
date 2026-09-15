// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"context"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/latch"
	"github.com/is7qin/c3api/internal/rule"
)

// LatchSink 是规则引擎的 typed health action 本地宿（B18/B19 根因重开）：
// Throttle 直写 RuntimeHealth；FailAccount 只做 TryAcquire + Hub.Dispatch，
// 不碰 sched view、不调 sched.FailAccount——内存摘除经 Hub 同步扇出到
// Scheduler.onRuleFailure（同协程，零异步窗口）。
//
// 位置说明：语义上它属于 latch 中介，但 Go 禁止 import 环——latch 包不能
// 引用 *scheduler.RuntimeHealth（scheduler 反向持有 *latch.LatchStore）。
// 故宿体落本包，构造仍由 main 一次性完成（latch→hub→sink 序），无回填。
type LatchSink struct {
	Health *RuntimeHealth
	Latch  *latch.LatchStore
	Hub    *latch.Hub
}

// NewLatchSink 构造规则引擎本地宿（main 恰好调用一次；Health/Hub 为构造契约，
// 调用 FailAccount 前必须非 nil——装配序保证，见 cmd/server/main.go）。
func NewLatchSink(h *RuntimeHealth, l *latch.LatchStore, hub *latch.Hub) *LatchSink {
	return &LatchSink{Health: h, Latch: l, Hub: hub}
}

var _ rule.HealthSink = (*LatchSink)(nil)

// Throttle typed 限流动作：account 作用全 RouteClass wildcard；account_route
// 作用单 RouteClass。health nil = 跳过（测试/纯选号无健康面）。
func (s *LatchSink) Throttle(ev rule.Event, th domain.ThrottleAction) error {
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
		return nil
	}
	key := HealthKey{AccountID: ev.AccountID, Revision: ev.ExpectedRevision}
	if th.Scope == domain.ThrottleScopeAccount {
		key.Quality = "*"
	} else if th.Scope == domain.ThrottleScopeAccountRoute {
		if ev.RouteClassID == "" || ev.QualityClassID == "" {
			return nil
		}
		key.Quality = ev.QualityClassID
	} else {
		return nil
	}
	if s.Health != nil {
		if _, err := s.Health.Throttle(context.Background(), key, state, ttl); err != nil {
			return err
		}
	}
	return nil
}

// FailAccount typed 失效动作：revision/指纹缺失 fail-closed（先于 persist 的
// 第一道门——引擎内 sink 调用先于 enqueuePersist）；锁存后经 Hub 同步扇出，
// fence（revision/指纹双检查）与内存摘除在 Scheduler.onRuleFailure 内执行。
func (s *LatchSink) FailAccount(ev rule.Event) error {
	if ev.ExpectedRevision <= 0 {
		return ErrMissingExpectedRevision
	}
	fp := ev.CandidateFingerprint
	if fp == "" {
		return ErrMissingCandidateFingerprint
	}
	if s.Latch != nil {
		s.Latch.TryAcquire(ev.AccountID, fp, ev.ExpectedRevision)
	}
	s.Hub.Dispatch(ev)
	return nil
}

// onRuleFailure 是 Hub 订阅回调（New 内订阅，首个 MarkResult 前完成）：fence
// fail-closed（revision/指纹不对即丢，不摘除）+ 快照内存摘除。persist func 内的
// 第二重 fence（NewRulePersistFunc）保持——双 fail-closed，比旧单点更严。
func (s *Scheduler) onRuleFailure(ev rule.Event) {
	if v := s.View(); v != nil {
		if as, ok := v.Account(ev.AccountID); ok {
			av := as.static.Load()
			if av.acc.LifecycleRevision != ev.ExpectedRevision {
				return
			}
			current, err := candidateFingerprint(&av.acc)
			if err != nil {
				return
			}
			if ev.CandidateFingerprint == "" || ev.CandidateFingerprint != current {
				return
			}
		}
	}
	s.FailAccount(ev.AccountID)
}
