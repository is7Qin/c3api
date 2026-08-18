// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/sdkbridge"
)

// fakeUsageSnapshotter 快照数据源替身（断言收参 + 可编程返回——service 纯编排
// 测试注入面）。
type fakeUsageSnapshotter struct {
	mu    sync.Mutex
	creds []*domain.AccountCredential
	snap  *domain.CodexUsageSnapshot
	err   error
}

func (f *fakeUsageSnapshotter) GetUsageSnapshot(ctx context.Context, cred *domain.AccountCredential) (*domain.CodexUsageSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.creds = append(f.creds, cred)
	return f.snap, f.err
}

func (f *fakeUsageSnapshotter) calls() []*domain.AccountCredential {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.creds
}

// TestAccountUsageTypeDispatch 账号类型判定矩阵（数据流——凭据取 + 类型判定 +
// 调用）：api-key（无 ext 行）→ nil 快照零 sdkbridge 调用；codex-oauth/
// codex-pat → 调用（cred 直传断言收参）。
func TestAccountUsageTypeDispatch(t *testing.T) {
	fake := &fakeUsageSnapshotter{snap: &domain.CodexUsageSnapshot{PlanType: "chatgpt-plus"}}
	svc := &Service{store: newFakeStore()}
	svc.SetUsageSnapshotter(fake)
	ctx := context.Background()

	// api-key：无 ext 行 → GetAccountExt ErrNotFound → nil 快照零调用
	snap, err := svc.AccountUsage(ctx, 1)
	require.NoError(t, err)
	require.Nil(t, snap, "api-key（无 ext 行）→ nil 快照")
	require.Empty(t, fake.calls(), "零 sdkbridge 调用")

	// codex-oauth：ext 行 → cred 直传
	exp := time.Now().Add(time.Hour)
	f := svc.store.(*fakeStore)
	f.accExts[2] = &domain.AccountExt{
		AccountID: 2, CredentialType: credential.TypeCodexOAuth,
		CodexOAuthToken: strPtr("at"), CodexOAuthRefreshToken: strPtr("rt"), CodexOAuthExpiresAt: &exp,
	}
	snap, err = svc.AccountUsage(ctx, 2)
	require.NoError(t, err)
	require.Equal(t, "chatgpt-plus", snap.PlanType, "快照透传")
	creds := fake.calls()
	require.Len(t, creds, 1)
	require.Equal(t, int64(2), creds[0].AccountID)
	require.Equal(t, "at", creds[0].OAuthToken)
	require.Equal(t, "rt", creds[0].OAuthRefreshToken, "oauth 列组派生 cred")

	// codex-pat：pat 列组派生 cred
	f.accExts[3] = &domain.AccountExt{AccountID: 3, CredentialType: credential.TypeCodexPAT, CodexPATKey: strPtr("pat-x")}
	snap, err = svc.AccountUsage(ctx, 3)
	require.NoError(t, err)
	require.NotNil(t, snap)
	creds = fake.calls()
	require.Len(t, creds, 2)
	require.Equal(t, "pat-x", creds[1].PATKey, "pat 列组派生 cred")
}

// TestAccountUsageErrorPassthrough 错误分类透传（task 2 upstream_error 映射
// 输入）：sdkbridge 哨兵原样透传。
func TestAccountUsageErrorPassthrough(t *testing.T) {
	svc := &Service{store: newFakeStore()}
	ctx := context.Background()
	f := svc.store.(*fakeStore)
	f.accExts[1] = &domain.AccountExt{AccountID: 1, CredentialType: credential.TypeCodexPAT, CodexPATKey: strPtr("pat-x")}

	for _, want := range []error{sdkbridge.ErrAuthExpired, sdkbridge.ErrUpstream} {
		svc.SetUsageSnapshotter(&fakeUsageSnapshotter{err: want})
		_, err := svc.AccountUsage(ctx, 1)
		require.ErrorIs(t, err, want, "分类错误原样透传")
	}
}
