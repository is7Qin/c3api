// SPDX-License-Identifier: AGPL-3.0-or-later
package sdkbridge

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// TestCodexCredentialFromAccount 凭据派生只写一份（probe 与 429 补拉 resolver
// 同源）：账号行 → 完整凭据，ID/K 必须来自同一次快照读。
func TestCodexCredentialFromAccount(t *testing.T) {
	acct := &domain.Account{
		ID:               5,
		IdentityRevision: 7,
		Template:         &domain.Template{ID: 1, CredentialType: credential.TypeCodexPAT},
		Ext: &domain.AccountExt{
			AccountID:      5,
			CredentialType: credential.TypeCodexPAT,
			CodexPATKey:    new("pat-key"),
		},
	}
	cred, ok := CodexCredentialFromAccount(acct)
	require.True(t, ok)
	require.Equal(t, int64(5), cred.AccountID)
	require.Equal(t, int64(7), cred.IdentityRevision)
	require.Equal(t, "pat-key", cred.PATKey)

	// 非 codex 族：没有额度面（调用方按"跳过"处理）。
	_, ok = CodexCredentialFromAccount(&domain.Account{ID: 6, Template: &domain.Template{CredentialType: credential.TypeAPIKey}})
	require.False(t, ok)
	// 无模板 / 账号行缺失：同样 false（不是错误）。
	_, ok = CodexCredentialFromAccount(&domain.Account{ID: 7})
	require.False(t, ok)
	_, ok = CodexCredentialFromAccount(nil)
	require.False(t, ok)
}

// TestResolveThrottleDeadlineSkipsNonCodex 三条"无事可做"的分支都必须是
// (zero,false,nil) 或显式错误——绝不 panic、绝不误报成功（误报会让 worker 写
// 一条凭空的冷却截止）。
func TestResolveThrottleDeadlineSkipsNonCodex(t *testing.T) {
	a := &Codex{}
	ctx := context.Background()

	_, ok, err := a.ResolveThrottleDeadline(ctx, nil)
	require.NoError(t, err)
	require.False(t, ok)

	_, ok, err = a.ResolveThrottleDeadline(ctx, &domain.Account{ID: 1, Template: &domain.Template{CredentialType: credential.TypeAPIKey}})
	require.NoError(t, err)
	require.False(t, ok, "非 codex 族 ⇒ ok=false, err=nil（不是错误）")

	var nilAdapter *Codex
	_, ok, err = nilAdapter.ResolveThrottleDeadline(ctx, nil)
	require.Error(t, err, "typed-nil 适配器必须显式报错（装配缺陷），不得 panic")
	require.False(t, ok)
}
