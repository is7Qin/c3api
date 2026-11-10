// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

// 额度面（无 Selection）的凭据带来源 K 之后，SDK 自刷新产生的轮转回写必须用这份 K
// 通过存储围栏并落库——否则 DB 会停在已被轮换掉的旧 RT。

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestUsageRotationPersistsWithSourceRevision 轮转回写的围栏是**构造这条 Auth 的那版
// 凭据**的 K：请求在飞期间同签名身份写入把条目抬到 9，SDK 的轮转回调仍必须以 5 写出
// （不是 9，也不是 0）——否则这份轮转会被当成陈旧事件丢掉，DB 停在已被轮换掉的旧 RT。
//
// 凭据形状即额度面（无 Selection，K 由账号行补齐）的产物；写入方成功路径返回 nil，
// 因此回调不得 panic（panic 会被 SDK 当成投递失败并重试到判死）。
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
	require.Equal(t, int64(5), calls[0].sourceRevision,
		"回写围栏 = 这份 Auth 构造时的来源 K（不是抬升后的 9，也不是 0）")
	require.Equal(t, "rt-new", calls[0].rt, "轮转结果按来源 K 落库")
}
