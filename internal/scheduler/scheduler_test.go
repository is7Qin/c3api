// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/rule"
)

// --- 测试 Loader（内存实现） ---

type memLoader struct {
	mu      sync.Mutex
	byGroup map[int64][]*domain.Account
}

func newMemLoader(byGroup map[int64][]*domain.Account) *memLoader {
	return &memLoader{byGroup: byGroup}
}

func (m *memLoader) LoadGroupsAccounts(ctx context.Context) (map[int64][]*domain.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[int64][]*domain.Account, len(m.byGroup))
	for k, v := range m.byGroup {
		out[k] = v
	}
	return out, nil
}

func (m *memLoader) LoadGroupAccounts(ctx context.Context, id int64) ([]*domain.Account, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.byGroup[id], nil
}

func testCfg() Config {
	return Config{
		DefaultMaxConcurrency: 2,
		SyncInterval:          100 * time.Hour, // 测试中不触发定时同步
	}
}

// fakeRuleStore 内存 RuleStore：种子写入 + 列表查询（值语义副本）。
type fakeRuleStore struct {
	mu    sync.Mutex
	rules map[int64]domain.Rule
	next  int64
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
	f.rules[r.ID] = r
	return nil
}

func (f *fakeRuleStore) DeleteRule(ctx context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rules, id)
	return nil
}

func (f *fakeRuleStore) DeleteRulesBatch(ctx context.Context, ids []int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
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

var _ repository.RuleStore = (*fakeRuleStore)(nil)

func intPtr(v int) *int       { return &v }
func strPtr(s string) *string { return &s }

func tpl(id int64, format domain.RequestFormat, models []string) *domain.Template {
	return &domain.Template{ID: id, BaseURL: "https://u/v1", CredentialType: credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{format}, Models: models}
}

func acc(id int64, t *domain.Template, maxConc int) *domain.Account {
	return &domain.Account{ID: id, TemplateID: t.ID, Template: t, UpstreamKey: "k", Enabled: true, MaxConcurrency: maxConc, LifecycleRevision: 1}
}

// newSched 构造已加载快照且已武装编译道的调度器：reload 产出静态视图，
// wireSources+compileOnce 同步产出 DecisionView——Select 走编译计划（未编译
// 路由直接 ErrFormatUnavailable）。
func newSched(t *testing.T, m *memLoader) *Scheduler {
	t.Helper()
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background()))
	s := New(testCfg(), m, re, nil)
	require.NoError(t, s.reload(context.Background()))
	wireSources(s, nil, nil)
	s.compileOnce()
	return s
}

// newTestScheduler 用给定账号（固定放入组 10）构建已加载快照的调度器。
func newTestScheduler(t *testing.T, accs []*domain.Account) *Scheduler {
	t.Helper()
	return newSched(t, newMemLoader(map[int64][]*domain.Account{10: accs}))
}

// newSchedStatic 仅加载静态视图、不武装编译道——编译车道测试用（自行
// wireSources+compileOnce 验证未编译→已编译的转换）。
func newSchedStatic(t *testing.T, m *memLoader) *Scheduler {
	t.Helper()
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background()))
	s := New(testCfg(), m, re, nil)
	require.NoError(t, s.reload(context.Background()))
	return s
}

// newTestSchedulerStatic 静态视图版 newTestScheduler（组 10）。
func newTestSchedulerStatic(t *testing.T, accs []*domain.Account) *Scheduler {
	t.Helper()
	return newSchedStatic(t, newMemLoader(map[int64][]*domain.Account{10: accs}))
}

func TestSelectFormatHardFilter(t *testing.T) {
	chat := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"})
	ant := tpl(2, domain.FormatAnthropic, []string{"claude"})
	m := newMemLoader(map[int64][]*domain.Account{
		10: {acc(1, chat, 4), acc(2, ant, 4)},
	})
	s := newSched(t, m)

	// anthropic 路径下只命中 anthropic 模板账号
	sel, err := s.Select(10, domain.FormatAnthropic, "claude")
	require.NoError(t, err)
	require.Equal(t, int64(2), sel.AccountID)
	s.Release(sel.AccountID)

	// 格式不匹配（组内只有 chat 模板）→ ErrFormatUnavailable
	m2 := newMemLoader(map[int64][]*domain.Account{10: {acc(1, chat, 4)}})
	s2 := newSched(t, m2)
	_, err = s2.Select(10, domain.FormatOpenAIResponses, "gpt-4o")
	require.ErrorIs(t, err, ErrFormatUnavailable)
}

// TestSelectCredentialTypeFromTemplate 钉死：Selection.CredentialType 只来自
// 模板（账号级无该字段；一个模板 = 一种号池）。
func TestSelectCredentialTypeFromTemplate(t *testing.T) {
	codexTpl := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"})
	codexTpl.CredentialType = credential.TypeCodexOAuth
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, codexTpl, 4)}})
	s := newSched(t, m)
	sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, credential.TypeCodexOAuth, sel.CredentialType, "类型随模板传播")
	s.Release(sel.AccountID)

	// api_key 默认模板 → Selection 携带 api_key（行为不变路径）
	s2 := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k", Enabled: true, MaxConcurrency: 4, LifecycleRevision: 1},
	})
	sel2, err := s2.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, credential.TypeAPIKey, sel2.CredentialType, "默认模板类型 api_key 传播到 Selection")
}

// TestSelectModelPreference 钉死白名单命中优先：同格式两账号，一个 Serves(model)
// 一个带模型空间但不含该模型（白名单账号未命中→不进候选），Select 只命中前者。
func TestSelectModelPreference(t *testing.T) {
	tA := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"})
	tB := tpl(2, domain.FormatOpenAIChat, []string{"other"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tA, 4), acc(2, tB, 4)}})
	s := newSched(t, m)
	sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.AccountID, "model preference tier")
	s.Release(sel.AccountID)
}

func TestConcurrencyLimit(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 1)}})
	s := newSched(t, m)
	sel1, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	_, err = s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrAttemptsExhausted)
	s.Release(sel1.AccountID)
	_, err = s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err, "available after release")
}

func TestSelectUnknownGroup(t *testing.T) {
	m := newMemLoader(map[int64][]*domain.Account{})
	s := newSched(t, m)
	_, err := s.Select(99, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrGroupNotFound)
}

// TestSelectNilStoreNoPanic 快照未加载（首刷失败）时 Select 优雅失败而非 panic。
func TestSelectNilStoreNoPanic(t *testing.T) {
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	s := New(testCfg(), newMemLoader(nil), re, nil) // 不 reload：模拟首刷失败
	_, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrGroupNotFound)
}

func TestInvalidateGroupReloads(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)
	m.mu.Lock()
	m.byGroup[10] = append(m.byGroup[10], acc(2, tplx, 4))
	m.mu.Unlock()
	s.InvalidateGroup(10) // 同步 reload
	s.compileOnce()       // 组级重载后重编译决策视图
	// 两账号并发上限各 4，不释放地连续选 5 次必须全部成功，且两账号都至少被选中一次。
	var sels []*Selection
	for i := 0; i < 5; i++ {
		sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
		require.NoError(t, err, "5 selects must succeed with both accounts in pool")
		sels = append(sels, sel)
	}
	var has1, has2 bool
	for _, sel := range sels {
		has1 = has1 || sel.AccountID == 1
		has2 = has2 || sel.AccountID == 2
	}
	require.True(t, has1 && has2, "both accounts should serve")
	for _, sel := range sels {
		s.Release(sel.AccountID)
	}
}

// TestInvalidateGroupByIDRebuild 回归：InvalidateGroup 后 byID 必须与 groups 同步重建。
func TestInvalidateGroupByIDRebuild(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 2), acc(2, tplx, 2)}})
	s := newSched(t, m)

	m.mu.Lock()
	m.byGroup[10] = []*domain.Account{acc(2, tplx, 2), acc(3, tplx, 2)}
	m.mu.Unlock()
	s.InvalidateGroup(10)
	s.compileOnce()

	// 占满并发：两账号各上限 2、总容量 4 → 4 次选择后各持 2 个槽。
	var sels []*Selection
	for i := 0; i < 4; i++ {
		sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
		require.NoError(t, err)
		sels = append(sels, sel)
	}
	for _, sel := range sels {
		s.Release(sel.AccountID)
	}
	ri, ok := s.Runtime(2)
	require.True(t, ok)
	require.Equal(t, int64(0), ri.Concurrency, "retained account release hits the new snapshot")
	ri, ok = s.Runtime(3)
	require.True(t, ok, "added account must be in byID")
	require.Equal(t, int64(0), ri.Concurrency, "added account release hits the new snapshot")

	// 被移除账号 1：Runtime 不可见，MarkResult/Release 安全 no-op。
	_, ok = s.Runtime(1)
	require.False(t, ok, "removed account must not be in byID")
	s.MarkResult(1, rule.Kind5xx, nil, 500, "", "")
	s.FlushRules()
	s.Release(1)

	require.NoError(t, s.Close(context.Background()))
}

// TestInvalidateGroupShrinkByID 回归：组内账号收缩时保留账号 byID 指向新快照，
// 被移除账号消失。
func TestInvalidateGroupShrinkByID(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{11: {acc(4, tplx, 2), acc(5, tplx, 2)}})
	s := newSched(t, m)

	m.mu.Lock()
	m.byGroup[11] = []*domain.Account{acc(4, tplx, 1)} // 5 移除；4 的并发上限 2→1
	m.mu.Unlock()
	s.InvalidateGroup(11)
	s.compileOnce()

	sel, err := s.Select(11, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, int64(4), sel.AccountID)
	ri, ok := s.Runtime(4)
	require.True(t, ok)
	require.Equal(t, int64(1), ri.Concurrency, "select hits the new snapshot (max 1)")
	s.Release(sel.AccountID)
	ri, _ = s.Runtime(4)
	require.Equal(t, int64(0), ri.Concurrency, "release hits the new snapshot")

	_, ok = s.Runtime(5)
	require.False(t, ok, "removed account must not be in byID")
	s.MarkResult(5, rule.KindNetwork, nil, 0, "", "")
	s.FlushRules()
	s.Release(5)

	require.NoError(t, s.Close(context.Background()))
}

// TestMarkResultDisabledStaysDisabled 回归：失效/禁用账号（快照 disabled）在途
// 请求的 MarkResult 不得投递事件（防复活守卫同步短路），也不得被选中。
func TestMarkResultDisabledStaysDisabled(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)

	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)

	// 管理端禁用：以 failed_at 置位重载组快照（runtimeStatusFor → disabled）。
	m.mu.Lock()
	failed := time.Date(2026, 8, 7, 11, 0, 0, 0, time.UTC)
	m.byGroup[10] = []*domain.Account{{
		ID: 1, TemplateID: 1, Template: tplx, UpstreamKey: "k",
		Enabled: true, FailedAt: &failed, MaxConcurrency: 4, LifecycleRevision: 1,
	}}
	m.mu.Unlock()
	s.InvalidateGroup(10)
	s.compileOnce()
	ri, ok := s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri.Status, "failed_at 装载即 disabled")

	// 在途请求完成：OK/429/5xx 均不得投递事件、不得改写 disabled。
	s.MarkResult(1, rule.KindOK, nil, 0, "", "")
	s.MarkResult(1, rule.Kind429, nil, 0, "", "")
	s.MarkResult(1, rule.Kind5xx, nil, 500, "", "")
	s.FlushRules()
	ri, _ = s.Runtime(1)
	require.Equal(t, domain.StatusDisabled, ri.Status, "禁用账号不得被事件复活")

	// 禁用账号不可再被选中
	_, err = s.Select(10, domain.FormatOpenAIChat, "m")
	require.Error(t, err)
	s.Release(sel.AccountID)

	require.NoError(t, s.Close(context.Background()))
}

// TestWorkerContract 满足 worker.Worker 契约：Name + 幂等 Start。
func TestWorkerContract(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)

	require.Equal(t, "scheduler", s.Name())

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	require.NoError(t, s.Start(ctx))
	require.EqualError(t, s.Start(ctx), "scheduler: already started")
}

// tplWith 构造可指纹化的模板（api_key + 非空 base_url）。需要"指纹不可派生"
// 语义的测试自行构造裸模板。
func tplWith(ff domain.RequestFormat, models []string) *domain.Template {
	return &domain.Template{BaseURL: "https://u/v1", CredentialType: credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{ff}, Models: models}
}

func TestBuildRoutesBucketsAndDefault(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"gpt-4o", "gpt-4o-mini"})
	pool := []*accountSnapshot{newAccountSnapshot(&snapshotStatic{acc: domain.Account{ID: 1}, tpl: tpl}, &accState{status: domain.StatusActive})}
	routes := buildRoutes(pool)
	// 已知模型桶
	_, ok := routes[routeKey{domain.FormatOpenAIChat, "gpt-4o"}]
	require.True(t, ok, "gpt-4o 在 models 里 → 建桶")
	// 默认桶（未知模型回落）：白名单账号（有模型空间）不进默认桶 → 无默认路由
	_, ok = routes[routeKey{domain.FormatOpenAIChat, ""}]
	require.False(t, ok, "默认桶仅含全模型账号，白名单账号被排除")
	// 其他格式无桶
	_, ok = routes[routeKey{domain.FormatAnthropic, "gpt-4o"}]
	require.False(t, ok)
}

func TestBuildRoutesFormatModelsLimit(t *testing.T) {
	tpl := &domain.Template{
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatAnthropic},
		Models:           []string{"gpt-4o", "special"},
		FormatModels:     map[domain.RequestFormat][]string{domain.FormatAnthropic: {"special"}},
	}
	pool := []*accountSnapshot{newAccountSnapshot(&snapshotStatic{acc: domain.Account{ID: 1}, tpl: tpl}, &accState{status: domain.StatusActive})}
	routes := buildRoutes(pool)
	// anthropic 只支持 special（format_models 限制）→ special 有桶
	_, ok := routes[routeKey{domain.FormatAnthropic, "special"}]
	require.True(t, ok, "FormatModels 配置格式 → special 模型走 anthropic 桶")
	// gpt-4o 不在 anthropic 的 format_models 列表 → 该组合无桶
	_, ok = routes[routeKey{domain.FormatAnthropic, "gpt-4o"}]
	require.False(t, ok, "gpt-4o ∉ FormatModels[anthropic] → 格式不支持该模型")
	// chat 未配置 format_models → 全部模型
	_, ok = routes[routeKey{domain.FormatOpenAIChat, "gpt-4o"}]
	require.True(t, ok, "未配置格式 → 全部模型")
	// responses 不在 supported → 无桶
	_, ok = routes[routeKey{domain.FormatOpenAIResponses, "special"}]
	require.False(t, ok, "格式不在 supported → 无桶")
}

// —— 模板模型硬白名单（用户裁决 2026-08-18）：Serves 未命中 + 白名单账号 →
// 404；全模型账号（无模型空间）保留默认桶兜底 ——

func TestSelectWhitelistHitMiss(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k1", Enabled: true, MaxConcurrency: 1000, LifecycleRevision: 1},
	})
	sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.AccountID)
	s.Release(sel.AccountID)
	_, err = s.Select(10, domain.FormatOpenAIChat, "claude-3-5-sonnet-20241022")
	require.ErrorIs(t, err, ErrFormatUnavailable, "白名单外模型 → 404")
}

// TestSelectFormatModelsOnlyBoundary 评审 M-1：Models=[] + FormatModels={chat:[gpt-4o]}
// + supported_formats 含 anthropic 的账号——anthropic 格式任意模型 → 404。
func TestSelectFormatModelsOnlyBoundary(t *testing.T) {
	tplFm := &domain.Template{
		BaseURL: "https://u/v1", CredentialType: credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatAnthropic},
		FormatModels:     map[domain.RequestFormat][]string{domain.FormatOpenAIChat: {"gpt-4o"}},
	}
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplFm, UpstreamKey: "k1", Enabled: true, MaxConcurrency: 1000, LifecycleRevision: 1},
	})
	_, err := s.Select(10, domain.FormatAnthropic, "claude-3-5-sonnet-20241022")
	require.ErrorIs(t, err, ErrFormatUnavailable, "FormatModels-only 账号在未列模型格式上不建路由 → 404")
	sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.AccountID)
	s.Release(sel.AccountID)
}

// TestSelectFormatModelsEmptyList 评审 Minor ② 防回归：FormatModels={chat:[]}
// （覆盖但空列表）→ 该格式全 404。
func TestSelectFormatModelsEmptyList(t *testing.T) {
	tplFm := &domain.Template{
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		FormatModels:     map[domain.RequestFormat][]string{domain.FormatOpenAIChat: {}},
	}
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplFm, UpstreamKey: "k1", Enabled: true, MaxConcurrency: 1000, LifecycleRevision: 1},
	})
	for _, m := range []string{"gpt-4o", "unknown-model-xyz"} {
		_, err := s.Select(10, domain.FormatOpenAIChat, m)
		require.ErrorIs(t, err, ErrFormatUnavailable, "空列表格式（覆盖但空）→ 全 404")
	}
}

// TestSelectMappingKeyWhitelist 评审 O-5：mapping key 命中 → 选中；映射目标不复查。
func TestSelectMappingKeyWhitelist(t *testing.T) {
	tplMap := &domain.Template{
		BaseURL: "https://u/v1", CredentialType: credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		ModelMapping:     map[string]domain.ModelMappingEntry{"gpt-4o": {MappedModel: "deepseek-chat", Mode: domain.ModelMappingModeExplicit}},
	}
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplMap, UpstreamKey: "k1", Enabled: true, MaxConcurrency: 1000, LifecycleRevision: 1},
	})
	sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.AccountID)
	require.Equal(t, "deepseek-chat", sel.Model)
	s.Release(sel.AccountID)
	_, err = s.Select(10, domain.FormatOpenAIChat, "deepseek-chat")
	require.ErrorIs(t, err, ErrFormatUnavailable, "映射目标（上游模型名）不复查")
}

// TestSelectFullModelTier2Fallback 全模型账号（模型空间空）→ 默认桶兜底转发。
func TestSelectFullModelTier2Fallback(t *testing.T) {
	tplOpen := &domain.Template{BaseURL: "https://u/v1", CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}}
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplOpen, UpstreamKey: "k1", Enabled: true, MaxConcurrency: 1000, LifecycleRevision: 1},
	})
	for _, m := range []string{"any-model-1", "gpt-4o", "claude-3-5-sonnet-20241022"} {
		sel, err := s.Select(10, domain.FormatOpenAIChat, m)
		require.NoError(t, err, "全模型账号：任意模型 200（默认桶兜底保留）")
		require.Equal(t, int64(1), sel.AccountID)
		s.Release(sel.AccountID)
	}
}

// TestSelectDefaultBucketExcludesWhitelist 默认桶不含白名单账号：未知模型 + 组内
// 仅白名单账号 → 404。
func TestSelectDefaultBucketExcludesWhitelist(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k1", Enabled: true, MaxConcurrency: 1000, LifecycleRevision: 1},
	})
	_, err := s.Select(10, domain.FormatOpenAIChat, "unknown-model-xyz")
	require.ErrorIs(t, err, ErrFormatUnavailable, "默认桶不含白名单账号 → 未知模型 404")
}

// TestSelectMixedGroupWhitelistFullModel 混合组：A 白名单 ["gpt-4o"] + B 全模型——
// 请求 gpt-4o 两账号都在候选（A Serves、B 全模型兜底）；白名单外模型 → 仅 B。
func TestSelectMixedGroupWhitelistFullModel(t *testing.T) {
	tplOpen := &domain.Template{ID: 2, BaseURL: "https://u/v1", CredentialType: credential.TypeAPIKey, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}}
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k1", Enabled: true, MaxConcurrency: 1000, LifecycleRevision: 1},
		{ID: 2, TemplateID: 2, Template: tplOpen, UpstreamKey: "k2", Enabled: true, MaxConcurrency: 1000, LifecycleRevision: 1},
	})
	// gpt-4o → 候选含 A（Serves）与 B（全模型兜底）；选中任一即可
	sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	require.Contains(t, []int64{1, 2}, sel.AccountID)
	s.Release(sel.AccountID)
	// 白名单外模型 → 仅 B（默认桶仅全模型账号）
	sel, err = s.Select(10, domain.FormatOpenAIChat, "claude-3-5-sonnet-20241022")
	require.NoError(t, err)
	require.Equal(t, int64(2), sel.AccountID, "白名单外模型 → 默认桶（仅全模型账号）")
	s.Release(sel.AccountID)
}

// 并发 CAS 竞争：单账号两并发 Select，恰一成功、另一返回错误。
func TestSelectConcurrentCASRace(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k1", Enabled: true, MaxConcurrency: 1, LifecycleRevision: 1},
	})
	type pairResult struct {
		sel *Selection
		err error
	}
	const pairs = 200
	for i := 0; i < pairs; i++ {
		start := make(chan struct{})
		ready := make(chan struct{}, 2)
		results := make(chan pairResult, 2)
		var wg sync.WaitGroup
		for j := 0; j < 2; j++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ready <- struct{}{}
				<-start
				sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
				results <- pairResult{sel: sel, err: err}
			}()
		}
		<-ready
		<-ready
		close(start)
		done := make(chan struct{})
		go func() {
			wg.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("iter %d: pair did not finish in time — CAS loser must return promptly, not spin", i)
		}
		close(results)
		var winner *Selection
		okCount, failCount := 0, 0
		for r := range results {
			switch {
			case r.err == nil:
				okCount++
				winner = r.sel
			case errors.Is(r.err, ErrNoAvailable) || errors.Is(r.err, ErrAttemptsExhausted):
				failCount++
			default:
				t.Fatalf("iter %d: unexpected error: %v", i, r.err)
			}
		}
		require.Equal(t, 1, okCount, "iter %d: exactly one success per pair", i)
		require.Equal(t, 1, failCount, "iter %d: loser gets no-available (never two successes)", i)
		require.NotNil(t, winner, "iter %d: winner carries a selection", i)
		s.Release(winner.AccountID)
	}
}

// TestReloadPreservesInFlightConcurrency：跨 reload 的在途请求 Release 后计数不得为负。
func TestReloadPreservesInFlightConcurrency(t *testing.T) {
	chat := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"})
	s := newTestScheduler(t, []*domain.Account{acc(1, chat, 4)})

	sel1, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	sel2, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)

	require.NoError(t, s.reload(context.Background()))

	s.Release(sel1.AccountID)
	s.Release(sel2.AccountID)

	ri, ok := s.Runtime(sel1.AccountID)
	require.True(t, ok)
	require.Equal(t, int64(0), ri.Concurrency, "reload 后并发计数必须回到 0，不得为负")
	require.GreaterOrEqual(t, ri.Concurrency, int64(0))

	s3, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	ri2, _ := s.Runtime(sel1.AccountID)
	require.Equal(t, int64(1), ri2.Concurrency, "继承后新请求占槽计数为 1")
	s.Release(s3.AccountID)
}

// TestMultiGroupSharedInstance 回归（O2 实证修复）：多组账号必须共享同一
// accountSnapshot 实例——Select（经组路由）与 Release（经 byID）命中同一计数器。
func TestMultiGroupSharedInstance(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	a := acc(1, tplx, 2)
	m := newMemLoader(map[int64][]*domain.Account{10: {a}, 11: {a}})
	s := newSched(t, m)

	byID := s.View().ByID()
	groups := s.View().Groups()
	require.Same(t, byID[1], groups[10].accounts[0], "组 10 路由与 byID 共享实例")
	require.Same(t, byID[1], groups[11].accounts[0], "组 11 路由与 byID 共享实例")
	require.ElementsMatch(t, []int64{10, 11}, byID[1].static.Load().groupIDs, "跨组引用集登记完整")

	sel1, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	sel2, err := s.Select(11, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel1.AccountID)
	require.Equal(t, int64(1), sel2.AccountID)
	ri, _ := s.Runtime(1)
	require.Equal(t, int64(2), ri.Concurrency, "共享实例：跨组计数合并，不得分裂")

	_, err = s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrAttemptsExhausted, "共享实例：真实槽位满")

	s.Release(sel1.AccountID)
	s.Release(sel2.AccountID)
	ri, _ = s.Runtime(1)
	require.Equal(t, int64(0), ri.Concurrency, "释放后计数归零")

	sel3, err := s.Select(11, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel3.AccountID)
	s.Release(sel3.AccountID)
}

// TestInvalidateGroupMultiGroupShared 组级重载的共享实例纪律。
func TestInvalidateGroupMultiGroupShared(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{
		10: {acc(1, tplx, 2)},
		11: {acc(1, tplx, 2)},
	})
	s := newSched(t, m)

	m.mu.Lock()
	m.byGroup[10] = []*domain.Account{acc(1, tplx, 1)}
	m.mu.Unlock()
	s.InvalidateGroup(10)
	s.compileOnce()

	byID := s.View().ByID()
	groups := s.View().Groups()
	require.Same(t, byID[1], groups[10].accounts[0], "重载组路由 → 新实例")
	require.Same(t, byID[1], groups[11].accounts[0], "其它组引用 → 新实例（共享纪律）")

	sel1, err := s.Select(11, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel1.AccountID)
	ri, _ := s.Runtime(1)
	require.Equal(t, int64(1), ri.Concurrency, "经其它组路由命中新实例（新上限 1）")
	_, err = s.Select(11, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrAttemptsExhausted, "新实例真实槽位满")
	s.Release(sel1.AccountID)
	ri, _ = s.Runtime(1)
	require.Equal(t, int64(0), ri.Concurrency, "Release 经 byID 命中新实例")

	sel2, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel2.AccountID)
	s.Release(sel2.AccountID)
}

// TestInvalidateGroupMultiGroupRemove 从组移除的多组账号处理。
func TestInvalidateGroupMultiGroupRemove(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{
		10: {acc(1, tplx, 2)},
		11: {acc(1, tplx, 2)},
	})
	s := newSched(t, m)

	m.mu.Lock()
	m.byGroup[10] = []*domain.Account{}
	m.mu.Unlock()
	s.InvalidateGroup(10)
	s.compileOnce()

	byID := s.View().ByID()
	groups := s.View().Groups()
	_, ok := byID[1]
	require.True(t, ok, "仍属其它组 → byID 保留")
	require.Equal(t, []int64{11}, byID[1].static.Load().groupIDs, "本组引用已摘除")
	require.Empty(t, groups[10].accounts, "组 10 已空")
	require.Len(t, groups[11].accounts, 1, "组 11 引用保留")
	require.Same(t, byID[1], groups[11].accounts[0], "组 11 路由仍指向共享实例")

	_, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.Error(t, err, "空组不可选号")
	sel, err := s.Select(11, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.AccountID)
	s.Release(sel.AccountID)
	ri, _ := s.Runtime(1)
	require.Equal(t, int64(0), ri.Concurrency, "Release 经 byID 命中保留实例")

	m.mu.Lock()
	m.byGroup[11] = []*domain.Account{}
	m.mu.Unlock()
	s.InvalidateGroup(11)
	// Atomic publication: the staged removal pairs on the next compile.
	s.compileOnce()
	_, ok = s.View().ByID()[1]
	require.False(t, ok, "不再属于任何组 → 从 byID 删除")
	_, ok = s.Runtime(1)
	require.False(t, ok)
	s.Release(1) // no-op 安全
}

// countingLoader 计数 Loader 包装（热路径零 DB 断言）。
type countingLoader struct {
	mu    sync.Mutex
	inner Loader
	loads int
}

func (c *countingLoader) LoadGroupsAccounts(ctx context.Context) (map[int64][]*domain.Account, error) {
	c.mu.Lock()
	c.loads++
	c.mu.Unlock()
	return c.inner.LoadGroupsAccounts(ctx)
}

func (c *countingLoader) LoadGroupAccounts(ctx context.Context, id int64) ([]*domain.Account, error) {
	c.mu.Lock()
	c.loads++
	c.mu.Unlock()
	return c.inner.LoadGroupAccounts(ctx, id)
}

func (c *countingLoader) loadsN() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loads
}

// TestSelectCarriesAccountExt 账号 ext 快照 → Selection.Ext。
func TestSelectCarriesAccountExt(t *testing.T) {
	ext := &domain.AccountExt{
		AccountID: 7, CredentialType: credential.TypeCodexOAuth,
		CodexIdentity: &domain.CodexIdentity{InstallationID: "inst-1", SessionID: "s1", ThreadID: "t1"},
	}
	tpl := &domain.Template{ID: 1, BaseURL: "", CredentialType: credential.TypeCodexOAuth,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS}, Models: []string{"gpt-4o"}}
	a := acc(7, tpl, 4)
	a.Ext = ext
	s := newTestScheduler(t, []*domain.Account{a})
	sel, err := s.Select(10, domain.FormatOpenAIResponsesWS, "gpt-4o")
	require.NoError(t, err)
	require.Same(t, ext, sel.Ext, "Selection.Ext = 快照账号 Ext（指针复制零拷贝）")
	require.Equal(t, credential.TypeCodexOAuth, sel.CredentialType)
	s.Release(sel.AccountID)
	s.MarkResult(sel.AccountID, rule.KindOK, nil, 200, "", "")
}

// TestRequestPathZeroLoaderCalls 热路径零 DB：加载只发生在快照加载期。
func TestRequestPathZeroLoaderCalls(t *testing.T) {
	tpl := tpl(1, domain.FormatOpenAIResponsesWS, []string{"gpt-4o"})
	inner := newMemLoader(map[int64][]*domain.Account{10: {acc(7, tpl, 4)}})
	cl := &countingLoader{inner: inner}
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background()))
	s := New(testCfg(), cl, re, nil)
	require.NoError(t, s.reload(context.Background()))
	wireSources(s, nil, nil)
	s.compileOnce()
	before := cl.loadsN()
	for i := 0; i < 50; i++ {
		sel, err := s.Select(10, domain.FormatOpenAIResponsesWS, "gpt-4o")
		require.NoError(t, err)
		s.Release(sel.AccountID)
		s.MarkResult(sel.AccountID, rule.KindOK, nil, 200, "", "")
	}
	require.Equal(t, before, cl.loadsN(), "请求期（Select/MarkResult/Release）零加载器触达——热路径零 DB")
}

// —— RuleKindOf 单点分流（gate r3/r5） ——

func TestRuleKindOf(t *testing.T) {
	cases := []struct {
		name string
		code int
		want rule.Kind
	}{
		{"0 → network", 0, rule.KindNetwork},
		{"500 → 5xx", 500, rule.Kind5xx},
		{"503 → 5xx", 503, rule.Kind5xx},
		{"400 防御 → 5xx", 400, rule.Kind5xx},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, RuleKindOf(tc.code))
		})
	}
}

// TestSchedulerClassify scheduler.Classify 包装：快照取 TemplateID/GroupID 后
// 委托引擎；then.ResponseCode nil=透码，CustomMessage nil=透文；快照外账号 → (domain.RuleThen{}, false)。
func TestSchedulerClassify(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIResponsesWS, []string{"gpt-4o"})
	m := newMemLoader(map[int64][]*domain.Account{10: {
		{ID: 1, TemplateID: 1, Template: tplx, UpstreamKey: "k1", Enabled: true, MaxConcurrency: 1000, LifecycleRevision: 1},
	}})
	rstore := &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}
	http400, http401 := 400, 401
	dur := int64(30 * 60 * 1000)
	_, err := rstore.CreateRule(context.Background(), domain.Rule{
		Name: "transmit-400", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Kind: strPtr("4xx"), HTTPStatus: &http400},
		Then: domain.RuleThen{},
	})
	require.NoError(t, err)
	_, err = rstore.CreateRule(context.Background(), domain.Rule{
		Name: "punish-401", Enabled: true, Priority: 20,
		When: domain.RuleWhen{Kind: strPtr("4xx"), HTTPStatus: &http401},
		Then: domain.RuleThen{Throttle: &domain.ThrottleAction{Scope: domain.ThrottleScopeAccount, Mode: domain.ThrottleModeOpen, DurationMs: &dur}, ResponseCode: intPtr(502), CustomMessage: strPtr("upstream rejected request")},
	})
	require.NoError(t, err)
	re := rule.New(rule.Config{}, rstore, nil)
	require.NoError(t, re.Reload(context.Background()))
	s := New(testCfg(), m, re, nil)
	require.NoError(t, s.reload(context.Background()))

	// 400 → 全透
	then, pu := s.Classify(rule.Event{AccountID: 1, Kind: rule.Kind4xx, HTTPStatus: &http400})
	require.Nil(t, then.ResponseCode)
	require.Nil(t, then.CustomMessage)
	require.False(t, pu)
	// 401 → punish（throttle），ResponseCode 502 覆写
	then, pu = s.Classify(rule.Event{AccountID: 1, Kind: rule.Kind4xx, HTTPStatus: &http401})
	require.NotNil(t, then.ResponseCode)
	require.Equal(t, 502, *then.ResponseCode)
	require.True(t, pu)
	// 快照外账号 → (domain.RuleThen{}, false)
	then, pu = s.Classify(rule.Event{AccountID: 999, Kind: rule.Kind4xx, HTTPStatus: &http401})
	require.Nil(t, then.ResponseCode)
	require.Nil(t, then.CustomMessage)
	require.False(t, pu)
}

// --- 快照重建复用旧实例（计数器连续性，2026-08-18） ---

// reuseByID 取当前 byID 快照中账号的实例。
func reuseByID(s *Scheduler, id int64) *accountSnapshot {
	return s.View().ByID()[id]
}

// TestReuseConcurrencyContinuity 复用后 concurrency 保留且新请求 CAS 连续。
func TestReuseConcurrencyContinuity(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)

	sel1, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	sel2, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	before := reuseByID(s, 1)

	require.NoError(t, s.reload(context.Background()))
	after := reuseByID(s, 1)
	require.Same(t, before, after, "复用实例指针不变")
	require.Equal(t, int64(2), after.runtime.concurrency.Load(), "重建后计数保持")

	s.Release(sel1.AccountID)
	s.Release(sel2.AccountID)
	require.Equal(t, int64(0), after.runtime.concurrency.Load(), "释放归零，不得为负")

	sel3, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, int64(1), after.runtime.concurrency.Load(), "重建后新请求在原子计数上连续 +1")
	s.Release(sel3.AccountID)
}

// TestReuseSyncsStaticFieldsFromDB 静态字段 DB 权威同步：管理面改动
// （max_concurrency）→ 重建后新 leaf 读到新值。
func TestReuseSyncsStaticFieldsFromDB(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)
	before := reuseByID(s, 1)

	oldView := s.View()
	m.mu.Lock()
	a := m.byGroup[10][0]
	a.MaxConcurrency = 1
	m.mu.Unlock()
	require.NoError(t, s.reload(context.Background()))
	s.compileOnce()

	after := reuseByID(s, 1)
	require.NotSame(t, before, after, "new immutable leaf")
	require.Same(t, before.runtime, after.runtime, "shared runtime")
	require.Same(t, before, oldView.ByID()[1], "old view stable")
	require.Equal(t, 1, after.static.Load().acc.MaxConcurrency, "max_concurrency 同步 DB 新值")

	// 门禁按新 max 生效：占满 1 个槽后第二个请求 ErrAttemptsExhausted
	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.AccountID)
	_, err = s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrAttemptsExhausted, "max_concurrency=1 → 第二个请求假满")
	s.Release(sel.AccountID)
}

// TestReuseClampsMaxConcurrency 复用分支的 MaxConcurrency 钳制（评审 M-2）。
func TestReuseClampsMaxConcurrency(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 0)}})
	s := newSched(t, m) // 首次加载：新建分支钳制
	require.Equal(t, 2, reuseByID(s, 1).static.Load().acc.MaxConcurrency, "新建分支钳制 defaultMax=2")
	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err, "钳制后门禁不恒满")
	s.Release(sel.AccountID)

	require.NoError(t, s.reload(context.Background()))
	require.Equal(t, 2, reuseByID(s, 1).static.Load().acc.MaxConcurrency, "复用分支钳制 defaultMax=2")
	sel, err = s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err, "复用后门禁不恒满")
	s.Release(sel.AccountID)
}

// TestReuseGroupIDsResetOnRemoval groupIDs 首次出现重置（评审 M-1）。
func TestReuseGroupIDsResetOnRemoval(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	a := acc(1, tplx, 4)
	m := newMemLoader(map[int64][]*domain.Account{10: {a}, 20: {a}})
	s := newSched(t, m)
	before := reuseByID(s, 1)
	require.ElementsMatch(t, []int64{10, 20}, before.static.Load().groupIDs, "多组账号跨组引用集完整")

	oldView := s.View()
	m.mu.Lock()
	m.byGroup[20] = nil
	m.mu.Unlock()
	require.NoError(t, s.reload(context.Background()))
	// Atomic publication: the staged removal pairs on the next compile.
	s.compileOnce()
	after := reuseByID(s, 1)
	require.NotSame(t, before, after, "new immutable leaf")
	require.Same(t, before.runtime, after.runtime, "shared runtime")
	require.Same(t, before, oldView.ByID()[1], "old view stable")
	require.Equal(t, []int64{10}, after.static.Load().groupIDs, "旧 gid 20 不得残留")
}

// TestReuseNewAccountCreatesFresh 新账号（DB 新增）→ 新建实例。
func TestReuseNewAccountCreatesFresh(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)
	old1 := reuseByID(s, 1)

	m.mu.Lock()
	m.byGroup[10] = append(m.byGroup[10], acc(2, tplx, 0))
	m.mu.Unlock()
	require.NoError(t, s.reload(context.Background()))
	// Atomic publication: the staged addition pairs on the next compile.
	s.compileOnce()

	require.Same(t, old1, reuseByID(s, 1), "已存在账号仍复用")
	as2 := reuseByID(s, 2)
	require.NotNil(t, as2, "新账号进入 byID")
	require.Equal(t, 2, as2.static.Load().acc.MaxConcurrency, "新账号新建分支钳制 defaultMax=2")
	require.Equal(t, []int64{10}, as2.static.Load().groupIDs, "新账号组引用集登记")
	require.Zero(t, as2.runtime.concurrency.Load(), "新账号计数自 0 起")
	require.Zero(t, as2.statePtr().errCount, "新账号状态全新")
}

// TestReuseConcurrentSelectReloadRace 评审 Critical 回归：复用分支的静态字段
// 原子发布与热路径无锁读并发——-race 必须静默。
func TestReuseConcurrentSelectReloadRace(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 1000)}})
	s := newSched(t, m)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 8000; i++ {
			sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
			if err != nil {
				continue
			}
			s.Release(sel.AccountID)
			s.MarkResult(sel.AccountID, rule.KindOK, nil, 200, "", "")
			s.Classify(rule.Event{AccountID: sel.AccountID, Kind: rule.Kind5xx})
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 400; i++ {
			require.NoError(t, s.reload(context.Background()))
		}
	}()
	wg.Wait()
}
