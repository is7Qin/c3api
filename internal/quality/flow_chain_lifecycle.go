// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

import (
	"errors"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

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
	if termCount != 1 || c.recorder == nil {
		if termCount != 1 {
			return errors.New("exactly one terminal required")
		}
		return errors.New("no recorder")
	}
	rows := make([]repository.RoutingFlowRow, c.count)
	for i := 0; i < c.count; i++ {
		row := c.rows[i]
		row.TerminalMinute = time.Unix(c.terminalMinute, 0).UTC()
		row.IdentityVersion = int16(domain.RoutingIdentityVersion)
		rows[i] = row
	}
	if c.recorder.flow.Submit(c.terminalMinute, rows) != SubmitAccepted {
		flowChainEnqueueOverflow.Add(1)
	}
	c.completed = true
	return nil
}

func (c *FlowChain) Finalize() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.completed {
		return errors.New("already completed")
	}
	if c.closed {
		return ErrFlowChainAlreadyClosed
	}
	if c.count == 0 {
		return ErrFlowChainNotTerminal
	}
	if !c.hasTerminal {
		c.rows[c.count-1].IsTerminal = true
		c.meta[c.count-1].IsTerminal = true
		c.hasTerminal = true
		c.terminalMinute = c.now().UTC().Truncate(time.Minute).Unix()
	}
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
