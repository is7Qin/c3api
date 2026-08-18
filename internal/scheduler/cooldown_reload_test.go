// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
)

// —— 规则冷却不生效修复（2026-08-19 缺陷 1/2） ——

// newSchedCooldownOnly 构造带 cooldown-only 规则（429 → 冷却 5h，无 status/
// weight/transmit）的调度器。种子规则不带入——种子全带 status（punish 恒
// true），测不出 A-1 回归（缺陷 1 正是 cooldown-only 规则永不投递）。
func newSchedCooldownOnly(t *testing.T, m Loader) *Scheduler {
	t.Helper()
	rstore := &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}
	_, err := rstore.CreateRule(context.Background(), domain.Rule{
		Name: "cd-only", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Kind: strPtr("429")},
		Then: domain.RuleThen{Cooldown: strPtr("5h")},
	})
	require.NoError(t, err)
	re := rule.New(rule.Config{}, rstore, nil)
	require.NoError(t, re.Reload(context.Background()))
	s := New(testCfg(), m, re, nil)
	require.NoError(t, s.reload(context.Background()))
	return s
}

// mustCooldown 取账号当前内存冷却（恒非 nil 断言——测试前提：冷却已设置）。
func mustCooldown(t *testing.T, s *Scheduler, id int64) *time.Time {
	t.Helper()
	ri, ok := s.Runtime(id)
	require.True(t, ok)
	require.NotNil(t, ri.CooldownUntil)
	return ri.CooldownUntil
}

// TestCooldownOnlyRuleFullChain 评审 M-3① 全链路（用户场景自动化）：cooldown-
// only 规则 → Classify(429) 判定 punish（A-1）→ MarkResult → apply 设置内存
// 冷却 → Select 拦截 ErrNoAvailable → 回写落库 DB cooldown_until → 重启等价
// 重建后仍拦截。A-1 若回归（punish=false）→ MarkResult 不投递 → Select 放行
// → 本测试失败——现有清单无任何测试经 Classify 验证 cooldown-only，此即护栏。
func TestCooldownOnlyRuleFullChain(t *testing.T) {
	pl := newPersistLoader(map[int64][]*domain.Account{10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)}})
	s := newSchedCooldownOnly(t, pl)

	// Classify(429)：cooldown-only 规则命中 → punish=true（修复前 false）
	tx, pu := s.Classify(rule.Event{AccountID: 1, Kind: rule.Kind429, HTTPStatus: intPtr(429)})
	require.False(t, tx)
	require.True(t, pu, "cooldown-only 规则 punish=true（漏 Cooldown → 冷却静默丢弃）")

	// MarkResult → 规则引擎 apply：st=nil（不碰状态）、冷却 = 事件时刻 + 5h
	s.MarkResult(1, rule.Kind429, nil, 429, "")
	s.FlushRules()
	ri, ok := s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusActive, ri.Status, "cooldown-only 动作不改状态（st=nil）")
	require.NotNil(t, ri.CooldownUntil, "冷却已设置")
	require.WithinDuration(t, time.Now().Add(5*time.Hour), *ri.CooldownUntil, 30*time.Second, "冷却 = 事件时刻 + 5h")

	// Select 拦截（不再打到上游 429 → "no available account 风暴"消失）
	_, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrNoAvailable, "冷却中账号不可调度")

	// 回写落库：DB cooldown_until 有值（回写丢弃/失败之外的正常路径）
	drainWrites(t, s)
	pl.mu.Lock()
	require.NotNil(t, pl.byGroup[10][0].CooldownUntil, "回写落库 DB cooldown_until")
	pl.mu.Unlock()

	// 重启等价：从数据源重建快照（DB 有值 → 同步）→ 冷却仍拦截
	require.NoError(t, s.InvalidateAllSync())
	_, err = s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrNoAvailable, "重建后仍拦截（DB cooldown 同步）")
}

// TestCooldownOnlyPersistReloadPreserved 缺陷 2 修复（回写不持久化路径）：
// memLoader（UpdateAccountStatus 只记录不落数据）→ 冷却生效 → 全量重建
//（≤30s 定时同步/重启快照重载同路径）→ 内存冷却保留——修复前 DB cooldown
// 恒 nil 被重建清零 → 5h 冷却缩水 ≤30s（缺陷 2 第二层）。修复后 DB nil →
// 保留内存值（与 errRate 同款连续性，管理面无清冷却操作、无语义损失）。
func TestCooldownOnlyPersistReloadPreserved(t *testing.T) {
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)}})
	s := newSchedCooldownOnly(t, m)

	s.MarkResult(1, rule.Kind429, nil, 429, "")
	s.FlushRules()
	_, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrNoAvailable, "冷却生效")
	before := *mustCooldown(t, s, 1)

	// 全量重建：数据源 cooldown_until 恒 nil → 保留内存冷却
	require.NoError(t, s.InvalidateAllSync())
	require.Equal(t, before, *mustCooldown(t, s, 1), "DB nil → 保留内存冷却（不因重建缩水）")
	_, err = s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrNoAvailable, "重建后仍拦截")
}

// TestCooldownDBValueSyncsOnReload DB 有值 → 同步（回写成功路径，与内存一致）：
// 数据源 cooldown_until 非 nil 时重建必须同步该值；管理面 status 变更同经此
// 路径生效（DB status 权威回归，TestReuseSyncsStaticFieldsFromDB 的冷却侧）。
func TestCooldownDBValueSyncsOnReload(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	future := time.Now().Add(2 * time.Hour)
	m := newMemLoader(map[int64][]*domain.Account{10: {{
		ID: 1, TemplateID: 1, Template: tplx, UpstreamKey: "k",
		Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4,
		CooldownUntil: &future,
	}}})
	s := newSched(t, m)
	require.Equal(t, future, *mustCooldown(t, s, 1), "首刷装载 DB cooldown_until")

	// 数据源冷却推进 + 管理面 status 变更 → 重建同步（DB 权威）
	future2 := time.Now().Add(3 * time.Hour)
	m.mu.Lock()
	m.byGroup[10][0].CooldownUntil = &future2
	m.byGroup[10][0].Status = domain.Status429
	m.mu.Unlock()
	require.NoError(t, s.reload(context.Background()))
	require.Equal(t, future2, *mustCooldown(t, s, 1), "DB 有值 → 重建同步（与内存一致）")
	ri, _ := s.Runtime(1)
	require.Equal(t, domain.Status429, ri.Status, "管理面 status 变更经重建生效（DB 权威回归）")
	_, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrNoAvailable, "DB 冷却同步后拦截")
}

// TestCooldownPreservedOnInvalidateGroup 评审 M-3② 组级重载路径：A-2 修复点
// 在 buildSnapshots，同时服务全量 reload 与组级重载（InvalidateGroup——
// NOTIFY/管理端账号变更同路径）。断言：组级重载后冷却保留（Select 仍
// ErrNoAvailable）；管理面 status 变更经组级重载生效且冷却不受影响；DB 恒
// nil 下 status=active 也不得隐式清内存冷却（修复前 ≤30s 重建即清零）。
func TestCooldownPreservedOnInvalidateGroup(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSchedCooldownOnly(t, m)

	s.MarkResult(1, rule.Kind429, nil, 429, "")
	s.FlushRules()
	before := *mustCooldown(t, s, 1)
	_, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrNoAvailable, "冷却生效")

	// 组级 NOTIFY 重载（同构既有 InvalidateGroup 测试装置）：冷却保留
	s.InvalidateGroup(10)
	require.Equal(t, before, *mustCooldown(t, s, 1), "组级重载后冷却保留（内存值）")
	_, err = s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrNoAvailable, "组级重载后仍拦截")

	// 管理面 status 变更经组级重载生效（DB status 权威），冷却不受影响
	m.mu.Lock()
	m.byGroup[10][0].Status = domain.StatusDisabled
	m.mu.Unlock()
	s.InvalidateGroup(10)
	ri, _ := s.Runtime(1)
	require.Equal(t, domain.StatusDisabled, ri.Status, "管理面 status 变更经组级重载生效")
	require.Equal(t, before, *ri.CooldownUntil, "冷却跨 status 变更保留")

	// 管理面改回 active（DB cooldown 恒 nil）：冷却仍保留（一致性语义——
	// status=active 不再能隐式清内存冷却，与 DB 有值情形同步旧值一致）
	m.mu.Lock()
	m.byGroup[10][0].Status = domain.StatusActive
	m.mu.Unlock()
	s.InvalidateGroup(10)
	ri, _ = s.Runtime(1)
	require.Equal(t, domain.StatusActive, ri.Status)
	require.Equal(t, before, *ri.CooldownUntil, "DB nil → 保留内存冷却（active 不清冷却）")
	_, err = s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrNoAvailable, "冷却仍拦截")
}
