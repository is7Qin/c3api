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

// 本文件钉住 S2 的两条续接判定，它们的判据都是**逐 Type 能力位**，不是执行通道：
//
//	① 计划级：planAllCandidatesNativeContinuation（编译期）→ AttemptPlan 只读位
//	   ⇒ proxy 决定"要不要解析 previous_response_id 绑定"（fail-closed 方向）；
//	② 派发级：skipContinuationFor（预留装配期）→ Selection.SkipContinuation 只读位
//	   ⇒ proxy 决定"这次派发完全跳过解析"。
//
// 为什么必须分开锁：通道（谁鉴权）与能力（能不能续接）今天取值恰好一致，任何把它们
// 焊成一个等式的实现都会在**全部**真实类型上通过。两个替身用例分别把两条轴掰开。

// TestSkipContinuationForFollowsCapabilities 全类型 golden：今天只有 BPS 同时缺
// "原生续接"与"绑定账本"，因此只有它完全跳过解析。
//
// 断言用**手抄**的期望，不用 `!supports && !staticKey` 反推——反推会与实现同源重言。
func TestSkipContinuationForFollowsCapabilities(t *testing.T) {
	want := map[credential.Type]bool{
		credential.TypeAPIKey:           false, // 无原生续接，但吃绑定账本 ⇒ 必须解析
		credential.TypeResponsesSpecial: false,
		credential.TypeCustom:           false,
		credential.TypeCodexOAuth:       false, // 原生续接
		credential.TypeCodexPAT:         false,
		credential.TypeBPS:              true, // 两者皆无（BPS 没有服务端续接）⇒ 跳过
	}
	for typ, skip := range want {
		require.Equal(t, skip, skipContinuationFor(typ), "%q 的续接派发政策漂移", typ)
		require.True(t, typ.Valid(), "golden 只允许覆盖表内类型：%q", typ)
	}

	// 判据是两条能力位的合取，各自逐类型可读——不是通道投影：
	// 静态 key 类型「无原生续接」但仍不跳过，正是"能力 ≠ 是否跳过"的证据。
	require.False(t, credential.TypeAPIKey.SupportsContinuation())
	require.True(t, credential.TypeAPIKey.UsesStaticKey())
	require.False(t, credential.TypeBPS.UsesStaticKey())
}

// TestSkipContinuationForUnknownTypeIsFailClosed 表外类型（含 CredentialType == ""）
// 必须返回 **false**（不跳过解析 = fail-closed）。
//
// 为什么这条是 Critical 方向：表外类型的两个能力位都是 false，若判据直接写成
// `!SupportsContinuation() && !UsesStaticKey()` 就会得 true ⇒ "缺失绑定 ⇒ 410" 悄悄
// 变成"丢掉 previous_response_id 放行"。而这条路径**可达**：
// routing_compiler_candidates.go 的候选过滤只拒 `CredentialType != "" && !Valid()`，
// 空类型模板照常成为候选并产出 Selection。语义与 service/format_gate.go 同一选择
// （"表外历史 credential_type 因此 fail-closed——这是刻意的行为选择"）。
//
// "能力 ≠ 通道" 的判别力不靠这条（表外类型两边都给 false，没有区分度）：它由
//   - credential 的 TestContinuationCapabilityIsNotChannel（表内注入 codex 通道 +
//     能力 false 的替身，证明两条轴可共存），与
//   - 计划级的 TestPlanAllCandidatesNativeContinuationReadsCapabilityNotChannel
//     （同一批 codex 候选、两个能力答案 ⇒ 相反结论）承担。
func TestSkipContinuationForUnknownTypeIsFailClosed(t *testing.T) {
	for _, unknown := range []credential.Type{"", "codex-future-no-continuation", "bogus"} {
		require.False(t, unknown.Valid(), "%q 必须是表外类型", unknown)
		require.False(t, unknown.SupportsContinuation(), "能力位对表外类型 false（ok 守卫）")
		require.False(t, skipContinuationFor(unknown),
			"%q 表外 ⇒ 不猜能力，交给绑定账本（缺失绑定照旧 410），不得 fail-open 跳过解析", unknown)
	}
}

// planKeyWithType 是构造候选最小事实的辅助：计划级判据只读 planKey.credentialType。
func planKeyWithType(t credential.Type) planKey {
	return planKey{credentialType: t}
}

// TestPlanAllCandidatesNativeContinuationReadsCapabilityNotChannel 计划级判据的
// **能力轴**证据：同一批候选（同一个凭据类型）在两个能力答案下必须给出相反结论。
//
// 这就是"未来 codex 通道类型不支持续接"的最小形状——候选的凭据类型是 codex，但能力
// 读投影回答 false。通道判定在这一格里两个方向都返回同一个值（都是 codex），所以
// 本条对通道实现**必然**红。
func TestPlanAllCandidatesNativeContinuationReadsCapabilityNotChannel(t *testing.T) {
	cands := []CompiledCandidate{
		{PlanKey: planKeyWithType(credential.TypeCodexOAuth)},
		{PlanKey: planKeyWithType(credential.TypeCodexPAT)},
	}

	// ① 真实能力表：codex 族原生支持续接 ⇒ 全部原生 ⇒ 计划不需要解析。
	require.True(t, planAllCandidatesNativeContinuation(cands, nil, nil, credential.Type.SupportsContinuation),
		"纯 codex 计划：全部原生支持续接")

	// ② 未来类型替身：同一批 codex 候选，但能力位回答 false ⇒ 计划必须 fail-closed
	//    （AttemptPlan.ContinuationIncompatible() 为真 ⇒ proxy 先 contResolve）。
	futureNoContinuation := func(typ credential.Type) bool {
		return typ != credential.TypeCodexOAuth && typ != credential.TypeCodexPAT
	}
	require.False(t, planAllCandidatesNativeContinuation(cands, nil, nil, futureNoContinuation),
		"codex 通道但能力位 false ⇒ 不得当成「全都原生支持」")

	// ③ 空表按"全部原生"（没有候选的计划不可能派发；见函数注释）。
	require.True(t, planAllCandidatesNativeContinuation(nil, nil, nil, credential.Type.SupportsContinuation))

	// ④ 三条车道都要数到：只放 Primary 会把 Degraded/Explore 的能力遗漏。
	require.False(t, planAllCandidatesNativeContinuation(
		[]CompiledCandidate{{PlanKey: planKeyWithType(credential.TypeCodexOAuth)}},
		[]CompiledCandidate{{PlanKey: planKeyWithType(credential.TypeAPIKey)}},
		nil, credential.Type.SupportsContinuation), "Explore.Ordered 里的静态 key 候选必须被数到")
	require.False(t, planAllCandidatesNativeContinuation(
		nil, nil,
		[]CompiledCandidate{{PlanKey: planKeyWithType(credential.TypeAPIKey)}},
		credential.Type.SupportsContinuation), "Degraded 里的静态 key 候选必须被数到")
}

// bpsAcc 构造可路由的 BPS 账号（候选过滤要求 bpsProfileDigest 非空，它来自
// account_ext.bps_profile 的规范摘要）。
func bpsAcc(id int64, maxConc int) *domain.Account {
	t := tpl(id, domain.FormatOpenAIResponses, []string{"m"})
	t.CredentialType = credential.TypeBPS
	a := acc(id, t, maxConc)
	a.Ext = &domain.AccountExt{
		AccountID:      id,
		CredentialType: credential.TypeBPS,
		BPSProfile:     []byte(`{"profile_id":"p","device_id":"d"}`),
	}
	return a
}

// TestPlanContinuationBitFromCompiledRoutes 计划级判据的**端到端**推导：编译期按
// 真实能力表算出的结论必须落到计划上（构造期一次拷贝，数据面只读一位）。
//
// 每行都断言两件事：路由决策上的编译期结论，以及 AttemptPlan 上的只读位——两者
// 必须同源，否则"编译期定型"只是换个地方现算。
func TestPlanContinuationBitFromCompiledRoutes(t *testing.T) {
	cases := []struct {
		name string
		accs []*domain.Account
		// wantIncompatible = 计划里有人可能不支持续接 ⇒ 必须 fail-closed 解析。
		wantIncompatible bool
	}{
		{"pure_codex", []*domain.Account{codexAcc(1, domain.FormatOpenAIResponses, "m", 4, "inst-1")}, false},
		{"static_key", []*domain.Account{acc(1, tpl(1, domain.FormatOpenAIResponses, []string{"m"}), 4)}, true},
		{"bps", []*domain.Account{bpsAcc(1, 4)}, true},
		{"mixed", []*domain.Account{
			codexAcc(1, domain.FormatOpenAIResponses, "m", 4, "inst-1"),
			acc(2, tpl(2, domain.FormatOpenAIResponses, []string{"m"}), 4),
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestScheduler(t, tc.accs)
			route := RouteRefFor(10, string(domain.FormatOpenAIResponses), "m")

			dec := s.View().decision.routes[normRouteRef(route)]
			require.NotNil(t, dec, "路由必须已编译")
			require.Equal(t, !tc.wantIncompatible, dec.ContinuationNativeOnly,
				"RouteDecision.ContinuationNativeOnly 必须等于「全部候选原生支持续接」")

			plan, err := s.NewAttemptPlan(AttemptPlanIdentity{RequestID: "req-cont"}, route)
			require.NoError(t, err)
			require.Equal(t, tc.wantIncompatible, plan.ContinuationIncompatible(),
				"计划只读位必须与编译期结论同源（有人可能不支持 ⇒ fail-closed 解析）")
		})
	}
}

// TestContinuationBitZeroValueIsFailClosed 手工拼装的路由决策（测试替身/旁路构造）
// 不带这一位：零值必须是**保守**方向（继续解析），否则 API key 的缺失绑定不再 410、
// 模型静默丢上下文。这条锁住"肯定式字段 + 构造期换向"这个设计决定本身。
func TestContinuationBitZeroValueIsFailClosed(t *testing.T) {
	plan := mustNewAttemptPlan(t, AttemptPlanIdentity{RequestID: "req-zero"}, &RouteDecision{Primary: ccPrimary(1)})
	require.True(t, plan.ContinuationIncompatible(),
		"未声明 ContinuationNativeOnly 的路由决策必须按「有人可能不支持续接」处理")

	var nilPlan *AttemptPlan
	require.True(t, nilPlan.ContinuationIncompatible(), "nil 计划同样 fail-closed")
	require.True(t, (&AttemptPlan{}).ContinuationIncompatible(), "未绑定路由的计划同样 fail-closed")
}

// TestSelectionSkipContinuationPinnedAtReservation 派发级判据的端到端推导：预留
// 装配点按能力位算好，Selection 只携带结果（数据面不再查通道表/能力表）。
//
// 表外/空类型不在这里列：它们今天**拿不到 Selection**（候选指纹要求 Valid()，
// 见 TestEmptyCredentialTypeIsUndispatchable），"不跳过解析"由
// TestSkipContinuationForUnknownTypeIsFailClosed 在单元层钉住。
func TestSelectionSkipContinuationPinnedAtReservation(t *testing.T) {
	cases := []struct {
		name string
		accs []*domain.Account
		want bool
	}{
		{"codex", []*domain.Account{codexAcc(1, domain.FormatOpenAIResponses, "m", 4, "inst-1")}, false},
		{"static_key", []*domain.Account{acc(1, tpl(1, domain.FormatOpenAIResponses, []string{"m"}), 4)}, false},
		{"bps", []*domain.Account{bpsAcc(1, 4)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestScheduler(t, tc.accs)
			sel, err := s.Select(10, domain.FormatOpenAIResponses, "m")
			require.NoError(t, err)
			defer sel.Release()
			require.Equal(t, tc.want, sel.SkipContinuation,
				"Selection.SkipContinuation 必须在预留装配点按逐 Type 能力位定型")
		})
	}
}
