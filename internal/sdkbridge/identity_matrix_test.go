// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

// 身份版本字段矩阵的**行为**固定（spec §3.3b 收敛轮 S3）：AT / RT / PAT / 上游账号 /
// installation / 出口各影响哪个生命周期，用例逐条钉住，而不是让测试去固化旧错位。
//
// 矩阵（与 credKey 上方注释同源）：
//
//	字段                  | 条目/Auth 重建 | 会话 ts（owner） | 说明
//	--------------------- | ------------- | ---------------- | ----
//	AT 轮转               | 否            | 否               | 派生缓存
//	RT 轮转               | 是            | 否               | Auth 构造输入变了
//	PAT 变更              | 是            | 是               | PAT 是秘密也是身份
//	上游账号 ID 改绑       | 是            | 是               | 跨账号串台风险
//	installation 变更     | 否            | 否               | 会话身份配置（会话重建另测）
//	出口 / 传输变更        | 否            | 否               | clientKey 层

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/egress"
)

// TestIdentityMatrixATRotationReusesEntry AT 是派生缓存：只换 AT 不重建条目/Auth，
// owner 也不变（不清 ts）。
func TestIdentityMatrixATRotationReusesEntry(t *testing.T) {
	a := NewCodex(nil, testEgressPool(nil), nil, RotationDeps{})
	base := &domain.AccountCredential{AccountID: 7, InstallationID: "inst-1", CodexAccountID: "acc-1",
		OAuthToken: "at-1", OAuthRefreshToken: "rt-1"}
	p1, err := a.Prepare(base)
	require.NoError(t, err)

	rotated := *base
	rotated.OAuthToken = "at-2"
	p2, err := a.Prepare(&rotated)
	require.NoError(t, err)

	require.Same(t, p1.entry, p2.entry, "AT 轮转不重建条目（派生缓存不入 credKey）")
	require.Equal(t, p1.Owner(), p2.Owner(), "AT 轮转不换 owner（不清 ts）")
}

// TestIdentityMatrixRTRotationRebuildsAuthKeepsOwner RT 是 Auth 构造输入：换 RT 重建
// 条目/Auth（新 Auth 用新令牌起步），但上游对话身份未变 ⇒ owner 不变（ts 保留）。
func TestIdentityMatrixRTRotationRebuildsAuthKeepsOwner(t *testing.T) {
	a := NewCodex(nil, testEgressPool(nil), nil, RotationDeps{})
	base := &domain.AccountCredential{AccountID: 7, InstallationID: "inst-1", CodexAccountID: "acc-1",
		OAuthToken: "at-1", OAuthRefreshToken: "rt-1"}
	p1, err := a.Prepare(base)
	require.NoError(t, err)

	rotated := *base
	rotated.OAuthToken = "at-2"
	rotated.OAuthRefreshToken = "rt-2"
	p2, err := a.Prepare(&rotated)
	require.NoError(t, err)

	require.NotSame(t, p1.entry, p2.entry, "RT 变化 ⇒ Auth 重建")
	require.Equal(t, p1.Owner(), p2.Owner(), "RT 轮转不换对话身份（owner 不含 RT）")
	require.Same(t, p2.entry, a.clients.Get(7), "新条目成为当前版本")
}

// TestIdentityMatrixPATRebuildsAuthAndOwner PAT 既是秘密也是身份：换 PAT 重建条目且
// 清 ts（owner 变）。
func TestIdentityMatrixPATRebuildsAuthAndOwner(t *testing.T) {
	a := NewCodex(nil, testEgressPool(nil), nil, RotationDeps{})
	p1, err := a.Prepare(&domain.AccountCredential{AccountID: 7, PATKey: "pat-1"})
	require.NoError(t, err)
	p2, err := a.Prepare(&domain.AccountCredential{AccountID: 7, PATKey: "pat-2"})
	require.NoError(t, err)

	require.NotSame(t, p1.entry, p2.entry, "PAT 变化 ⇒ 重建")
	require.NotEqual(t, p1.Owner(), p2.Owner(), "PAT 变化 ⇒ 换 owner（清 ts）")
}

// TestIdentityMatrixAccountIDRebindRebuildsAuthAndOwner 改绑上游账号：Auth 重建 +
// owner 变（跨账号串台风险，ts 必须清）。
func TestIdentityMatrixAccountIDRebindRebuildsAuthAndOwner(t *testing.T) {
	a := NewCodex(nil, testEgressPool(nil), nil, RotationDeps{})
	base := &domain.AccountCredential{AccountID: 7, InstallationID: "inst-1", CodexAccountID: "acc-1",
		OAuthToken: "at-1", OAuthRefreshToken: "rt-1"}
	p1, err := a.Prepare(base)
	require.NoError(t, err)

	rebound := *base
	rebound.CodexAccountID = "acc-2"
	p2, err := a.Prepare(&rebound)
	require.NoError(t, err)

	require.NotSame(t, p1.entry, p2.entry, "上游账号改绑 ⇒ 重建")
	require.NotEqual(t, p1.Owner(), p2.Owner(), "上游账号改绑 ⇒ 换 owner（清 ts）")
}

// TestIdentityMatrixInstallationKeepsEntryAndOwner installation 是**会话身份配置**：
// 它不在 credKey（条目/Auth 复用）也不在 owner（凭据身份不变）——ts 清空由调度器
// 的会话重建承担（新池新线程、ts 自然为空，见 G11）。
func TestIdentityMatrixInstallationKeepsEntryAndOwner(t *testing.T) {
	a := NewCodex(nil, testEgressPool(nil), nil, RotationDeps{})
	base := &domain.AccountCredential{AccountID: 7, InstallationID: "inst-1", PATKey: "pat-1"}
	p1, err := a.Prepare(base)
	require.NoError(t, err)

	reinstalled := *base
	reinstalled.InstallationID = "inst-2"
	p2, err := a.Prepare(&reinstalled)
	require.NoError(t, err)

	require.Same(t, p1.entry, p2.entry, "installation 不进 credKey（条目复用）")
	require.Equal(t, p1.Owner(), p2.Owner(), "installation 不进 owner（会话层重建才是 ts 清空点）")
}

// TestIdentityMatrixEgressOnlyChangesBucket 出口/传输只作用于 clientKey 层：条目与
// owner 不变，只是换一个桶。
func TestIdentityMatrixEgressOnlyChangesBucket(t *testing.T) {
	a := NewCodex(nil, testEgressPool(nil), nil, RotationDeps{})
	cred := &domain.AccountCredential{AccountID: 7, PATKey: "pat-1"}
	p, err := a.Prepare(cred)
	require.NoError(t, err)

	egA := *a.DirectEgress()
	egA.Spec = egress.New("http", "localhost", 8100, "", "")
	egB := *a.DirectEgress()
	egB.Spec = egress.New("http", "localhost", 8101, "", "")

	cA, err := a.clientForEntry(p.entry, &egA, domain.CodexResponsesTransportDefault)
	require.NoError(t, err)
	cB, err := a.clientForEntry(p.entry, &egB, domain.CodexResponsesTransportDefault)
	require.NoError(t, err)

	require.NotSame(t, cA, cB, "换出口 ⇒ 换桶")
	require.Same(t, p.entry, a.clients.Get(7), "条目不变（出口不进 credKey）")
	require.Len(t, testEntryBuckets(a, p.entry), 2, "两个出口各一个桶，互不覆盖")

	// 同出口再取一次命中同一个桶（身份维度不参与 clientKey）。
	again, err := a.clientForEntry(p.entry, &egA, domain.CodexResponsesTransportDefault)
	require.NoError(t, err)
	require.Same(t, cA, again)
}
