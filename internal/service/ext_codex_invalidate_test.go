// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// alwaysStaleExtCASStore 让 ext 终写恒冲突（围栏永不过期→调用方看到的即非竞态
// 冲突原样上抛），其余能力全部委托给 fakeStore——只替换终写动词，故断言的是
// “写失败后服务层的失效动作面”。
type alwaysStaleExtCASStore struct {
	*fakeStore
}

func (f *alwaysStaleExtCASStore) AdminUpsertAccountExtCAS(ctx context.Context, e *domain.AccountExt, expectedRevision int64) (*domain.AccountExt, error) {
	return nil, fmt.Errorf("%w: account_id=%d forced stale for test", repository.ErrConflict, e.AccountID)
}

// seedGroupedExtAccount 建归组账号（ext 测试用；codex 类型模板凭据走 ext 行，
// 静态 key 可空——沿用 seedExtAccount 的静态 key 写法以便与既有断言同形）。
func seedGroupedExtAccount(t *testing.T, svc *Service, tplID int64, gids ...int64) *domain.Account {
	t.Helper()
	a, err := svc.CreateAccount(context.Background(), repository.AccountPatch{
		Name: strPtr("a"), TemplateID: &tplID, UpstreamKey: strPtr("sk-a"),
		MaxConcurrency: intPtr(8), GroupIDs: &gids,
	})
	require.NoError(t, err)
	return a
}

// TestUpsertAccountExtInvalidates ext 行是调度快照经 Selection.Ext 消费的
// codex 凭据原料：成功写入后必须按账号写面统一失效面做组级定向重载 + NOTIFY，
// 否则凭据轮换后快照叶子仍持旧令牌直到同步周期兜底。首写与已有行更新同语义
// （全列更新、写入即变更，不按值比较）。ext 行不在 aiclient 工厂键内，故不置
// Clients 位。
func TestUpsertAccountExtInvalidates(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	rec, pr := &invRecorder{}, &pubRecorder{}
	svc := &Service{store: fs, inv: rec, pub: pr, log: nil}
	tpl := seedExtTemplate(t, svc, "t-oauth-inv", credential.TypeCodexOAuth, domain.FormatOpenAIResponses)
	g, err := svc.CreateGroup(ctx, "g-ext-inv", domain.GroupVisibilityPublic, nil, nil)
	require.NoError(t, err)
	acc := seedGroupedExtAccount(t, svc, tpl.ID, g.ID)

	// 首写成功 → 组级定向重载一次 + 一条 Groups NOTIFY。
	beforeInv, beforePub := rec.countKind("accounts"), pr.total()
	saved, err := svc.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: acc.ID, CredentialType: credential.TypeCodexOAuth,
		CodexOAuthToken: strPtr("at"),
	})
	require.NoError(t, err)
	require.Equal(t, "at", *saved.CodexOAuthToken)
	require.Equal(t, beforeInv+1, rec.countKind("accounts"), "ext 首写成功必须组级定向重载")
	got := rec.last()
	require.Equal(t, "accounts", got.kind)
	require.ElementsMatch(t, []int64{g.ID}, got.gids, "失效定向到账号所在组")
	require.False(t, got.key, "ext 写不触 aiclient 工厂键 → keyChanged=false")
	require.Equal(t, beforePub+1, pr.total(), "一次操作一条 NOTIFY")
	pub := pr.last()
	require.NotNil(t, pub)
	require.ElementsMatch(t, []int64{g.ID}, pub.Groups, "NOTIFY 定向到账号所在组")
	require.False(t, pub.Clients, "ext 行是 codex 专用 → 不置 Clients")

	// 已有行更新（凭据轮换）→ 同样一次失效 + 一条 NOTIFY。
	beforeInv, beforePub = rec.countKind("accounts"), pr.total()
	_, err = svc.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: acc.ID, CredentialType: credential.TypeCodexOAuth,
		CodexOAuthToken: strPtr("at-rotated"),
	})
	require.NoError(t, err)
	require.Equal(t, beforeInv+1, rec.countKind("accounts"), "ext 更新成功必须组级定向重载")
	require.Equal(t, beforePub+1, pr.total(), "一次操作一条 NOTIFY")
}

// TestUpsertAccountExtFailureLeavesNoInvalidation 写失败路径不得留下半套失效：
// CAS 冲突耗尽（围栏未推进→原样上抛）与校验失败（列组违规→400）都不落库，故
// 组级重载 / NOTIFY 必须为零——否则快照会按未落库的值重建。
func TestUpsertAccountExtFailureLeavesNoInvalidation(t *testing.T) {
	ctx := context.Background()

	t.Run("CAS 冲突耗尽", func(t *testing.T) {
		fs := newFakeStore()
		rec, pr := &invRecorder{}, &pubRecorder{}
		svc := &Service{store: &alwaysStaleExtCASStore{fakeStore: fs}, inv: rec, pub: pr, log: nil}
		tpl := seedExtTemplate(t, svc, "t-oauth-stale", credential.TypeCodexOAuth, domain.FormatOpenAIResponses)
		g, err := svc.CreateGroup(ctx, "g-ext-stale", domain.GroupVisibilityPublic, nil, nil)
		require.NoError(t, err)
		acc := seedGroupedExtAccount(t, svc, tpl.ID, g.ID)
		// 预置存量行：失败路径走已有行分支（无 TryInsert 首写），被拒写入不
		// 得改动存量。
		inserted, err := fs.TryInsertAccountExt(ctx, &domain.AccountExt{
			AccountID: acc.ID, CredentialType: credential.TypeCodexOAuth,
			CodexIdentity: &domain.CodexIdentity{
				InstallationID: "11111111-2222-3333-4444-555555555555",
				SessionID:      "s", ThreadID: "s", WindowID: "s:0",
			},
			CodexOAuthToken: strPtr("at-old"),
		})
		require.NoError(t, err)
		require.True(t, inserted)

		beforeInv, beforePub := rec.total(), pr.total()
		_, err = svc.UpsertAccountExt(ctx, &domain.AccountExt{
			AccountID: acc.ID, CredentialType: credential.TypeCodexOAuth,
			CodexOAuthToken: strPtr("at-new"),
		})
		require.ErrorIs(t, err, ErrConflict)
		require.Equal(t, beforeInv, rec.total(), "a rolled-back write must not mark any snapshot dirty")
		require.Equal(t, beforePub, pr.total(), "a rolled-back write must not publish NOTIFY")
		got, err := fs.GetAccountExt(ctx, acc.ID)
		require.NoError(t, err)
		require.Equal(t, "at-old", *got.CodexOAuthToken, "被拒写入不改动存量行")
	})

	t.Run("校验失败", func(t *testing.T) {
		fs := newFakeStore()
		rec, pr := &invRecorder{}, &pubRecorder{}
		svc := &Service{store: fs, inv: rec, pub: pr, log: nil}
		tpl := seedExtTemplate(t, svc, "t-oauth-bad", credential.TypeCodexOAuth, domain.FormatOpenAIResponses)
		g, err := svc.CreateGroup(ctx, "g-ext-bad", domain.GroupVisibilityPublic, nil, nil)
		require.NoError(t, err)
		acc := seedGroupedExtAccount(t, svc, tpl.ID, g.ID)

		beforeInv, beforePub := rec.total(), pr.total()
		// oauth 行缺令牌 → 列组最小完整性违规 → 400，不落库。
		_, err = svc.UpsertAccountExt(ctx, &domain.AccountExt{
			AccountID: acc.ID, CredentialType: credential.TypeCodexOAuth,
		})
		require.ErrorIs(t, err, ErrInvalidInput)
		require.Equal(t, beforeInv, rec.total(), "校验失败不得失效")
		require.Equal(t, beforePub, pr.total(), "校验失败不得发布 NOTIFY")
	})
}
