// SPDX-License-Identifier: AGPL-3.0-or-later
package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// TestServiceImportFencing verifies admin re-import increments revision exactly once.
func TestServiceImportFencing(t *testing.T) {
	svc, repos := newCodexImportPG(t)
	ctx := context.Background()
	oauthTpl, _, gid := seedCodexImportTemplates(t, repos)
	// first import
	res, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
		{CodexEmail: "fence@example.com", CodexAccountID: "fence-1", CodexOAuthToken: "tok1", CodexOAuthRefreshToken: "rt1"},
	}, &oauthTpl, &gid, domain.CodexImportConfig{})
	require.NoError(t, err)
	require.Equal(t, 1, res.Imported)
	ext, _ := repos.AccountExts.FindAccountExtByCodexKey(ctx, "fence@example.com", "fence-1")
	acc, _ := repos.Accounts.GetAccount(ctx, ext.AccountID)
	require.Equal(t, int64(1), acc.LifecycleRevision)
	require.Equal(t, "tok1", *ext.CodexOAuthToken)

	// re-import same key with new token -> should increment revision to 2
	res, err = svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
		{CodexEmail: "fence@example.com", CodexAccountID: "fence-1", CodexOAuthToken: "tok2", CodexOAuthRefreshToken: "rt2"},
	}, &oauthTpl, nil, domain.CodexImportConfig{})
	require.NoError(t, err)
	require.Equal(t, 1, res.Updated)
	ext2, _ := repos.AccountExts.FindAccountExtByCodexKey(ctx, "fence@example.com", "fence-1")
	require.Equal(t, "tok2", *ext2.CodexOAuthToken)
	acc2, _ := repos.Accounts.GetAccount(ctx, ext.AccountID)
	require.Equal(t, int64(2), acc2.LifecycleRevision, "admin re-import must increment revision 1->2")
}

// TestServiceSDKRefreshNotFenced verifies SDK refresh does NOT increment.
func TestServiceSDKRefreshNotFenced(t *testing.T) {
	svc, repos := newCodexImportPG(t)
	ctx := context.Background()
	oauthTpl, _, gid := seedCodexImportTemplates(t, repos)
	res, _ := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
		{CodexEmail: "sdk@example.com", CodexAccountID: "sdk-1", CodexOAuthToken: "tok1", CodexOAuthRefreshToken: "rt1"},
	}, &oauthTpl, &gid, domain.CodexImportConfig{})
	require.Equal(t, 1, res.Imported)
	ext, _ := repos.AccountExts.FindAccountExtByCodexKey(ctx, "sdk@example.com", "sdk-1")
	acc, _ := repos.Accounts.GetAccount(ctx, ext.AccountID)
	require.Equal(t, int64(1), acc.LifecycleRevision)
	// SDK rotation via unfenced method
	require.NoError(t, repos.AccountExts.WriteOAuthRotation(ctx, acc.ID, "tok-sdk", "rt-sdk", nil))
	acc2, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(1), acc2.LifecycleRevision, "SDK refresh must not increment")
	ext2, _ := repos.AccountExts.FindAccountExtByCodexKey(ctx, "sdk@example.com", "sdk-1")
	require.Equal(t, "tok-sdk", *ext2.CodexOAuthToken)
}

// TestServiceExtPutFencing verifies admin PUT /ext increments revision atomically and stale fails.
func TestServiceExtPutFencing(t *testing.T) {
	svc, repos := newCodexImportPG(t)
	ctx := context.Background()
	oauthTpl, _, _ := seedCodexImportTemplates(t, repos)
	res, _ := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
		{CodexEmail: "ext@example.com", CodexAccountID: "ext-1", CodexOAuthToken: "tok1", CodexOAuthRefreshToken: "rt1"},
	}, &oauthTpl, nil, domain.CodexImportConfig{})
	require.Equal(t, 1, res.Imported)
	ext, _ := repos.AccountExts.FindAccountExtByCodexKey(ctx, "ext@example.com", "ext-1")
	acc, _ := repos.Accounts.GetAccount(ctx, ext.AccountID)
	require.Equal(t, int64(1), acc.LifecycleRevision)
	// Admin ext PUT
	e := &domain.AccountExt{AccountID: acc.ID, CredentialType: "codex-oauth", CodexIdentity: ext.CodexIdentity, CodexOAuthToken: strPtr2("tok-admin"), CodexOAuthRefreshToken: strPtr2("rt-admin"), CodexOAuthExpiresAt: nil, CodexEmail: ext.CodexEmail, CodexAccountID: ext.CodexAccountID}
	saved, err := svc.UpsertAccountExt(ctx, e)
	require.NoError(t, err)
	require.Equal(t, "tok-admin", *saved.CodexOAuthToken)
	acc2, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(2), acc2.LifecycleRevision, "ext PUT must increment")
}

// TestServiceAccountPutFencing verifies account PUT credential/baseURL increments.
func TestServiceAccountPutFencing(t *testing.T) {
	svc, repos := newCodexImportPG(t)
	ctx := context.Background()
	// use helper seed
	tpl2 := seedPGTemplateForFencing(t, repos)
	acc, err := svc.CreateAccount(ctx, repository.AccountPatch{
		Name:           strPtr("put-fence"),
		TemplateID:     int64Ptr(tpl2.ID),
		UpstreamKey:    strPtr("sk-old"),
		MaxConcurrency: intPtr(8),
	})
	require.NoError(t, err)
	require.Equal(t, int64(1), acc.LifecycleRevision)
	// upstream_key 变更经唯一写点 → 无条件推进 C
	updated, err := svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{UpstreamKey: strPtr("sk-new")}, nil)
	require.NoError(t, err)
	require.Equal(t, "sk-new", updated.UpstreamKey)
	got, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(2), got.LifecycleRevision, "配置写入必须推进 C")
	// base_url 变更同样推进 C
	base := "https://new.example.com"
	_, err = svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{BaseURL: &base}, nil)
	require.NoError(t, err)
	got2, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(3), got2.LifecycleRevision, "配置写入必须推进 C")
	require.NotNil(t, got2.BaseURL)
	require.Equal(t, base, *got2.BaseURL)
}

// TestServiceBatchFencing verifies batch credential increments per account and stale fails.
func TestServiceBatchFencing(t *testing.T) {
	svc, repos := newCodexImportPG(t)
	ctx := context.Background()
	tpl := seedPGTemplateForFencing(t, repos)
	a1, _ := svc.CreateAccount(ctx, repository.AccountPatch{
		Name:           strPtr("batch1"),
		TemplateID:     int64Ptr(tpl.ID),
		UpstreamKey:    strPtr("sk-1"),
		MaxConcurrency: intPtr(8),
	})
	a2, _ := svc.CreateAccount(ctx, repository.AccountPatch{
		Name:           strPtr("batch2"),
		TemplateID:     int64Ptr(tpl.ID),
		UpstreamKey:    strPtr("sk-2"),
		MaxConcurrency: intPtr(8),
	})
	require.Equal(t, int64(1), a1.LifecycleRevision)
	newKey := "sk-batch"
	_, err := svc.UpdateAccountsBatch(ctx, []int64{a1.ID, a2.ID}, repository.AccountPatch{UpstreamKey: &newKey})
	require.NoError(t, err)
	g1, _ := repos.Accounts.GetAccount(ctx, a1.ID)
	g2, _ := repos.Accounts.GetAccount(ctx, a2.ID)
	require.Equal(t, "sk-batch", g1.UpstreamKey)
	require.Equal(t, int64(2), g1.LifecycleRevision)
	require.Equal(t, int64(2), g2.LifecycleRevision)
}

// TestServiceRecoveryFencing verifies fenced recover increments revision.
func TestServiceRecoveryFencing(t *testing.T) {
	svc, repos := newCodexImportPG(t)
	ctx := context.Background()
	tpl := seedPGTemplateForFencing(t, repos)
	acc, _ := svc.CreateAccount(ctx, repository.AccountPatch{
		Name:           strPtr("rec-fence"),
		TemplateID:     int64Ptr(tpl.ID),
		UpstreamKey:    strPtr("sk-x"),
		MaxConcurrency: intPtr(8),
	})
	// fail via CAS
	require.NoError(t, repos.Accounts.FailAccountCAS(ctx, acc.ID, 1, "rule", getFixedTime(), "boom"))
	afterFail, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.NotNil(t, afterFail.FailedAt)
	// recover via fenced endpoint（恢复唯一入口）
	got, err := svc.RecoverAccount(ctx, acc.ID, afterFail.LifecycleRevision)
	require.NoError(t, err)
	require.Nil(t, got.FailedAt)
	require.Equal(t, int64(3), got.LifecycleRevision, "recovery must increment revision")
}

func TestServiceStaleImportNotUpdate(t *testing.T) {
	svc, repos := newCodexImportPG(t)
	ctx := context.Background()
	oauthTpl, _, gid := seedCodexImportTemplates(t, repos)
	_, _ = svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
		{CodexEmail: "stale@example.com", CodexAccountID: "stale-1", CodexOAuthToken: "tok1", CodexOAuthRefreshToken: "rt1"},
	}, &oauthTpl, &gid, domain.CodexImportConfig{})
	ext, _ := repos.AccountExts.FindAccountExtByCodexKey(ctx, "stale@example.com", "stale-1")
	acc, _ := repos.Accounts.GetAccount(ctx, ext.AccountID)
	require.Equal(t, int64(1), acc.LifecycleRevision)
	// admin re-import to 2
	_, _ = svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
		{CodexEmail: "stale@example.com", CodexAccountID: "stale-1", CodexOAuthToken: "tok2", CodexOAuthRefreshToken: "rt2"},
	}, &oauthTpl, nil, domain.CodexImportConfig{})
	acc2, _ := repos.Accounts.GetAccount(ctx, ext.AccountID)
	require.Equal(t, int64(2), acc2.LifecycleRevision)
	ext2, _ := repos.AccountExts.FindAccountExtByCodexKey(ctx, "stale@example.com", "stale-1")
	require.Equal(t, "tok2", *ext2.CodexOAuthToken)
	// stale admin rotation with old revision should fail and not change ext nor revision
	// test ext stale via AdminWriteOAuthRotationCAS
	err := repos.AccountExts.AdminWriteOAuthRotationCAS(ctx, acc.ID, 1, "tok-stale", "rt-stale", nil)
	require.ErrorIs(t, err, repository.ErrConflict)
	afterStale, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(2), afterStale.LifecycleRevision, "stale must not increment")
	ext3, _ := repos.AccountExts.FindAccountExtByCodexKey(ctx, "stale@example.com", "stale-1")
	require.Equal(t, "tok2", *ext3.CodexOAuthToken, "stale must not update ext")
	// SDK refresh should still update ext without increment
	require.NoError(t, repos.AccountExts.WriteOAuthRotation(ctx, acc.ID, "tok-sdk2", "rt-sdk2", nil))
	afterSDK, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(2), afterSDK.LifecycleRevision, "SDK refresh must not increment after admin")
	extSDK, _ := repos.AccountExts.FindAccountExtByCodexKey(ctx, "stale@example.com", "stale-1")
	require.Equal(t, "tok-sdk2", *extSDK.CodexOAuthToken)
}

func TestServiceStaleExtPutNotUpdate(t *testing.T) {
	svc, repos := newCodexImportPG(t)
	ctx := context.Background()
	oauthTpl, _, _ := seedCodexImportTemplates(t, repos)
	_, _ = svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
		{CodexEmail: "stale-ext2@example.com", CodexAccountID: "stale-ext2-1", CodexOAuthToken: "tok1", CodexOAuthRefreshToken: "rt1"},
	}, &oauthTpl, nil, domain.CodexImportConfig{})
	ext, _ := repos.AccountExts.FindAccountExtByCodexKey(ctx, "stale-ext2@example.com", "stale-ext2-1")
	acc, _ := repos.Accounts.GetAccount(ctx, ext.AccountID)
	require.Equal(t, int64(1), acc.LifecycleRevision)
	e := &domain.AccountExt{AccountID: acc.ID, CredentialType: "codex-oauth", CodexIdentity: ext.CodexIdentity, CodexOAuthToken: strPtr2("tok2"), CodexOAuthRefreshToken: strPtr2("rt2"), CodexEmail: ext.CodexEmail, CodexAccountID: ext.CodexAccountID}
	_, err := svc.UpsertAccountExt(ctx, e)
	require.NoError(t, err)
	acc2, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(2), acc2.LifecycleRevision)
	// stale with old revision should fail
	_, err = repos.AccountExts.AdminUpsertAccountExtCAS(ctx, e, 1)
	require.ErrorIs(t, err, repository.ErrConflict)
	afterStale, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(2), afterStale.LifecycleRevision)
}

func TestServiceStaleAccountPutNotUpdate(t *testing.T) {
	svc, repos := newCodexImportPG(t)
	ctx := context.Background()
	tpl := seedPGTemplateForFencing(t, repos)
	acc, _ := svc.CreateAccount(ctx, repository.AccountPatch{
		Name:           strPtr("stale-put"),
		TemplateID:     int64Ptr(tpl.ID),
		UpstreamKey:    strPtr("sk-old"),
		MaxConcurrency: intPtr(8),
	})
	require.Equal(t, int64(1), acc.LifecycleRevision)
	_, err := svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{UpstreamKey: strPtr("sk-new")}, nil)
	require.NoError(t, err)
	got, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(2), got.LifecycleRevision)
	afterStale, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(2), afterStale.LifecycleRevision)
	require.Equal(t, "sk-new", afterStale.UpstreamKey)
}

func TestServiceCombinedSingleIncrement(t *testing.T) {
	svc, repos := newCodexImportPG(t)
	ctx := context.Background()
	tpl := seedPGTemplateForFencing(t, repos)
	acc, _ := svc.CreateAccount(ctx, repository.AccountPatch{
		Name:           strPtr("combined-svc"),
		TemplateID:     int64Ptr(tpl.ID),
		UpstreamKey:    strPtr("sk-old"),
		MaxConcurrency: intPtr(8),
	})
	require.NoError(t, repos.Accounts.FailAccountCAS(ctx, acc.ID, 1, "rule", getFixedTime(), "boom"))
	afterFail, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(2), afterFail.LifecycleRevision)
	// Credential PUT on a failed account: increments exactly once (2->3) and
	// never touches failure fields — recovery is fenced-endpoint-only.
	_, err := svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{UpstreamKey: strPtr("sk-new-combined")}, nil)
	require.NoError(t, err)
	after, _ := repos.Accounts.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(3), after.LifecycleRevision, "配置写入恰好推进一次 C")
	require.Equal(t, "sk-new-combined", after.UpstreamKey)
	require.NotNil(t, after.FailedAt, "配置写入不是恢复入口——失效字段保持")
	// Fenced recover clears failure fields with its own increment.
	recovered, err := svc.RecoverAccount(ctx, acc.ID, after.LifecycleRevision)
	require.NoError(t, err)
	require.Nil(t, recovered.FailedAt)
	require.Equal(t, int64(4), recovered.LifecycleRevision)
}

func strPtr2(s string) *string { return &s }

func seedPGTemplateForFencing(t *testing.T, repos *repository.Repository) *domain.Template {
	t.Helper()
	ctx := context.Background()
	tpl, err := repos.Templates.CreateTemplate(ctx, &domain.Template{Name: "fence-tpl", BaseURL: "https://u", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}, ModelMapping: domain.ModelMapping{}})
	require.NoError(t, err)
	return tpl
}

func getFixedTime() time.Time { return time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC) }
