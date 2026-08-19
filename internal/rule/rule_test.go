// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package rule

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/logx"
)

// —— 内存 fake RuleStore（值语义副本，参照 internal/service/fakestore_test.go 风格） ——

type fakeRuleStore struct {
	mu    sync.Mutex
	rules map[int64]domain.Rule
	next  int64
}

func newFakeRuleStore() *fakeRuleStore {
	return &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}
}

func (f *fakeRuleStore) ListRules(ctx context.Context, enabled *bool) ([]domain.Rule, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]domain.Rule, 0, len(f.rules))
	for _, r := range f.rules {
		if enabled != nil && r.Enabled != *enabled {
			continue
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b domain.Rule) int { return a.Priority - b.Priority })
	return out, nil
}

func (f *fakeRuleStore) CreateRule(ctx context.Context, r domain.Rule) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r.ID = f.next
	f.next++
	f.rules[r.ID] = r
	return r.ID, nil
}

func (f *fakeRuleStore) UpdateRule(ctx context.Context, r domain.Rule) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rules[r.ID]; !ok {
		return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, r.ID)
	}
	f.rules[r.ID] = r
	return nil
}

func (f *fakeRuleStore) DeleteRule(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.rules[id]; !ok {
		return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
	}
	delete(f.rules, id)
	return nil
}

func (f *fakeRuleStore) DeleteRulesBatch(ctx context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range ids {
		if _, ok := f.rules[id]; !ok {
			return fmt.Errorf("%w: id=%d missing", repository.ErrNotFound, id)
		}
	}
	for _, id := range ids {
		delete(f.rules, id)
	}
	return nil
}

func (f *fakeRuleStore) CountRules(ctx context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.rules)), nil
}

// —— 记录型 ApplyFunc ——

type applied struct {
	aid      int64
	status   *domain.AccountStatus
	cooldown *time.Time
	weight   *int
	errMsg   string
}

type recorder struct {
	mu      sync.Mutex
	applied []applied
}

func (r *recorder) fn(aid int64, st *domain.AccountStatus, cd *time.Time, w *int, errMsg string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.applied = append(r.applied, applied{aid: aid, status: st, cooldown: cd, weight: w, errMsg: errMsg})
}

func (r *recorder) get() []applied {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.applied)
}

// —— 测试基座 ——

var testBase = time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)

func at(sec int) time.Time { return testBase.Add(time.Duration(sec) * time.Second) }

func evAt(kind Kind, sec int) Event {
	return Event{AccountID: 1, Kind: kind, OccurredAt: at(sec)}
}

func newTestEngine(t *testing.T, rules ...domain.Rule) (*RuleEngine, *fakeRuleStore) {
	t.Helper()
	st := newFakeRuleStore()
	for _, r := range rules {
		_, err := st.CreateRule(context.Background(), r)
		require.NoError(t, err)
	}
	e := New(Config{}, st, nil)
	require.NoError(t, e.Reload(context.Background()))
	return e, st
}

// —— 校验 ——

func TestValidateWhen(t *testing.T) {
	intPtr := func(v int) *int { return &v }
	f64Ptr := func(v float64) *float64 { return &v }
	strPtr := func(s string) *string { return &s }
	cases := []struct {
		name string
		w    domain.RuleWhen
		ok   bool
	}{
		{"empty when matches everything", domain.RuleWhen{}, true},
		{"kind 429", domain.RuleWhen{Kind: strPtr("429")}, true},
		{"kind 4xx", domain.RuleWhen{Kind: strPtr("4xx")}, true},
		{"kind 5xx", domain.RuleWhen{Kind: strPtr("5xx")}, true},
		{"kind network", domain.RuleWhen{Kind: strPtr("network")}, true},
		{"kind error rejected", domain.RuleWhen{Kind: strPtr("error")}, false},
		{"bad kind", domain.RuleWhen{Kind: strPtr("banana")}, false},
		{"kind ok with error_message_contains dead", domain.RuleWhen{Kind: strPtr("ok"), ErrorMessageContains: strPtr("boom")}, false},
		{"kind ok observer count_429", domain.RuleWhen{Kind: strPtr("ok"), Count429GE: intPtr(3)}, true},
		{"window zero", domain.RuleWhen{WindowSeconds: intPtr(0)}, false},
		{"negative count", domain.RuleWhen{Count429GE: intPtr(-1)}, false},
		{"negative count total", domain.RuleWhen{CountTotalGE: intPtr(-2)}, false},
		{"ratio over 1", domain.RuleWhen{Ratio429GE: f64Ptr(1.5), CountTotalGE: intPtr(4)}, false},
		{"ratio without total", domain.RuleWhen{Ratio429GE: f64Ptr(0.5)}, false},
		{"ratio error without total", domain.RuleWhen{RatioFailureGE: f64Ptr(0.5)}, false},
		{"ratio with total", domain.RuleWhen{Ratio429GE: f64Ptr(0.5), CountTotalGE: intPtr(4)}, true},
		{"full valid", domain.RuleWhen{
			Kind: strPtr("5xx"), CountFailureGE: intPtr(2), WindowSeconds: intPtr(30),
		}, true},
		{"model empty rejected", domain.RuleWhen{Model: strPtr("")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateWhen(tc.w)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestValidateThen(t *testing.T) {
	status := domain.Status429
	cooldown := "30s"
	weight := 40
	cases := []struct {
		name string
		t    domain.RuleThen
		ok   bool
	}{
		{"no action", domain.RuleThen{}, false},
		{"status only", domain.RuleThen{Status: &status}, true},
		{"cooldown only", domain.RuleThen{Cooldown: &cooldown}, true},
		{"custom_message only", domain.RuleThen{CustomMessage: strPtr("rate limited")}, true},
		{"response_code only 502", domain.RuleThen{ResponseCode: intPtr(502)}, true},
		{"response_code with cooldown", domain.RuleThen{ResponseCode: intPtr(429), Cooldown: &cooldown}, true},
		{"custom_message with cooldown", domain.RuleThen{CustomMessage: strPtr("x"), Cooldown: &cooldown}, true},
		{"response_code 400 ok", domain.RuleThen{ResponseCode: intPtr(400)}, true},
		{"response_code 599 ok", domain.RuleThen{ResponseCode: intPtr(599)}, true},
		{"response_code 200 rejected", domain.RuleThen{ResponseCode: intPtr(200)}, false},
		{"response_code 399 rejected", domain.RuleThen{ResponseCode: intPtr(399)}, false},
		{"response_code 600 rejected", domain.RuleThen{ResponseCode: intPtr(600)}, false},
		{"response_code -1 rejected", domain.RuleThen{ResponseCode: intPtr(-1)}, false},
		{"custom_message empty rejected", domain.RuleThen{CustomMessage: strPtr("")}, false},
		{"bad status", domain.RuleThen{Status: statusPtr(domain.AccountStatus("banana"))}, false},
		{"unparseable cooldown", domain.RuleThen{Cooldown: strPtr("30x")}, false},
		{"zero cooldown", domain.RuleThen{Cooldown: strPtr("0s")}, false},
		{"negative cooldown", domain.RuleThen{Cooldown: strPtr("-5s")}, false},
		{"weight over 100", domain.RuleThen{Weight: intPtr(101)}, false},
		{"negative weight", domain.RuleThen{Weight: intPtr(-1)}, false},
		{"weight only", domain.RuleThen{Weight: &weight}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateThen(tc.t)
			if tc.ok {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

// intPtr 由 engine.go 提供（同包）。
func f64Ptr(v float64) *float64 { return &v }
func i64Ptr(v int64) *int64     { return &v }

// —— 窗口 ——

func TestWindowAdvanceAndMerge(t *testing.T) {
	var wm windowMap
	wm.reset(60*time.Second, true)                                // 10s × 6 完整桶 + 1 当前桶
	wm.Add(evAt(Kind5xx, 0))                                      // 桶 [0,10)
	wm.Add(evAt(Kind5xx, 5))                                      // 桶 [0,10)
	wm.Add(evAt(Kind5xx, 15))                                     // 桶 [10,20)，推进环
	wm.Add(Event{AccountID: 2, Kind: Kind429, OccurredAt: at(2)}) // 桶 [0,10)

	// 跨窗合并：20s 窗口含 [0,20) 两桶
	require.Equal(t, windowSnapshot{failure: 3}, wm.Snapshot(1, 20, at(15)))
	// 请求秒数 < 桶内偏移 → 只统计当前桶（t=0/5 在 [0,10)，不在 5s 窗口）
	require.Equal(t, windowSnapshot{failure: 1}, wm.Snapshot(1, 5, at(15)))
	// 请求秒数超覆盖 → 钳制全环
	require.Equal(t, windowSnapshot{failure: 3}, wm.Snapshot(1, 600, at(15)))
	// 账号隔离
	require.Equal(t, windowSnapshot{t429: 1}, wm.Snapshot(2, 60, at(15)))

	// 推进到 25s：三事件仍全在 60s 窗口内
	require.Equal(t, windowSnapshot{failure: 3}, wm.Snapshot(1, 60, at(25)))

	// 推进到 70s：t=0/5（65s/70s 前）滑出 60s 窗口，t=15（55s 前）仍在
	require.Equal(t, windowSnapshot{failure: 1}, wm.Snapshot(1, 60, at(70)))

	// 推进到 81s：t=15（66s 前）滑出
	require.Equal(t, windowSnapshot{}, wm.Snapshot(1, 60, at(81)))
}

// TestWindowDecay 固定粒度近似边界：窗口 [6,36] 不含 [0,5) 桶的早期事件；
// 桶内偏移 < 窗口秒数时，部分重叠桶全计（近似误差 ≤ 一个粒度）。
func TestWindowDecay(t *testing.T) {
	var wm windowMap
	wm.reset(30*time.Second, true) // 5s × 6 完整桶 + 1 当前桶
	wm.Add(evAt(Kind5xx, 0))
	wm.Add(evAt(Kind5xx, 1))
	wm.Add(evAt(Kind5xx, 2))
	wm.Add(evAt(Kind5xx, 31))

	// 窗口 [1,31]：桶 [0,5) 与窗口部分重叠 → 全计（近似），t=0/1/2 仍未衰减
	require.Equal(t, windowSnapshot{failure: 4}, wm.Snapshot(1, 30, at(31)))
	// 窗口 [6,36]：桶 [0,5) 完全滑出 → 只剩 t=31
	require.Equal(t, windowSnapshot{failure: 1}, wm.Snapshot(1, 30, at(36)))
}

func TestWindowTrackOK(t *testing.T) {
	var wm windowMap
	wm.reset(60*time.Second, true) // needsOK=true：维护 ok 计数
	wm.Add(evAt(KindOK, 0))
	wm.Add(evAt(Kind429, 1))
	require.Equal(t, windowSnapshot{ok: 1, t429: 1}, wm.Snapshot(1, 60, at(2)))

	wm.reset(60*time.Second, false) // needsOK=false：ok 不计数
	wm.Add(evAt(KindOK, 0))
	wm.Add(evAt(Kind429, 1))
	require.Equal(t, windowSnapshot{t429: 1}, wm.Snapshot(1, 60, at(2)))
}

func TestWindowCleanup(t *testing.T) {
	var wm windowMap
	wm.reset(60*time.Second, true) // retention = 2 × 70s = 140s
	wm.Add(evAt(Kind5xx, 0))       // aid1 @0
	wm.Add(Event{AccountID: 2, Kind: Kind5xx, OccurredAt: at(100)})

	wm.cleanup(at(141)) // cutoff = 1s：aid1 过期、aid2 新鲜（时间序扫描即停）
	require.Len(t, wm.lastSeen, 1)
	require.Equal(t, windowSnapshot{}, wm.Snapshot(1, 60, at(141)))
	require.Equal(t, windowSnapshot{failure: 1}, wm.Snapshot(2, 60, at(141)))
}

// —— 评估 ——

func TestMatch(t *testing.T) {
	okEv := func(kind Kind) Event { return Event{AccountID: 1, TemplateID: 2, Model: "m1", Kind: kind} }
	gid := i64Ptr(7)
	http500 := 500
	http429 := 429
	cases := []struct {
		name string
		w    domain.RuleWhen
		ev   Event
		wc   windowSnapshot
		want bool
	}{
		{"empty when matches everything", domain.RuleWhen{}, okEv(KindOK), windowSnapshot{}, true},
		{"kind match", domain.RuleWhen{Kind: strPtr("5xx")}, okEv(Kind5xx), windowSnapshot{}, true},
		{"kind mismatch", domain.RuleWhen{Kind: strPtr("5xx")}, okEv(KindOK), windowSnapshot{}, false},
		{"http status nil event", domain.RuleWhen{HTTPStatus: &http500}, okEv(Kind5xx), windowSnapshot{}, false},
		{"http status equal", domain.RuleWhen{HTTPStatus: &http429}, Event{HTTPStatus: &http429}, windowSnapshot{}, true},
		{"message contains", domain.RuleWhen{ErrorMessageContains: strPtr("rate limit")},
			Event{ErrorMessage: "upstream 429 rate limited"}, windowSnapshot{}, true},
		{"message not contains", domain.RuleWhen{ErrorMessageContains: strPtr("timeout")},
			Event{ErrorMessage: "upstream 429 rate limited"}, windowSnapshot{}, false},
		{"account id", domain.RuleWhen{AccountID: i64Ptr(1)}, okEv(KindOK), windowSnapshot{}, true},
		{"account id mismatch", domain.RuleWhen{AccountID: i64Ptr(9)}, okEv(KindOK), windowSnapshot{}, false},
		{"template id", domain.RuleWhen{TemplateID: i64Ptr(2)}, okEv(KindOK), windowSnapshot{}, true},
		{"model", domain.RuleWhen{Model: strPtr("m1")}, okEv(KindOK), windowSnapshot{}, true},
		{"model mismatch", domain.RuleWhen{Model: strPtr("m2")}, okEv(KindOK), windowSnapshot{}, false},
		{"group id ev nil", domain.RuleWhen{GroupID: gid}, okEv(KindOK), windowSnapshot{}, false},
		{"count 429 below", domain.RuleWhen{Count429GE: intPtr(2)}, okEv(Kind429), windowSnapshot{t429: 1}, false},
		{"count 429 ok", domain.RuleWhen{Count429GE: intPtr(2)}, okEv(Kind429), windowSnapshot{t429: 2}, true},
		{"count failure ok", domain.RuleWhen{CountFailureGE: intPtr(2)}, okEv(Kind5xx), windowSnapshot{failure: 3}, true},
		{"count ok below", domain.RuleWhen{CountOKGE: intPtr(2)}, okEv(KindOK), windowSnapshot{ok: 1}, false},
		{"count total below", domain.RuleWhen{CountTotalGE: intPtr(4)}, okEv(KindOK), windowSnapshot{ok: 2, failure: 1}, false},
		{"ratio below threshold", domain.RuleWhen{Ratio429GE: f64Ptr(0.5), CountTotalGE: intPtr(4)},
			okEv(Kind429), windowSnapshot{t429: 1, ok: 3}, false},
		{"ratio at threshold", domain.RuleWhen{Ratio429GE: f64Ptr(0.5), CountTotalGE: intPtr(4)},
			okEv(Kind429), windowSnapshot{t429: 2, ok: 2}, true},
		{"ratio total below floor", domain.RuleWhen{Ratio429GE: f64Ptr(0.5), CountTotalGE: intPtr(4)},
			okEv(Kind429), windowSnapshot{t429: 1, ok: 1}, false},
		{"ratio failure ok", domain.RuleWhen{RatioFailureGE: f64Ptr(0.75), CountTotalGE: intPtr(4)},
			okEv(Kind5xx), windowSnapshot{failure: 3, ok: 1}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, Match(tc.w, tc.ev, tc.wc))
		})
	}
}

func TestApply(t *testing.T) {
	status := domain.StatusActive
	weight := 40
	resetAt := at(90)
	ev := Event{AccountID: 1, Kind: Kind429, OccurredAt: at(10), ResetAt: &resetAt}

	// cooldown = OccurredAt + 30s
	st, cd, w := Apply(domain.RuleThen{Status: statusPtr(domain.Status429), Cooldown: strPtr("30s")}, ev)
	require.NotNil(t, st)
	require.Equal(t, domain.Status429, *st)
	require.Equal(t, at(40), *cd)
	require.Nil(t, w)

	// cooldown 未配 + ResetAt 非 nil → ResetAt（M2 残留语义）
	st, cd, _ = Apply(domain.RuleThen{Status: &status}, ev)
	require.Equal(t, domain.StatusActive, *st)
	require.Equal(t, at(90), *cd)

	// cooldown 优先于 ResetAt
	_, cd, _ = Apply(domain.RuleThen{Cooldown: strPtr("5s")}, ev)
	require.Equal(t, at(15), *cd)

	// 只改权重：status nil；无 cooldown 时 ResetAt 兜底（M2 残留语义）
	st, cd, w = Apply(domain.RuleThen{Weight: &weight}, ev)
	require.Nil(t, st)
	require.Equal(t, at(90), *cd)
	require.Equal(t, 40, *w)

	// 无 cooldown 且无 ResetAt → 无冷却
	_, cd, _ = Apply(domain.RuleThen{Weight: &weight}, Event{AccountID: 1, OccurredAt: at(10)})
	require.Nil(t, cd)

	// 非法 cooldown（校验已挡，防御性跳过）
	_, cd, _ = Apply(domain.RuleThen{Cooldown: strPtr("bogus")}, ev)
	require.Nil(t, cd)
}

// —— 引擎 ——

func TestReloadNeedsOKEvents(t *testing.T) {
	cases := []struct {
		name  string
		when  domain.RuleWhen
		needs bool
	}{
		{"kind nil", domain.RuleWhen{}, true},
		{"kind ok", domain.RuleWhen{Kind: strPtr("ok")}, true},
		{"kind 5xx", domain.RuleWhen{Kind: strPtr("5xx")}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e, _ := newTestEngine(t, domain.Rule{
				Name: "r", Enabled: true, Priority: 10,
				When: tc.when, Then: domain.RuleThen{Status: statusPtr(domain.StatusActive)},
			})
			require.Equal(t, tc.needs, e.NeedsOKEvents())
		})
	}
}

func TestSeedRules(t *testing.T) {
	st := newFakeRuleStore()
	e := New(Config{}, st, nil)
	require.NoError(t, e.Reload(context.Background()))

	// 种子 5 条（fresh setup 哲学，指针即意图）：429/30s+nil/rate limited、4xx+400/nil/nil 全透、5xx/unhealthy/10m+502/generic、
	// network/unhealthy/5s+502/generic、ok/active，priority 10/15/20/25/30
	require.Equal(t, int64(5), mustCountAny(t, st))
	require.True(t, e.NeedsOKEvents()) // 种子含 kind=ok 恢复规则（C1）

	rules, err := st.ListRules(context.Background(), nil)
	require.NoError(t, err)
	require.Len(t, rules, 5)
	require.Equal(t, []int{10, 15, 20, 25, 30}, []int{
		rules[0].Priority, rules[1].Priority, rules[2].Priority, rules[3].Priority, rules[4].Priority,
	})
	require.Equal(t, "429", *rules[0].When.Kind)
	require.Equal(t, domain.Status429, *rules[0].Then.Status)
	require.Equal(t, "30s", *rules[0].Then.Cooldown)
	require.Nil(t, rules[0].Then.ResponseCode, "seed-429 码透传 nil")
	require.NotNil(t, rules[0].Then.CustomMessage)
	require.Equal(t, "rate limited", *rules[0].Then.CustomMessage, "seed-429 文不透 rate limited")
	require.Equal(t, "4xx", *rules[1].When.Kind)
	require.Equal(t, 400, *rules[1].When.HTTPStatus)
	require.Nil(t, rules[1].Then.ResponseCode, "seed-4xx-400 码透传 nil")
	require.Nil(t, rules[1].Then.CustomMessage, "seed-4xx-400 文透传 nil（全透，种子特例）")
	require.Nil(t, rules[1].Then.Status)
	require.Nil(t, rules[1].Then.Cooldown)
	require.Equal(t, "5xx", *rules[2].When.Kind)
	require.Equal(t, domain.StatusUnhealthy, *rules[2].Then.Status)
	require.Equal(t, "10m", *rules[2].Then.Cooldown, "seed-5xx 冷却 10m（用户裁决）")
	require.NotNil(t, rules[2].Then.ResponseCode)
	require.Equal(t, 502, *rules[2].Then.ResponseCode)
	require.Equal(t, "Upstream request failed", *rules[2].Then.CustomMessage)
	require.Equal(t, "network", *rules[3].When.Kind)
	require.Equal(t, domain.StatusUnhealthy, *rules[3].Then.Status)
	require.Equal(t, "5s", *rules[3].Then.Cooldown, "seed-network 冷却 5s（连接级独立，不吃 10m）")
	require.NotNil(t, rules[3].Then.ResponseCode)
	require.Equal(t, 502, *rules[3].Then.ResponseCode)
	require.Equal(t, "Upstream request failed", *rules[3].Then.CustomMessage)
	require.Equal(t, "ok", *rules[4].When.Kind)
	require.Equal(t, domain.StatusActive, *rules[4].Then.Status)
	require.Nil(t, rules[4].Then.Cooldown)

	// 非空表不重复写种子
	require.NoError(t, e.Reload(context.Background()))
	require.Equal(t, int64(5), mustCountAny(t, st))
}

// mustCountAny 表内规则数（接受唯一约束包装 store；种子幂等测试用）。
func mustCountAny(t *testing.T, st repository.RuleStore) int64 {
	t.Helper()
	n, err := st.CountRules(context.Background())
	require.NoError(t, err)
	return n
}

func TestPriorityHitOrder(t *testing.T) {
	e, _ := newTestEngine(t,
		domain.Rule{Name: "low", Enabled: true, Priority: 10,
			When: domain.RuleWhen{Kind: strPtr("5xx")},
			Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy), Cooldown: strPtr("5s")}},
		domain.Rule{Name: "high", Enabled: true, Priority: 20,
			When: domain.RuleWhen{Kind: strPtr("5xx")},
			Then: domain.RuleThen{Status: statusPtr(domain.Status429), Cooldown: strPtr("30s")}},
	)
	var rec recorder
	e.SetApply(rec.fn)

	// 两规则都命中：priority 低者首中，只执行一次
	e.HandleEvent(context.Background(), evAt(Kind5xx, 0))
	app := rec.get()
	require.Len(t, app, 1)
	require.Equal(t, domain.StatusUnhealthy, *app[0].status)
	require.Equal(t, at(5), *app[0].cooldown)

	// ok 事件两规则都不命中
	e.HandleEvent(context.Background(), evAt(KindOK, 1))
	require.Len(t, rec.get(), 1)
}

func TestDisabledRuleNotLoaded(t *testing.T) {
	e, _ := newTestEngine(t, domain.Rule{
		Name: "off", Enabled: false, Priority: 10,
		When: domain.RuleWhen{Kind: strPtr("5xx")},
		Then: domain.RuleThen{Status: statusPtr(domain.Status429)},
	})
	var rec recorder
	e.SetApply(rec.fn)
	require.False(t, e.NeedsOKEvents())
	e.HandleEvent(context.Background(), evAt(Kind5xx, 0))
	require.Empty(t, rec.get())
}

// TestHitKeepsCountsThenDecays 命中不清零窗口计数（C2）：阈值 2 连续命中两次；
// 滑动衰减后（[0,5) 桶整体滑出 30s 窗口）阈值重新可达。
func TestHitKeepsCountsThenDecays(t *testing.T) {
	e, _ := newTestEngine(t, domain.Rule{
		Name: "escalate", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Kind: strPtr("5xx"), CountFailureGE: intPtr(2), WindowSeconds: intPtr(30)},
		Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy)},
	})
	var rec recorder
	e.SetApply(rec.fn)

	e.HandleEvent(context.Background(), evAt(Kind5xx, 0)) // err=1，未命中
	require.Empty(t, rec.get())
	e.HandleEvent(context.Background(), evAt(Kind5xx, 1)) // err=2，命中
	require.Len(t, rec.get(), 1)
	e.HandleEvent(context.Background(), evAt(Kind5xx, 2)) // err=3（不清零），再命中
	require.Len(t, rec.get(), 2)

	e.HandleEvent(context.Background(), evAt(Kind5xx, 36)) // 窗口 [6,36]：仅 +36，err=1 未命中
	require.Len(t, rec.get(), 2)
}

func TestRatioMatchWithTotalFloor(t *testing.T) {
	e, _ := newTestEngine(t, domain.Rule{
		Name: "hot-account", Enabled: true, Priority: 10,
		When: domain.RuleWhen{
			Kind: nil, Ratio429GE: f64Ptr(0.5), CountTotalGE: intPtr(4), WindowSeconds: intPtr(30),
		},
		Then: domain.RuleThen{Status: statusPtr(domain.Status429), Cooldown: strPtr("30s")},
	})
	var rec recorder
	e.SetApply(rec.fn)

	// 429×2 + ok×2 → total=4、ratio=0.5 → 命中（needsOK：kind=nil → ok 计数维护）
	e.HandleEvent(context.Background(), evAt(Kind429, 0))
	e.HandleEvent(context.Background(), evAt(Kind429, 1))
	e.HandleEvent(context.Background(), evAt(KindOK, 2))
	e.HandleEvent(context.Background(), evAt(KindOK, 3))
	app := rec.get()
	require.Len(t, app, 1)
	require.Equal(t, domain.Status429, *app[0].status)
	require.Equal(t, at(33), *app[0].cooldown)

	// 样本不足：total=2 < 4 → 比例不参与
	e2, _ := newTestEngine(t, domain.Rule{
		Name: "hot2", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Ratio429GE: f64Ptr(0.5), CountTotalGE: intPtr(4), WindowSeconds: intPtr(30)},
		Then: domain.RuleThen{Status: statusPtr(domain.Status429)},
	})
	var rec2 recorder
	e2.SetApply(rec2.fn)
	e2.HandleEvent(context.Background(), evAt(Kind429, 0))
	e2.HandleEvent(context.Background(), evAt(Kind429, 1))
	require.Empty(t, rec2.get())
}

// —— worker ——

func TestName(t *testing.T) {
	e := New(Config{}, newFakeRuleStore(), nil)
	require.Equal(t, "rule-engine", e.Name())
}

func TestEnqueueFullDrops(t *testing.T) {
	e := New(Config{EventQueueSize: 1}, newFakeRuleStore(), nil)
	e.Enqueue(evAt(Kind5xx, 0)) // 占满
	require.Equal(t, uint64(0), e.dropped.Load())
	e.Enqueue(evAt(Kind5xx, 1)) // 满 → 丢弃
	e.Enqueue(evAt(Kind5xx, 2))
	require.Equal(t, uint64(2), e.dropped.Load())
	require.Len(t, e.ch, 1)
}

func TestStartTwice(t *testing.T) {
	e := New(Config{}, newFakeRuleStore(), nil)
	require.NoError(t, e.Start(context.Background()))
	require.Error(t, e.Start(context.Background()))
}

func TestCloseDrainsQueue(t *testing.T) {
	e, _ := newTestEngine(t, domain.Rule{
		Name: "fail", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Kind: strPtr("5xx")},
		Then: domain.RuleThen{Status: statusPtr(domain.Status429), Cooldown: strPtr("30s")},
	})
	var rec recorder
	e.SetApply(rec.fn)
	// 未 Start：Close 直接排空
	e.Enqueue(evAt(Kind5xx, 5))
	require.NoError(t, e.Close(context.Background()))
	app := rec.get()
	require.Len(t, app, 1)
	require.Equal(t, domain.Status429, *app[0].status)
	require.Equal(t, at(35), *app[0].cooldown)

	// Close 幂等
	require.NoError(t, e.Close(context.Background()))
}

func TestStartConsumesThenCloseDrains(t *testing.T) {
	e, _ := newTestEngine(t, domain.Rule{
		Name: "fail", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Kind: strPtr("5xx")},
		Then: domain.RuleThen{Status: statusPtr(domain.Status429)},
	})
	var rec recorder
	e.SetApply(rec.fn)

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, e.Start(ctx))
	e.Enqueue(evAt(Kind5xx, 0))
	require.Eventually(t, func() bool { return len(rec.get()) == 1 }, 5*time.Second, 10*time.Millisecond)

	// 取消后：loop 退出，Close 排空剩余
	cancel()
	e.Enqueue(evAt(Kind5xx, 1))
	closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
	defer closeCancel()
	require.NoError(t, e.Close(closeCtx))
	require.Eventually(t, func() bool { return len(rec.get()) == 2 }, 5*time.Second, 10*time.Millisecond)
}

// uniqueRuleStore 在 fakeRuleStore 之上施加 name/priority 唯一约束（模拟真实
// repo 的 ent 唯一索引——种子幂等测试的冲突源，设计文档 R2）。
type uniqueRuleStore struct {
	*fakeRuleStore
}

func (u *uniqueRuleStore) CreateRule(ctx context.Context, r domain.Rule) (int64, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for _, existing := range u.rules {
		if existing.Name == r.Name || existing.Priority == r.Priority {
			return 0, fmt.Errorf("%w: priority=%d or name=%q", repository.ErrConflict, r.Priority, r.Name)
		}
	}
	r.ID = u.next
	u.next++
	u.rules[r.ID] = r
	return r.ID, nil
}

// TestSeedRulesIdempotentConcurrent 多实例启动种子竞态（设计文档 R2 / 必改 10）：
// 两引擎并发 Reload 空表 → 一方唯一约束冲突被容忍（跳过继续），双双成功，
// 最终种子并集恰 3 条。
func TestSeedRulesIdempotentConcurrent(t *testing.T) {
	st := &uniqueRuleStore{newFakeRuleStore()}
	e1 := New(Config{}, st, nil)
	e2 := New(Config{}, st, nil)
	var wg sync.WaitGroup
	var err1, err2 error
	wg.Add(2)
	go func() { defer wg.Done(); err1 = e1.Reload(context.Background()) }()
	go func() { defer wg.Done(); err2 = e2.Reload(context.Background()) }()
	wg.Wait()
	require.NoError(t, err1)
	require.NoError(t, err2, "冲突方不得失败（唯一约束 → 跳过继续，并集收敛）")
	require.Equal(t, int64(5), mustCountAny(t, st), "种子并集恰 5 条（不双写）")
}

// TestSeedRulesIdempotentRepeat 已种子表重复 Reload 不重写（幂等回归）。
func TestSeedRulesIdempotentRepeat(t *testing.T) {
	st := &uniqueRuleStore{newFakeRuleStore()}
	e := New(Config{}, st, nil)
	require.NoError(t, e.Reload(context.Background()))
	require.Equal(t, int64(5), mustCountAny(t, st))
	require.NoError(t, e.Reload(context.Background()))
	require.Equal(t, int64(5), mustCountAny(t, st), "重复 Reload 不重写种子")
}

// TestReloadRulesAdapter ReloadRules 与 Reload 同实现（invalidate.RulesReloader
// 适配）：空表可种子、非空表加载。
func TestReloadRulesAdapter(t *testing.T) {
	st := newFakeRuleStore()
	e := New(Config{}, st, nil)
	require.NoError(t, e.ReloadRules(context.Background()))
	require.Equal(t, int64(5), mustCountAny(t, st), "ReloadRules 空表同样写种子")
}

// —— 热点修复 B：Enqueue 丢弃阈值告警（errlog 模式对齐） ——

// TestEnqueueDropWarnEdge（errlog_test TestErrLogWorkerDropWarnEdge 同款结构）：
// 丢弃累计 ≥ 阈值 → Warn 恰好一次（带累计数）；队列排空（Flush）边沿回落 →
// 再次风暴再次 Warn（每风暴一次，不刷屏——风暴期 12k 条刷屏不复现）。
func TestEnqueueDropWarnEdge(t *testing.T) {
	old := ruleDropWarnThreshold
	ruleDropWarnThreshold = 50
	t.Cleanup(func() { ruleDropWarnThreshold = old })

	e := New(Config{EventQueueSize: 8}, newFakeRuleStore(), nil)
	logger, out := newTestRuleLogger(t)
	e.log = logger

	// 第一轮风暴（无消费方，队列满 → 92 丢弃 ≥ 50 阈值）→ 恰好一次 Warn
	for i := 0; i < 100; i++ {
		e.Enqueue(evAt(Kind5xx, 0))
	}
	require.Equal(t, uint64(92), e.dropped.Load(), "丢弃计数 = 到达 - 队列容量")
	e.Flush(context.Background()) // 排空队列 → 告警边沿回落
	// 第二轮风暴 → 再次 Warn
	for i := 0; i < 100; i++ {
		e.Enqueue(evAt(Kind5xx, 0))
	}
	require.Equal(t, uint64(184), e.dropped.Load(), "丢弃计数跨风暴累计（只回落边沿，不清计数）")
	e.Flush(context.Background())
	require.NoError(t, logger.Sync())
	b, err := os.ReadFile(out)
	require.NoError(t, err)
	require.Equal(t, 2, strings.Count(string(b), "rule-engine event queue full, dropping events"),
		"每风暴一次 Warn（边沿回落）")
	require.Contains(t, string(b), `"dropped":50`, "首风暴在阈值跨越点（50）恰好告警一次")
	require.Contains(t, string(b), `"dropped":93`, "次风暴边沿回落后在首个丢弃点（92+1）再告警")
	require.Contains(t, string(b), `"threshold":50`, "Warn 带阈值")
}

// newTestRuleLogger warn 级文件 logger（Warn 断言用；errlog_test 同款，
// Windows 句柄 best-effort）。
func newTestRuleLogger(t *testing.T) (*logx.Logger, string) {
	t.Helper()
	dir, err := os.MkdirTemp("", "rule-test-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	out := filepath.Join(dir, "out.json")
	logger, err := logx.New("warn", out)
	require.NoError(t, err)
	return logger, out
}

// —— Classify（错误分类决策） ——

// TestClassify 分类矩阵：遍历 enabled 规则 priority 升序首中（非窗口条件
// 维度）；命中 → transmit = 命中规则 then.transmit、punish = 有状态动作；
// 无命中 → (false, false)（默认归一）。
func TestClassify(t *testing.T) {
	http400, http401 := 400, 401
	e, _ := newTestEngine(t,
		domain.Rule{Name: "r4xx-401", Enabled: true, Priority: 10,
			When: domain.RuleWhen{Kind: strPtr("4xx"), HTTPStatus: &http401},
			Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy), Cooldown: strPtr("30m"), ResponseCode: intPtr(502), CustomMessage: strPtr("Upstream request failed")}},
		domain.Rule{Name: "r4xx-400-transmit", Enabled: true, Priority: 15,
			When: domain.RuleWhen{Kind: strPtr("4xx"), HTTPStatus: &http400},
			Then: domain.RuleThen{}},
		domain.Rule{Name: "r5xx", Enabled: true, Priority: 20,
			When: domain.RuleWhen{Kind: strPtr("5xx")},
			Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy), ResponseCode: intPtr(502), CustomMessage: strPtr("Upstream request failed")}},
		domain.Rule{Name: "rnet", Enabled: true, Priority: 25,
			When: domain.RuleWhen{Kind: strPtr("network")},
			Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy), ResponseCode: intPtr(502), CustomMessage: strPtr("Upstream request failed")}},
	)
	ev := func(kind Kind, code int, msg string) Event {
		var hp *int
		if code > 0 {
			hp = &code
		}
		return Event{AccountID: 1, Kind: kind, HTTPStatus: hp, ErrorMessage: msg}
	}

	// 无规则 4xx（其他状态码）→ 默认归一 502（engine 默认）
	then, pu := e.Classify(ev(Kind4xx, 403, "forbidden"))
	require.NotNil(t, then.ResponseCode)
	require.Equal(t, 502, *then.ResponseCode)
	require.Equal(t, "upstream rejected request", *then.CustomMessage)
	require.False(t, pu)

	// kind=4xx + http=401 → 401 (502+generic, true)（unhealthy 30m——用户案例，指针归一）
	then, pu = e.Classify(ev(Kind4xx, 401, "no balance"))
	require.NotNil(t, then.ResponseCode)
	require.Equal(t, 502, *then.ResponseCode, "未透传 → 归一 502")
	require.NotNil(t, then.CustomMessage)
	require.True(t, pu, "有状态动作 → 投递")

	// kind=4xx + http=400 + passthrough → 400 (nil/nil, false)（指针 nil 即透传）
	then, pu = e.Classify(ev(Kind4xx, 400, "bad request"))
	require.Nil(t, then.ResponseCode, "透传规则 → ResponseCode nil")
	require.Nil(t, then.CustomMessage, "透传规则 → CustomMessage nil")
	require.False(t, pu, "透传-only 无状态动作")

	// kind=5xx → 5xx (502+generic, true)
	then, pu = e.Classify(ev(Kind5xx, 500, "boom"))
	require.NotNil(t, then.ResponseCode)
	require.Equal(t, 502, *then.ResponseCode)
	require.True(t, pu)

	// kind=network → network (502+generic, true)
	then, pu = e.Classify(ev(KindNetwork, 0, "dial tcp: refused"))
	require.NotNil(t, then.ResponseCode)
	require.True(t, pu)

	// ok 事件：无 kind=ok 规则 → 无命中
	then, pu = e.Classify(ev(KindOK, 200, ""))
	require.Nil(t, then.ResponseCode)
	require.False(t, pu)

	// 429：无 kind=429 规则（r4xx-401 的 http=401 不匹配 429）→ 默认归一 502
	then, pu = e.Classify(ev(Kind429, 429, "rate limited"))
	require.NotNil(t, then.ResponseCode)
	require.Equal(t, 502, *then.ResponseCode)
	require.Equal(t, "upstream rejected request", *then.CustomMessage)
	require.False(t, pu)
}

// TestClassifyCooldownPunish A-1 修复回归（2026-08-19 缺陷 1 直接根因）：
// punish 判定必须含 Cooldown——cooldown-only 规则（transmit=false、status/
// weight nil）命中后必须 punish=true，否则 429/5xx/network 分支不投递
// MarkResult、冷却动作静默丢弃（账号恒 active、请求恒 "no available
// account"）。矩阵：cooldown-only / 带 status（回归）/ transmit-only（回归，
// 透传不冷却语义不变）/ transmit+cooldown 组合（评审 O-3）/ 窗口条件
// cooldown-only（保守"可能命中"语义，与既有窗口规则测试同构）。
func TestClassifyCooldownPunish(t *testing.T) {
	http401, http400, http402 := 401, 400, 402
	e, _ := newTestEngine(t,
		domain.Rule{Name: "cd-only", Enabled: true, Priority: 10,
			When: domain.RuleWhen{Kind: strPtr("429")},
			Then: domain.RuleThen{Cooldown: strPtr("5h"), ResponseCode: intPtr(502), CustomMessage: strPtr("Upstream request failed")}},
		domain.Rule{Name: "status-429", Enabled: true, Priority: 20,
			When: domain.RuleWhen{Kind: strPtr("429"), HTTPStatus: &http401},
			Then: domain.RuleThen{Status: statusPtr(domain.Status429), Cooldown: strPtr("30s"), ResponseCode: intPtr(502), CustomMessage: strPtr("Upstream request failed")}},
		domain.Rule{Name: "transmit-only", Enabled: true, Priority: 30,
			When: domain.RuleWhen{Kind: strPtr("4xx"), HTTPStatus: &http400},
			Then: domain.RuleThen{}},
		domain.Rule{Name: "transmit-cd", Enabled: true, Priority: 40,
			When: domain.RuleWhen{Kind: strPtr("4xx"), HTTPStatus: &http402},
			Then: domain.RuleThen{Cooldown: strPtr("30s")}},
		domain.Rule{Name: "window-cd", Enabled: true, Priority: 50,
			When: domain.RuleWhen{Kind: strPtr("5xx"), CountFailureGE: intPtr(5), WindowSeconds: intPtr(60)},
			Then: domain.RuleThen{Cooldown: strPtr("5h"), ResponseCode: intPtr(502), CustomMessage: strPtr("Upstream request failed")}},
	)
	ev := func(kind Kind, code int) Event {
		var hp *int
		if code > 0 {
			hp = &code
		}
		return Event{AccountID: 1, Kind: kind, HTTPStatus: hp}
	}

	// cooldown-only 规则 → punish=true，归一 502（修复前 false——缺陷 1 根因，指针归一）
	then, pu := e.Classify(ev(Kind429, 0))
	require.NotNil(t, then.ResponseCode)
	require.Equal(t, 502, *then.ResponseCode)
	require.True(t, pu, "cooldown-only 规则 punish=true（漏 Cooldown → 冷却永不生效）")

	// 带 status 规则 → punish=true（回归，归一）
	then, pu = e.Classify(ev(Kind429, http401))
	require.NotNil(t, then.ResponseCode)
	require.True(t, pu, "带 status 规则 punish=true 回归")

	// transmit-only 规则 → punish=false（回归——透传 nil/nil 不冷却，语义不变）
	then, pu = e.Classify(ev(Kind4xx, http400))
	require.Nil(t, then.ResponseCode, "transmit-only 指针 nil 透传")
	require.Nil(t, then.CustomMessage)
	require.False(t, pu, "transmit-only 规则 punish=false 回归")

	// transmit+cooldown 组合 → (nil/nil passthrough, true)（评审 O-3，指针透传+冷却）
	then, pu = e.Classify(ev(Kind4xx, http402))
	require.Nil(t, then.ResponseCode, "transmit+cooldown 透传码")
	require.True(t, pu, "transmit+cooldown 组合 punish=true")

	// 窗口条件 cooldown-only 规则 → 保守 punish=true（投递后 worker 窗口精确判，归一）
	then, pu = e.Classify(ev(Kind5xx, 500))
	require.NotNil(t, then.ResponseCode)
	require.True(t, pu, "窗口条件 cooldown-only 规则 punish=true（可能命中）")
}

// TestClassifyMessageContains message_contains 参与分类（含/不含）。
func TestClassifyMessageContains(t *testing.T) {
	e, _ := newTestEngine(t,
		domain.Rule{Name: "balance", Enabled: true, Priority: 10,
			When: domain.RuleWhen{Kind: strPtr("4xx"), ErrorMessageContains: strPtr("balance")},
			Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy)}},
	)
	ev := func(msg string) Event {
		code := 401
		return Event{AccountID: 1, Kind: Kind4xx, HTTPStatus: &code, ErrorMessage: msg}
	}
	_, pu := e.Classify(ev("no balance"))
	require.True(t, pu, "contains balance → 命中")
	_, pu = e.Classify(ev("invalid api key"))
	require.False(t, pu, "不含 balance → 不命中 → 归一")
}

// TestClassifyWindowRulePossibleHit 窗口条件规则按"可能命中"保守处理：非窗口
// 维度命中即视为命中（punish=true 保证事件投递，worker 窗口精确裁决）——
// count_failure_ge 规则不得因预判跳过导致事件不投递。
func TestClassifyWindowRulePossibleHit(t *testing.T) {
	e, _ := newTestEngine(t,
		domain.Rule{Name: "escalate", Enabled: true, Priority: 10,
			When: domain.RuleWhen{Kind: strPtr("4xx"), CountFailureGE: intPtr(5), WindowSeconds: intPtr(60)},
			Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy), ResponseCode: intPtr(502), CustomMessage: strPtr("Upstream request failed")}},
	)
	code := 401
	then, pu := e.Classify(Event{AccountID: 1, Kind: Kind4xx, HTTPStatus: &code, ErrorMessage: "x"})
	require.NotNil(t, then.ResponseCode)
	require.Equal(t, 502, *then.ResponseCode)
	require.True(t, pu, "窗口规则可能命中 → 保守 punish（投递后 worker 精确判）")
}

// TestWindowErrBucket4xx5xxNetwork 窗口计数防呆（gate r4）：枚举重构后
// Kind4xx/Kind5xx/KindNetwork 事件必须进 failure 桶——count_failure_ge 规则经
// 完整引擎路径命中（漏加 case 则静默失真）。
func TestWindowErrBucket4xx5xxNetwork(t *testing.T) {
	e, _ := newTestEngine(t, domain.Rule{
		Name: "escalate", Enabled: true, Priority: 10,
		When: domain.RuleWhen{CountFailureGE: intPtr(3), WindowSeconds: intPtr(30)},
		Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy)},
	})
	var rec recorder
	e.SetApply(rec.fn)

	// 1×5xx + 1×4xx + 1×network → failure 桶 = 3 → count_failure_ge=3 命中
	//（kind 不限——只测 failure 桶计数；漏加 case 则三类事件不进桶，永不命中）
	e.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind5xx, OccurredAt: at(0)})
	require.Empty(t, rec.get())
	e.HandleEvent(context.Background(), Event{AccountID: 1, Kind: Kind4xx, OccurredAt: at(1)})
	require.Empty(t, rec.get())
	e.HandleEvent(context.Background(), Event{AccountID: 1, Kind: KindNetwork, OccurredAt: at(2)})
	require.Len(t, rec.get(), 1, "4xx/5xx/network 三类事件全部计入 failure 桶（防呆 a）")
}

// TestClassifyModelSemantics P2-1 最终模型三面一致：ModelMapping gpt-5->gpt-5-0611 时
// when.model=gpt-5-0611 命中、when.model=gpt-5 不命中，横跨 Classify/Match/sanitizeErrLog。
// 三面中 sanitizeErrLog 复用 Classify 同一策略引擎（user/convert.go:sanitizeErrLog → rules.Classify），
// 故覆盖 Classify 即覆盖 sanitizeErrLog；Match 为 worker 精确路径。此测试固化最终模型口径：
// 事件 Model 恒为映射后最终模型（pipeline/scheduler 侧 sel.Model），规则仅按最终模型等值匹配。
func TestClassifyModelSemantics(t *testing.T) {
	finalModel := "gpt-5-0611"
	rawModel := "gpt-5"
	// Event 使用最终模型（映射后），Kind 任意——仅测 Model 维度等值匹配（大小写敏感）。
	ev := Event{AccountID: 1, Kind: Kind5xx, Model: finalModel, OccurredAt: at(0)}
	whenFinal := domain.RuleWhen{Model: strPtr(finalModel)}
	whenRaw := domain.RuleWhen{Model: strPtr(rawModel)}

	// —— Match 面：精确等值，大小写敏感 ——
	require.True(t, Match(whenFinal, ev, windowSnapshot{}), "when.model=gpt-5-0611 命中最终模型事件")
	require.False(t, Match(whenRaw, ev, windowSnapshot{}), "when.model=gpt-5 不命中最终模型 gpt-5-0611")

	// 反向：raw 事件不命中 final 规则（对称性）
	evRaw := Event{AccountID: 1, Kind: Kind5xx, Model: rawModel, OccurredAt: at(0)}
	require.False(t, Match(whenFinal, evRaw, windowSnapshot{}), "final 规则不命中 raw 事件")
	require.True(t, Match(whenRaw, evRaw, windowSnapshot{}), "raw 规则命中 raw 事件")

	// —— Classify 面（预判，与 Match 同口径，不含窗口条件，透传 sanitizeErrLog）——
	// rule when.model=gpt-5-0611 的引擎：最终模型事件应 punish=true
	eFinal, _ := newTestEngine(t, domain.Rule{
		Name: "final-model", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Model: strPtr(finalModel)},
		Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy)},
	})
	_, pu := eFinal.Classify(ev)
	require.True(t, pu, "Classify: when.model=gpt-5-0611 命中最终模型 → punish")

	// 同引擎对 raw 事件不命中
	_, pu = eFinal.Classify(evRaw)
	require.False(t, pu, "Classify: when.model=gpt-5-0611 不命中 raw 模型")

	// rule when.model=gpt-5 的引擎：最终模型事件不命中
	eRaw, _ := newTestEngine(t, domain.Rule{
		Name: "raw-model", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Model: strPtr(rawModel)},
		Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy)},
	})
	_, pu = eRaw.Classify(ev)
	require.False(t, pu, "Classify: when.model=gpt-5 不命中最终模型 gpt-5-0611")

	_, pu = eRaw.Classify(evRaw)
	require.True(t, pu, "Classify: when.model=gpt-5 命中 raw 模型")

	// 空 Model 事件不命中任意 model 限定规则
	evEmpty := Event{AccountID: 1, Kind: Kind5xx, OccurredAt: at(0)}
	require.False(t, Match(whenFinal, evEmpty, windowSnapshot{}), "空 Model 不命中 final 规则")
	require.False(t, Match(whenRaw, evEmpty, windowSnapshot{}), "空 Model 不命中 raw 规则")
}
