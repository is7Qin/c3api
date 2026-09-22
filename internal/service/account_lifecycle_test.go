// SPDX-License-Identifier: AGPL-3.0-or-later
package service

import (
	"context"
	"errors"
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
	_, err = svc.CreateAccount(context.Background(), repository.AccountPatch{Name: strPtr("a"), TemplateID: int64Ptr(1), UpstreamKey: strPtr("sk-x"), UpstreamCostMultiplierBp: intPtr(-1)})
	require.ErrorIs(t, err, ErrInvalidInput)

	// invalid domain -> invalid
	bad := "not a domain!"
	_, err = svc.CreateAccount(context.Background(), repository.AccountPatch{Name: strPtr("a2"), TemplateID: int64Ptr(1), UpstreamKey: strPtr("sk-x"), CacheDomain: &bad})
	require.ErrorIs(t, err, ErrInvalidInput)

	// valid shared domain
	good := "shared.example.com"
	acc, err := svc.CreateAccount(context.Background(), repository.AccountPatch{Name: strPtr("a3"), TemplateID: int64Ptr(1), UpstreamKey: strPtr("sk-x"), CacheDomain: &good, UpstreamCostMultiplierBp: intPtr(25000)})
	require.NoError(t, err)
	require.Equal(t, 25000, acc.UpstreamCostMultiplierBp)

	// empty domain (nil) allowed - private
	acc2, err := svc.CreateAccount(context.Background(), repository.AccountPatch{Name: strPtr("a4"), TemplateID: int64Ptr(1), UpstreamKey: strPtr("sk-x")})
	require.NoError(t, err)
	require.NotNil(t, acc2)

	// batch patch validation: negative cost
	costNeg := -5
	_, err = svc.UpdateAccountsBatch(context.Background(), []int64{acc.ID}, repository.AccountPatch{UpstreamCostMultiplierBp: &costNeg})
	require.ErrorIs(t, err, ErrInvalidInput)

	// batch invalid domain
	badDom := "bad_domain!"
	_, err = svc.UpdateAccountsBatch(context.Background(), []int64{acc.ID}, repository.AccountPatch{CacheDomain: &badDom})
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
	res, err := svc.ImportCodexOAuthAccounts(context.Background(), items, &tplOauth.ID, nil, domain.CodexImportConfig{})
	require.NoError(t, err)
	require.Equal(t, 1, res.Imported)
}

// probeCall 记录 recover→PROBING 健康写入（recover-side prober 契约面）。
type probeCall struct {
	accountID int64
	revision  int64
}

// fakeRecoverProber 实现 RecoverProber（scheduler.RuntimeHealth.SetProbing 同签名）。
type fakeRecoverProber struct {
	calls []probeCall
	err   error
}

func (p *fakeRecoverProber) SetProbing(_ context.Context, accountID, identityRevision int64) error {
	p.calls = append(p.calls, probeCall{accountID, identityRevision})
	return p.err
}

// fakeRecoverLatch latch 显式释放记录面。
type fakeRecoverLatch struct{ cleared []int64 }

func (l *fakeRecoverLatch) Clear(accountID int64) { l.cleared = append(l.cleared, accountID) }

// fakeRecoverHealthClear 健康记录显式清除记录面。
type fakeRecoverHealthClear struct {
	cleared []int64
	err     error
}

func (c *fakeRecoverHealthClear) ClearAccount(_ context.Context, accountID int64) (int64, error) {
	if c.err != nil {
		return 0, c.err
	}
	c.cleared = append(c.cleared, accountID)
	return 1, nil
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
	latch := &fakeRecoverLatch{}
	healthClear := &fakeRecoverHealthClear{}
	inv := &invRecorder{}
	svc := &Service{store: fs, inv: inv, recoverProber: prober, recoverLatch: latch, recoverHealthClear: healthClear}

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
	require.Equal(t, []int64{acc.ID}, latch.cleared, "recover 必须显式释放 latch")
	require.Equal(t, []int64{acc.ID}, healthClear.cleared, "recover 必须显式清除该账号健康记录")
	require.Len(t, inv.calls, 1, "recover 后必须触发组级失效")

	// stale revision（重放同一 expected_revision）→ 409，不再写 PROBING
	_, err = svc.RecoverAccount(ctx, acc.ID, 5)
	require.ErrorIs(t, err, ErrConflict)
	require.Len(t, prober.calls, 1)
	require.Len(t, latch.cleared, 1, "409 不得释放 latch")
	require.Len(t, healthClear.cleared, 1, "409 不得清除健康记录")

	_, err = svc.RecoverAccount(ctx, 999, 1)
	require.ErrorIs(t, err, ErrNotFound)
}

// TestRecoverAccountProbingWriteFailureIsBestEffort 钉住 PROBING 写失败的语义：
// 持久恢复（CAS 清失效 + C+1）已提交，故健康写入失败**不**回滚、不上抛，只记
// Warn——探针环由同步周期兜底。回滚已提交的 CAS 会让管理员看到"恢复失败"而实际
// 已恢复，语义更差；反过来，latch 释放与健康清除必须仍然执行（否则账号即使恢复
// 也不可选）。
func TestRecoverAccountProbingWriteFailureIsBestEffort(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	acc := seedLifecycleAccount(t, fs)
	prober := &fakeRecoverProber{err: errors.New("redis down")}
	latch := &fakeRecoverLatch{}
	healthClear := &fakeRecoverHealthClear{}
	inv := &invRecorder{}
	svc := &Service{store: fs, inv: inv, recoverProber: prober, recoverLatch: latch, recoverHealthClear: healthClear}

	got, err := svc.RecoverAccount(ctx, acc.ID, 5)
	require.NoError(t, err, "已提交的持久恢复不得因健康写入失败而上抛")
	require.Nil(t, got.FailedAt)
	require.Equal(t, int64(6), got.LifecycleRevision, "CAS 已提交，C 必须推进")
	require.Equal(t, []int64{acc.ID}, latch.cleared, "健康写入失败不得跳过 latch 释放")
	require.Equal(t, []int64{acc.ID}, healthClear.cleared)
	require.Len(t, inv.calls, 1, "恢复后的组级失效与健康写入成败无关")
}

// TestRecoverAccountWithoutClearers 未装配 latch/健康清除面时 recover 仍完成持久
// 恢复（nil = 跳过，不阻断）。
func TestRecoverAccountWithoutClearers(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	acc := seedLifecycleAccount(t, fs)
	svc := &Service{store: fs, inv: &invRecorder{}}
	got, err := svc.RecoverAccount(ctx, acc.ID, 5)
	require.NoError(t, err)
	require.Nil(t, got.FailedAt)
	require.Equal(t, int64(6), got.LifecycleRevision)
}

// TestPatchAccountEnabled 账号启用/禁用经唯一写点：落值 + 无条件推进 C；
// disable 不清失效字段（失效恢复唯一入口 = RecoverAccount）。
func TestPatchAccountEnabled(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	acc := seedLifecycleAccount(t, fs)
	svc := &Service{store: fs, inv: &invRecorder{}}

	off := false
	got, err := svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{Enabled: &off}, nil)
	require.NoError(t, err)
	require.False(t, got.Enabled)
	require.Equal(t, int64(6), got.LifecycleRevision, "配置写入无条件推进 C")
	require.NotNil(t, got.FailedAt, "disable 不清失效字段")

	on := true
	got, err = svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{Enabled: &on}, nil)
	require.NoError(t, err)
	require.True(t, got.Enabled)
	require.Equal(t, int64(7), got.LifecycleRevision)
}

// TestPatchAccountCostMultiplier 采购倍率经唯一写点：合法 bp 落库 + 推进 C；
// 越界（<0 / >×10）→ ErrInvalidInput。
func TestPatchAccountCostMultiplier(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	acc := seedLifecycleAccount(t, fs)
	svc := &Service{store: fs, inv: &invRecorder{}}

	bp := 15000
	got, err := svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{UpstreamCostMultiplierBp: &bp}, nil)
	require.NoError(t, err)
	require.Equal(t, 15000, got.UpstreamCostMultiplierBp)
	require.Equal(t, int64(6), got.LifecycleRevision)

	neg := -1
	_, err = svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{UpstreamCostMultiplierBp: &neg}, nil)
	require.ErrorIs(t, err, ErrInvalidInput)
	over := 100001
	_, err = svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{UpstreamCostMultiplierBp: &over}, nil)
	require.ErrorIs(t, err, ErrInvalidInput)
}

// TestPatchAccountCacheDomain 缓存域经唯一写点：设置 + 推进 C；空串 = 清空
// （回账号私有域）；非法域 → ErrInvalidInput。
func TestPatchAccountCacheDomain(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	acc := seedLifecycleAccount(t, fs)
	svc := &Service{store: fs, inv: &invRecorder{}}

	other := "other.example.com"
	got, err := svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{CacheDomain: &other}, nil)
	require.NoError(t, err)
	require.Equal(t, other, *got.CacheDomain)
	require.Equal(t, int64(6), got.LifecycleRevision)

	empty := ""
	got, err = svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{CacheDomain: &empty}, nil)
	require.NoError(t, err)
	require.Nil(t, got.CacheDomain, "空串 = 清空回私有域")
	require.Equal(t, int64(7), got.LifecycleRevision)

	bad := "BAD domain!"
	_, err = svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{CacheDomain: &bad}, nil)
	require.ErrorIs(t, err, ErrInvalidInput)
}

// TestPatchAccountRejectsEmptyPatch 空补丁（无任何字段）→ 400：写面不接受无字段
// 请求（否则等于一次纯 C 推进的空写）。判定在 service 收口，handler 不重复。
func TestPatchAccountRejectsEmptyPatch(t *testing.T) {
	ctx := context.Background()
	fs := newFakeStore()
	acc := seedLifecycleAccount(t, fs)
	svc := &Service{store: fs, inv: &invRecorder{}}
	_, err := svc.PatchAccount(ctx, acc.ID, repository.AccountPatch{}, nil)
	require.ErrorIs(t, err, ErrInvalidInput)
}
