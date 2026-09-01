// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"context"
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

func (p *Proxy) normalizedAttempts() int {
	n := p.cfg.FailoverAttempts
	if n <= 0 {
		return 3
	}
	return min(n, 8)
}

// selectWithPlan binds a request to its compiled attempt plan. Every real AI
// dispatch is plan-backed: an absent compiled plan fails closed through the
// typed selection error path (handleSelectError/statusFor), never a second
// selection lane.
func (p *Proxy) selectWithPlan(groupID int64, format domain.RequestFormat, model string, identity scheduler.AttemptPlanIdentity) (*scheduler.Selection, *scheduler.AttemptPlan, error) {
	if p.sched == nil {
		return nil, nil, scheduler.ErrGroupNotFound
	}
	route := scheduler.RouteRefFor(groupID, string(format), model)
	return p.selectWithPlanForRoute(route, groupID, format, model, identity)
}

func (p *Proxy) selectWithPlanForRoute(route scheduler.RouteRef, groupID int64, format domain.RequestFormat, model string, identity scheduler.AttemptPlanIdentity) (*scheduler.Selection, *scheduler.AttemptPlan, error) {
	if p.sched == nil {
		return nil, nil, scheduler.ErrGroupNotFound
	}
	identity.MaxAttempts = uint8(p.normalizedAttempts())
	plan, err := p.sched.NewAttemptPlan(identity, route)
	if err != nil {
		return nil, nil, err
	}
	sel, err := p.reservePlanAttempt(plan)
	if err != nil {
		return nil, plan, err
	}
	return sel, plan, nil
}

// reservePlanAttempt advances one plan dispatch and enforces the canonical
// identity contract: a reserved Attempt that fails validation (e.g. a
// model-less request resolving the default bucket) carries no usable
// route/quality/model identity, so the lease is released and the selection
// fails closed as format-unavailable — never dispatched under a fabricated
// identity.
func (p *Proxy) reservePlanAttempt(plan *scheduler.AttemptPlan) (*scheduler.Selection, error) {
	sel, attempt, err := p.sched.ReserveAttempt(plan)
	if err != nil {
		return nil, err
	}
	if attempt.Validate() != nil {
		sel.Release()
		return nil, scheduler.ErrFormatUnavailable
	}
	return sel, nil
}

// retryOutcomeForAttempt projects the real plan-canonical attempt identity
// onto the terminal facts of a handled=false classification — the shape the
// typed retry matrix consumes. Identity comes from the scheduler.Attempt
// (never a placeholder); commit/result/status come from the observed code,
// callErr and client context.
func retryOutcomeForAttempt(attempt scheduler.Attempt, code int, callErr error, ctx context.Context) AttemptOutcome {
	clientCancel := code == 0 && ctx != nil && ctx.Err() != nil
	terminal := true
	switch {
	case clientCancel:
	case code == 0 && callErr != nil:
		terminal = false
	case code == http.StatusTooManyRequests:
		terminal = false
	}
	return dispatchFailureOutcome(pipelineBase(attempt), code, clientCancel, terminal)
}

// shouldRetryWithPlan consults the typed retry matrix with the canonical
// identity of the attempt that just ran. Without a plan there is no attempt
// identity and no failover: the plan-less lane does not exist.
func (p *Proxy) shouldRetryWithPlan(ctx context.Context, code int, callErr error, plan *scheduler.AttemptPlan) bool {
	if plan == nil {
		return false
	}
	attempt, ok := plan.CurrentAttempt()
	if !ok {
		return false
	}
	return CanRetry(CallerCategory(attempt.CallerCategory), retryOutcomeForAttempt(attempt, code, callErr, ctx))
}

// selectNextWithPlan advances the request-local plan: a compiled plan's
// verdict is final (ErrAttemptsExhausted / ErrNoAvailable propagate to the
// exhaustion path; reservation rejects consumed no attempt inside
// ReserveAttempt). There is no plan-less fall-through — a nil plan has no
// selection lane and fails closed.
func (p *Proxy) selectNextWithPlan(plan *scheduler.AttemptPlan) (*scheduler.Selection, error) {
	return p.reservePlanAttempt(plan)
}
