// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// TestInvalidateAccountReloadsExt 轮转回写后的快照同步（§1）：账号的
// AccountExt 内存快照条目失效 → 组级定向重载（复用 InvalidateGroup）→ 快照
// 携带新凭据（下个会话重载新凭据——避免旧令牌 401 额外往返）。
func TestInvalidateAccountReloadsExt(t *testing.T) {
	ext := &domain.AccountExt{AccountID: 1, CredentialType: credential.TypeCodexOAuth, CodexIdentity: &domain.CodexIdentity{InstallationID: "inst-1"}}
	acc := &domain.Account{ID: 1, TemplateID: 1, Enabled: true, Ext: ext}
	byGroup := map[int64][]*domain.Account{10: {acc}}
	m := newMemLoader(byGroup)
	s := newSched(t, m)
	require.NoError(t, s.InvalidateAllSync())

	// 轮转回写落库后 loader 数据已变（新凭据）
	extNew := &domain.AccountExt{AccountID: 1, CredentialType: credential.TypeCodexOAuth,
		CodexIdentity: &domain.CodexIdentity{InstallationID: "inst-1"}, CodexOAuthToken: strPtrT("at-new"), CodexOAuthRefreshToken: strPtrT("rt-new")}
	m.mu.Lock()
	m.byGroup[10][0].Ext = extNew
	m.mu.Unlock()

	s.InvalidateAccount(1)
	// Atomic publication: the staged rotation pairs on the next compile.
	s.compileOnce()
	byID := s.View().ByID()
	got, ok := byID[1]
	require.True(t, ok, "账号仍在快照")
	require.Same(t, extNew, got.static.Load().acc.Ext, "回写后快照条目重载新凭据（下个会话 Selection.Ext 新值）")
	// 并发槽继承（组级重载纪律）：失效不丢在途计数
	require.Equal(t, int64(0), got.runtime.concurrency.Load())
}

// TestInvalidateAccountUnknownNoop 快照外账号 / 无分组账号 → no-op 不 panic
// （轮转回调低频防御；失效上报同哲学——快照外无状态可改）。
func TestInvalidateAccountUnknownNoop(t *testing.T) {
	ext := &domain.AccountExt{AccountID: 1, CredentialType: credential.TypeCodexOAuth, CodexIdentity: &domain.CodexIdentity{InstallationID: "inst-1"}}
	byGroup := map[int64][]*domain.Account{10: {{ID: 1, TemplateID: 1, Enabled: true, Ext: ext}}}
	m := newMemLoader(byGroup)
	s := newSched(t, m)
	require.NoError(t, s.InvalidateAllSync())

	require.NotPanics(t, func() {
		s.InvalidateAccount(999) // 快照外
	})
	byID := s.View().ByID()
	require.Same(t, ext, byID[1].static.Load().acc.Ext, "未知账号失效不影响既有快照")
}

// TestInvalidateAccountPrefersPendingAndDoesNotResurrectDeletedMembership: the
// account's groups are read from the PENDING leaf when one exists, so a
// membership deleted by an earlier batch is not re-added by a later
// InvalidateAccount for that account.
func TestInvalidateAccountPrefersPendingAndDoesNotResurrectDeletedMembership(t *testing.T) {
	tp := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	a1 := acc(1, tp, 4) // groups 10 and 20
	ldr := newT1Loader(map[int64][]*domain.Account{10: {a1}, 20: {a1}})
	s := New(testCfg(), ldr, newTestRuleEngine(t), nil, nil, nil, nil)
	require.NoError(t, s.reload(context.Background()))

	// Server deletes a1 from group 20; the batch stages a pending leaf [10].
	ldr.mu.Lock()
	ldr.byGroup[20] = nil
	ldr.mu.Unlock()
	s.InvalidateGroups([]int64{20})
	require.Equal(t, []int64{10}, s.publisher.pending.byID[1].static.Load().groupIDs)

	// InvalidateAccount must read the pending leaf → reload group 10 only.
	ldr.reset()
	s.InvalidateAccount(1)
	require.Equal(t, []int64{10}, ldr.groupCallsCopy(), "must reload only the pending leaf's groups")

	// Group 20 must not be re-added.
	require.Equal(t, []int64{10}, s.publisher.pending.byID[1].static.Load().groupIDs)
	require.Contains(t, s.publisher.pending.groups, int64(10))
}

// strPtrT 测试用字符串指针。
func strPtrT(s string) *string { return &s }
