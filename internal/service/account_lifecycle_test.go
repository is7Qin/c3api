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

func TestAccountValidationCostCache(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}}
	// need template
	_, err := svc.CreateTemplate(context.Background(), &domain.Template{Name: "t", BaseURL: "https://u", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}})
	require.NoError(t, err)

	// negative multiplier -> invalid
	_, err = svc.CreateAccount(context.Background(), &domain.Account{Name: "a", TemplateID: 1, UpstreamKey: "sk-x", UpstreamCostMultiplierBp: -1})
	require.ErrorIs(t, err, ErrInvalidInput)

	// invalid domain -> invalid
	bad := "not a domain!"
	_, err = svc.CreateAccount(context.Background(), &domain.Account{Name: "a2", TemplateID: 1, UpstreamKey: "sk-x", CacheDomain: &bad})
	require.ErrorIs(t, err, ErrInvalidInput)

	// valid shared domain
	good := "shared.example.com"
	acc, err := svc.CreateAccount(context.Background(), &domain.Account{Name: "a3", TemplateID: 1, UpstreamKey: "sk-x", CacheDomain: &good, UpstreamCostMultiplierBp: 25000})
	require.NoError(t, err)
	require.Equal(t, 25000, acc.UpstreamCostMultiplierBp)

	// empty domain (nil) allowed - private
	acc2, err := svc.CreateAccount(context.Background(), &domain.Account{Name: "a4", TemplateID: 1, UpstreamKey: "sk-x"})
	require.NoError(t, err)
	require.NotNil(t, acc2)

	// batch patch validation: negative cost
	costNeg := -5
	err = svc.UpdateAccountsBatch(context.Background(), []int64{acc.ID}, repository.AccountPatch{UpstreamCostMultiplierBp: &costNeg})
	require.ErrorIs(t, err, ErrInvalidInput)

	// batch invalid domain
	badDom := "bad_domain!"
	err = svc.UpdateAccountsBatch(context.Background(), []int64{acc.ID}, repository.AccountPatch{CacheDomain: &badDom})
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestAccountValidationLifecycle(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}}
	_, err := svc.CreateTemplate(context.Background(), &domain.Template{Name: "t", BaseURL: "https://u", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}})
	require.NoError(t, err)
	acc, err := svc.CreateAccount(context.Background(), &domain.Account{Name: "a", TemplateID: 1, UpstreamKey: "sk-x"})
	require.NoError(t, err)
	// lifecycle revision negative invalid
	acc.LifecycleRevision = -1
	_, err = svc.UpdateAccount(context.Background(), acc)
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestCodexImport(t *testing.T) {
	// Verify the codex import path remains functional.
	fs := newFakeStore()
	svc := &Service{store: fs, inv: &invRecorder{}}
	// need templates of codex type
	tplOauth, err := svc.CreateTemplate(context.Background(), &domain.Template{Name: "tpl-oauth", CredentialType: "codex-oauth", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}})
	require.NoError(t, err)
	// import oauth
	items := []domain.CodexOAuthImportItem{{CodexEmail: "a@example.com", CodexAccountID: "acc1", CodexOAuthToken: "tok", CodexOAuthRefreshToken: "rt"}}
	res, err := svc.ImportCodexOAuthAccounts(context.Background(), items, &tplOauth.ID, nil)
	require.NoError(t, err)
	require.Equal(t, 1, res.Imported)
}

// probeCall 记录 recover→PROBING 健康写入（recover-side prober 契约面）。
type probeCall struct {
	accountID int64
	revision  int64
}

// fakeRecoverProber 实现 RecoverProber（scheduler.RuntimeHealth.SetProbing 同签名）。
type fakeRecoverProber struct{ calls []probeCall }

func (p *fakeRecoverProber) SetProbing(_ context.Context, accountID, identityRevision int64) error {
	p.calls = append(p.calls, probeCall{accountID, identityRevision})
	return nil
}

// seedLifecycleAccount 建模板 + 一个 rev=5 已失效账号（failed_at/failure_source/
// last_error 置位，enabled=true，倍率 25000，共享缓存域）。
func seedLifecycleAccount(t *testing.T, fs *fakeStore) *domain.Account {
	t.Helper()
	ctx := context.Background()
	_, err := fs.CreateTemplate(ctx, &domain.Template{ID: 1, Name: "tpl", CredentialType: "api_key"})
	require.NoError(t, err)
	failed := time.Date(2026, 8, 30, 10, 0, 0, 0, time.UTC)
	src := "rule"
	reason := "fatal"
	dom := "shared.example.com"
	acc, err := fs.CreateAccount(ctx, &domain.Account{
		Name: "a", TemplateID: 1, UpstreamKey: "sk-a",
		Enabled: true, FailedAt: &failed, FailureSource: &src, LastError: &reason,
		LifecycleRevision: 5, IdentityRevision: 3, UpstreamCostMultiplierBp: 25000, CacheDomain: &dom,
	})
	require.NoError(t, err)
	return acc
}

// TestRecoverAccountFenced recover 校验 revision：正确 revision → 清失效三字段 +
// revision+1 + 新 revision 写 PROBING + 组级失效；stale revision → ErrConflict（409）
// 且不清失效；缺 id → ErrNotFound。
func TestRecoverAccountFenced(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	acc := seedLifecycleAccount(t, fs)
	prober := &fakeRecoverProber{}
	inv := &invRecorder{}
	svc := &Service{store: fs, inv: inv, recoverProber: prober}

	got, err := svc.RecoverAccount(ctx, acc.ID, 5)
	require.NoError(t, err)
	require.Nil(t, got.FailedAt)
	require.Nil(t, got.FailureSource)
	require.Nil(t, got.LastError)
	require.Equal(t, int64(6), got.LifecycleRevision)
	// PROBING 必须落在**身份代际 K**（=3），不是 C（CAS 后 =6）：健康记录按 K
	// 隔离，EffectiveState 以 K 查询；传 C 会让该记录永不被命中。取 3≠5 是刻意的
	// ——若有人把 SetProbing 改回传 C，断言立即失败。
	require.Equal(t, []probeCall{{acc.ID, 3}}, prober.calls, "PROBING 必须以 K 落键（不是 C）")
	require.Len(t, inv.calls, 1, "recover 后必须触发组级失效")

	// stale revision（重放同一 expected_revision）→ 409，不再写 PROBING
	_, err = svc.RecoverAccount(ctx, acc.ID, 5)
	require.ErrorIs(t, err, ErrConflict)
	require.Len(t, prober.calls, 1)

	_, err = svc.RecoverAccount(ctx, 999, 1)
	require.ErrorIs(t, err, ErrNotFound)
}

// TestSetAccountEnabledFenced enabled 切换 CAS fencing：正确 revision 翻转 +1；
// enable 不清失效字段；stale → ErrConflict。
func TestSetAccountEnabledFenced(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	acc := seedLifecycleAccount(t, fs)
	svc := &Service{store: fs, inv: &invRecorder{}}

	got, err := svc.SetAccountEnabled(ctx, acc.ID, 5, false)
	require.NoError(t, err)
	require.False(t, got.Enabled)
	require.Equal(t, int64(6), got.LifecycleRevision)
	require.NotNil(t, got.FailedAt, "disable 不清失效字段")

	_, err = svc.SetAccountEnabled(ctx, acc.ID, 5, true)
	require.ErrorIs(t, err, ErrConflict)
}

// TestUpdateAccountCostMultiplierFenced 采购倍率 CAS：合法 bp 落库 +1；越界
// （<0 / >10×）→ ErrInvalidInput；stale → ErrConflict。
func TestUpdateAccountCostMultiplierFenced(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	acc := seedLifecycleAccount(t, fs)
	svc := &Service{store: fs, inv: &invRecorder{}}

	got, err := svc.UpdateAccountCostMultiplier(ctx, acc.ID, 5, 15000)
	require.NoError(t, err)
	require.Equal(t, 15000, got.UpstreamCostMultiplierBp)
	require.Equal(t, int64(6), got.LifecycleRevision)

	_, err = svc.UpdateAccountCostMultiplier(ctx, acc.ID, 6, -1)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.UpdateAccountCostMultiplier(ctx, acc.ID, 6, 100001)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.UpdateAccountCostMultiplier(ctx, acc.ID, 5, 15000)
	require.ErrorIs(t, err, ErrConflict)
}

// TestUpdateAccountCacheDomainFenced 缓存域 CAS：设置/清空（nil）各 +1；非法域
// → ErrInvalidInput；stale → ErrConflict。
func TestUpdateAccountCacheDomainFenced(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	acc := seedLifecycleAccount(t, fs)
	svc := &Service{store: fs, inv: &invRecorder{}}

	other := "other.example.com"
	got, err := svc.UpdateAccountCacheDomain(ctx, acc.ID, 5, &other)
	require.NoError(t, err)
	require.Equal(t, other, *got.CacheDomain)
	require.Equal(t, int64(6), got.LifecycleRevision)

	got, err = svc.UpdateAccountCacheDomain(ctx, acc.ID, 6, nil)
	require.NoError(t, err)
	require.Nil(t, got.CacheDomain, "nil = 清空回私有域")
	require.Equal(t, int64(7), got.LifecycleRevision)

	bad := "BAD domain!"
	_, err = svc.UpdateAccountCacheDomain(ctx, acc.ID, 7, &bad)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.UpdateAccountCacheDomain(ctx, acc.ID, 6, &other)
	require.ErrorIs(t, err, ErrConflict)
}

// TestUpdateAccountPreservesLifecycleFields PUT 全量更新不得 clobber 生命周期
// 独占字段（enabled/采购倍率/缓存域/revision 归 fenced 端点所有）——handler
// accountFromBody 不携带这些字段，零值直落 repo 即静默清空（回归钉）。
func TestUpdateAccountPreservesLifecycleFields(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	_, err := fs.CreateTemplate(ctx, &domain.Template{ID: 1, Name: "tpl", CredentialType: "api_key"})
	require.NoError(t, err)
	dom := "shared.example.com"
	acc, err := fs.CreateAccount(ctx, &domain.Account{
		Name: "a", TemplateID: 1, UpstreamKey: "sk-a",
		Enabled: true, LifecycleRevision: 5, UpstreamCostMultiplierBp: 25000, CacheDomain: &dom,
	})
	require.NoError(t, err)
	svc := &Service{store: fs, inv: &invRecorder{}}

	// 模拟 handler 转换产物：仅路由字段，生命周期字段全零值
	body := &domain.Account{ID: acc.ID, Name: "renamed", TemplateID: 1,
		UpstreamKey: "sk-a", MaxConcurrency: 4}
	_, err = svc.UpdateAccount(ctx, body)
	require.NoError(t, err)

	got, err := svc.GetAccount(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, "renamed", got.Name, "路由字段正常更新")
	require.True(t, got.Enabled, "PUT 不得禁用账号")
	require.Equal(t, 25000, got.UpstreamCostMultiplierBp, "PUT 不得重置采购倍率")
	require.NotNil(t, got.CacheDomain)
	require.Equal(t, "shared.example.com", *got.CacheDomain, "PUT 不得清空缓存域")
	require.Equal(t, int64(5), got.LifecycleRevision, "非生命周期写不增 revision")
}
