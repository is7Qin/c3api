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

// TestCredOwnerStableAcrossTokenRefresh owner 只含**稳定身份标识**（§3.2 凭据替换的
// 括注：owner 不是"每次 token refresh"）：access token 会轮转，不能参与 owner，否则
// 每次 OAuth 刷新都会清掉该账号全部会话的 ts。
//
// 判据是行为：同一账号 refresh 前后 owner 相等 ⇒ 租借内的 ts 保留；而 PAT / 上游
// 账号 / installation 变化（真的是另一版凭据/身份）必须换 owner ⇒ ts 清空。
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

	// ② 换了 refresh-token ⇒ 另一版凭据：ts 必须清。
	rerotated := *base
	rerotated.OAuthRefreshToken = "rt-2"
	sel.AlignCodexOwner(sdkbridge.CredOwner(&rerotated))
	require.Empty(t, sel.CodexIdentity().TurnState, "refresh-token 变化 = 换凭据 ⇒ 清 ts")

	// ③ 上游账号改绑 ⇒ 跨账号串台风险：ts 必须清。
	rebound := *base
	rebound.CodexAccountID = "acc-2"
	sel.AlignCodexOwner(sdkbridge.CredOwner(&rebound))
	require.Empty(t, sel.CodexIdentity().TurnState, "CodexAccountID 改绑 ⇒ 清 ts")

	// ④ installation 变化 ⇒ 身份变了：ts 必须清。
	reinstalled := *base
	reinstalled.InstallationID = "inst-2"
	sel.AlignCodexOwner(sdkbridge.CredOwner(&reinstalled))
	require.Empty(t, sel.CodexIdentity().TurnState, "installation 变化 ⇒ 清 ts")
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
