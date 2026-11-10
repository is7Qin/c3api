// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestPreparedOwnerComesFromEntryNotSnapshot owner 必须从**准备好的条目句柄**读，不能
// 对请求里的凭据快照重新散列（oracle B2）。
//
// 判别场景：installation 不在 credSig 里（条目重建判据只看 token/rt/pat/account id）。
// 管理面改了 installation、条目还没重建时：
//   - owner 从条目读 ⇒ 仍是条目那一版（与它的 auth/client 同版）；
//   - 对请求快照散列 ⇒ 立刻变成新值，于是"新 owner 的会话 + 旧 auth 出网"分叉。
//
// 随后一次真正的凭据替换（PAT 变）才重建条目：新句柄的 owner/client 与旧句柄不同，
// 而旧句柄（在飞请求）继续保持自己的版本——同一句柄 ⟺ 同一 owner/auth/client。
func TestPreparedOwnerComesFromEntryNotSnapshot(t *testing.T) {
	a := NewCodex(nil, testEgressPool(nil), nil, RotationDeps{})
	eg := a.DirectEgress()
	oldCred := &domain.AccountCredential{AccountID: 7, InstallationID: "install-1", PATKey: "pat-old"}

	oldPrep, err := a.Prepare(oldCred)
	require.NoError(t, err)
	require.Equal(t, CredOwner(oldCred), oldPrep.Owner(), "条目刚建：owner = 构造它的那版凭据")
	oldClient, err := a.clientForEntry(oldPrep.entry, eg, domain.CodexResponsesTransportDefault)
	require.NoError(t, err)

	// installation 变（sig 不含它 ⇒ 条目复用）：owner 只认条目稳定字段。
	reinstalled := *oldCred
	reinstalled.InstallationID = "install-2"
	samePrep, err := a.Prepare(&reinstalled)
	require.NoError(t, err)
	require.Same(t, oldPrep.entry, samePrep.entry, "installation 不在重建判据里 ⇒ 同一条目")
	require.Equal(t, oldPrep.Owner(), samePrep.Owner(), "owner 从条目读，不跟请求快照")
	require.NotEqual(t, CredOwner(&reinstalled), samePrep.Owner(),
		"对请求快照散列会得到不同的 owner —— 那正是 B2 禁止的分叉")

	// 真正的凭据替换（PAT 变）⇒ 条目重建，owner/client 换版。
	newCred := *oldCred
	newCred.PATKey = "pat-new"
	newPrep, err := a.Prepare(&newCred)
	require.NoError(t, err)
	require.NotSame(t, oldPrep.entry, newPrep.entry, "PAT 变 ⇒ 条目重建")
	require.NotEqual(t, oldPrep.Owner(), newPrep.Owner())
	require.Same(t, newPrep.entry, a.clients.Get(7))

	newClient, err := a.clientForEntry(newPrep.entry, eg, domain.CodexResponsesTransportDefault)
	require.NoError(t, err)
	require.NotSame(t, oldClient, newClient, "不同 owner 的请求不得复用同一 client 桶")

	// 旧句柄继续用自己的版本（在飞请求不受替换影响）：条目重建时旧桶已被摘除并 Close，
	// 迟到的取用只会在**死条目自己的 map** 里重建——按旧 auth，且对活路径不可见。
	staleClient, err := a.clientForEntry(oldPrep.entry, eg, domain.CodexResponsesTransportDefault)
	require.NoError(t, err)
	require.NotSame(t, newClient, staleClient, "旧句柄不得拿到新 auth 的 client")
	require.Same(t, staleClient, testCachedClient(a, oldPrep.entry, clientKey{}),
		"同一句柄 ⟺ 同一 owner/auth（写入只落在死条目自己的桶）")
	require.Equal(t, CredOwner(oldCred), oldPrep.Owner(), "旧句柄的 owner 不随后续 Prepare 变化")
	require.Same(t, newClient, testCachedClient(a, a.clients.Get(7), clientKey{}),
		"活条目的直连桶是**新** auth 的 client")
	require.NotSame(t, oldClient, testCachedClient(a, a.clients.Get(7), clientKey{}),
		"旧 auth 的 client 不得留在活条目的桶里")
}
