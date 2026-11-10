// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/sdkbridge"
)

// TestCredOwnerStableAcrossTokenRefresh owner 只含**稳定身份标识**：AT 与 RT 都会在
// OAuth 刷新中轮转（RT 也会换），都不能参与 owner，否则每个刷新周期都会清掉该账号
// 全部会话的 ts。
//
// 判据是行为：同一账号 refresh 前后（含 RT 轮转）owner 相等 ⇒ 租借内的 ts 保留；
// 而 PAT / 上游账号变化（真的是另一版凭据/身份）必须换 owner ⇒ ts 清空。
// installation 不进 owner（会话身份配置，由会话重建承担 ts 清空）。
func TestCredOwnerStableAcrossTokenRefresh(t *testing.T) {
	base := &domain.AccountCredential{
		AccountID:         7,
		InstallationID:    "inst-1",
		CodexAccountID:    "acc-1",
		OAuthToken:        "at-1",
		OAuthRefreshToken: "rt-1",
	}

	sess := newCodexSession("inst-1", testPolicy())
	sel := &Selection{session: sess}
	require.True(t, sess.busy.CompareAndSwap(false, true))
	sel.AlignCodexOwner(sdkbridge.CredOwner(base))
	sess.SetTurnState("T-keep")

	// ① token refresh：at 换了（过期时刻可能一起换），refresh-token 未变 ⇒ 不是换 owner。
	refreshed := *base
	refreshed.OAuthToken = "at-2"
	sel.AlignCodexOwner(sdkbridge.CredOwner(&refreshed))
	require.Equal(t, "T-keep", sel.CodexIdentity().TurnState,
		"token refresh 不得清 ts（owner 排除会轮转的 access token）")

	// ② refresh-token 也轮转（SDK 每次 refresh 都可能换 RT）⇒ 仍不是换 owner：
	// 令牌换了，同一条上游对话没换。
	rerotated := *base
	rerotated.OAuthToken = "at-3"
	rerotated.OAuthRefreshToken = "rt-2"
	sel.AlignCodexOwner(sdkbridge.CredOwner(&rerotated))
	require.Equal(t, "T-keep", sel.CodexIdentity().TurnState,
		"RT 轮转 = 换令牌不换对话身份 ⇒ ts 保留（owner 排除 RT）")

	// ③ 上游账号改绑 ⇒ 跨账号串台风险：ts 必须清。
	rebound := *base
	rebound.CodexAccountID = "acc-2"
	sel.AlignCodexOwner(sdkbridge.CredOwner(&rebound))
	require.Empty(t, sel.CodexIdentity().TurnState, "CodexAccountID 改绑 ⇒ 清 ts")

	// ④ installation 变化 ⇒ owner 不变：它是会话身份配置，ts 清空由**会话重建**
	// 承担（新池新线程、ts 自然为空，见 G11），不塞进凭据 owner——否则条目何时
	// 重建会决定 owner 值，与冻结在条目上的版本分叉。
	sel.AlignCodexOwner(sdkbridge.CredOwner(base)) // ③ 已换到 acc-2：先对齐回本账号
	sess.SetTurnState("T-inst")
	reinstalled := *base
	reinstalled.InstallationID = "inst-2"
	sel.AlignCodexOwner(sdkbridge.CredOwner(&reinstalled))
	require.Equal(t, "T-inst", sel.CodexIdentity().TurnState,
		"installation 变化不改凭据 owner（会话层重建才是 ts 清空点）")
	sess.release()

	// ⑤ PAT 轮换 ⇒ 另一版凭据：ts 必须清（与 refresh 区分开）。
	pat := &domain.AccountCredential{AccountID: 9, PATKey: "pat-1"}
	patSess := newCodexSession("inst-1", testPolicy())
	patSel := &Selection{session: patSess}
	require.True(t, patSess.busy.CompareAndSwap(false, true))
	patSel.AlignCodexOwner(sdkbridge.CredOwner(pat))
	patSess.SetTurnState("T-pat")
	nextPat := *pat
	nextPat.PATKey = "pat-2"
	patSel.AlignCodexOwner(sdkbridge.CredOwner(&nextPat))
	require.Empty(t, patSel.CodexIdentity().TurnState, "PAT 轮换 = 换凭据 ⇒ 清 ts")
	patSess.release()
}
