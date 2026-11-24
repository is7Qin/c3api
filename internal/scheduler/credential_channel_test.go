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

// 本文件是 S2「凭据按通道分派」的**行为**证据：通道判定改写成 ChannelOf 之后，
// 每一处通道分支的结论都必须与本轮之前逐字节一致。门禁（credential_branch_gate_test.go）
// 只钉形状，钉不住结论——结论在这里，用最小的端到端路径断言。
//
// 与 §8.2 的五类一一对应（编号见各条注释的 testName）：
//   - ② BPS 误填 Codex installation：TestBPSIgnoresCodexIdentity；
//   - ③ 缺 profile 静默放行：TestBPSWithoutProfileIsUnroutable；
//   - ⑤ Codex 槽上限误套 BPS：TestIdentityPoolCapacityClamped 的 BPS 行（identity_pool_test.go）；
//   - ① 错接通道（变异）与 ④ dirty transport 由实仓变异 / 既有用例守，见报告。

// bpsAccWith 构造 BPS 账号：ext 由调用方给（用于注入 Codex installation 这类
// "错填"场景）。
func bpsAccWith(id int64, maxConc int, ext *domain.AccountExt) *domain.Account {
	t := tpl(id, domain.FormatOpenAIResponses, []string{"m"})
	t.CredentialType = credential.TypeBPS
	a := acc(id, t, maxConc)
	a.Ext = ext
	return a
}

// TestBPSIgnoresCodexIdentity §8.2②：BPS 账号即使被错填了 Codex installation
// （账号 ext 里同时带 CodexIdentity），调度层也必须完全无视它——
//
//   - 不建会话池（会话池只按 codex 通道建）；
//   - installation / cacheDomain 不进静态视图（它们是 codex 会话路径的事实）；
//   - factsDigest 与「同一账号但 ext 不带 CodexIdentity」的孪生体同键（installation
//     是 factsDigest 的一段，而 BPS 上该段恒为空，错填值换不掉叶子）；
//   - 反向对照：同一 ext 挂在 codex 模板上时 installation 真的进 factsDigest——
//     证明"不进摘要"不是因为夹具没填。
func TestBPSIgnoresCodexIdentity(t *testing.T) {
	mixedExt := func(id int64, inst string) *domain.AccountExt {
		return &domain.AccountExt{
			AccountID: id, CredentialType: credential.TypeBPS,
			BPSProfile:    []byte(`{"profile_id":"p","device_id":"d"}`),
			CodexIdentity: &domain.CodexIdentity{InstallationID: inst},
		}
	}

	bps := bpsAccWith(1, 4, mixedExt(1, "inst-bps"))
	codexTpl := tpl(2, domain.FormatOpenAIResponses, []string{"m"})
	codexTpl.CredentialType = credential.TypeCodexOAuth
	codex := acc(2, codexTpl, 4)
	codex.Ext = mixedExt(2, "inst-codex")

	s := newTestScheduler(t, []*domain.Account{bps, codex})
	v := s.View()

	bpsAv := v.static.byID[1].static.Load()
	require.Empty(t, bpsAv.installationID, "BPS 不读 Codex installation")
	require.Empty(t, bpsAv.cacheDomain, "BPS 不走 codex 会话路径（无缓存域事实）")
	require.NotEmpty(t, bpsAv.bpsProfileDigest, "BPS 的 profile 摘要才是它的事实")
	require.Nil(t, v.static.identityPools.Pools[1], "BPS 不得建 codex 会话池")

	// 错填的 installation 不得进 BPS 的 factsDigest：与「同一账号但 ext 不带
	// CodexIdentity」的孪生体**同键**（键里的事实不比叶子带着的事实更细）。
	bpsTwin := *bps
	bpsTwin.Ext = &domain.AccountExt{
		AccountID: 1, CredentialType: credential.TypeBPS,
		BPSProfile: []byte(`{"profile_id":"p","device_id":"d"}`),
	}
	require.Equal(t, bpsAv.key.factsDigest,
		newSnapshotStatic(bpsTwin, bps.Template, nil, directEgress(), nil).key.factsDigest,
		"错填的 installation 不得进 BPS 的 factsDigest")
	require.NotEqual(t, [32]byte{}, bpsAv.key.factsDigest,
		"BPS 的身份事实（账号归属 + profile 摘要）必须真的进了摘要")

	codexAv := v.static.byID[2].static.Load()
	require.Equal(t, "inst-codex", codexAv.installationID, "反向对照：codex 通道照常读 installation")
	require.NotNil(t, v.static.identityPools.Pools[2], "反向对照：codex 建池")
	// 反向对照：codex 的 installation 进 factsDigest（同一账号只改 installation）。
	codexTwin := *codex
	codexTwin.Ext = mixedExt(2, "inst-other")
	require.NotEqual(t, codexAv.key.factsDigest,
		newSnapshotStatic(codexTwin, codex.Template, nil, directEgress(), nil).key.factsDigest,
		"反向对照：codex 的 installation 进 factsDigest")
}

// TestEmptyCredentialTypeIsUndispatchable 钉住一条**当前不可达**的路径，它的价值是
// 给 skipContinuationFor 的 fail-closed 分支上第二道锁：
//
//   - routing_compiler_candidates.go 的候选过滤只拒 `CredentialType != "" && !Valid()`，
//     所以未设 credential_type 的历史模板**能**编进路由（本用例第 1 段）；
//   - 但它拿不到候选指纹：domain.CandidateFingerprint 要求 `CredType.Valid()`，派生失败
//     被 deriveCompilerAccountFacts 吞掉（fingerprint 留空），预留谓词的
//     `c.Fingerprint == ""` 于是恒拒 ⇒ **永远不会产出 Selection**（第 2 段）。
//
// 所以 skipContinuationFor 的 `!t.Valid() → false` 是防御性的（表外 ⇒ 不猜能力 ⇒ 交给
// 绑定账本 ⇒ 缺失 410）。谁将来放宽了指纹这一关、让空类型真的可派发，本用例就会红：
// 届时必须重新核对续接政策位，不能默认它"反正不可达"。
func TestEmptyCredentialTypeIsUndispatchable(t *testing.T) {
	tp := tpl(1, domain.FormatOpenAIResponses, []string{"m"})
	tp.CredentialType = ""
	s := newTestScheduler(t, []*domain.Account{acc(1, tp, 4)})

	dec := s.View().decision.routes[normRouteRef(RouteRefFor(10, string(domain.FormatOpenAIResponses), "m"))]
	require.NotNil(t, dec, "空 credential_type 的模板能编进路由（候选过滤放行）")
	require.NotEmpty(t, dec.Explore.Ordered, "它确实进了候选表")

	_, err := s.Select(10, domain.FormatOpenAIResponses, "m")
	require.ErrorIs(t, err, ErrAttemptsExhausted,
		"但候选指纹为空 ⇒ 预留恒拒：空类型今天拿不到 Selection（放宽这条会红）")
}

// TestBPSWithoutProfileIsUnroutable §8.2③：BPS 候选缺 bpsProfileDigest ⇒ 不可路由。
//
// 为什么必须拒绝而不是放行：BPS 的身份/端点/profile 全部来自 account_ext，缺摘要
// 意味着拿不到权威 profile，此时"回落成静态 key 或 codex 身份"会静默用错凭据。
// 该用例此前已存在（filterCandidates 的注释），这里从行为面把它钉在"选号失败"上。
func TestBPSWithoutProfileIsUnroutable(t *testing.T) {
	missing := bpsAccWith(1, 4, &domain.AccountExt{AccountID: 1, CredentialType: credential.TypeBPS})
	s := newTestScheduler(t, []*domain.Account{missing})

	_, err := s.Select(10, domain.FormatOpenAIResponses, "m")
	require.Error(t, err, "缺 profile 摘要的 BPS 账号不得被选中")
	require.ErrorIs(t, err, ErrNoAvailable, "拒绝的形状是「无可用账号」，不是静默放行")

	// 正例对照：同一账号补上 profile 即可路由（否则上面的 error 可能是别的原因）。
	fixed := bpsAccWith(2, 4, &domain.AccountExt{
		AccountID: 2, CredentialType: credential.TypeBPS,
		BPSProfile: []byte(`{"profile_id":"p"}`),
	})
	s2 := newTestScheduler(t, []*domain.Account{fixed})
	sel, err := s2.Select(10, domain.FormatOpenAIResponses, "m")
	require.NoError(t, err)
	require.Equal(t, int64(2), sel.AccountID)
	sel.Handle.Release()
}
