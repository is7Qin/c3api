// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
)

// persistLoader 模拟 DB 持久化的 Loader：UpdateAccountStatus 把状态写回数据源
// （重启快照重载 = 从数据源重建快照仍摘除——memLoader 只记录不落数据，无法
// 表达"重启"语义）。
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

func (m *persistLoader) UpdateAccountStatus(ctx context.Context, id int64, status domain.AccountStatus, cooldown *time.Time, lastErr *string, weight *int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, accs := range m.byGroup {
		for _, a := range accs {
			if a.ID == id {
				a.Status = status
				a.CooldownUntil = cooldown
				a.LastError = lastErr
				return nil
			}
		}
	}
	return nil
}

// drainWrites 排空回写队列（等价 Close 的排空逻辑；测试并发无写回循环时使用）。
func drainWrites(t *testing.T, s *Scheduler) {
	t.Helper()
	require.NoError(t, s.Close(context.Background()))
}

// newSchedLoader 同 newSched，接受任意 Loader（memLoader / persistLoader）。
func newSchedLoader(t *testing.T, m Loader) *Scheduler {
	t.Helper()
	re := rule.New(rule.Config{}, &fakeRuleStore{rules: map[int64]domain.Rule{}, next: 1}, nil)
	require.NoError(t, re.Reload(context.Background()))
	s := New(testCfg(), m, re, nil)
	require.NoError(t, s.reload(context.Background()))
	return s
}

// TestFailAccountRemovesFromSelection 失效摘除：快照置 disabled 后 pickFrom
// 复用既有过滤器跳过（Select → ErrNoAvailable）。
func TestFailAccountRemovesFromSelection(t *testing.T) {
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)}})
	s := newSched(t, m)

	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.AccountID)
	s.Release(sel.AccountID)

	s.FailAccount(1, "auth permanently revoked")

	_, err = s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrNoAvailable, "失效账号不得再被选中")

	ri, ok := s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri.Status)
}

// TestFailAccountPersistsAcrossReload 摘除必须持久化（brief 明示：仅内存摘除则
// 重启后快照重载复活——pickFrom 不查 failed_at，必须落库 status=disabled）：
// FailAccount → 回写 drain（loader 落库）→ 全量重载（模拟重启快照重建）→ 仍摘除。
func TestFailAccountPersistsAcrossReload(t *testing.T) {
	pl := newPersistLoader(map[int64][]*domain.Account{10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)}})
	s := newSchedLoader(t, pl)

	s.FailAccount(1, "auth permanently revoked")
	drainWrites(t, s) // 回写经 loader 落库（status=disabled + last_error）

	pl.mu.Lock()
	require.Equal(t, domain.StatusDisabled, pl.byGroup[10][0].Status, "loader 落库 status=disabled")
	pl.mu.Unlock()

	require.NoError(t, s.InvalidateAllSync()) // 重启等价：从数据源全量重建快照
	_, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrNoAvailable, "重启快照重载后仍摘除")
}

// TestFailAccountAuditsLastError 审计 last_error：回写携带失效原因摘要
// （域内截断 500——与 last_error 列共用截断语义）。
func TestFailAccountAuditsLastError(t *testing.T) {
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)}})
	s := newSched(t, m)

	long := strings.Repeat("x", 600)
	s.FailAccount(1, long)
	drainWrites(t, s)

	m.mu.Lock()
	defer m.mu.Unlock()
	require.Len(t, m.writes, 1)
	require.Equal(t, domain.StatusDisabled, m.writes[0].status)
	require.NotNil(t, m.writes[0].lastErr)
	require.Equal(t, domain.ErrMsgMaxLen, len(*m.writes[0].lastErr), "last_error 域内截断 500")
}

// TestFailAccountMarkResultGuard 防复活守卫复用：失效后 MarkResult（成功/错误）
// 不得把状态重置回 active 并回写 DB（MarkResult 对 disabled 短路）。
func TestFailAccountMarkResultGuard(t *testing.T) {
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)}})
	s := newSched(t, m)

	// 在途请求占槽后失效（模拟：失效时仍有在途请求完成回流）
	_, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err)

	s.FailAccount(1, "auth permanently revoked")
	s.MarkResult(1, rule.KindOK, nil, 200, "")
	s.MarkResult(1, rule.KindNetwork, nil, 0, "stale error")
	s.FlushRules()
	s.Release(1)
	drainWrites(t, s)

	m.mu.Lock()
	defer m.mu.Unlock()
	require.Len(t, m.writes, 1, "仅失效摘除一次回写；MarkResult 全部短路")
	require.Equal(t, domain.StatusDisabled, m.writes[0].status)

	ri, ok := s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri.Status, "快照保持 disabled")
}

// TestFailAccountQueuedEventsBeforeFailure 入队在先防复活（P1 评审——探针
// 确定性复现）：规则事件在失效置位**之前**已入队（MarkResult 时账号仍 active，
// 守卫放行；网络/5xx/KindOK 各一）→ FailAccount 置 disabled → FlushRules
// 处理入队事件 → apply 必须跳过 disabled 快照的状态覆盖——快照与 loader 落库
// 均保持 disabled + 不可调度（修复前：apply 覆盖为 unhealthy/active，回写合并
// 后写覆盖先写把 DB 落成 unhealthy/active，账号重新可调度）。
func TestFailAccountQueuedEventsBeforeFailure(t *testing.T) {
	pl := newPersistLoader(map[int64][]*domain.Account{10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)}})
	s := newSchedLoader(t, pl)

	// 入队在先：error→unhealthy+冷却 / ok→active 两条事件在失效前入队
	s.MarkResult(1, rule.Kind5xx, nil, 500, "boom")
	s.MarkResult(1, rule.KindOK, nil, 200, "")
	s.FailAccount(1, "auth permanently revoked")
	s.FlushRules() // 消费入队事件 → apply
	drainWrites(t, s)

	ri, ok := s.Runtime(1)
	require.True(t, ok)
	require.Equal(t, domain.StatusDisabled, ri.Status, "入队在先事件不得覆盖 disabled 快照")

	pl.mu.Lock()
	require.Equal(t, domain.StatusDisabled, pl.byGroup[10][0].Status, "DB 落库保持 disabled（apply 回写不得覆盖）")
	pl.mu.Unlock()

	_, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.ErrorIs(t, err, ErrNoAvailable, "失效账号不可调度")
}

// TestFailAccountUnknownAccount 快照外/未加载账号：no-op 不 panic。
func TestFailAccountUnknownAccount(t *testing.T) {
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)}})
	s := newSched(t, m)

	s.FailAccount(999, "unknown") // 快照外账号
	s.FailAccount(1, "known")     // 正常路径不破坏
	drainWrites(t, s)

	m.mu.Lock()
	defer m.mu.Unlock()
	require.Len(t, m.writes, 1)
	require.Equal(t, int64(1), m.writes[0].id)
}

// TestFailAccountIdempotentDisabled 幂等早退（spec M2 采纳语义）：账号已
// disabled 时再次 FailAccount 不重复写 lastError 审计与回写——新语义：
// 首个置位者写审计（终态 disabled 不变，仅审计内容与旧"最后写者"不同）。
func TestFailAccountIdempotentDisabled(t *testing.T) {
	m := newMemLoader(map[int64][]*domain.Account{10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)}})
	s := newSched(t, m)

	s.FailAccount(1, "first reason")
	s.FailAccount(1, "second reason") // 已 disabled：幂等早退，不重复审计/回写
	drainWrites(t, s)

	m.mu.Lock()
	defer m.mu.Unlock()
	require.Len(t, m.writes, 1, "仅首个失效一次回写；重复失效不重复投递")
	require.Equal(t, "first reason", *m.writes[0].lastErr, "首个置位者写审计")
}

// TestFailAccountApplyConcurrentNoRevive 并发交错防复活（spec C3）：规则动作
// （apply(active)）与失效（FailAccount）并发交错。修复前守卫是 check-then-act
// （disabled 检查与 Store 之间的窗口：FailAccount 可在此窗口置 disabled，
// apply 随后用旧读覆盖回 active——内存+DB 双双复活）；修复后 copy-on-write
// CAS 把两个转换串行化（+ 回写前复查挡住 active 回写晚于 disabled 入队）。
//
// 用例形态：**逐轮全新调度器 + 单次 apply‖单次 FailAccount**——每轮两个转换
// 的 Store 顺序是公平竞态（谁后写谁胜，修复前复活率 ≈50%），多轮必现；若用
// "A 循环 apply / B 循环 FailAccount"形态，FailAccount 每轮含阻塞入队、整体
// 比 apply 慢，其最后一次 Store 恒收尾（末位写者胜利）→ 终态恒 disabled，
// 断言被架空（实测修复前 0/6 命中——不采用）。修复后每轮终态确定性 disabled
// （apply 要么 CAS 先成功被 FailAccount 覆盖、要么读到 disabled 早退）。
// 断言：每轮内存终态恒 disabled（确定性断言——-race 是概率检测，终态断言
// 才是保证）。
//
// 注意：DB 终态断言不在并发交错下做——复查-入队指令间隙的残余窗口（spec
// M1b 接受：FailAccount 的 CAS+阻塞入队整体落进该间隙 → 通道序 [disabled,
// active] 后写覆盖 → DB active）在测试抢占下实测可达（-race 千轮量级一次），
// 断言"DB 恒 disabled"在此是概率失败而非确定性护栏。DB 终态/重载不复活由
// 顺序用例确定性覆盖：TestFailAccountPersistsAcrossReload（回写排空后 DB 恒
// disabled + 重载快照不复活）。
func TestFailAccountApplyConcurrentNoRevive(t *testing.T) {
	active := domain.StatusActive
	const rounds = 200
	for i := 0; i < rounds; i++ {
		pl := newPersistLoader(map[int64][]*domain.Account{10: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)}})
		s := newSchedLoader(t, pl)

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			s.apply(1, &active, nil, nil, "") // 规则引擎 worker 同构的 apply
		}()
		go func() {
			defer wg.Done()
			s.FailAccount(1, "auth revoked") // sdkbridge 失效回调同构
		}()
		wg.Wait()

		ri, ok := s.Runtime(1)
		require.True(t, ok)
		require.Equal(t, domain.StatusDisabled, ri.Status, "第 %d 轮交错后内存终态恒 disabled", i)
	}
}

// TestApplyWeightConcurrentInvalidateNoRace 权重写与组级重载并发（spec C3）：
// apply 的 acc.Weight 写（锁外→C2 移入 reloadMu 锁区）与 InvalidateGroup 的
// 锁内读（重建其它组路由时 buildRoutes 读非本组旧实例的 Weight）并发 =
// 修复前的 Go 内存模型数据竞态（-race 报告）；修复后写读同锁闭合。
// 用例构造：账号 3 仅在组 20——InvalidateGroup(10) 重建组 20 路由时经 repl
// 保留并读取账号 3 旧实例的 Weight，与 apply(3) 的写并发。断言：-race 无
// 报告（概率检测）+ 交错后路由一致（两组均可调度）。
func TestApplyWeightConcurrentInvalidateNoRace(t *testing.T) {
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	m := newMemLoader(map[int64][]*domain.Account{
		10: {acc(1, tplx, 4)},
		20: {acc(1, tplx, 4), acc(3, tpl(3, domain.FormatOpenAIChat, []string{"m"}), 4)},
	})
	s := newSched(t, m)

	const n = 2000
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			w := 10 + i%5 // 权重持续变化（≥1）
			s.apply(3, nil, nil, &w, "")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < n; i++ {
			s.InvalidateGroup(10)
		}
	}()
	wg.Wait()

	sel, err := s.Select(10, domain.FormatOpenAIChat, "m")
	require.NoError(t, err, "组 10 路由一致")
	require.Equal(t, int64(1), sel.AccountID)
	s.Release(sel.AccountID)

	sel2, err := s.Select(20, domain.FormatOpenAIChat, "m")
	require.NoError(t, err, "组 20 路由一致（组级重载未破坏共享实例引用）")
	s.Release(sel2.AccountID)
}
