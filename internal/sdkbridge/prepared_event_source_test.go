// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

// 事件来源必须是**请求自己那一版** K（§3.3b-2）的判别用例：条目按凭据签名复用，
// 同签名的后到请求会把条目上的 `srcRev` 单调抬升——若事件在产生时回读这个可变字段，
// 在飞请求的旧判决就会顶上新身份。用例只用真实组件与确定性交错（无 sleep）。

import (
	"testing"

	"github.com/stretchr/testify/require"

	codexsdk "github.com/is7Qin/codex-sdk"
)

// TestSameSignatureCannotPromoteOldFatal 同签名复用不得把旧请求的判决升格成新身份的
// 事实：
//
//	请求 A: Prepare(K=1)            → 条目 e（srcRev=1）
//	库里的身份推进到 K=2
//	请求 B: Prepare(同 at/rt, K=2)   → 命中签名复用，条目 srcRev 被抬到 2（Auth 不重建）
//	A: 收到 fatal                   → 上报必须带**来源 K=1**
//
// 两段断言：上报链收到的来源版本是 1（绝不是 2）；端到端上该事件被存储围栏拒
// （库已是 K=2），新身份不被禁用。
func TestSameSignatureCannotPromoteOldFatal(t *testing.T) {
	handler := &recordingHandler{}
	a := NewCodex(handler.add, testEgressPool(nil), nil, RotationDeps{})

	old, err := a.Prepare(oauthCredWithRev(7, "at-1", "rt-1", 1))
	require.NoError(t, err)
	// 同凭据内容的身份写入：K 抬升、签名不变 ⇒ 复用同一条目（不动 Auth）。
	promoted, err := a.Prepare(oauthCredWithRev(7, "at-1", "rt-1", 2))
	require.NoError(t, err)
	require.Same(t, old.entry, promoted.entry, "同签名复用同一条目")
	require.Equal(t, int64(2), promoted.entry.srcRev.Load(),
		"条目当前值已被抬到 2（正是不能当事件来源的那个值）")

	a.FatalPrepared(old, &codexsdk.AuthPermanentlyRevokedError{Code: "token_invalidated"})
	calls := handler.snapshot()
	require.Len(t, calls, 1)
	require.Equal(t, int64(1), calls[0].sourceRevision,
		"事件绑定请求准备时冻结的 K（回读条目当前值会让旧判决拿到 2）")

	// 端到端：库里的身份已推进到 2 ⇒ 上面这条 K=1 的事件在持久化围栏处被拒。
	store := &configRevisionStore{acct: newCodexAccountForRetry(7, 1)}
	failer := &retryFakeFailer{}
	b := NewCodex(NewFailureHandler(FailureDeps{Store: store, Failer: failer, Latch: newRetryFakeLatch()}),
		testEgressPool(nil), nil, RotationDeps{})

	oldPrep, err := b.Prepare(oauthCredWithRev(7, "at-1", "rt-1", 1))
	require.NoError(t, err)
	store.acct.IdentityRevision = 2 // 管理面完成身份写入（K 1→2），期间没有新请求
	_, err = b.Prepare(oauthCredWithRev(7, "at-1", "rt-1", 2))
	require.NoError(t, err)
	require.True(t, b.clients.isCurrent(oldPrep.entry), "旧句柄指向的仍是当前条目（指针门挡不住这个窗口）")

	b.FatalPrepared(oldPrep, &codexsdk.AuthPermanentlyRevokedError{Code: "token_invalidated"})
	require.Nil(t, store.acct.FailedAt, "来源 K=1 的旧判决不得禁用已推进到 K=2 的身份")
	require.Empty(t, failer.calls, "围栏拒绝 ⇒ 内存摘除不得先发生")
	require.Empty(t, store.gotExpect, "K 不符 ⇒ 连 CAS 都不该尝试")
}

// TestPreparedCarriesItsOwnSourceRevision 并存的句柄各自带各自的 K：账号 21 的句柄
// 来自被抬升前的旧版本（4），账号 22 的句柄来自它自己那一版（9）——两条上报都不得
// 被"条目当前值"污染。
func TestPreparedCarriesItsOwnSourceRevision(t *testing.T) {
	handler := &recordingHandler{}
	a := NewCodex(handler.add, testEgressPool(nil), nil, RotationDeps{})

	older, err := a.Prepare(oauthCredWithRev(21, "at-a", "rt-a", 4))
	require.NoError(t, err)
	other, err := a.Prepare(oauthCredWithRev(22, "at-b", "rt-b", 9))
	require.NoError(t, err)
	// 账号 21 的身份被推进到 6（同签名复用）：条目当前值变了，旧句柄不变。
	_, err = a.Prepare(oauthCredWithRev(21, "at-a", "rt-a", 6))
	require.NoError(t, err)
	require.Equal(t, int64(6), a.clients.Get(21).srcRev.Load())

	a.FatalPrepared(older, &codexsdk.AuthPermanentlyRevokedError{Code: "token_invalidated"})
	a.FatalPrepared(other, &codexsdk.AuthPermanentlyRevokedError{Code: "token_invalidated"})

	calls := handler.snapshot()
	require.Len(t, calls, 2, "两个句柄各上报一次（去重是条目级的，两个账号互不吞并）")
	require.Equal(t, int64(21), calls[0].accountID)
	require.Equal(t, int64(4), calls[0].sourceRevision, "旧句柄带它自己的 K=4（不是条目当前值 6）")
	require.Equal(t, int64(22), calls[1].accountID)
	require.Equal(t, int64(9), calls[1].sourceRevision, "另一个句柄带它自己的 K=9")
	require.NotSame(t, older.entry, other.entry)
}
