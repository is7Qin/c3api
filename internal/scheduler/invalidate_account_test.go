// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// TestInvalidateAccountReloadsExt 轮转回写后的快照同步（T5 §1 P3-3）：账号的
// AccountExt 内存快照条目失效 → 组级定向重载（复用 InvalidateGroup）→ 快照
// 携带新凭据（下个会话重载新凭据——避免旧令牌 401 额外往返）。
func TestInvalidateAccountReloadsExt(t *testing.T) {
	ext := &domain.AccountExt{AccountID: 1, CredentialType: credential.TypeCodexOAuth, InstallationID: "inst-1"}
	acc := &domain.Account{ID: 1, TemplateID: 1, Status: domain.StatusActive, Ext: ext}
	byGroup := map[int64][]*domain.Account{10: {acc}}
	m := newMemLoader(byGroup)
	s := newSched(t, m)
	require.NoError(t, s.InvalidateAllSync())

	// 轮转回写落库后 loader 数据已变（新凭据）
	extNew := &domain.AccountExt{AccountID: 1, CredentialType: credential.TypeCodexOAuth,
		InstallationID: "inst-1", OAuthToken: strPtrT("at-new"), OAuthRefreshToken: strPtrT("rt-new")}
	m.mu.Lock()
	m.byGroup[10][0].Ext = extNew
	m.mu.Unlock()

	s.InvalidateAccount(1)
	byID := s.store.byID.Load().(map[int64]*accountSnapshot)
	got, ok := byID[1]
	require.True(t, ok, "账号仍在快照")
	require.Same(t, extNew, got.static.Load().acc.Ext, "回写后快照条目重载新凭据（下个会话 Selection.Ext 新值）")
	// 并发槽继承（组级重载纪律）：失效不丢在途计数
	require.Equal(t, int64(0), got.concurrency.Load())
}

// TestInvalidateAccountUnknownNoop 快照外账号 / 无分组账号 → no-op 不 panic
//（轮转回调低频防御；失效上报同哲学——快照外无状态可改）。
func TestInvalidateAccountUnknownNoop(t *testing.T) {
	ext := &domain.AccountExt{AccountID: 1, CredentialType: credential.TypeCodexOAuth, InstallationID: "inst-1"}
	byGroup := map[int64][]*domain.Account{10: {{ID: 1, TemplateID: 1, Status: domain.StatusActive, Ext: ext}}}
	m := newMemLoader(byGroup)
	s := newSched(t, m)
	require.NoError(t, s.InvalidateAllSync())

	require.NotPanics(t, func() {
		s.InvalidateAccount(999) // 快照外
	})
	byID := s.store.byID.Load().(map[int64]*accountSnapshot)
	require.Same(t, ext, byID[1].static.Load().acc.Ext, "未知账号失效不影响既有快照")
}

// strPtrT 测试用字符串指针。
func strPtrT(s string) *string { return &s }
