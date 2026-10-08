// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// ext_codex_scope_pg_test.go C1 收官（真实 PG）：账号 ext 的 HTTP 单读/单写服务
// 编排必须走作用域谓词——越域读/写 ⇒ 404，且转属交错（屏障，无 sleep）下写事务
// 按作用域拒绝，不落引他人账号。
//
// 跑法：TEST_DATABASE_URL=... go test ./internal/service/ -run 'AccountExtScope|AccountExtTransfer' -count=1 -v

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// scopeCtxSvc 注入供应商面行层作用域（镜像 handler.SupplierScopeInject）。
func scopeCtxSvc(uid int64) context.Context {
	return domain.WithAccountScope(context.Background(), domain.SupplierAccountScope(uid))
}

func seedSupplierUserSvc(t *testing.T, repos *repository.Repository, email string) *domain.User {
	t.Helper()
	u, err := repos.CreateUser(context.Background(), &domain.User{
		Email: email, PasswordHash: "h-" + email, Role: domain.RoleSupplier, Status: domain.UserStatusActive,
	})
	require.NoError(t, err)
	return u
}

func seedOwnedCodexAccountSvc(t *testing.T, repos *repository.Repository, tplID, uid int64, name string) *domain.Account {
	t.Helper()
	a, err := repos.Accounts.CreateAccount(context.Background(), &domain.Account{
		Name: name, TemplateID: tplID, UpstreamKey: "sk-" + name,
		MaxConcurrency: 8, Enabled: true, SupplierUserID: uid,
	})
	require.NoError(t, err)
	return a
}

// TestServiceAccountExtScope 越域 ext 读/写 ⇒ 404（不读他人 ext、不落引他人账号）。
func TestServiceAccountExtScope(t *testing.T) {
	svc, repos := newCodexImportPG(t)
	ctx := context.Background()
	oauthTpl, _, _ := seedCodexImportTemplates(t, repos)
	owner := seedSupplierUserSvc(t, repos, "ext-scope-owner@example.com")
	other := seedSupplierUserSvc(t, repos, "ext-scope-other@example.com")
	acc := seedOwnedCodexAccountSvc(t, repos, oauthTpl, owner.ID, "ext-scope-acc")

	// 管理面首写 ext 行（越域断言基线）。
	_, err := repos.AccountExts.UpsertAccountExt(ctx, &domain.AccountExt{
		AccountID: acc.ID, CredentialType: credential.TypeCodexOAuth,
		CodexIdentity:   &domain.CodexIdentity{InstallationID: "11111111-2222-3333-4444-555555555555"},
		CodexOAuthToken: strPtr2("owner-token"),
	})
	require.NoError(t, err)

	// ① 供应商面本人读 → 200 内容。
	got, err := svc.GetAccountExt(scopeCtxSvc(owner.ID), acc.ID)
	require.NoError(t, err)
	require.Equal(t, "owner-token", *got.CodexOAuthToken)

	// ② 越域读 → 404（不读他人 ext）。
	_, err = svc.GetAccountExt(scopeCtxSvc(other.ID), acc.ID)
	require.ErrorIs(t, err, ErrNotFound, "越域 ext 单读必须 404（C1）")

	// ③ 越域写 → 404（不落库）。
	_, err = svc.UpsertAccountExt(scopeCtxSvc(other.ID), &domain.AccountExt{
		AccountID: acc.ID, CredentialType: credential.TypeCodexOAuth,
		CodexIdentity:   &domain.CodexIdentity{InstallationID: "11111111-2222-3333-4444-555555555555"},
		CodexOAuthToken: strPtr2("attacker"),
	})
	require.ErrorIs(t, err, ErrNotFound, "越域 ext 写必须 404（C1）")

	// ④ 他人 ext 未被改动。
	cur, err := repos.AccountExts.GetAccountExt(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, "owner-token", *cur.CodexOAuthToken, "越域写不得改动他人凭据")
}

// barrierExtCASStore 在首次 AdminUpsertAccountExtCAS 调用前后设置屏障：让测试在
// 服务「已通过作用域读、即将进入写事务」时插入一次转属（无 sleep 的确定性交错）。
type barrierExtCASStore struct {
	*repository.Repository
	ready   chan struct{}
	proceed chan struct{}
	once    sync.Once
}

func (s *barrierExtCASStore) AdminUpsertAccountExtCAS(ctx context.Context, e *domain.AccountExt, rev int64) (*domain.AccountExt, error) {
	s.once.Do(func() {
		close(s.ready)
		<-s.proceed
	})
	return s.Repository.AdminUpsertAccountExtCAS(ctx, e, rev)
}

// TestServiceAccountExtTransferInterleave 转属交错（屏障，禁 sleep）：服务已按
// 作用域读到「本人账号」，随后账号被转出作用域，写事务必须在同一事务内按作用域
// 拒绝（404），**不落引他人账号**（首写也不得在转属后落 ext 行）。
func TestServiceAccountExtTransferInterleave(t *testing.T) {
	_, repos := newCodexImportPG(t)
	ctx := context.Background()
	oauthTpl, _, _ := seedCodexImportTemplates(t, repos)
	owner := seedSupplierUserSvc(t, repos, "ext-xfer-owner@example.com")
	target := seedSupplierUserSvc(t, repos, "ext-xfer-target@example.com")
	acc := seedOwnedCodexAccountSvc(t, repos, oauthTpl, owner.ID, "ext-xfer-acc")
	// 无 ext 行 ⇒ 走首写路径（旧实现会先 TryInsert 落引、后 CAS 拒绝）。

	bar := &barrierExtCASStore{Repository: repos, ready: make(chan struct{}), proceed: make(chan struct{})}
	svc := New(Deps{Store: bar, Scheduler: nil, Invalidate: NopInvalidator{}, Publisher: nil, RuleReload: nil, Keys: nil, Log: nil, EmailCodeStore: testEmailCodes})

	var wg sync.WaitGroup
	var werr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, werr = svc.UpsertAccountExt(scopeCtxSvc(owner.ID), &domain.AccountExt{
			AccountID: acc.ID, CredentialType: credential.TypeCodexOAuth,
			CodexOAuthToken: strPtr2("tok-owner"),
		})
	}()

	<-bar.ready // 服务已通过作用域读，正要进入写事务
	// 转属：把账号移出 owner 作用域（→ target，独立管理面连接）。
	_, err := repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc.ID}, repository.AccountPatch{SupplierUserID: &target.ID})
	require.NoError(t, err)
	close(bar.proceed) // 放行写事务：作用域复核应失败
	wg.Wait()

	require.ErrorIs(t, werr, ErrNotFound, "转属后写事务必须按作用域 404（不写他人账号）")
	// ext 行不得落库（首写不得在转属后落引）。
	_, err = repos.AccountExts.GetAccountExt(ctx, acc.ID)
	require.ErrorIs(t, err, repository.ErrNotFound, "转属交错不得留下 ext 行")
	// 账号仍归 target。
	moved, err := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, target.ID, moved.SupplierUserID)
}
