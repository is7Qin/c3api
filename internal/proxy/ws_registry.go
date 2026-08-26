// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"sync"

	"github.com/coder/websocket"
)

// wsRegistry tracks hijacked WS client connections so graceful shutdown can
// CloseNow them deterministically. Register/unregister never held across I/O,
// zero contention on the frame-relay hot path.
type wsRegistry struct {
	mu    sync.Mutex
	conns map[*websocket.Conn]struct{}
}

func newWSRegistry() *wsRegistry {
	return &wsRegistry{conns: make(map[*websocket.Conn]struct{})}
}

func (r *wsRegistry) add(c *websocket.Conn) {
	r.mu.Lock()
	r.conns[c] = struct{}{}
	r.mu.Unlock()
}

func (r *wsRegistry) remove(c *websocket.Conn) {
	r.mu.Lock()
	delete(r.conns, c)
	r.mu.Unlock()
}

// closeAll snapshots the set under lock then CloseNow outside lock.
// 停机尽力而为：单连接失败不阻断其余；错误丢弃（会话侧自有 classify 收尾）。
func (r *wsRegistry) closeAll() {
	r.mu.Lock()
	snap := make([]*websocket.Conn, 0, len(r.conns))
	for c := range r.conns {
		snap = append(snap, c)
	}
	r.mu.Unlock()
	for _, c := range snap {
		_ = c.CloseNow()
	}
}

func (p *Proxy) registerWS(c *websocket.Conn) {
	if p.wsConns != nil {
		p.wsConns.add(c)
	}
}

func (p *Proxy) unregisterWS(c *websocket.Conn) {
	if p.wsConns != nil {
		p.wsConns.remove(c)
	}
}
