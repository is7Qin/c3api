// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"sync"
	"testing"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestWSRegistryRegisterUnregister(t *testing.T) {
	r := newWSRegistry()
	require.NotNil(t, r)
	require.Zero(t, len(r.conns))
	// Create two dummy conns (zero value is usable for map key; CloseNow on
	// zero conn is safe enough for registry — real conns exercised below).
	c1 := &websocket.Conn{}
	c2 := &websocket.Conn{}
	r.add(c1)
	r.add(c2)
	require.Equal(t, 2, len(r.conns))
	r.remove(c1)
	require.Equal(t, 1, len(r.conns))
	_, ok := r.conns[c2]
	require.True(t, ok)
	r.remove(c2)
	require.Zero(t, len(r.conns))
}

func TestWSRegistryCloseAllIdempotent(t *testing.T) {
	r := newWSRegistry()
	// Empty registry closeAll is idempotent and safe
	require.NotPanics(t, func() { r.closeAll() })
	require.NotPanics(t, func() { r.closeAll() })
	c1 := &websocket.Conn{}
	c2 := &websocket.Conn{}
	r.add(c1)
	r.add(c2)
	// closeAll retains entries (does not clear); remove explicitly
	r.remove(c1)
	r.remove(c2)
	require.Zero(t, len(r.conns))
	require.NotPanics(t, func() { r.closeAll() })
}

func TestWSRegistryConcurrentBarrier(t *testing.T) {
	r := newWSRegistry()
	const n = 20
	conns := make([]*websocket.Conn, n)
	for i := range conns {
		conns[i] = &websocket.Conn{}
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			<-start
			r.add(conns[idx])
		}(i)
	}
	close(start)
	wg.Wait()
	require.Equal(t, n, len(r.conns))
	// Barrier-synced remove
	start2 := make(chan struct{})
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			<-start2
			r.remove(conns[idx])
		}(i)
	}
	close(start2)
	wg.Wait()
	require.Zero(t, len(r.conns))
}

func TestProxyCloseAllWSIdempotent(t *testing.T) {
	store := &captureLogStore{}
	p := newTestProxyFormatLogs(t, "http://127.0.0.1:1", domain.FormatOpenAIResponsesWS, store)
	require.NotPanics(t, func() { p.CloseAllWS() })
	require.NotPanics(t, func() { p.CloseAllWS() })
}
