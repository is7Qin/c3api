// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// 本文件的用例全部是**合成结构体**：分支必须由模板类型驱动，而 fail-closed 之后
// 数据库里两列的 BPS 位永远一致（spec J7.5：不许用 DB 夹具证明这条）。合成结构体
// 可以把"模板类型与 ext 行类型相反"直接摆出来。

// TestProjectExtIdentitySelectsBranchFromTemplate 钉住 v4 修正 2 的唯一取值规则：
// Responses provider 模板 ⇒ 模板类型选分支；静态通道模板 ⇒ ext 行自身类型。
// 断言落在**投影产物形状**上（BPSProfile / CodexIdentity 二选一），不调中间
// helper——分支选择已内联进 projectExtIdentity（S3b 谓词预算只许变短）。
func TestProjectExtIdentitySelectsBranchFromTemplate(t *testing.T) {
	at, rt, upstream, pat := "at", "rt", "acct", "pat"
	env := &domain.IdentityEnvelope{
		"installation_id": json.RawMessage(`"inst-branch"`),
		"bps":             json.RawMessage(`{"profile_id":"p-branch"}`),
	}
	bpsExt := func() *domain.AccountExt {
		return &domain.AccountExt{
			AccountID: 21, CredentialType: credential.TypeBPS,
			CodexOAuthToken: &at, CodexOAuthRefreshToken: &rt, CodexAccountID: &upstream,
		}
	}
	codexExt := func() *domain.AccountExt {
		return &domain.AccountExt{AccountID: 22, CredentialType: credential.TypeCodexPAT, CodexPATKey: &pat}
	}

	t.Run("BPS 模板 ⇒ BPS 分支", func(t *testing.T) {
		ext := bpsExt()
		tpl := credential.TypeBPS
		require.NoError(t, projectExtIdentity(ext, env, &tpl))
		require.NotNil(t, ext.BPSProfile, "模板是 BPS：必须投影 profile")
		require.Nil(t, ext.CodexIdentity, "模板是 BPS：不得产出 codex 身份")
	})

	t.Run("codex 模板 ⇒ codex 分支", func(t *testing.T) {
		ext := codexExt()
		tpl := credential.TypeCodexPAT
		require.NoError(t, projectExtIdentity(ext, env, &tpl))
		require.NotNil(t, ext.CodexIdentity, "模板是 codex：必须投影 installation")
		require.Nil(t, ext.BPSProfile, "模板是 codex：不得产出 BPS profile")
	})

	t.Run("静态通道 + BPS 行 ⇒ 走 BPS 分支（行类型）", func(t *testing.T) {
		ext := bpsExt()
		tpl := credential.TypeAPIKey
		require.NoError(t, projectExtIdentity(ext, env, &tpl))
		require.NotNil(t, ext.BPSProfile, "静态模板：分支按行自己的类型（BPS）")
		require.Nil(t, ext.CodexIdentity)
	})

	t.Run("静态通道 + codex 行 ⇒ 走 codex 分支（行类型）", func(t *testing.T) {
		ext := codexExt()
		tpl := credential.TypeCustom
		require.NoError(t, projectExtIdentity(ext, env, &tpl))
		require.NotNil(t, ext.CodexIdentity, "静态模板：分支按行自己的类型（codex）")
		require.Nil(t, ext.BPSProfile)
	})
}

// TestProjectExtIdentityKeepsRowTypeAndAssertsTemplateBits 钉住两件事：
//  1. 投影走**模板**选的分支（ext 类型相反时被断言拦住，绝不按 ext 类型投影）；
//  2. ext.CredentialType 永远保持行原值（同通道内列布局由行自己决定，v4 修正 2）。
func TestProjectExtIdentityKeepsRowTypeAndAssertsTemplateBits(t *testing.T) {
	at, rt, upstream, pat := "at", "rt", "acct", "pat"

	t.Run("同通道内按模板选分支且不覆盖行类型", func(t *testing.T) {
		ext := &domain.AccountExt{
			AccountID: 9, CredentialType: credential.TypeCodexOAuth,
			CodexOAuthToken: &at, CodexOAuthRefreshToken: &rt, CodexAccountID: &upstream,
		}
		tpl := credential.TypeCodexPAT
		require.NoError(t, projectExtIdentity(ext, &domain.IdentityEnvelope{
			"installation_id": json.RawMessage(`"inst-1"`),
		}, &tpl))
		require.Equal(t, credential.TypeCodexOAuth, ext.CredentialType,
			"ext 行的类型是列布局的权威（codex-oauth 取 token 组 / codex-pat 取 PATKey），任何路径都不许覆盖")
		require.NotNil(t, ext.CodexIdentity)
		require.Equal(t, "inst-1", ext.CodexIdentity.InstallationID)
		require.Nil(t, ext.BPSProfile, "codex 分支不得产出 BPS profile")
	})

	t.Run("模板 BPS 而 ext 是 codex：断言拦住，不按 ext 类型投影", func(t *testing.T) {
		ext := &domain.AccountExt{AccountID: 10, CredentialType: credential.TypeCodexPAT, CodexPATKey: &pat}
		tpl := credential.TypeBPS
		err := projectExtIdentity(ext, nil, &tpl)
		require.ErrorIs(t, err, ErrCredentialTypeMismatch)
		require.Contains(t, err.Error(), "account_id=10", "错误必须携带 account_id（spec J2）")
		require.Equal(t, credential.TypeCodexPAT, ext.CredentialType, "断言失败也不许改写行类型")
		require.Nil(t, ext.BPSProfile)
	})

	t.Run("模板 codex 而 ext 是 BPS：断言拦住", func(t *testing.T) {
		ext := &domain.AccountExt{
			AccountID: 11, CredentialType: credential.TypeBPS,
			CodexOAuthToken: &at, CodexOAuthRefreshToken: &rt, CodexAccountID: &upstream,
		}
		tpl := credential.TypeCodexOAuth
		require.ErrorIs(t, projectExtIdentity(ext, nil, &tpl), ErrCredentialTypeMismatch)
	})

	t.Run("静态通道模板不断言：分支退回行类型（今天的行为）", func(t *testing.T) {
		ext := &domain.AccountExt{
			AccountID: 12, CredentialType: credential.TypeCodexPAT, CodexPATKey: &pat,
		}
		tpl := credential.TypeAPIKey
		require.NoError(t, projectExtIdentity(ext, &domain.IdentityEnvelope{
			"installation_id": json.RawMessage(`"inst-legacy"`),
		}, &tpl), "api_key 模板 + codex-pat 遗留行是**有意允许**的（N1 回归）")
		require.Equal(t, credential.TypeCodexPAT, ext.CredentialType)
		require.NotNil(t, ext.CodexIdentity)
	})

	t.Run("无模板上下文：分支退回行类型", func(t *testing.T) {
		ext := &domain.AccountExt{
			AccountID: 13, CredentialType: credential.TypeBPS,
			CodexOAuthToken: &at, CodexOAuthRefreshToken: &rt, CodexAccountID: &upstream,
		}
		require.NoError(t, projectExtIdentity(ext, &domain.IdentityEnvelope{
			"bps": json.RawMessage(`{"profile_id":"p"}`),
		}, nil))
		require.NotNil(t, ext.BPSProfile)
		require.Nil(t, ext.CodexIdentity)
	})
}

// TestAssertExtCredentialTypeOnlyComparesBPSBit 钉住助手语义：只比 BPS 位。
func TestAssertExtCredentialTypeOnlyComparesBPSBit(t *testing.T) {
	require.ErrorIs(t, assertExtCredentialType(credential.TypeBPS, credential.TypeCodexPAT), ErrCredentialTypeMismatch)
	require.ErrorIs(t, assertExtCredentialType(credential.TypeCodexOAuth, credential.TypeBPS), ErrCredentialTypeMismatch)
	require.NoError(t, assertExtCredentialType(credential.TypeCodexPAT, credential.TypeCodexOAuth),
		"同通道内 codex-oauth ↔ codex-pat 行为等价，不构成损坏")
	require.NoError(t, assertExtCredentialType(credential.TypeAPIKey, credential.TypeCodexPAT),
		"两列的 BPS 位都是 false ⇒ 助手放行；'静态模板不断言'是调用点的策略，不是助手的语义")
}

// TestMergeExtSnapshotRowSkipsMismatchWithoutTouchingIdentityRevision 钉住合并点的
// 脏行隔离：不赋 Ext、**不覆盖 IdentityRevision**、不改写行类型。
func TestMergeExtSnapshotRowSkipsMismatchWithoutTouchingIdentityRevision(t *testing.T) {
	pat := "pat"
	d := &domain.Account{
		ID: 3, IdentityRevision: 7,
		Template: &domain.Template{CredentialType: credential.TypeBPS},
	}
	row := accountExtSnapshotRow{
		ext:               &domain.AccountExt{AccountID: 3, CredentialType: credential.TypeCodexPAT, CodexPATKey: &pat},
		extCredentialType: credential.TypeCodexPAT,
		identityRevision:  99, // ext 扫描读到的 K：脏行绝不许把它写进账号
	}
	mismatched, err := mergeExtSnapshotRow(d, row)
	require.NoError(t, err, "脏行是账号级隔离，不是装载错误")
	require.True(t, mismatched)
	require.Nil(t, d.Ext, "脏行不得进入快照")
	require.Equal(t, int64(7), d.IdentityRevision, "脏行绝不许覆盖 d.IdentityRevision")
	require.Equal(t, credential.TypeCodexPAT, row.ext.CredentialType, "行类型原值保持")
}

// TestMergeExtSnapshotRowKeepsLegacyRowType 钉住 N1：静态通道模板 + 遗留 ext 行
// 照常合并（不计数、不改类型），K 来自同一次 ext 扫描。
func TestMergeExtSnapshotRowKeepsLegacyRowType(t *testing.T) {
	pat := "pat"
	d := &domain.Account{
		ID: 4, IdentityRevision: 1,
		Template: &domain.Template{CredentialType: credential.TypeAPIKey},
	}
	row := accountExtSnapshotRow{
		ext:               &domain.AccountExt{AccountID: 4, CredentialType: credential.TypeCodexPAT, CodexPATKey: &pat},
		extCredentialType: credential.TypeCodexPAT,
		env:               &domain.IdentityEnvelope{"installation_id": json.RawMessage(`"inst"`)},
		identityRevision:  42,
	}
	mismatched, err := mergeExtSnapshotRow(d, row)
	require.NoError(t, err)
	require.False(t, mismatched, "遗留行豁免：api_key 模板 + codex-pat ext 行不是损坏")
	require.NotNil(t, d.Ext)
	require.Equal(t, credential.TypeCodexPAT, d.Ext.CredentialType, "ext 列原值不被模板类型改写")
	require.NotNil(t, d.Ext.CodexIdentity)
	require.Equal(t, int64(42), d.IdentityRevision, "通过的行：内容与 K 成对来自 ext 扫描")
}

// TestMergeExtSnapshotRowRequiresTemplate 钉住 J1 的显式错误：有 ext 行但没有模板
// 上下文不许静默丢、不许当成无 ext。
func TestMergeExtSnapshotRowRequiresTemplate(t *testing.T) {
	pat := "pat"
	d := &domain.Account{ID: 5, IdentityRevision: 3}
	_, err := mergeExtSnapshotRow(d, accountExtSnapshotRow{
		ext:               &domain.AccountExt{AccountID: 5, CredentialType: credential.TypeCodexPAT, CodexPATKey: &pat},
		extCredentialType: credential.TypeCodexPAT,
		identityRevision:  88,
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "without template")
	require.Nil(t, d.Ext)
	require.Equal(t, int64(3), d.IdentityRevision)
}
