// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// oauthRotationFixture 搭一个 codex-oauth 单账号调度器：模板类型与账号 ext 行
// 一致（指纹可派生），token 可轮转。返回调度器、内存 loader 与路由。
func oauthRotationFixture(t *testing.T, token, refresh string) (*Scheduler, *memLoader, RouteRef) {
	t.Helper()
	tplx := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	tplx.CredentialType = credential.TypeCodexOAuth
	a := acc(1, tplx, 4)
	a.Ext = &domain.AccountExt{
		AccountID: 1, CredentialType: credential.TypeCodexOAuth,
		CodexIdentity:          &domain.CodexIdentity{InstallationID: "inst-1", SessionID: "s1", ThreadID: "t1", WindowID: "w1"},
		CodexAccountID:         strPtrT("ca-1"),
		CodexOAuthToken:        strPtrT(token),
		CodexOAuthRefreshToken: strPtrT(refresh),
	}
	m := newMemLoader(map[int64][]*domain.Account{10: {a}})
	s := newSched(t, m)
	route := RouteRefFor(10, string(domain.FormatOpenAIChat), "m")
	return s, m, route
}

// rotateTokenKNeutral 模拟 SDK 自动刷新的落库语义（WriteOAuthRotation）：只写
// account_ext 行的令牌三元组，不触 accounts 行，K（IdentityRevision）不变。
// 重载读到的是全新 Ext 对象（DB 行重映射）：整体替换 Ext 指针，旧对象保持
// 不动——旧快照叶的不可变性正依赖这一点。
func rotateTokenKNeutral(m *memLoader, token, refresh string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old := m.byGroup[10][0].Ext
	next := *old
	next.CodexOAuthToken = strPtrT(token)
	next.CodexOAuthRefreshToken = strPtrT(refresh)
	m.byGroup[10][0].Ext = &next
}

// bindStalePlan 绑定一个新会话：该会话的候选是"变更前编译的"，用来验证
// 变更后的预留行为。会话从发布视图当时绑定的决策取候选，随后视图更新，
// 会话手里的即为旧计划。
func bindStalePlan(t *testing.T, s *Scheduler, route RouteRef, reqID string) AttemptPlan {
	t.Helper()
	plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: reqID, ApplyModelMapping: true}, route)
	require.NoError(t, err)
	return plan
}

// TestCompiledPlanSurvivesOAuthTokenRotation 只改 token（K 中性路径）⇒
// 已编译计划仍可预留。SDK 自动刷新只写 account_ext 行、不推进 K：计划摘要
// 不变而叶子换新——在途计划保持有效，且 Selection 装配的是新 token。
//
// 三个断言缺一不可：预留成功证明计划有效；叶子换新证明不是"叶子没换"的假象；
// Selection.Ext 是新 token 证明载荷确实取了新值（SDK 拿到新令牌）。
func TestCompiledPlanSurvivesOAuthTokenRotation(t *testing.T) {
	s, m, route := oauthRotationFixture(t, "at-old", "rt-old")

	oldLeaf, ok := s.View().Account(1)
	require.True(t, ok)
	oldStatic := oldLeaf.static.Load()
	require.NotNil(t, oldStatic)
	oldPlanKey := planKeyOf(oldStatic)
	require.Equal(t, "at-old", derefString(oldStatic.acc.Ext.CodexOAuthToken))

	plan := bindStalePlan(t, s, route, "req-rotation")

	// K 中性轮转 + 组级定向重载 + 配对编译（生产 SDK 回调后的失效路径）。
	rotateTokenKNeutral(m, "at-new", "rt-new")
	s.InvalidateAccount(1)
	s.compileOnce()

	curLeaf, ok := s.View().Account(1)
	require.True(t, ok)
	require.NotSame(t, oldLeaf, curLeaf, "rotated payload must mint a new leaf (staticKey changed)")
	curStatic := curLeaf.static.Load()
	require.NotNil(t, curStatic)
	require.Equal(t, "at-new", derefString(curStatic.acc.Ext.CodexOAuthToken), "new leaf must carry the rotated token")
	require.Equal(t, oldStatic.acc.IdentityRevision, curStatic.acc.IdentityRevision, "K-neutral rotation must not advance K")
	require.True(t, oldPlanKey == planKeyOf(curStatic), "token-only rotation must not change the plan key")

	sel, attempt, err := s.ReserveAttempt(&plan)
	require.NoError(t, err, "compiled plan must still reserve after token-only rotation")
	require.Equal(t, int64(1), attempt.AccountID)
	require.Equal(t, int64(1), sel.AccountID)
	require.NotNil(t, sel.Ext)
	require.Equal(t, "at-new", derefString(sel.Ext.CodexOAuthToken), "Selection must assemble the fresh token from the current leaf")
	require.Equal(t, "rt-new", derefString(sel.Ext.CodexOAuthRefreshToken))
	sel.Release()

	// 运维面同样不受影响：候选仍投影完整元数据，而非 identity-only 幽灵行。
	proj := s.CurrentRoutingPlan()
	require.Len(t, proj.Routes, 1)
	require.Len(t, proj.Routes[0].Candidates, 1)
	require.Equal(t, int64(1), proj.Routes[0].Candidates[0].AccountID)
	require.NotEmpty(t, proj.Routes[0].Candidates[0].Fingerprint, "payload-only rotation must not degrade the projected candidate")
}

// TestCompiledPlanInvalidatedByUpstreamKeyChange 改身份（upstream_key）⇒
// 计划被打断：planKey 变，旧计划在新视图上预留必须失败。
func TestCompiledPlanInvalidatedByUpstreamKeyChange(t *testing.T) {
	s, m, route := oauthRotationFixture(t, "at-old", "rt-old")
	plan := bindStalePlan(t, s, route, "req-identity-change")

	m.mu.Lock()
	m.byGroup[10][0].UpstreamKey = "k-rotated"
	m.mu.Unlock()
	s.InvalidateAccount(1)
	s.compileOnce()

	_, _, err := s.ReserveAttempt(&plan)
	require.Error(t, err, "identity change must invalidate the compiled plan")

	// 新编译的计划在新视图上正常预留（打断只针对旧计划）。
	fresh, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-fresh", ApplyModelMapping: true}, route)
	require.NoError(t, err)
	sel, _, err := s.ReserveAttempt(&fresh)
	require.NoError(t, err)
	require.Equal(t, int64(1), sel.AccountID)
	sel.Release()
}

// TestCompiledPlanInvalidatedByDecisionInputChange 改决策输入（enabled）⇒
// 计划被打断：旧计划在新视图上预留必须失败。单账号夹具——旧计划的唯一候选
// 失效后无处可退，失败只能是"旧计划被打断"。
func TestCompiledPlanInvalidatedByDecisionInputChange(t *testing.T) {
	s, m, route := oauthRotationFixture(t, "at-old", "rt-old")

	plan := bindStalePlan(t, s, route, "req-decision-change")

	m.mu.Lock()
	m.byGroup[10][0].Enabled = false
	m.mu.Unlock()
	s.InvalidateAccount(1)
	s.compileOnce()

	_, _, err := s.ReserveAttempt(&plan)
	require.Error(t, err, "decision-input change must invalidate the compiled plan")
}
