// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"context"
	"errors"
	"strings"
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
	writes  []statusWrite
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

func (m *memLoader) UpdateAccountStatus(ctx context.Context, id int64, status domain.AccountStatus, cooldown *time.Time, lastErr *string, weight *int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.writes = append(m.writes, statusWrite{id: id, status: status, cooldown: cooldown, lastErr: lastErr, weight: weight})
	return nil
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

func intPtr(v int) *int { return &v }
func statusPtr(s domain.AccountStatus) *domain.AccountStatus { return &s }

func tpl(id int64, format domain.RequestFormat, models []string) *domain.Template {
	return &domain.Template{ID: id, BaseURL: "https://u/v1", CredentialType: credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{format}, Models: models}
}

func acc(id int64, t *domain.Template, maxConc int) *domain.Account {
	return &domain.Account{ID: id, TemplateID: t.ID, Template: t, UpstreamKey: "k", Status: domain.StatusActive, Weight: 100, MaxConcurrency: maxConc}
}

func newSched(t *testing.T, m *memLoader) *Scheduler {
	t.Helper()
	// 规则引擎：空表 Reload 写种子（429/30s、error/unhealthy/5s、ok/active），
	// 行为等价于旧硬编码状态机（Task C 改造后 MarkResult 走规则路径）。
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background()))
	s := New(testCfg(), m, re, nil)
	require.NoError(t, s.reload(context.Background()))
	return s
}

// newTestScheduler 用给定账号（固定放入组 10）构建已加载快照的调度器。
func newTestScheduler(t *testing.T, accs []*domain.Account) *Scheduler {
	t.Helper()
	return newSched(t, newMemLoader(map[int64][]*domain.Account{10: accs}))
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
// 模板（账号级无该字段；一个模板 = 一种号池）。模板 api_key 默认 → 传播 api_key；
// 模板非 api_key 类型（如未来 codex 生态）→ 原样传播（合法性由 proxy credentialFor 把关）。
func TestSelectCredentialTypeFromTemplate(t *testing.T) {
	codexTpl := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"})
	codexTpl.CredentialType = credential.Type("codex_oauth")
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, codexTpl, 4)}})
	s := newSched(t, m)
	sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, credential.Type("codex_oauth"), sel.CredentialType, "类型随模板传播")
	s.Release(sel.AccountID)

	// api_key 默认模板 → Selection 携带 api_key（行为不变路径）
	s2 := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4},
	})
	sel2, err := s2.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, credential.TypeAPIKey, sel2.CredentialType, "默认模板类型 api_key 传播到 Selection")
}

func TestSelectModelPreference(t *testing.T) {
	// 两账号同格式：一个 Serves(model)，一个不——未命中的 tB 带模型空间
	// （白名单账号）→ 不进 tier2（硬白名单：gpt-4o 桶路由仅 tier1）
	tA := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"})
	tB := tpl(2, domain.FormatOpenAIChat, []string{"other"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tA, 4), acc(2, tB, 4)}})
	s := newSched(t, m)
	sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.AccountID, "model preference tier")
	gs := s.store.groups.Load().(map[int64]*groupSnapshot)[10]
	rt := gs.routes[routeKey{domain.FormatOpenAIChat, "gpt-4o"}]
	require.NotNil(t, rt)
	require.NotNil(t, rt.tier1)
	require.Nil(t, rt.tier2, "未命中白名单账号（有模型空间）→ 跳过，不进 tier2")
}

func TestConcurrencyLimit(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 1)}})
	s := newSched(t, m)
	sel1, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	_, err = s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrNoAvailable)
	s.Release(sel1.AccountID)
	_, err = s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err, "available after release")
}

func TestMark429CooldownAndRecover(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)

	// 启动异步回写循环（否则 statusWrite 永远不会被消费）
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.writebackLoop(ctx)

	// 种子规则：429 → status=429 + cooldown 30s（MarkResult 异步投递，flush 同步处理）
	s.MarkResult(1, rule.Kind429, nil, 0, "")
	s.FlushRules()
	_, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrNoAvailable, "in cooldown should be unavailable")
	// 冷却过期后惰性恢复（种子 cooldown 30s > 15s，需推进 35s）
	s.timeNow = func() time.Time { return time.Now().Add(35 * time.Second) }
	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err, "available after cooldown")
	s.MarkResult(sel.AccountID, rule.KindOK, nil, 0, "")
	s.FlushRules()
	s.Release(sel.AccountID)
	// C-M2 语义钉：OK 恢复 active 但残留 cooldownUntil 保留至过期（新 apply 仅
	// cooldownUntil 非 nil 才设置；种子 ok 规则无 cooldown → 旧冷却不清除）。
	ri, _ := s.Runtime(1)
	require.Equal(t, domain.StatusActive, ri.Status, "OK 恢复 active")
	require.NotNil(t, ri.CooldownUntil, "OK 不清除残留冷却（保留至过期，Select 按时间判定不受影响）")
	require.Eventually(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.writes) > 0
	}, time.Second, 10*time.Millisecond, "expected async status write")
}

func TestMarkErrorBackoff(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)

	// 种子规则：error → unhealthy + cooldown 5s（指数退避已废弃——升级惩罚由规则表达）
	s.MarkResult(1, rule.Kind5xx, nil, 500, "")
	s.FlushRules()
	ri, ok := s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusUnhealthy, ri.Status)
	require.Equal(t, 1, ri.ErrCount)
	require.NotNil(t, ri.CooldownUntil)
	require.True(t, ri.CooldownUntil.After(time.Now().Add(4*time.Second)), "seed cooldown 5s applied")
	s.MarkResult(1, rule.Kind5xx, nil, 500, "")
	s.FlushRules()
	ri, _ = s.Runtime(1)
	require.Equal(t, 2, ri.ErrCount)
	s.MarkResult(1, rule.KindOK, nil, 0, "")
	s.FlushRules()
	ri, _ = s.Runtime(1)
	require.Equal(t, domain.StatusActive, ri.Status, "success resets status")
	require.Equal(t, 0, ri.ErrCount, "success resets err count")
}

func TestSelectUnknownGroup(t *testing.T) {
	m := newMemLoader(map[int64][]*domain.Account{})
	s := newSched(t, m)
	_, err := s.Select(99, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrGroupNotFound)
}

// TestSelectNilStoreNoPanic 快照未加载（首刷失败 / DB 故障启动——注册表
// ReloadAll 失败仅 Warn，评审 R3 M-1）时 Select 优雅失败而非断言 panic：
// 旧启动序在此失败 fatalf（进程退出，无流量），Warn-and-serve 语义下客户端
// 应见 404（group not found）而非断连。构造后不 reload（store 恒 nil）——
// 首次调用即命中断言 ok 分支。
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
	// 账号 2 已进入候选。两账号并发上限各 4，不释放地连续选 5 次必须全部成功
	//（账号 1 单独最多承接 4 次 → 第 5 次必由账号 2 承接；若 reload 未生效则第 5 次 ErrNoAvailable），
	// 且由鸽巢原理两个账号都至少被选中一次（各 ≤4，总数 5）。
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
// 组内账号 [1,2] → [2,3]（1 移除、3 新增）：Release/MarkResult 必须命中新快照，
// 被移除账号必须从 byID 消失（no-op 安全）。
func TestInvalidateGroupByIDRebuild(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 2), acc(2, tplx, 2)}})
	s := newSched(t, m)

	m.mu.Lock()
	m.byGroup[10] = []*domain.Account{acc(2, tplx, 2), acc(3, tplx, 2)}
	m.mu.Unlock()
	s.InvalidateGroup(10)

	// 占满并发：两账号各上限 2、总容量 4 → 4 次选择后各持 2 个槽（鸽巢原理，确定性）。
	var sels []*Selection
	for i := 0; i < 4; i++ {
		sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
		require.NoError(t, err)
		sels = append(sels, sel)
	}
	// 释放 2/3 的槽：并发计数必须在新快照上递减（byID 指向旧快照则计数错位）。
	for _, sel := range sels {
		s.Release(sel.AccountID)
	}
	ri, ok := s.Runtime(2)
	require.True(t, ok)
	require.Equal(t, int64(0), ri.Concurrency, "retained account release hits the new snapshot")
	ri, ok = s.Runtime(3)
	require.True(t, ok, "added account must be in byID")
	require.Equal(t, int64(0), ri.Concurrency, "added account release hits the new snapshot")

	// 新增账号 3 的结果回流必须落新快照并触发回写。
	s.MarkResult(3, rule.KindNetwork, nil, 0, "")
	s.FlushRules()
	ri, _ = s.Runtime(3)
	require.Equal(t, domain.StatusUnhealthy, ri.Status, "markresult hits the new snapshot")
	require.Equal(t, 1, ri.ErrCount)

	// 被移除账号 1：Runtime 不可见，MarkResult/Release 安全 no-op（无回写）。
	_, ok = s.Runtime(1)
	require.False(t, ok, "removed account must not be in byID")
	s.MarkResult(1, rule.Kind5xx, nil, 500, "")
	s.FlushRules()
	s.Release(1)

	require.NoError(t, s.Close(context.Background()))
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []int64
	for _, w := range m.writes {
		ids = append(ids, w.id)
	}
	require.Contains(t, ids, int64(3), "writeback fires for the added account")
	require.NotContains(t, ids, int64(1), "no writeback for the removed account")
}

// TestInvalidateGroupShrinkByID 回归：组内账号从 [4,5] 收缩为 [4] 时，
// 保留账号 4 的 byID 必须指向新快照（并发上限 2→1 生效），被移除账号 5 必须消失。
func TestInvalidateGroupShrinkByID(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{11: {acc(4, tplx, 2), acc(5, tplx, 2)}})
	s := newSched(t, m)

	m.mu.Lock()
	m.byGroup[11] = []*domain.Account{acc(4, tplx, 1)} // 5 移除；4 的并发上限 2→1
	m.mu.Unlock()
	s.InvalidateGroup(11)

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
	s.MarkResult(5, rule.KindNetwork, nil, 0, "")
	s.FlushRules()
	s.Release(5)

	require.NoError(t, s.Close(context.Background()))
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, w := range m.writes {
		require.NotEqual(t, int64(5), w.id, "no writeback for the removed account")
	}
}

// TestMarkResultDisabledStaysDisabled 回归：管理端禁用账号后（InvalidateGroup
// 以 disabled 状态重载快照），在途请求的 MarkResult(OK) 不得把账号复活为
// active，也不得回写 DB——否则禁用被静默抹除、30s 同步后账号复现（评审发现）。
func TestMarkResultDisabledStaysDisabled(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)

	// 在途请求：先选中账号
	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)

	// 管理端禁用：以 disabled 状态重载组快照（与账号管理变更同路径）
	m.mu.Lock()
	m.byGroup[10] = []*domain.Account{{
		ID: 1, TemplateID: 1, Template: tplx, UpstreamKey: "k",
		Status: domain.StatusDisabled, Weight: 100, MaxConcurrency: 4,
	}}
	m.mu.Unlock()
	s.InvalidateGroup(10)
	ri, ok := s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri.Status, "禁用已在快照生效")

	// 在途请求完成：OK 不得把状态重置回 active、不得重置错误计数（守卫同步短路，不投递）
	s.MarkResult(1, rule.KindOK, nil, 0, "")
	ri, _ = s.Runtime(1)
	require.Equal(t, domain.StatusDisabled, ri.Status, "OK 不得复活禁用账号")
	require.Zero(t, ri.ErrCount, "OK 不得重置禁用账号的错误计数")

	// 防御性：429/错误分支同样不得给禁用账号设置冷却或改写状态
	s.MarkResult(1, rule.Kind429, nil, 0, "")
	ri, _ = s.Runtime(1)
	require.Equal(t, domain.StatusDisabled, ri.Status, "429 不得把禁用账号改写为 429")
	require.Nil(t, ri.CooldownUntil, "429 不得给禁用账号设置冷却")
	s.MarkResult(1, rule.Kind5xx, nil, 500, "")
	ri, _ = s.Runtime(1)
	require.Equal(t, domain.StatusDisabled, ri.Status, "错误分支不得改写禁用账号")

	// 禁用账号不可再被选中
	_, err = s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrNoAvailable)
	s.Release(sel.AccountID)

	// 无回写：Close 排空后 DB 写入列表必须为空
	require.NoError(t, s.Close(context.Background()))
	m.mu.Lock()
	defer m.mu.Unlock()
	require.Empty(t, m.writes, "禁用账号不得有状态回写")
}

// TestWeightActionRebuildsRoutes 权重动作（I1/I5）：命中后快照权重更新 + 组路由
// 重建（weightedSeq 预生成缓存），选号分布立即按新权重；纯 weight 动作不更新
// 状态与 EWMA；DB 回写携带 weight。
func TestWeightActionRebuildsRoutes(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"})
	m := newMemLoader(map[int64][]*domain.Account{10: {
		{ID: 1, TemplateID: 1, Template: tplx, UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
		{ID: 2, TemplateID: 1, Template: tplx, UpstreamKey: "k2", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
	}})
	// 自定义规则表（非种子）：5xx → 纯 weight 动作（weight 10）
	rstore := &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}
	_, err := rstore.CreateRule(context.Background(), domain.Rule{
		Name: "throttle", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Kind: strPtr("5xx")},
		Then: domain.RuleThen{Weight: intPtr(10)},
	})
	require.NoError(t, err)
	re := rule.New(rule.Config{}, rstore, nil)
	require.NoError(t, re.Reload(context.Background()))
	s := New(testCfg(), m, re, nil)
	require.NoError(t, s.reload(context.Background()))

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.writebackLoop(ctx)

	s.MarkResult(1, rule.Kind5xx, nil, 500, "")
	s.FlushRules()

	// 纯 weight 动作：状态/EWMA 不动，快照权重更新
	ri, ok := s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusActive, ri.Status, "纯 weight 动作不动状态")
	require.Zero(t, ri.ErrRate, "纯 weight 动作不更新 EWMA")
	byID := s.store.byID.Load().(map[int64]*accountSnapshot)
	require.Equal(t, 10, byID[1].static.Load().acc.Weight, "快照权重已更新")

	// 回写携带 weight
	require.Eventually(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.writes) == 1 && m.writes[0].weight != nil && *m.writes[0].weight == 10
	}, time.Second, 10*time.Millisecond, "writeback carries weight")

	// 路由序列已重建：选号分布按新权重（100:10 → ≈10:1）
	const n = 20_000
	counts := map[int64]int{}
	for i := 0; i < n; i++ {
		sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
		require.NoError(t, err)
		counts[sel.AccountID]++
		s.Release(sel.AccountID)
	}
	ratio := float64(counts[2]) / float64(counts[1])
	require.InDelta(t, ratio, 10.0, 0.5, "weight 100:10 → 频率比 ≈ 10:1（路由已按新权重重建）")
}

// TestProcessWriteMergeKeepsWeight 回归（评审 I-1）：同账号 weight 写先入队、
// status 写（weight=nil）后入队，processWrite 合并不得丢 weight——否则 DB 不持久化，
// ≤30s reload 后内存回退，weight 动作被静默撤销。
func TestProcessWriteMergeKeepsWeight(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)

	// weight 写先入队、status 写后入队（weight=nil）
	s.enqueueWrite(1, accState{status: domain.Status429, errCount: 1}, intPtr(10))
	s.enqueueWrite(1, accState{status: domain.StatusActive}, nil)

	require.NoError(t, s.Close(context.Background())) // 排空触发 processWrite 合并
	m.mu.Lock()
	defer m.mu.Unlock()
	require.Len(t, m.writes, 1, "same-account writes merged into one")
	require.Equal(t, domain.StatusActive, m.writes[0].status, "后写 status 覆盖先写")
	require.NotNil(t, m.writes[0].weight, "后写 weight=nil 不得丢弃已入队的 weight")
	require.Equal(t, 10, *m.writes[0].weight, "最终写必须携带 weight")
}

// TestWorkerContract 满足 worker.Worker 契约（Global Constraints #5）：Name + 幂等 Start。
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

// TestCloseDrainsWritebacks Close 排空 pending 回写且幂等；ctx 超时路径不阻塞。
// 事件经 FlushRules 同步处理后进入回写队列（生产路径由规则引擎 worker 消费）。
func TestCloseDrainsWritebacks(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)

	s.MarkResult(1, rule.Kind5xx, nil, 500, "")
	s.FlushRules()                                    // 事件 → apply → 回写入队
	require.NoError(t, s.Close(context.Background())) // 排空 pending 回写
	require.NoError(t, s.Close(context.Background())) // 幂等
	m.mu.Lock()
	require.Len(t, m.writes, 1, "close drains exactly the pending writeback")
	m.mu.Unlock()

	// ctx 已取消：限时路径直接返回（丢弃/尽最大努力），不阻塞。
	s.MarkResult(1, rule.Kind5xx, nil, 500, "")
	s.FlushRules()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.NoError(t, s.Close(ctx))
}

func mkAcc(id int64, weight int, tpl *domain.Template) *accountSnapshot {
	a := &accountSnapshot{}
	a.static.Store(&snapshotStatic{acc: domain.Account{ID: id, Weight: weight}, tpl: tpl})
	a.state.Store(&accState{status: domain.StatusActive})
	return a
}

func tplWith(ff domain.RequestFormat, models []string) *domain.Template {
	return &domain.Template{SupportedFormats: []domain.RequestFormat{ff}, Models: models}
}

func TestNewWeightedSeqGcdNormalization(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"})
	pool := []*accountSnapshot{mkAcc(1, 100, tpl), mkAcc(2, 50, tpl)}
	ws := newWeightedSeq(pool)
	require.Len(t, ws.seq, 3, "weight 100:50 → GCD=50 → 序列长 3")
	count1, count2 := 0, 0
	for _, a := range ws.seq {
		if a.static.Load().acc.ID == 1 {
			count1++
		} else {
			count2++
		}
	}
	require.Equal(t, 2, count1)
	require.Equal(t, 1, count2)
}

func TestNewWeightedSeqEqualWeights(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"})
	pool := []*accountSnapshot{mkAcc(1, 100, tpl), mkAcc(2, 100, tpl), mkAcc(3, 100, tpl)}
	ws := newWeightedSeq(pool)
	require.Len(t, ws.seq, 3, "全同权重 → 每账号 1 次")
	require.ElementsMatch(t, []int64{1, 2, 3}, []int64{ws.seq[0].static.Load().acc.ID, ws.seq[1].static.Load().acc.ID, ws.seq[2].static.Load().acc.ID})
}

func TestNewWeightedSeqLengthCap(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"})
	// 反例构造超长：权重 9999 与 1 → GCD=1 → 长 10000 > 4096
	pool2 := []*accountSnapshot{mkAcc(1, 9999, tpl), mkAcc(2, 1, tpl)}
	ws := newWeightedSeq(pool2)
	require.LessOrEqual(t, len(ws.seq), maxSeqLen, "长度上限 4096")
	require.Contains(t, []int64{ws.seq[0].static.Load().acc.ID, ws.seq[1].static.Load().acc.ID}, int64(1), "权重高的账号至少出现一次")
}

func TestBuildRoutesBucketsAndDefault(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"gpt-4o", "gpt-4o-mini"})
	pool := []*accountSnapshot{mkAcc(1, 100, tpl)}
	routes := buildRoutes(pool)
	// 已知模型桶
	rt, ok := routes[routeKey{domain.FormatOpenAIChat, "gpt-4o"}]
	require.True(t, ok)
	require.NotNil(t, rt.tier1, "gpt-4o 在 models 里 → tier1")
	require.Nil(t, rt.tier2)
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
	pool := []*accountSnapshot{mkAcc(1, 100, tpl)}
	routes := buildRoutes(pool)
	// anthropic 只支持 special（format_models 限制）→ special 有桶且 tier1（∈ Models → Serves true）
	rt, ok := routes[routeKey{domain.FormatAnthropic, "special"}]
	require.True(t, ok, "FormatModels 配置格式 → special 模型走 anthropic 桶")
	require.NotNil(t, rt.tier1, "special ∈ Models → Serves true → tier1")
	// gpt-4o 不在 anthropic 的 format_models 列表 → 该组合无桶
	_, ok = routes[routeKey{domain.FormatAnthropic, "gpt-4o"}]
	require.False(t, ok, "gpt-4o ∉ FormatModels[anthropic] → 格式不支持该模型")
	// chat 未配置 format_models → 全部模型
	rtC, ok := routes[routeKey{domain.FormatOpenAIChat, "gpt-4o"}]
	require.True(t, ok, "未配置格式 → 全部模型")
	require.NotNil(t, rtC.tier1, "gpt-4o ∈ Models → tier1")
	// responses 不在 supported → 无桶
	_, ok = routes[routeKey{domain.FormatOpenAIResponses, "special"}]
	require.False(t, ok, "格式不在 supported → 无桶")
}

// —— 模板模型硬白名单（用户裁决 2026-08-18）：Serves 未命中 + 白名单账号 →
// 404；全模型账号（无模型空间）保留 tier2/默认桶兜底 ——

// TestSelectWhitelistHitMiss 白名单命中 → tier1 选中；白名单外模型 → 404
// （ErrFormatUnavailable，不再 tier2 兜底转发）。
func TestSelectWhitelistHitMiss(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
	})
	require.NoError(t, s.InvalidateAllSync())
	sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.AccountID)
	s.Release(sel.AccountID)
	_, err = s.Select(10, domain.FormatOpenAIChat, "claude-3-5-sonnet-20241022")
	require.ErrorIs(t, err, ErrFormatUnavailable, "白名单外模型 → 404（此前 tier2 兜底转发）")
}

// TestSelectFormatModelsOnlyBoundary 评审 M-1：Models=[] + FormatModels={chat:[gpt-4o]}
// + supported_formats 含 anthropic 的账号——anthropic 格式任意模型 → 404（未列
// 模型的格式不建路由）；chat + gpt-4o → tier1 命中。
func TestSelectFormatModelsOnlyBoundary(t *testing.T) {
	tplFm := &domain.Template{
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatAnthropic},
		FormatModels:     map[domain.RequestFormat][]string{domain.FormatOpenAIChat: {"gpt-4o"}},
	}
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplFm, UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
	})
	require.NoError(t, s.InvalidateAllSync())
	// 未配置 FormatModels 但 supported 含的格式（anthropic）：任意模型 → 404
	_, err := s.Select(10, domain.FormatAnthropic, "claude-3-5-sonnet-20241022")
	require.ErrorIs(t, err, ErrFormatUnavailable, "FormatModels-only 账号在未列模型格式上不建路由 → 404")
	// 配置格式 + 白名单模型 → tier1
	sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.AccountID)
	s.Release(sel.AccountID)
}

// TestSelectFormatModelsEmptyList 评审 Minor ② 防回归：FormatModels={chat:[]}
//（覆盖但空列表）退化配置——HasModelSpace true（FormatModels 非空）→ 归白名单
// 账号 → 默认桶排除；FormatSupports(chat, m) 对空列表恒 false → 该格式全 404
//（含未知模型回落，不再经默认桶绕过 format_models 限制）。
func TestSelectFormatModelsEmptyList(t *testing.T) {
	tplFm := &domain.Template{
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		FormatModels:     map[domain.RequestFormat][]string{domain.FormatOpenAIChat: {}},
	}
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplFm, UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
	})
	require.NoError(t, s.InvalidateAllSync())
	for _, m := range []string{"gpt-4o", "unknown-model-xyz"} {
		_, err := s.Select(10, domain.FormatOpenAIChat, m)
		require.ErrorIs(t, err, ErrFormatUnavailable, "空列表格式（覆盖但空）→ 全 404（含未知模型回落）")
	}
}

// TestSelectMappingKeyWhitelist 评审 O-5：mapping key（gpt-4o）命中 → tier1（白名单
// 别名）；映射目标（deepseek-chat）不复查——直接请求目标模型 → 404。
func TestSelectMappingKeyWhitelist(t *testing.T) {
	tplMap := &domain.Template{
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		ModelMapping:     map[string]string{"gpt-4o": "deepseek-chat"},
	}
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplMap, UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
	})
	require.NoError(t, s.InvalidateAllSync())
	// mapping key 即白名单别名 → tier1，Selection.Model = 映射目标（pickFrom 内映射）
	sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.AccountID)
	require.Equal(t, "deepseek-chat", sel.Model)
	s.Release(sel.AccountID)
	// 映射目标不复查：直接请求 deepseek-chat → 未命中白名单 → 404
	_, err = s.Select(10, domain.FormatOpenAIChat, "deepseek-chat")
	require.ErrorIs(t, err, ErrFormatUnavailable, "映射目标（上游模型名）不复查")
}

// TestSelectFullModelTier2Fallback 全模型账号（模型空间空）未命中任何白名单 →
// tier2/默认桶兜底转发（保留）。
func TestSelectFullModelTier2Fallback(t *testing.T) {
	tplOpen := &domain.Template{SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}}
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplOpen, UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
	})
	require.NoError(t, s.InvalidateAllSync())
	for _, m := range []string{"any-model-1", "gpt-4o", "claude-3-5-sonnet-20241022"} {
		sel, err := s.Select(10, domain.FormatOpenAIChat, m)
		require.NoError(t, err, "全模型账号：任意模型 200（tier2 兜底保留）")
		require.Equal(t, int64(1), sel.AccountID)
		s.Release(sel.AccountID)
	}
}

// TestSelectDefaultBucketExcludesWhitelist 默认桶不含白名单账号：未知模型 + 组内
// 仅白名单账号 → 404（白名单账号不得参与未知模型回落误转发）。
func TestSelectDefaultBucketExcludesWhitelist(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
	})
	require.NoError(t, s.InvalidateAllSync())
	_, err := s.Select(10, domain.FormatOpenAIChat, "unknown-model-xyz")
	require.ErrorIs(t, err, ErrFormatUnavailable, "默认桶不含白名单账号 → 未知模型 404")
}

// TestSelectMixedGroupWhitelistFullModel 混合组：A 白名单 ["gpt-4o"] + B 全模型——
// 请求 gpt-4o 走 tier1 A；A 冷却 → tier2 B 兜底；请求白名单外模型 → 直接 B。
func TestSelectMixedGroupWhitelistFullModel(t *testing.T) {
	tplOpen := &domain.Template{ID: 2, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}}
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
		{ID: 2, TemplateID: 2, Template: tplOpen, UpstreamKey: "k2", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
	})
	require.NoError(t, s.InvalidateAllSync())
	// gpt-4o → tier1 A 优先
	sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.AccountID, "白名单命中 → tier1 A")
	s.Release(sel.AccountID)
	// A 冷却 → tier2 B 兜底
	s.MarkResult(1, rule.Kind429, nil, 0, "")
	s.FlushRules()
	sel, err = s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(2), sel.AccountID, "tier1 冷却 → 全模型账号 tier2 兜底")
	s.Release(sel.AccountID)
	// 白名单外模型 → 直接 B（默认桶仅全模型账号，无 tier1）
	sel, err = s.Select(10, domain.FormatOpenAIChat, "claude-3-5-sonnet-20241022")
	require.NoError(t, err)
	require.Equal(t, int64(2), sel.AccountID, "白名单外模型 → 默认桶（仅全模型账号）")
	s.Release(sel.AccountID)
}

// 分布：10 万次选号，频率 vs 权重比例（±5% 容差，shuffle 后的轮询分布）
func TestSelectWeightDistribution(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
		{ID: 2, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k2", Status: domain.StatusActive, Weight: 50, MaxConcurrency: 1000},
	})
	require.NoError(t, s.InvalidateAllSync())
	const n = 100_000
	counts := map[int64]int{}
	for i := 0; i < n; i++ {
		sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
		require.NoError(t, err)
		counts[sel.AccountID]++
		s.Release(sel.AccountID)
		s.MarkResult(sel.AccountID, rule.KindOK, nil, 0, "")
	}
	ratio := float64(counts[1]) / float64(counts[2])
	// 注意：testify 无 InRange，用 InDelta（±0.1 窗口等价于 [1.9, 2.1]）
	require.InDelta(t, ratio, 2.0, 0.1, "weight 100:50 → 频率比 ≈ 2:1")
}

// 动态状态跳过：冷却中的账号被跳过，选中其他账号
func TestSelectSkipsCooldown(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
		{ID: 2, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k2", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
	})
	require.NoError(t, s.InvalidateAllSync())
	// 账号 1 进 429 冷却
	s.MarkResult(1, rule.Kind429, nil, 0, "")
	s.FlushRules()
	for i := 0; i < 50; i++ {
		sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
		require.NoError(t, err)
		require.Equal(t, int64(2), sel.AccountID, "冷却中的账号 1 必须被跳过")
		s.Release(sel.AccountID)
	}
}

// 全不可用（全冷却）→ ErrNoAvailable，且有限时间内返回
func TestSelectAllCooldownReturnsNoAvailable(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
	})
	require.NoError(t, s.InvalidateAllSync())
	s.MarkResult(1, rule.Kind429, nil, 0, "")
	s.FlushRules()
	done := make(chan error, 1)
	go func() {
		_, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
		done <- err
	}()
	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrNoAvailable)
	case <-time.After(time.Second):
		t.Fatal("全冷却必须有限时间内返回 ErrNoAvailable")
	}
}

// 未知模型回落默认桶：请求 model 不在任何模板可服务集合 → 默认格式 tier2 选中。
// 硬白名单语义：白名单账号（有模型空间）不进默认桶 → 组内仅白名单账号时未知
// 模型 404（默认桶兜底仅全模型账号，见 TestSelectFullModelTier2Fallback）。
func TestSelectUnknownModelDefaultBucket(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
	})
	require.NoError(t, s.InvalidateAllSync())
	_, err := s.Select(10, domain.FormatOpenAIChat, "unknown-model-xyz")
	require.ErrorIs(t, err, ErrFormatUnavailable, "组内仅白名单账号 → 未知模型 404（不进默认桶）")
}

// tier 回落：tier1 全冷却 → tier2 选中（tier2 账号须为全模型账号——白名单
// 账号未命中已跳过，见 TestSelectModelPreference）
func TestSelectTierFallback(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
		{ID: 2, TemplateID: 2, Template: &domain.Template{ID: 2, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}}, UpstreamKey: "k2", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
	})
	require.NoError(t, s.InvalidateAllSync())
	// 账号 1（tier1）进冷却 → 请求 gpt-4o 应回落 tier2（账号 2，全模型账号 Serves 为 false）
	s.MarkResult(1, rule.Kind429, nil, 0, "")
	s.FlushRules()
	sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(2), sel.AccountID, "tier1 全不可用 → tier2 回落")
	s.Release(sel.AccountID)
}

// tier 回落（并发满，Task 2 评审钉死）：tier1 账号并发满 → 回落 tier2（可用性优先）。
// 规范裁定：旧实现（并发满账号在分档前被剔除 → tier1 为空）在此场景直接
// ErrNoAvailable 的语义不可取；新实现 tier1 序列扫描失败后必须回落 tier2。
func TestSelectTier1FullFallsBackToTier2(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1},
		{ID: 2, TemplateID: 2, Template: &domain.Template{ID: 2, SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}}, UpstreamKey: "k2", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 4},
	})
	require.NoError(t, s.InvalidateAllSync())
	// 账号 1 是唯一 Serves gpt-4o 的账号（tier1 序列只有它）→ 确定性占用其唯一并发槽
	sel1, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel1.AccountID, "tier1 唯一账号先被选中")
	// tier1 并发满 → 必须回落 tier2（账号 2，全模型账号 Serves 恒 false 但同默认格式）
	sel2, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	require.Equal(t, int64(2), sel2.AccountID, "tier1 并发满 → tier2 回落（可用性优先）")
	s.Release(sel1.AccountID)
	s.Release(sel2.AccountID)
}

// 并发 CAS 竞争（Task 2 评审钉死）：单账号（n=1 序列）两并发 Select，
// 恰一成功、另一返回 ErrNoAvailable——单遍单次 CAS 语义（败者不自旋重试，
// 调用方重试）。屏障对齐两 goroutine 后 200 轮放大真实 CAS 冲突。
func TestSelectConcurrentCASRace(t *testing.T) {
	s := newTestScheduler(t, []*domain.Account{
		{ID: 1, TemplateID: 1, Template: tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"}), UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1},
	})
	require.NoError(t, s.InvalidateAllSync())
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
		close(start) // 同时放行，最大化 CAS 读-改-写冲突窗口
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
		okCount, noAvailCount := 0, 0
		for r := range results {
			switch {
			case r.err == nil:
				okCount++
				winner = r.sel
			case errors.Is(r.err, ErrNoAvailable):
				noAvailCount++
			default:
				t.Fatalf("iter %d: unexpected error: %v", i, r.err)
			}
		}
		require.Equal(t, 1, okCount, "iter %d: exactly one success per pair", i)
		require.Equal(t, 1, noAvailCount, "iter %d: loser gets ErrNoAvailable (never two successes)", i)
		require.NotNil(t, winner, "iter %d: winner carries a selection", i)
		s.Release(winner.AccountID) // 恢复槽位，下一轮从 0 并发开始
	}
}

// TestReloadPreservesInFlightConcurrency：跨 reload 的在途请求 Release 后计数不得为负。
// 根因：reload 重建快照把 concurrency 归零，在途请求（旧快照占槽）结束后 Release 命中
// 新快照 → Add(-1) 拉成负数（管理页并发列显示负值）。修复：reload 继承旧快照并发计数。
func TestReloadPreservesInFlightConcurrency(t *testing.T) {
	chat := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"})
	s := newTestScheduler(t, []*domain.Account{acc(1, chat, 4)})

	// 两个在途请求占槽
	sel1, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	sel2, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)

	// reload 重建快照（30s 定时同步 / 模板账号变更 invalidate）
	require.NoError(t, s.reload(context.Background()))

	// 在途请求结束后 Release（命中新快照计数）
	s.Release(sel1.AccountID)
	s.Release(sel2.AccountID)

	ri, ok := s.Runtime(sel1.AccountID)
	require.True(t, ok)
	require.Equal(t, int64(0), ri.Concurrency, "reload 后并发计数必须回到 0，不得为负")
	require.GreaterOrEqual(t, ri.Concurrency, int64(0))

	// 继承语义核对：reload 时在途计数保持（新请求可继续占满剩余槽位）
	s3, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	ri2, _ := s.Runtime(sel1.AccountID)
	require.Equal(t, int64(1), ri2.Concurrency, "继承后新请求占槽计数为 1")
	s.Release(s3.AccountID)
}

// TestMultiGroupSharedInstance 回归（O2 实证修复）：多组账号必须共享同一
// accountSnapshot 实例——Select（经组路由）与 Release（经 byID）命中同一计数器，
// 否则并发计数分裂漂移 → 槽位假满 "no available account"（e2e 场景 4 实证；
// 去抖消除"每变更全量重载"后暴露；旧实现每次全量 reload 重置组实例计数掩盖）。
// 断言：两组的路由引用同一实例；真实槽位满（共享 max=2，跨组两请求后第三个
// 请求必须 ErrNoAvailable）；Release 经 byID 正确归零。
func TestMultiGroupSharedInstance(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	a := acc(1, tplx, 2)
	m := newMemLoader(map[int64][]*domain.Account{10: {a}, 11: {a}})
	s := newSched(t, m)

	byID := s.store.byID.Load().(map[int64]*accountSnapshot)
	groups := s.store.groups.Load().(map[int64]*groupSnapshot)
	require.Same(t, byID[1], groups[10].accounts[0], "组 10 路由与 byID 共享实例")
	require.Same(t, byID[1], groups[11].accounts[0], "组 11 路由与 byID 共享实例")
	require.ElementsMatch(t, []int64{10, 11}, byID[1].static.Load().groupIDs, "跨组引用集登记完整")

	// 跨组占满共享槽位：组 10 选 1、组 11 选 1 → 计数 2 == max 2
	sel1, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	sel2, err := s.Select(11, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel1.AccountID)
	require.Equal(t, int64(1), sel2.AccountID)
	ri, _ := s.Runtime(1)
	require.Equal(t, int64(2), ri.Concurrency, "共享实例：跨组计数合并，不得分裂")

	// 第三个请求必须假满（旧实现：组路由实例计数未满 → 放行 → 漂移）
	_, err = s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrNoAvailable, "共享实例：真实槽位满")

	// Release 经 byID 归零（旧实现：byID 与组路由实例不一致 → 计数只减不回）
	s.Release(sel1.AccountID)
	s.Release(sel2.AccountID)
	ri, _ = s.Runtime(1)
	require.Equal(t, int64(0), ri.Concurrency, "释放后计数归零")

	// 归零后可继续选（旧实现：另一组路由上的残留计数导致假满）
	sel3, err := s.Select(11, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel3.AccountID)
	s.Release(sel3.AccountID)
}

// TestInvalidateGroupMultiGroupShared 组级重载的共享实例纪律：重载组的新实例
// 必须同时替换 byID 与其账号的其它组引用——组 11 的 Select/Release 命中新实例
// （新并发上限生效），旧实现只换 byID/本组路由 → 其它组路由滞留旧实例 → 计数分裂。
func TestInvalidateGroupMultiGroupShared(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{
		10: {acc(1, tplx, 2)},
		11: {acc(1, tplx, 2)},
	})
	s := newSched(t, m)

	// 账号 1 并发上限 2→1 的组级重载（组 10）
	m.mu.Lock()
	m.byGroup[10] = []*domain.Account{acc(1, tplx, 1)}
	m.mu.Unlock()
	s.InvalidateGroup(10)

	byID := s.store.byID.Load().(map[int64]*accountSnapshot)
	groups := s.store.groups.Load().(map[int64]*groupSnapshot)
	require.Same(t, byID[1], groups[10].accounts[0], "重载组路由 → 新实例")
	require.Same(t, byID[1], groups[11].accounts[0], "其它组引用 → 新实例（共享纪律）")

	// 经组 11 的路由选中新实例：新上限 1 → 第二个请求假满；Release 经 byID 归零
	sel1, err := s.Select(11, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel1.AccountID)
	ri, _ := s.Runtime(1)
	require.Equal(t, int64(1), ri.Concurrency, "经其它组路由命中新实例（新上限 1）")
	_, err = s.Select(11, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrNoAvailable, "新实例真实槽位满")
	s.Release(sel1.AccountID)
	ri, _ = s.Runtime(1)
	require.Equal(t, int64(0), ri.Concurrency, "Release 经 byID 命中新实例")

	// 重载组本身同样生效
	sel2, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel2.AccountID)
	s.Release(sel2.AccountID)
}

// TestInvalidateGroupMultiGroupRemove 从组移除的多组账号：仍属其它组 → byID 保留
// 实例并摘除本组引用（其它组 Select/Release 继续命中同一实例）；不再属于任何组
// → 从 byID 删除（Runtime 不可见、Release no-op）。
func TestInvalidateGroupMultiGroupRemove(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{
		10: {acc(1, tplx, 2)},
		11: {acc(1, tplx, 2)},
	})
	s := newSched(t, m)

	// 从组 10 移除（仍属组 11）
	m.mu.Lock()
	m.byGroup[10] = []*domain.Account{}
	m.mu.Unlock()
	s.InvalidateGroup(10)

	byID := s.store.byID.Load().(map[int64]*accountSnapshot)
	groups := s.store.groups.Load().(map[int64]*groupSnapshot)
	_, ok := byID[1]
	require.True(t, ok, "仍属其它组 → byID 保留")
	require.Equal(t, []int64{11}, byID[1].static.Load().groupIDs, "本组引用已摘除")
	require.Empty(t, groups[10].accounts, "组 10 已空")
	require.Len(t, groups[11].accounts, 1, "组 11 引用保留")
	require.Same(t, byID[1], groups[11].accounts[0], "组 11 路由仍指向共享实例")

	// 组 10 路由已失效（空桶），组 11 正常服务
	_, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.Error(t, err, "空组不可选号")
	sel, err := s.Select(11, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.AccountID)
	s.Release(sel.AccountID)
	ri, _ := s.Runtime(1)
	require.Equal(t, int64(0), ri.Concurrency, "Release 经 byID 命中保留实例")

	// 从组 11 也移除 → 不再属于任何组 → byID 删除
	m.mu.Lock()
	m.byGroup[11] = []*domain.Account{}
	m.mu.Unlock()
	s.InvalidateGroup(11)
	_, ok = s.store.byID.Load().(map[int64]*accountSnapshot)[1]
	require.False(t, ok, "不再属于任何组 → 从 byID 删除")
	_, ok = s.Runtime(1)
	require.False(t, ok)
	s.Release(1) // no-op 安全
}

// TestMarkResultLastErrorWriteback 部署故障修复：事件错误文本经 apply 落
// last_error（有文本用文本、截断 500；无文本回退既有硬编码文案）；成功恢复
// 清空 last_error。
func TestMarkResultLastErrorWriteback(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.writebackLoop(ctx)

	// 错误事件带文本：last_error = errMsg（域内截断 500）
	s.MarkResult(1, rule.KindNetwork, nil, 0, strings.Repeat("dial", 200)) // 800 字符 → 截 500
	s.FlushRules()
	require.Eventually(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		if len(m.writes) == 0 || m.writes[0].lastErr == nil {
			return false
		}
		return len(*m.writes[0].lastErr) == domain.ErrMsgMaxLen
	}, time.Second, 10*time.Millisecond, "last_error 携带截断后的事件错误文本")

	// 无文本错误事件：回退硬编码文案（旧语义不变）
	m.mu.Lock()
	m.writes = nil
	m.mu.Unlock()
	s.MarkResult(1, rule.Kind5xx, nil, 500, "")
	s.FlushRules()
	require.Eventually(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.writes) == 1 && m.writes[0].lastErr != nil && *m.writes[0].lastErr == "upstream error"
	}, time.Second, 10*time.Millisecond, "无文本 → 回退 upstream error")

	// 成功恢复：last_error 清空（nil）
	m.mu.Lock()
	m.writes = nil
	m.mu.Unlock()
	s.MarkResult(1, rule.KindOK, nil, 200, "")
	s.FlushRules()
	require.Eventually(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return len(m.writes) == 1 && m.writes[0].lastErr == nil
	}, time.Second, 10*time.Millisecond, "恢复为 active → last_error 清空")
	require.NoError(t, s.Close(context.Background()))
}

// fakeGroupPub 记录 PublishGroups 收到的组 id（#14 T3a 发布断言目标）。
type fakeGroupPub struct {
	mu   sync.Mutex
	rows [][]int64
}

func (f *fakeGroupPub) PublishGroups(ctx context.Context, gids []int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows = append(f.rows, append([]int64(nil), gids...))
}
func (f *fakeGroupPub) calls() [][]int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]int64, len(f.rows))
	copy(out, f.rows)
	return out
}

// newSchedWithPub 构造带组级发布器的调度器（组发布断言测试用）。
func newSchedWithPub(t *testing.T, m *memLoader, gp GroupChangePublisher) *Scheduler {
	t.Helper()
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background()))
	cfg := testCfg()
	cfg.GroupPub = gp
	s := New(cfg, m, re, nil)
	require.NoError(t, s.reload(context.Background()))
	return s
}

// TestProcessWritePublishesGroupChange #14 T3a：状态回写成功后发布组级 NOTIFY
// （受影响组，去重合并；一次回写批次一条 NOTIFY——R3，设计文档 §1.3/§5 #6）。
func TestProcessWritePublishesGroupChange(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4), acc(2, tplx, 4)}})
	gp := &fakeGroupPub{}
	s := newSchedWithPub(t, m, gp)

	// 同组两账号状态回写合并进同一 processWrite 批 → 单条发布，组去重
	s.enqueueWrite(1, accState{status: domain.Status429, errCount: 1}, nil)
	s.enqueueWrite(2, accState{status: domain.Status429, errCount: 1}, nil)
	require.NoError(t, s.Close(context.Background())) // 排空触发 processWrite

	rows := gp.calls()
	require.Len(t, rows, 1, "一次回写批次一条 NOTIFY（R3）")
	require.ElementsMatch(t, []int64{10}, rows[0], "同组账号去重合并为单组")
	require.Len(t, m.writes, 2, "两条状态写均落库")
}

// TestProcessWritePublishMultiGroup 多组账号（共享实例 groupIDs=[10,20]）→
// 发布全部受影响组。
func TestProcessWritePublishMultiGroup(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{
		10: {acc(1, tplx, 4)},
		20: {acc(1, tplx, 4)}, // 同一账号 ID=1 跨两组
	})
	gp := &fakeGroupPub{}
	s := newSchedWithPub(t, m, gp)

	s.enqueueWrite(1, accState{status: domain.Status429, errCount: 1}, nil)
	require.NoError(t, s.Close(context.Background()))

	rows := gp.calls()
	require.Len(t, rows, 1)
	require.ElementsMatch(t, []int64{10, 20}, rows[0], "多组账号发布全部组")
}

// TestProcessWritePublishSkipsUnknown 快照外账号（已移除）：回写成功但无组可
// 传播 → 不发布（其余实例经 ≤30s 全量同步 / 60s 兜底收敛）。
func TestProcessWritePublishSkipsUnknown(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	gp := &fakeGroupPub{}
	s := newSchedWithPub(t, m, gp)

	s.enqueueWrite(99, accState{status: domain.Status429, errCount: 1}, nil) // 快照外
	require.NoError(t, s.Close(context.Background()))
	require.Empty(t, gp.calls(), "快照外账号无组可传播，不发布")
}

// TestProcessWriteNoPublisherNoop GroupPub 未装配（单实例/旧装配）→ 发布 no-op
// 不 panic。
func TestProcessWriteNoPublisherNoop(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m) // 默认 Config 无 GroupPub

	s.enqueueWrite(1, accState{status: domain.Status429, errCount: 1}, nil)
	require.NoError(t, s.Close(context.Background()))
	require.Len(t, m.writes, 1, "回写不受影响")
}

// TestProcessWriteConcurrentInvalidateGroupRace 评审 M-1 回归：processWrite 收集
// groupIDs 与 InvalidateGroup 的 removeGid 就地改写并发——-race 下复现（修复前
// 必报 DATA RACE，修复后静默）。loader 侧交替组 10 成员资格：账号仅属组 20 时
// InvalidateGroup(10) 触发 removeGid 改写 groupIDs 切片。
func TestProcessWriteConcurrentInvalidateGroupRace(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	a := acc(1, tplx, 4)
	m := newMemLoader(map[int64][]*domain.Account{10: {a}, 20: {a}})
	s := newSched(t, m)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.writebackLoop(ctx)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 3000; i++ {
			// 组 10 成员资格交替移除/恢复：移除后 InvalidateGroup(10) 走 removeGid
			// 就地改写 groupIDs；enqueue 与 removeGid 交替 → 回写循环的 groupIDs
			// 读取与改写持续重叠（修复前 -race 必报，修复后静默）。
			m.mu.Lock()
			m.byGroup[10] = nil
			m.mu.Unlock()
			s.InvalidateGroup(10)
			m.mu.Lock()
			m.byGroup[10] = []*domain.Account{a}
			m.mu.Unlock()
			s.InvalidateGroup(10)
			s.enqueueWrite(1, accState{status: domain.Status429, errCount: 1}, nil)
		}
	}()
	<-done
}

// --- T4 §3：快照 Ext eager-load + 热路径零 DB（Selection 扩展路线断言） ---

// countingLoader 计数 Loader 包装（热路径零 DB 断言：Select/MarkResult/
// Release 期间不得触达加载器）。
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

func (c *countingLoader) UpdateAccountStatus(ctx context.Context, id int64, s domain.AccountStatus, cooldown *time.Time, e *string, w *int) error {
	return c.inner.UpdateAccountStatus(ctx, id, s, cooldown, e, w)
}

func (c *countingLoader) loadsN() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loads
}

// TestSelectCarriesAccountExt 账号 ext 快照 → Selection.Ext（T4 P3-4 定死路线：
// accountSnapshot 携带 Ext，热路径零 DB 数据源——codex relay 凭据线读此）。
func TestSelectCarriesAccountExt(t *testing.T) {
	ext := &domain.AccountExt{
		AccountID: 7, CredentialType: credential.TypeCodexOAuth,
		InstallationID: "inst-1", SessionID: intPtrStr("s1"), ThreadID: intPtrStr("t1"),
	}
	// tpl() 硬编码 api_key 类型——codex 类型模板手动构造
	tpl := &domain.Template{ID: 1, BaseURL: "https://u/v1", CredentialType: credential.TypeCodexOAuth,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponsesWS}, Models: []string{"gpt-4o"}}
	a := acc(7, tpl, 4)
	a.Ext = ext
	s := newTestScheduler(t, []*domain.Account{a})
	sel, err := s.Select(10, domain.FormatOpenAIResponsesWS, "gpt-4o")
	require.NoError(t, err)
	require.Same(t, ext, sel.Ext, "Selection.Ext = 快照账号 Ext（指针复制零拷贝）")
	require.Equal(t, credential.TypeCodexOAuth, sel.CredentialType)
	s.Release(sel.AccountID)
	s.MarkResult(sel.AccountID, rule.KindOK, nil, 200, "")
}

func intPtrStr(s string) *string { return &s }

// TestRequestPathZeroLoaderCalls 热路径零 DB：加载只发生在快照加载期
// （InvalidateAllSync），Select/MarkResult/Release 请求期零加载器触达。
func TestRequestPathZeroLoaderCalls(t *testing.T) {
	tpl := tpl(1, domain.FormatOpenAIResponsesWS, []string{"gpt-4o"})
	inner := newMemLoader(map[int64][]*domain.Account{10: {acc(7, tpl, 4)}})
	cl := &countingLoader{inner: inner}
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background()))
	s := New(testCfg(), cl, re, nil)
	require.NoError(t, s.reload(context.Background()))
	before := cl.loadsN()
	for i := 0; i < 50; i++ {
		sel, err := s.Select(10, domain.FormatOpenAIResponsesWS, "gpt-4o")
		require.NoError(t, err)
		s.Release(sel.AccountID)
		s.MarkResult(sel.AccountID, rule.KindOK, nil, 200, "")
	}
	require.Equal(t, before, cl.loadsN(), "请求期（Select/MarkResult/Release）零加载器触达——热路径零 DB")
}

// —— RuleKindOf 单点分流（gate r3/r5） ——

// TestRuleKindOf RuleKindOf(httpStatus) 分流矩阵：0 → network（连接级）、
// ≥500 → 5xx、1-499（不可达防御——调用点恒 0/≥500，4xx 走骨架透传不至此）
// → 5xx。
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

// TestMarkResultNetworkVs5xxSplit 经 MarkResult 全链路：code==0 事件 → network
// 种子（5s 冷却）命中、code=500 → 5xx 种子（10m）命中——分流在 RuleKindOf 单点，
// 调用方零改动（failoverLoop/ws_relay/caller 全部连接级调用点自动正确）。
func TestMarkResultNetworkVs5xxSplit(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIResponsesWS, []string{"gpt-4o"})
	m := newMemLoader(map[int64][]*domain.Account{10: {
		{ID: 1, TemplateID: 1, Template: tplx, UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
	}})
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background())) // 空表写种子（含 seed-network 5s / seed-5xx 10m）
	s := New(testCfg(), m, re, nil)
	require.NoError(t, s.reload(context.Background()))

	// 连接级（code==0）→ seed-network → unhealthy + 5s 冷却（不吃 5xx 的 10m）
	s.MarkResult(1, rule.KindNetwork, nil, 0, "dial tcp: connection refused")
	s.FlushRules()
	ri, ok := s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusUnhealthy, ri.Status)
	require.NotNil(t, ri.CooldownUntil)
	d := ri.CooldownUntil.Sub(s.timeNow())
	require.InDelta(t, 5*time.Second, d, float64(2*time.Second), "seed-network 冷却 5s（连接级独立）")

	// 5xx（code=500）→ seed-5xx → unhealthy + 10m
	s.MarkResult(1, rule.Kind5xx, nil, 500, "boom")
	s.FlushRules()
	ri, ok = s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusUnhealthy, ri.Status)
	require.NotNil(t, ri.CooldownUntil)
	d = ri.CooldownUntil.Sub(s.timeNow())
	require.InDelta(t, 10*time.Minute, d, float64(30*time.Second), "seed-5xx 冷却 10m（用户裁决）")
}

// TestSchedulerClassify scheduler.Classify 包装：快照取 TemplateID/GroupID 后
// 委托引擎；transmit = 命中规则 then.transmit；快照外账号 → (false, false)。
func TestSchedulerClassify(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIResponsesWS, []string{"gpt-4o"})
	m := newMemLoader(map[int64][]*domain.Account{10: {
		{ID: 1, TemplateID: 1, Template: tplx, UpstreamKey: "k1", Status: domain.StatusActive, Weight: 100, MaxConcurrency: 1000},
	}})
	rstore := &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}
	http400, http401 := 400, 401
	_, err := rstore.CreateRule(context.Background(), domain.Rule{
		Name: "transmit-400", Enabled: true, Priority: 10,
		When: domain.RuleWhen{Kind: strPtr("4xx"), HTTPStatus: &http400},
		Then: domain.RuleThen{Transmit: true},
	})
	require.NoError(t, err)
	_, err = rstore.CreateRule(context.Background(), domain.Rule{
		Name: "punish-401", Enabled: true, Priority: 20,
		When: domain.RuleWhen{Kind: strPtr("4xx"), HTTPStatus: &http401},
		Then: domain.RuleThen{Status: statusPtr(domain.StatusUnhealthy), Cooldown: strPtr("30m")},
	})
	require.NoError(t, err)
	re := rule.New(rule.Config{}, rstore, nil)
	require.NoError(t, re.Reload(context.Background()))
	s := New(testCfg(), m, re, nil)
	require.NoError(t, s.reload(context.Background()))

	// 400 → transmit=true（seed 语义同）
	tx, pu := s.Classify(rule.Event{AccountID: 1, Kind: rule.Kind4xx, HTTPStatus: &http400})
	require.True(t, tx)
	require.False(t, pu)
	// 401 → punish（unhealthy 30m），transmit=false
	tx, pu = s.Classify(rule.Event{AccountID: 1, Kind: rule.Kind4xx, HTTPStatus: &http401})
	require.False(t, tx)
	require.True(t, pu)
	// 快照外账号 → (false, false)
	tx, pu = s.Classify(rule.Event{AccountID: 999, Kind: rule.Kind4xx, HTTPStatus: &http401})
	require.False(t, tx)
	require.False(t, pu)
}

// TestApplyDisabledActionPersistsAcrossReload 规则动作 disabled 必须回写（bug
// 2026-08-18）：规则匹配命中 → 动作置 disabled → apply 后 enqueueWrite 被调用
// 且回写经 loader 落库 status=disabled → ≤30s 全量同步等价（InvalidateAllSync
// 从数据源重建快照）后仍 disabled（不复活）。修复前：回写前复查把"本 apply
// 动作即 disabled"（CAS 后活快照已 disabled）误判为他人并发置位 → 跳过
// enqueueWrite → DB 恒 active → 全量同步拉回复活，规则禁用完全失效。断言装置
// 复用既有模式：persistLoader（fail_test.go）+ drainWrites + 落库断言
// （TestFailAccountPersistsAcrossReload 同构）。
func TestApplyDisabledActionPersistsAcrossReload(t *testing.T) {
	pl := newPersistLoader(map[int64][]*domain.Account{10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)}})
	s := newSchedLoader(t, pl)

	disabled := domain.StatusDisabled
	s.apply(1, &disabled, nil, nil, "") // 规则引擎 disabled 动作同构的 apply
	drainWrites(t, s)                   // 回写经 loader 落库

	ri, ok := s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri.Status, "快照已 disabled")

	pl.mu.Lock()
	require.Equal(t, domain.StatusDisabled, pl.byGroup[10][0].Status, "回写发生：loader 落库 status=disabled")
	pl.mu.Unlock()

	require.NoError(t, s.InvalidateAllSync()) // 重启/全量同步等价：从数据源重建快照
	ri, ok = s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri.Status, "全量重载后仍 disabled（不复活）")

	_, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrNoAvailable, "disabled 账号不可调度")
}

// --- 快照重建复用旧实例（计数器连续性，2026-08-18） ---

// reuseByID 取当前 byID 快照中账号的实例（复用断言用；测试单线程访问安全）。
func reuseByID(s *Scheduler, id int64) *accountSnapshot {
	return s.store.byID.Load().(map[int64]*accountSnapshot)[id]
}

// TestReusePreservesErrCountersAcrossReload 复用后 errRate/errCount/lastError
// 跨全量重建保留（不归零）——管理端账号列表 30s 清零问题（用户观察 2026-08-18）
// 的修复断言：实例指针不变（复用机制本体）+ 动态字段保留内存值。
func TestReusePreservesErrCountersAcrossReload(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)

	// 错误事件：errCount=1 + lastError + EWMA 非零（种子 5xx → unhealthy）
	s.MarkResult(1, rule.Kind5xx, nil, 500, "boom")
	s.FlushRules()
	before := reuseByID(s, 1)
	require.Equal(t, 1, before.statePtr().errCount)
	require.NotNil(t, before.statePtr().lastError)
	require.Equal(t, "boom", *before.statePtr().lastError)
	require.Greater(t, float64(before.errRate.Load())/errRateScale, 0.0, "EWMA 已更新")

	// 全量重建（≤30s 定时同步 / InvalidateAllSync 同路径）
	require.NoError(t, s.reload(context.Background()))
	after := reuseByID(s, 1)
	require.Same(t, before, after, "已存在账号复用旧实例（指针不变）")
	ri, _ := s.Runtime(1)
	require.Equal(t, 1, ri.ErrCount, "errCount 跨重建保留（不归零）")
	require.Equal(t, "boom", *after.statePtr().lastError, "lastError 跨重建保留")
	require.Greater(t, float64(after.errRate.Load())/errRateScale, 0.0, "errRate 跨重建保留（EWMA 连续）")
}

// TestReuseConcurrencyContinuity 复用后 concurrency 保留且新请求 CAS 连续
//（+1/-1 同实例）——Load-Store 间隙残留窗口消除（指针不变，原子操作天然连续）。
func TestReuseConcurrencyContinuity(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)

	// 两个在途请求占槽
	sel1, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	sel2, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	before := reuseByID(s, 1)

	// 重建复用：实例指针不变、计数不归零（原子连续，无需 Load-Store 继承搬运）
	require.NoError(t, s.reload(context.Background()))
	after := reuseByID(s, 1)
	require.Same(t, before, after, "复用实例指针不变")
	require.Equal(t, int64(2), after.concurrency.Load(), "重建后计数保持")

	// Release 命中同一实例：+1/-1 连续，不得拉负
	s.Release(sel1.AccountID)
	s.Release(sel2.AccountID)
	require.Equal(t, int64(0), after.concurrency.Load(), "释放归零，不得为负")

	// 重建后新请求 CAS 连续（同实例递增，无继承间隙窗口）
	sel3, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, int64(1), after.concurrency.Load(), "重建后新请求在原子计数上连续 +1")
	s.Release(sel3.AccountID)
}

// TestReuseSyncsStaticFieldsFromDB 静态字段 DB 权威同步：管理面改动
//（weight/status/max_concurrency）→ 重建后复用实例读到新值（管理面改动生效）。
func TestReuseSyncsStaticFieldsFromDB(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)
	before := reuseByID(s, 1)

	// 管理面改动 DB（memLoader 与快照共享账号指针——原地改即数据源变更）
	m.mu.Lock()
	a := m.byGroup[10][0]
	a.Weight = 50
	a.Status = domain.Status429
	a.MaxConcurrency = 1
	m.mu.Unlock()
	require.NoError(t, s.reload(context.Background()))

	after := reuseByID(s, 1)
	require.Same(t, before, after, "复用实例指针不变")
	require.Equal(t, 50, after.static.Load().acc.Weight, "weight 同步 DB 新值")
	require.Equal(t, 1, after.static.Load().acc.MaxConcurrency, "max_concurrency 同步 DB 新值")
	ri, _ := s.Runtime(1)
	require.Equal(t, domain.Status429, ri.Status, "status 同步 DB 新值")

	// 门禁按新 max 生效：占满 1 个槽后第二个请求 ErrNoAvailable
	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.AccountID)
	_, err = s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrNoAvailable, "max_concurrency=1 → 第二个请求假满")
	s.Release(sel.AccountID)
}

// TestReuseClampsMaxConcurrency 复用分支的 MaxConcurrency 钳制（评审 M-2）：
// DB max_concurrency=0 的账号复用后钳制为 defaultMax——不钳制则门禁
// cur >= 0 恒真 → 账号永久不可选（静默回归）。
func TestReuseClampsMaxConcurrency(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 0)}})
	s := newSched(t, m) // 首次加载：新建分支钳制
	require.Equal(t, 2, reuseByID(s, 1).static.Load().acc.MaxConcurrency, "新建分支钳制 defaultMax=2")
	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err, "钳制后门禁不恒满")
	s.Release(sel.AccountID)

	// 重建（复用分支）：同样钳制，不得把 DB=0 带进实例
	require.NoError(t, s.reload(context.Background()))
	require.Equal(t, 2, reuseByID(s, 1).static.Load().acc.MaxConcurrency, "复用分支钳制 defaultMax=2")
	sel, err = s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err, "复用后门禁不恒满")
	s.Release(sel.AccountID)
}

// TestReuseGroupIDsResetOnRemoval groupIDs 首次出现重置（评审 M-1）：账号从组
// 移除后全量重建 → 复用实例 groupIDs 不含旧 gid——append 旧值残留会导致
// processWrite 发布过期组、InvalidateGroup otherGids 推导错误。
func TestReuseGroupIDsResetOnRemoval(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	a := acc(1, tplx, 4)
	m := newMemLoader(map[int64][]*domain.Account{10: {a}, 20: {a}})
	s := newSched(t, m)
	before := reuseByID(s, 1)
	require.ElementsMatch(t, []int64{10, 20}, before.static.Load().groupIDs, "多组账号跨组引用集完整")

	// 从组 20 移除（DB 只属组 10）后全量重建：复用实例 groupIDs 重置为 [10]
	m.mu.Lock()
	m.byGroup[20] = nil
	m.mu.Unlock()
	require.NoError(t, s.reload(context.Background()))
	after := reuseByID(s, 1)
	require.Same(t, before, after, "实例复用（非新建）")
	require.Equal(t, []int64{10}, after.static.Load().groupIDs, "旧 gid 20 不得残留")
}

// TestInvalidateGroupReuseKeepsCounters 组级路径（评审 M-4）：组级 NOTIFY 重载
// 后 errCount/errRate/lastError 保留 + groupIDs 重建正确——组级重载也经
// buildSnapshots，复用机制自动覆盖（管理端改账号/组 NOTIFY 同路径）。
func TestInvalidateGroupReuseKeepsCounters(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)

	s.MarkResult(1, rule.Kind5xx, nil, 500, "boom")
	s.FlushRules()
	before := reuseByID(s, 1)
	require.Equal(t, 1, before.statePtr().errCount)

	// 组级重载（与全量同步同纪律的复用路径）
	s.InvalidateGroup(10)
	after := reuseByID(s, 1)
	require.Same(t, before, after, "组级重载复用旧实例（指针不变）")
	require.Equal(t, 1, after.statePtr().errCount, "errCount 跨组级重载保留")
	require.Equal(t, "boom", *after.statePtr().lastError, "lastError 跨组级重载保留")
	require.Greater(t, float64(after.errRate.Load())/errRateScale, 0.0, "errRate 跨组级重载保留")
	require.Equal(t, []int64{10}, after.static.Load().groupIDs, "groupIDs 重建为 [10]（无残留）")

	// 组级重载后仍可正常调度。冷却保留语义（2026-08-19 缺陷 2 修复：组级重载
	// 不再隐式清内存冷却——seed-5xx 的 10m 冷却随重载保留）→ 推进时间越过
	// 冷却后惰性恢复（TestMark429CooldownAndRecover 同款）。
	s.timeNow = func() time.Time { return time.Now().Add(11 * time.Minute) }
	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.AccountID)
	s.Release(sel.AccountID)
}

// TestReuseNewAccountCreatesFresh 新账号（DB 新增）→ 新建实例：复用只作用于
// 已存在账号；新实例含 state 初始化与钳制，动态字段自 0 起。
func TestReuseNewAccountCreatesFresh(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 4)}})
	s := newSched(t, m)
	old1 := reuseByID(s, 1)

	// DB 新增账号 2（max_concurrency=0 顺带验证新建分支钳制）
	m.mu.Lock()
	m.byGroup[10] = append(m.byGroup[10], acc(2, tplx, 0))
	m.mu.Unlock()
	require.NoError(t, s.reload(context.Background()))

	require.Same(t, old1, reuseByID(s, 1), "已存在账号仍复用")
	as2 := reuseByID(s, 2)
	require.NotNil(t, as2, "新账号进入 byID")
	require.Equal(t, 2, as2.static.Load().acc.MaxConcurrency, "新账号新建分支钳制 defaultMax=2")
	require.Equal(t, []int64{10}, as2.static.Load().groupIDs, "新账号组引用集登记")
	require.Zero(t, as2.concurrency.Load(), "新账号计数自 0 起")
	require.Zero(t, as2.statePtr().errCount, "新账号状态全新")
}

// TestReuseConcurrentSelectReloadRace 评审 Critical 回归：复用分支对已发布实例
// 的静态字段写（acc/tpl/gid/groupIDs 原子指针发布）与热路径无锁读
//（pickFrom/MarkResult/Classify 的 static.Load()）并发——修复前裸写实例字段
// 构成数据竞态（自建并发复现 -race 5 处 WARNING），修复后 -race 必须静默。
// 并发 Select/Release/MarkResult/Classify + 全量重建（触发复用分支）循环。
func TestReuseConcurrentSelectReloadRace(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tplx, 1000)}})
	s := newSched(t, m)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.writebackLoop(ctx) // 消费 MarkResult 的状态回写

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		// 请求热路径：Select（pickFrom 静态读点）+ Release + MarkResult/Classify
		//（TemplateID/gid 静态读点）
		defer wg.Done()
		for i := 0; i < 8000; i++ {
			sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
			if err != nil {
				continue
			}
			s.Release(sel.AccountID)
			s.MarkResult(sel.AccountID, rule.KindOK, nil, 200, "")
			s.Classify(rule.Event{AccountID: sel.AccountID, Kind: rule.Kind5xx})
		}
	}()
	wg.Add(1)
	go func() {
		// 重建侧：全量重建每次命中复用分支（静态字段视图整体原子替换）
		defer wg.Done()
		for i := 0; i < 400; i++ {
			require.NoError(t, s.reload(context.Background()))
		}
	}()
	wg.Wait()
}
