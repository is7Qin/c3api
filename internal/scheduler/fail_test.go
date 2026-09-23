// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/latch"
	"github.com/is7qin/c3api/internal/rule"
)

// persistLoader 模拟 DB 权威数据源：每次加载返回**值副本**（快照与数据源互不
// 干扰——重载即"从数据源重建"，等价重启）。持久失效事实 = failed_at（由
// sdkbridge CAS 落库，scheduler 只装载收敛，不回写持久状态）。
type persistLoader struct {
	mu      sync.Mutex
	byGroup map[int64][]*domain.Account
}

func newPersistLoader(byGroup map[int64][]*domain.Account) *persistLoader {
	return &persistLoader{byGroup: byGroup}
}

func (m *persistLoader) LoadGroupsAccounts(ctx context.Context) (map[int64][]*domain.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[int64][]*domain.Account, len(m.byGroup))
	for k, v := range m.byGroup {
		cp := make([]*domain.Account, len(v))
		for i, a := range v {
			ac := *a // 值副本：快照与数据源互不干扰
			cp[i] = &ac
		}
		out[k] = cp
	}
	return out, nil
}

func (m *persistLoader) LoadGroupAccounts(ctx context.Context, id int64) ([]*domain.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*domain.Account, len(m.byGroup[id]))
	for i, a := range m.byGroup[id] {
		ac := *a
		out[i] = &ac
	}
	return out, nil
}

// newSchedLoader 同 newSched，接受任意 Loader（memLoader / persistLoader）：
// reload 产出静态视图后武装编译道并同步编译，Select 走编译计划。
func newSchedLoader(t *testing.T, m Loader) *Scheduler {
	t.Helper()
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil, nil, nil)
	require.NoError(t, re.Reload(context.Background()))
	s := New(testCfg(), m, re, nil, nil, nil, nil)
	require.NoError(t, s.reload(context.Background()))
	wireSources(s, nil, nil)
	s.compileOnce()
	return s
}

// TestFailAccountRemovesFromSelection 失效摘除：快照置 disabled 后选号门跳过
// （候选存在但被拒 → ErrAttemptsExhausted）。
func TestFailAccountRemovesFromSelection(t *testing.T) {
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)}})
	s := newSched(t, m)

	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.AccountID)
	sel.Release()

	s.FailAccount(1)

	_, err = s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrAttemptsExhausted, "失效账号不得再被选中")

	ri, ok := s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri.Status)
}

// TestFailedAtLoadsDisabledAcrossReload 持久失效事实 = failed_at：装载时
// runtimeStatusFor 据 failed_at 置 disabled（重启快照重建仍摘除——恢复唯一入口
// /recover 清 failed_at）。内存 FailAccount 不落库，故持久化断言经 failed_at 表达。
func TestFailedAtLoadsDisabledAcrossReload(t *testing.T) {
	failed := time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC)
	pl := newPersistLoader(map[int64][]*domain.Account{10: {{
		ID: 1, TemplateID: 1, Template: tpl(1, domain.FormatOpenAIChat, []string{"m"}),
		UpstreamKey: "k", Enabled: true, FailedAt: &failed, MaxConcurrency: 4, LifecycleRevision: 1,
	}}})
	s := newSchedLoader(t, pl)

	ri, ok := s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri.Status, "failed_at 装载即 disabled")

	_, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.Error(t, err, "失效账号不可调度")

	require.NoError(t, s.InvalidateAllSync()) // 重启等价：从数据源全量重建快照
	ri, ok = s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri.Status, "重启快照重载后仍摘除（failed_at 持久）")
}

// TestFailAccountMarkResultGuard 防复活守卫复用：失效后 MarkResult（成功/错误）
// 不得投递事件把状态改回 active（MarkResult 对 disabled 短路）。
func TestFailAccountMarkResultGuard(t *testing.T) {
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)}})
	s := newSched(t, m)

	_, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)

	s.FailAccount(1)
	s.MarkResult(1, rule.KindOK, nil, 200, "", "")
	s.MarkResult(1, rule.KindNetwork, nil, 0, "stale error", "")
	s.FlushRules()
	releaseByID(s, 1)

	ri, ok := s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri.Status, "快照保持 disabled")
}

func TestMarkResultFailAccountUsesLifecycleRevision(t *testing.T) {
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)}})
	store := &fakeRuleStore{rules: map[int64]domain.Rule{
		1: {ID: 1, Name: "fail-5xx", Enabled: true, Priority: 1,
			When: domain.RuleWhen{Kind: strPtr("5xx")},
			Then: domain.RuleThen{FailAccount: true}},
	}, next: 2}
	// 构造期接线（无回填）：latch/hub 先行 → sink → rule → sched（New 内订阅）。
	ls := latch.NewLatchStore()
	hub := latch.NewHub()
	sink := NewLatchSink(nil, ls, hub)
	re := rule.New(rule.Config{}, store, nil, sink, nil)
	require.NoError(t, re.Reload(context.Background()))
	s := New(testCfg(), m, re, nil, nil, ls, hub)
	require.NoError(t, s.reload(context.Background()))
	wireSources(s, nil, nil)

	s.MarkResult(1, rule.Kind5xx, nil, 500, "boom", "m")
	s.FlushRules()

	runtime, ok := s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, runtime.Status, "fail_account must receive the current lifecycle revision")
}

// TestFailAccountQueuedEventsBeforeFailure 入队在先防复活：规则事件在失效置位
// **之前**已入队（MarkResult 时账号仍 active，守卫放行）→ FailAccount 置 disabled
// → FlushRules 消费入队事件。typed throttle 动作落 RuntimeHealth（非 accState
// status），disabled 快照不被事件改写——仍 disabled、不可调度。
func TestFailAccountQueuedEventsBeforeFailure(t *testing.T) {
	pl := newPersistLoader(map[int64][]*domain.Account{10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)}})
	s := newSchedLoader(t, pl)

	s.MarkResult(1, rule.Kind5xx, nil, 500, "boom", "")
	s.MarkResult(1, rule.KindOK, nil, 200, "", "")
	s.FailAccount(1)
	s.FlushRules()

	ri, ok := s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri.Status, "入队在先事件不得覆盖 disabled 快照")

	_, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.Error(t, err, "失效账号不可调度")
}

// TestFailAccountUnknownAccount 快照外/未加载账号：no-op 不 panic。
func TestFailAccountUnknownAccount(t *testing.T) {
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)}})
	s := newSched(t, m)

	s.FailAccount(999) // 快照外账号
	s.FailAccount(1)   // 正常路径不破坏

	ri, ok := s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri.Status)
}

// TestFailAccountIdempotentDisabled 幂等早退：账号已 disabled 时再次 FailAccount
// 直接返回（终态 disabled 不变）。
func TestFailAccountIdempotentDisabled(t *testing.T) {
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)}})
	s := newSched(t, m)

	s.FailAccount(1)
	s.FailAccount(1) // 已 disabled：幂等早退

	ri, ok := s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri.Status)
}

// TestFailAccountConcurrentAbsorbing 并发失效吸收态（spec C3）：多 goroutine
// 并发 FailAccount——copy-on-write CAS 把转换串行化，disabled 对双方都是吸收态，
// 终态确定性 disabled（每轮全新调度器放大竞态）。
func TestFailAccountConcurrentAbsorbing(t *testing.T) {
	const rounds = 200
	for i := 0; i < rounds; i++ {
		m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)}})
		s := newSched(t, m)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); s.FailAccount(1) }()
		go func() { defer wg.Done(); s.FailAccount(1) }()
		wg.Wait()

		ri, ok := s.Runtime(1)
		require.True(t, ok)
		require.Equal(t, domain.StatusDisabled, ri.Status, "第 %d 轮并发失效终态恒 disabled", i)
	}
}
