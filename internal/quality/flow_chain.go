// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

const flowChainCap = 8

var (
	ErrFlowChainOverflow        = errors.New("flow chain overflow: ninth dispatch")
	ErrFlowChainTerminalAlready = errors.New("flow chain already terminal")
	ErrFlowChainNotTerminal     = errors.New("flow chain not terminal")
	ErrFlowChainAlreadyClosed   = errors.New("flow chain already closed")
)

var flowChainCapacityOverflow atomic.Int64
var flowChainEnqueueOverflow atomic.Int64
var flowChainIncomplete atomic.Int64

const processCrashLossUnobservable = true

func FlowChainCapacityOverflow() int64 { return flowChainCapacityOverflow.Load() }
func FlowChainEnqueueOverflow() int64  { return flowChainEnqueueOverflow.Load() }
func FlowChainIncompleteObserved() int64 { return flowChainIncomplete.Load() }
func ProcessCrashLossUnobservable() bool { return processCrashLossUnobservable }

func ResetFlowChainCountersForTest() {
	flowChainCapacityOverflow.Store(0)
	flowChainEnqueueOverflow.Store(0)
	flowChainIncomplete.Store(0)
}

type FlowDispatch struct {
	RouteClassID      string
	QualityClassID    string
	Fingerprint       string
	TemplateID        int64
	AccountID         int64
	RequestedModel    string
	MappedModel       string
	Generation        int64
	LifecycleRevision int64
	Ordinal           uint8
	Lane              string
	PreviousAttemptID *string
	PreviousAccountID *int64
	PreviousOutcome   string
	TransitionReason  string
	Outcome           string
	IsTerminal        bool
}

func idBytesFromString(s string) [32]byte {
	if len(s) == 64 {
		if b, err := hex.DecodeString(s); err == nil && len(b) == 32 {
			var out [32]byte
			copy(out[:], b)
			return out
		}
	}
	h := sha256.Sum256([]byte(s))
	return h
}

type FlowChain struct {
	mu             sync.Mutex
	recorder       *Recorder
	now            func() time.Time
	rows           [flowChainCap]repository.RoutingFlowRow
	meta           [flowChainCap]FlowDispatch
	count          int
	hasTerminal    bool
	terminalMinute int64
	completed      bool
	closed         bool
}

func NewFlowChain(recorder *Recorder, now func() time.Time) *FlowChain {
	if now == nil {
		now = time.Now
	}
	return &FlowChain{recorder: recorder, now: now}
}

func (c *FlowChain) Append(d FlowDispatch) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.completed {
		return errors.New("flow chain already completed")
	}
	if c.closed {
		return ErrFlowChainAlreadyClosed
	}
	if c.count >= flowChainCap {
		flowChainCapacityOverflow.Add(1)
		return ErrFlowChainOverflow
	}
	if c.hasTerminal {
		return ErrFlowChainTerminalAlready
	}
	if d.RouteClassID == "" || d.QualityClassID == "" || d.Fingerprint == "" {
		return errors.New("route/quality/fingerprint required")
	}
	if d.TemplateID == 0 {
		return errors.New("template required")
	}
	if d.RequestedModel == "" || d.MappedModel == "" {
		return errors.New("model required")
	}
	if d.Generation <= 0 || d.LifecycleRevision <= 0 {
		return errors.New("generation/revision must be >0")
	}
	if d.Ordinal == 0 {
		return errors.New("ordinal must be >0")
	}
	if int(d.Ordinal) != c.count+1 {
		return errors.New("ordinal must be sequential")
	}
	if d.Lane == "" {
		return errors.New("lane required")
	}
	if d.TransitionReason == "" {
		return errors.New("transition required")
	}
	if d.Outcome == "" {
		return errors.New("outcome required")
	}
	if d.Ordinal == 1 && d.PreviousAttemptID != nil {
		return errors.New("previous linkage must be nil for ordinal 1")
	}
	if d.Ordinal > 1 && (d.PreviousAttemptID == nil || *d.PreviousAttemptID == "") {
		return errors.New("previous linkage required for ordinal >1")
	}
	// Build compact row per dispatch; preserve all dispatch metadata in meta, and project to storage row.
	routeBytes := idBytesFromString(d.RouteClassID)
	fpBytes := idBytesFromString(d.Fingerprint)
	row := repository.RoutingFlowRow{
		IdentityVersion:      int16(domain.RoutingIdentityVersion),
		RouteClassID:         domain.RouteClassIDVal(routeBytes),
		Ordinal:              int16(d.Ordinal),
		Lane:                 d.Lane,
		AccountID:            d.AccountID,
		PreviousAccountID:    d.PreviousAccountID,
		PreviousOutcome:      d.PreviousOutcome,
		TransitionReason:     d.TransitionReason,
		Outcome:              d.Outcome,
		IsTerminal:           d.IsTerminal,
		Generation:           d.Generation,
		CandidateFingerprint: domain.CandidateFingerprintVal(fpBytes),
		ChainCount:           1,
	}
	// Qualities template/model/quality retained in meta for completeness; row stores route/fingerprint/generation
	c.rows[c.count] = row
	c.meta[c.count] = d
	c.count++
	if d.IsTerminal {
		c.hasTerminal = true
		c.terminalMinute = c.now().UTC().Truncate(time.Minute).Unix()
	}
	return nil
}

func (c *FlowChain) DispatchCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.count
}

func (c *FlowChain) HasTerminal() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hasTerminal
}

func (c *FlowChain) TerminalMinute() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.terminalMinute
}

func (c *FlowChain) Complete() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.completed {
		return errors.New("already completed")
	}
	if c.closed {
		return ErrFlowChainAlreadyClosed
	}
	if !c.hasTerminal {
		return ErrFlowChainNotTerminal
	}
	if c.count == 0 {
		return errors.New("empty chain")
	}
	termCount := 0
	for i := 0; i < c.count; i++ {
		if c.rows[i].IsTerminal {
			termCount++
			if i != c.count-1 {
				return errors.New("terminal must be last")
			}
		}
	}
	if termCount != 1 {
		return errors.New("exactly one terminal required")
	}
	if c.recorder == nil {
		return errors.New("no recorder")
	}
	rows := make([]repository.RoutingFlowRow, c.count)
	for i := 0; i < c.count; i++ {
		r := c.rows[i]
		r.TerminalMinute = time.Unix(c.terminalMinute, 0).UTC()
		r.IdentityVersion = int16(domain.RoutingIdentityVersion)
		rows[i] = r
	}
	fm := NewFlowSnapshot(c.terminalMinute, rows)
	if err := c.recorder.EnqueueFlowMinute(fm); err != nil {
		if errors.Is(err, ErrCapacity) {
			flowChainEnqueueOverflow.Add(1)
		}
		return err
	}
	c.completed = true
	return nil
}

func (c *FlowChain) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed || c.completed {
		c.closed = true
		return
	}
	c.closed = true
	if !c.hasTerminal {
		flowChainIncomplete.Add(1)
	}
}

func (c *FlowChain) IsCompleted() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.completed
}

func (c *FlowChain) IsClosed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}
