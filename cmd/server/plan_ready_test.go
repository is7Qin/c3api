// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
)

// readyRuleStore 空规则表（构造 rule.New 只建结构，本测试不触达存储面）。
type readyRuleStore struct{}

func (readyRuleStore) ListRules(context.Context, *bool) ([]domain.Rule, error) { return nil, nil }
func (readyRuleStore) CreateRule(context.Context, domain.Rule) (int64, error)  { return 0, nil }
func (readyRuleStore) UpdateRule(context.Context, domain.Rule) error           { return nil }
func (readyRuleStore) DeleteRule(context.Context, int64) error                 { return nil }
func (readyRuleStore) DeleteRulesBatch(context.Context, []int64) error         { return nil }
func (readyRuleStore) CountRules(context.Context) (int64, error)               { return 0, nil }

type readyLoader struct{}

func (readyLoader) LoadGroupsAccounts(context.Context) (map[int64][]*domain.Account, error) {
	return nil, nil
}
func (readyLoader) LoadGroupAccounts(context.Context, int64) ([]*domain.Account, error) {
	return nil, nil
}

func newReadyScheduler(t *testing.T) *scheduler.Scheduler {
	t.Helper()
	re := rule.New(rule.Config{}, readyRuleStore{}, nil)
	return scheduler.New(scheduler.Config{DefaultMaxConcurrency: 4, SyncInterval: time.Hour}, readyLoader{}, re, nil)
}

// 冷启动就绪门：编译计划（DecisionView）发布前 AI 流量 503 拒入，发布后
// 放行——plan-only 选号面的启动契约（无计划即无 AI 派生）。
func TestPlanReadyGate_blocksAITrafficUntilCompiledPlanPublished(t *testing.T) {
	sched := newReadyScheduler(t)
	served := false
	gate := planReadyGate(sched, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served = true
		w.WriteHeader(http.StatusOK)
	}))

	// Given: 视图根未发布（冷启动，快照尚未装载）
	w := httptest.NewRecorder()
	gate.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	require.Equal(t, http.StatusServiceUnavailable, w.Code, "no view means no AI traffic")
	require.Equal(t, "1", w.Header().Get("Retry-After"))
	require.False(t, served)

	// Given: 静态快照已装载但编译计划未发布（reload 先行、compile 车道滞后）
	require.NoError(t, sched.InvalidateAllSync())
	w = httptest.NewRecorder()
	gate.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	require.Equal(t, http.StatusServiceUnavailable, w.Code, "static snapshot without a compiled plan must not admit AI traffic")
	require.False(t, served)

	// When: 编译计划发布（经唯一 publisher 缝）
	sched.PublishDecisionForTest(scheduler.RouteRefFor(1, string(domain.FormatOpenAIChat), ""), &scheduler.RouteDecision{})

	// Then: AI 流量放行
	w = httptest.NewRecorder()
	gate.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	require.Equal(t, http.StatusOK, w.Code)
	require.True(t, served, "a published compiled plan admits AI traffic")
}

// main.go 装配契约：AIHandler 必须经 planReadyGate 包装（冷启动不裸放行）。
func TestMainWiresPlanReadyGate(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok)
	src, err := os.ReadFile(filepath.Join(filepath.Dir(file), "main.go"))
	require.NoError(t, err)
	require.Contains(t, string(src), "AIHandler:         planReadyGate(sched, aiRouter)", "AI surface must be gated on compiled-plan readiness")
}
