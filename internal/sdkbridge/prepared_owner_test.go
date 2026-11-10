// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestPreparedOwnerComesFromEntry owner 从**准备好的条目句柄**读，不对请求里的凭据
// 快照重新散列：同一句柄 ⟺ 同一 owner/auth/client。
//
// 结构保证：owner 的字段（上游账号/PAT）是 credKey 的子集 ⇒ 任何 owner 维度变化都
// 会重建条目，冻结在条目上的 owner 恒与当前凭据版本一致，不存在"新 owner + 旧 auth"
// 的分叉。installation 不在这两套判据里（会话身份配置，由会话/线程版本承担）。
//
// 随后一次真正的凭据替换（PAT 变）才重建条目：新句柄的 owner/client 与旧句柄不同，
// 而旧句柄（在飞请求）继续保持自己的版本。
func TestPreparedOwnerComesFromEntry(t *testing.T) {
	a := NewCodex(nil, testEgressPool(nil), nil, RotationDeps{})
	eg := a.DirectEgress()
	oldCred := &domain.AccountCredential{AccountID: 7, InstallationID: "install-1", PATKey: "pat-old"}

	oldPrep, err := a.Prepare(oldCred)
	require.NoError(t, err)
	require.Equal(t, CredOwner(oldCred), oldPrep.Owner(), "条目刚建：owner = 构造它的那版凭据")
	oldClient, err := a.clientForEntry(oldPrep.entry, eg, domain.CodexResponsesTransportDefault)
	require.NoError(t, err)

	// installation 变（既不在 credKey 也不在 owner ⇒ 条目与 owner 都复用）：ts 清空
	// 由会话重建承担（新线程、ts 自然为空），不靠改 owner。
	reinstalled := *oldCred
	reinstalled.InstallationID = "install-2"
	samePrep, err := a.Prepare(&reinstalled)
	require.NoError(t, err)
	require.Same(t, oldPrep.entry, samePrep.entry, "installation 不在重建判据里 ⇒ 同一条目")
	require.Equal(t, oldPrep.Owner(), samePrep.Owner(), "owner 只认条目的凭据版本")
	require.Equal(t, CredOwner(&reinstalled), samePrep.Owner(),
		"owner 字段是 credKey 子集 ⇒ 快照与条目恒同版")

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
	require.NotSame(t, oldClient, newClient, "不同凭据版本的请求不得复用同一 client 桶")

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
