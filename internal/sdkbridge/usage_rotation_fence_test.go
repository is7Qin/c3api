// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

// 轮转回写的围栏是**条目当前**的身份代际：轮转换的是这个条目自己的 AT/RT，不是某一次
// 请求的证据。凭据内容不变（同签名）而 K 前进时，条目按签名复用、Auth 不重建，回写必须
// 落在新 K 上——否则 K 一前进，该条目此后的每一次轮转都被判陈旧、永不落库，重启后 DB
// 停在已被轮换掉的旧 RT。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestUsageRotationPersistsWithSourceRevision 额度面（无 Selection，K 由账号行补齐）的
// 凭据建出条目后，轮转回写用它落库：请求在飞期间同签名身份写入把条目抬到 9，SDK 的
// 轮转回调必须以 **9** 写出（不是构造时的 5，也不是 0），且写入方成功路径不得 panic
// （panic 会被 SDK 当成投递失败并重试到判死）。
func TestUsageRotationPersistsWithSourceRevision(t *testing.T) {
	up := newRotationUpstream(t, "at-old")
	newCodexMockRefresh(t, codexUpstreamStep{status: 200, body: `{"access_token":"at-new","refresh_token":"rt-new"}`})
	store := &fakeRotationStore{}
	a := NewCodex(nil, testEgressPool(newOfficialRewriteTransport(t, up.URL)), nil, RotationDeps{Store: store})

	cred := oauthCredWithRev(7, "at-old", "rt-1", 5)
	// 请求在飞、上游已收到旧 at 时推进身份（同签名 ⇒ 条目原地抬升到 9，Auth 不重建）。
	var promoteErr error
	up.onCall = func() {
		_, promoteErr = a.Prepare(oauthCredWithRev(7, "at-old", "rt-1", 9))
	}

	require.NotPanics(t, func() {
		_, gerr := a.GenerateImage(context.Background(), cred, a.DirectEgress(),
			&domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
		require.NoError(t, gerr)
	})
	require.NoError(t, promoteErr)
	require.Equal(t, int64(9), a.clients.Get(7).srcRev.Load(), "条目当前值确实已被抬升")

	calls := store.snapshot()
	require.Len(t, calls, 1)
	require.Equal(t, int64(9), calls[0].sourceRevision,
		"回写围栏 = 条目当前 K（不是构造这条 Auth 时的 5，也不是 0）")
	require.Equal(t, "rt-new", calls[0].rt, "轮转结果按条目当前 K 落库")
}

// TestIdentityAdvanceKeepsRotationPersisting K 前进不等于轮转断供：管理面只改身份类字段
// （base_url/template_id/installation）时 K 推进而**凭据内容不变** ⇒ 条目按签名复用、
// Auth 不重建。此后该条目的轮转仍必须按新 K 落库，否则这条凭据的令牌在 K 前进之后就再也
// 写不进 DB（重启/多实例后拿到已吊销的旧 RT）。
func TestIdentityAdvanceKeepsRotationPersisting(t *testing.T) {
	up := newRotationUpstream(t, "at-old")
	newCodexMockRefresh(t, codexUpstreamStep{status: 200, body: `{"access_token":"at-new","refresh_token":"rt-new"}`})
	store := &fakeRotationStore{}
	a := NewCodex(nil, testEgressPool(newOfficialRewriteTransport(t, up.URL)), nil, RotationDeps{Store: store})

	// K=1 建条目（凭据内容 = at-old/rt-1）。
	first, err := a.Prepare(oauthCredWithRev(7, "at-old", "rt-1", 1))
	require.NoError(t, err)
	// 管理面改 base_url：K 1→2，凭据内容逐字节不变 ⇒ 复用同一条目、Auth 不重建。
	promoted, err := a.Prepare(oauthCredWithRev(7, "at-old", "rt-1", 2))
	require.NoError(t, err)
	require.Same(t, first.entry, promoted.entry, "同签名复用同一条目（Auth 不重建）")
	require.Equal(t, int64(2), a.clients.Get(7).srcRev.Load())

	// 真实刷新链：401 → SDK 刷新 → OnTokenRotated → 回写。
	require.NotPanics(t, func() {
		_, gerr := a.GenerateImage(context.Background(), oauthCredWithRev(7, "at-old", "rt-1", 2),
			a.DirectEgress(), &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
		require.NoError(t, gerr)
	})

	calls := store.snapshot()
	require.Len(t, calls, 1)
	require.Equal(t, int64(2), calls[0].sourceRevision,
		"K 前进之后的轮转必须按新 K 落库（否则此后每次轮转都被判陈旧）")
	require.Equal(t, "rt-new", calls[0].rt)
	require.Equal(t, int64(7), calls[0].accountID)
}
