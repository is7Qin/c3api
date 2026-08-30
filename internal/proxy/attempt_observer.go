// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"errors"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/quality"
	"github.com/is7qin/c3api/internal/rule"
)

var (
	ErrAttemptObserverNotDispatched = errors.New("attempt observer: outcome is not dispatched")
	ErrAttemptObserverHealthKind    = errors.New("attempt observer: invalid health kind")
)

type AttemptHealthEvent struct {
	Kind         rule.Kind
	ResetAt      *time.Time
	ErrorMessage string
	Retryable    bool
}

type AttemptHealthMark func(AttemptOutcome, AttemptHealthEvent)
type AttemptFlowAppend func(AttemptOutcome)
type AttemptLeaseRelease func()

type AttemptObserver struct {
	quality    *quality.AttemptContext
	markHealth AttemptHealthMark
	appendFlow AttemptFlowAppend
	release    AttemptLeaseRelease
	completed  atomic.Bool
}

func NewAttemptObserver(qualityContext *quality.AttemptContext, markHealth AttemptHealthMark, appendFlow AttemptFlowAppend, release AttemptLeaseRelease) *AttemptObserver {
	return &AttemptObserver{
		quality:    qualityContext,
		markHealth: markHealth,
		appendFlow: appendFlow,
		release:    release,
	}
}

func (o *AttemptObserver) Complete(outcome AttemptOutcome, health *AttemptHealthEvent) error {
	if err := outcome.Validate(); err != nil {
		return err
	}
	if !outcome.IsDispatched() {
		return ErrAttemptObserverNotDispatched
	}
	if outcome.Result != ResultClientCancel && health != nil && !validHealthKind(health.Kind) {
		return ErrAttemptObserverHealthKind
	}
	if !o.completed.CompareAndSwap(false, true) {
		return nil
	}
	defer o.releaseOnce()

	if outcome.Result == ResultClientCancel {
		if o.quality != nil {
			o.quality.Cancel()
		}
	} else if observation, ok := AdaptOutcomeToObservation(outcome); ok && o.quality != nil {
		o.quality.CompleteObservation(observation)
	}
	if o.markHealth != nil && health != nil && outcome.IsCountedForQuality() {
		healthCopy := *health
		healthCopy.Retryable = CanRetry(outcome.CallerCategory, outcome)
		o.markHealth(outcome, healthCopy)
	}
	if o.appendFlow != nil {
		o.appendFlow(outcome)
	}
	return nil
}

func (o *AttemptObserver) Cancel(outcome AttemptOutcome) error {
	if outcome.Result != ResultClientCancel {
		return ErrAttemptObserverNotDispatched
	}
	return o.Complete(outcome, nil)
}

// Abandon is the deferred owner cleanup for a dispatch that ended without a
// terminal outcome (local reject after reservation, panic mid-call). It is not
// an observation: no quality attempt, no flow record, no health, no release —
// it only releases the AttemptContext pin taken at dispatch time so the
// recorder's bounded inflight cannot leak.
func (o *AttemptObserver) Abandon() {
	if o == nil || !o.completed.CompareAndSwap(false, true) {
		return
	}
	if o.quality != nil {
		o.quality.Cancel()
	}
}

func (o *AttemptObserver) releaseOnce() {
	if o.release != nil {
		o.release()
	}
}

func validHealthKind(kind rule.Kind) bool {
	switch kind {
	case rule.KindOK, rule.Kind429, rule.Kind4xx, rule.Kind5xx, rule.KindNetwork:
		return true
	default:
		return false
	}
}
