// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7qin.

package scheduler

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRuntimeHealthAccountHealthAggregatesWorstTier 钉住账号级冷却聚合（spec
// 2026-11-14 §3.1）的三条语义：
//  1. State 取该账号所有健康键里**最严重**的一档（open > retry_after > probing > ready）；
//  2. Until 取**该档**里最早的到期时刻——不是跨档取最小值（否则徽章会把
//     retry_after 的早到期时刻安到 open 头上，「最严重档何时恢复」就答错了）；
//  3. Routes 数**该档**的键数；无该账号的任何 entry ⇒ ok=false（「无记录」≠ READY）。
func TestRuntimeHealthAccountHealthAggregatesWorstTier(t *testing.T) {
	h := NewRuntimeHealth(nil, "self", nil, nil)

	base := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	openEarly := HealthKey{AccountID: 7, Quality: "q1", Identity: "i1", IdentityRevision: 3}
	openLate := HealthKey{AccountID: 7, Quality: "q2", Identity: "i2", IdentityRevision: 3}
	retrySoon := HealthKey{AccountID: 7, Quality: "*", Identity: "*", IdentityRevision: 3}
	probeOther := HealthKey{AccountID: 8, Quality: "*", Identity: "*", IdentityRevision: 1}

	h.view.Store(&healthView{entries: map[HealthKey]healthEntry{
		openEarly:  {Key: openEarly, State: StateOPEN, ExpiresAt: base.Add(30 * time.Minute).UnixMilli()},
		openLate:   {Key: openLate, State: StateOPEN, ExpiresAt: base.Add(90 * time.Minute).UnixMilli()},
		retrySoon:  {Key: retrySoon, State: StateRetryAfter, ExpiresAt: base.Add(5 * time.Minute).UnixMilli()},
		probeOther: {Key: probeOther, State: StateProbing, ExpiresAt: base.Add(time.Minute).UnixMilli()},
	}})

	got, ok := h.AccountHealth(7)
	require.True(t, ok)
	require.Equal(t, "open", got.State, "最严重档优先于更早到期的较轻档")
	require.Equal(t, 2, got.Routes, "Routes 数最严重档的键数（两个 open 键）")
	require.Equal(t, base.Add(30*time.Minute), got.Until.UTC(), "Until 取该档最早到期，而非 retry_after 的 5min")
	require.True(t, got.Until.Equal(base.Add(30*time.Minute)))

	// 另一账号只看自己的键（探针档 + 1min），互不串档。
	other, ok := h.AccountHealth(8)
	require.True(t, ok)
	require.Equal(t, "probing", other.State)
	require.Equal(t, 1, other.Routes)
	require.Equal(t, base.Add(time.Minute), other.Until.UTC())

	// 无 entry ⇒ false（前端按「无冷却」处理；这与「READY 记录存在」不同）。
	_, ok = h.AccountHealth(9)
	require.False(t, ok)

	// 视图未加载 ⇒ false（启动中/首刷失败不 panic）。
	empty := NewRuntimeHealth(nil, "self", nil, nil)
	_, ok = empty.AccountHealth(7)
	require.False(t, ok)
}

// TestSchedulerAccountHealthForwarding 转发面：未装配健康投影（health == nil）
// 时返回 false，而不是 panic 或编造一个 ready。
func TestSchedulerAccountHealthForwarding(t *testing.T) {
	s := &Scheduler{}
	_, ok := s.AccountHealth(1)
	require.False(t, ok)

	var nilSched *Scheduler
	_, ok = nilSched.AccountHealth(1)
	require.False(t, ok)

	// 装配健康投影后转发到 RuntimeHealth（同账号多键 ⇒ 同一聚合口径）。
	h := NewRuntimeHealth(nil, "self", nil, nil)
	hk := HealthKey{AccountID: 5, Quality: "*", Identity: "*", IdentityRevision: 1}
	exp := time.Date(2026, 11, 14, 13, 0, 0, 0, time.UTC)
	h.view.Store(&healthView{entries: map[HealthKey]healthEntry{
		hk: {Key: hk, State: StateRetryAfter, ExpiresAt: exp.UnixMilli()},
	}})
	s.health = h
	got, ok := s.AccountHealth(5)
	require.True(t, ok)
	require.Equal(t, "retry_after", got.State)
	require.Equal(t, exp, got.Until.UTC())
}
