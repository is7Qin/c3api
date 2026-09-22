// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// TestAccountUsageCredentialDispatch 凭据组装判定矩阵（纯数据面零上游调用）：
// api-key（无 ext 行）→ nil/nil；codex-oauth → oauth 列组派生 cred；
// codex-pat → pat 列组派生 cred；ext 行存在但凭据全空 → nil/nil。
func TestAccountUsageCredentialDispatch(t *testing.T) {
	svc := &Service{store: newFakeStore()}
	ctx := context.Background()

	// api-key：无 ext 行 → GetAccountExt ErrNotFound → nil/nil
	cred, err := svc.AccountUsageCredential(ctx, 1)
	require.NoError(t, err)
	require.Nil(t, cred, "api-key（无 ext 行）→ 无上游能力")

	// codex-oauth：ext 行 → oauth 列组派生 cred
	exp := time.Now().Add(time.Hour)
	f := svc.store.(*fakeStore)
	f.accExts[2] = &domain.AccountExt{
		AccountID: 2, CredentialType: credential.TypeCodexOAuth,
		CodexOAuthToken: strPtr("at"), CodexOAuthRefreshToken: strPtr("rt"), CodexOAuthExpiresAt: &exp,
	}
	cred, err = svc.AccountUsageCredential(ctx, 2)
	require.NoError(t, err)
	require.NotNil(t, cred)
	require.Equal(t, int64(2), cred.AccountID)
	require.Equal(t, "at", cred.OAuthToken)
	require.Equal(t, "rt", cred.OAuthRefreshToken, "oauth 列组派生 cred")

	// codex-pat：pat 列组派生 cred
	f.accExts[3] = &domain.AccountExt{AccountID: 3, CredentialType: credential.TypeCodexPAT, CodexPATKey: strPtr("pat-x")}
	cred, err = svc.AccountUsageCredential(ctx, 3)
	require.NoError(t, err)
	require.NotNil(t, cred)
	require.Equal(t, "pat-x", cred.PATKey, "pat 列组派生 cred")

	// ext 行存在但凭据全空 → nil/nil（防御分支）
	f.accExts[4] = &domain.AccountExt{AccountID: 4, CredentialType: credential.TypeCodexPAT}
	cred, err = svc.AccountUsageCredential(ctx, 4)
	require.NoError(t, err)
	require.Nil(t, cred, "凭据全空 → 无上游能力")
}

// TestAccountUsageCredentialStoreError store 故障透传：GetAccountExt
// 非 ErrNotFound 错误 → 非 nil 错误 + nil 凭据（handler 侧记 null/null，
// 不误标上游问题）。
func TestAccountUsageCredentialStoreError(t *testing.T) {
	svc := &Service{store: newFakeStore()}
	ctx := context.Background()
	f := svc.store.(*fakeStore)
	f.accExtErr[1] = errors.New("store down") // 非 ErrNotFound store 故障

	cred, err := svc.AccountUsageCredential(ctx, 1)
	require.Error(t, err, "store 故障透错")
	require.Nil(t, cred)
}
