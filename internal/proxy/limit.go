// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"sync"
	"sync/atomic"
	"time"
)

// fixedWindowLimiter 每分组 key 的固定窗口计数限流（规格 §10.6，默认关闭）。
// 多实例（#14 §3.2）：本地窗口限额 = ceil(rpm/N)，N 从 InstancesProvider 读
// （窗口建立时读一次——N 变更下次窗口生效，§3.4 偏放行/偏收紧瞬态可接受）。
// 误差上界：每窗口总放行 ≤ N×ceil(rpm/N) ≤ rpm + (N-1)（软保护，无 DB 复核）。
type fixedWindowLimiter struct {
	rpm       int
	instances atomic.Pointer[InstancesProvider] // nil → N=1（单实例/未装配兼容）
	mu        sync.Mutex
	win       map[int64]windowState
}

type windowState struct {
	start time.Time
	count int64
	limit int64 // 本地窗口限额 = ceil(rpm/N)；窗口建立时读 N
}

func newFixedWindowLimiter(rpm int) *fixedWindowLimiter {
	return &fixedWindowLimiter{rpm: rpm, win: make(map[int64]windowState)}
}

// SetInstancesProvider 注入集群实例数 N（装配期；与 Auth 同源——Proxy 统一转发）。
func (l *fixedWindowLimiter) SetInstancesProvider(p InstancesProvider) {
	l.instances.Store(&p)
}

func (l *fixedWindowLimiter) Allow(groupID int64, now time.Time) bool {
	if l.rpm <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	st, ok := l.win[groupID]
	if !ok || now.Sub(st.start) >= time.Minute {
		l.win[groupID] = windowState{start: now, count: 1, limit: l.localLimit()}
		return true
	}
	st.count++
	l.win[groupID] = st
	return st.count <= st.limit
}

// localLimit 本地窗口限额 = ceil(rpm/N)（§3.2 组 RPM 分摊；N 缺失/非法 → 1，
// 单实例等价）。
func (l *fixedWindowLimiter) localLimit() int64 {
	n := int64(1)
	if p := l.instances.Load(); p != nil && *p != nil {
		if v := int64((*p).ClusterInstances()); v > 0 {
			n = v
		}
	}
	return ceilDiv(int64(l.rpm), n)
}
