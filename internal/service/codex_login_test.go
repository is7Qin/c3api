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
	"github.com/is7qin/c3api/internal/repository"
)

// —— codex 设备码登录编排（无状态：发起 → 前端点完成 → 单次轮询 + 落库） ——

// fakeCodexLoginProvider 脚本化协议面：Start 返回固定设备码；Poll 按配置返回。
type fakeCodexLoginProvider struct {
	startErr error
	code     domain.CodexDeviceCode

	status   domain.CodexLoginStatus
	tokens   *domain.CodexLoginTokens
	pollErr  error
	pollHits int
	lastAuth string
	lastUser string
}

func (f *fakeCodexLoginProvider) StartDeviceFlow(context.Context) (*domain.CodexDeviceCode, error) {
	if f.startErr != nil {
		return nil, f.startErr
	}
	code := f.code
	return &code, nil
}

func (f *fakeCodexLoginProvider) PollDeviceFlow(_ context.Context, authID, userCode string) (domain.CodexLoginStatus, *domain.CodexLoginTokens, error) {
	f.pollHits++
	f.lastAuth, f.lastUser = authID, userCode
	if f.pollErr != nil {
		return "", nil, f.pollErr
	}
	return f.status, f.tokens, nil
}

// codexLoginFixture 组装模板/组/服务 + fake 协议面。
func codexLoginFixture(t *testing.T) (*Service, *fakeStore, *fakeCodexLoginProvider) {
	t.Helper()
	store := newFakeStore()
	store.tpls[1] = &domain.Template{ID: 1, Name: "t", CredentialType: credential.TypeCodexOAuth}
	store.tpls[2] = &domain.Template{ID: 2, Name: "pat", CredentialType: credential.TypeCodexPAT}
	store.groups[7] = &domain.Group{ID: 7, Name: "g", Visibility: domain.GroupVisibilityPublic}
	provider := &fakeCodexLoginProvider{
		code: domain.CodexDeviceCode{
			AuthID: "auth-1", UserCode: "ABCD-EFGHI", VerificationURL: "https://auth.example/device",
			Interval: 7 * time.Second, ExpiresIn: 15 * time.Minute,
		},
	}
	svc := New(Deps{Store: store, Invalidate: &invRecorder{}, EmailCodeStore: testEmailCodes, CodexLogin: provider})
	return svc, store, provider
}

// TestStartCodexOAuthDeviceLogin 发起：透出展示面与完成凭据，不轮询、不落库。
func TestStartCodexOAuthDeviceLogin(t *testing.T) {
	svc, store, provider := codexLoginFixture(t)

	ch, err := svc.StartCodexOAuthDeviceLogin(context.Background())
	require.NoError(t, err)
	require.Equal(t, "auth-1", ch.DeviceAuthID, "完成端点需要回传的凭据")
	require.Equal(t, "ABCD-EFGHI", ch.UserCode)
	require.Equal(t, "https://auth.example/device", ch.VerificationURL)
	require.Equal(t, 7, ch.IntervalSeconds)
	require.Equal(t, 900, ch.ExpiresInSeconds)
	require.Equal(t, 0, provider.pollHits, "发起阶段不应轮询")
	require.Empty(t, store.accs, "发起阶段不落任何本地状态")
	require.Empty(t, store.accExts)

	// interval 缺失（服务端不下发）→ 展示缺省 5s。
	provider.code.Interval = 0
	provider.code.ExpiresIn = 0
	ch, err = svc.StartCodexOAuthDeviceLogin(context.Background())
	require.NoError(t, err)
	require.Equal(t, 5, ch.IntervalSeconds)
	require.Equal(t, 0, ch.ExpiresInSeconds, "未下发即 0（前端不编造有效期）")

	// 上游失败 → ErrUpstream；未装配 → ErrUnavailable。
	provider.startErr = errors.New("boom")
	_, err = svc.StartCodexOAuthDeviceLogin(context.Background())
	require.ErrorIs(t, err, ErrUpstream)

	bare := New(Deps{Store: newFakeStore(), EmailCodeStore: testEmailCodes})
	_, err = bare.StartCodexOAuthDeviceLogin(context.Background())
	require.ErrorIs(t, err, ErrUnavailable)
}

// TestCompleteCodexOAuthDeviceLoginPending pending 不是终态：不落库，可重复调用。
func TestCompleteCodexOAuthDeviceLoginPending(t *testing.T) {
	svc, store, provider := codexLoginFixture(t)
	provider.status = domain.CodexLoginPending

	res, err := svc.CompleteCodexOAuthDeviceLogin(context.Background(), "auth-1", "ABCD-EFGHI", 1, nil)
	require.NoError(t, err)
	require.Equal(t, domain.CodexLoginPending, res.Status)
	require.Equal(t, "auth-1", provider.lastAuth, "回传凭据直接用于轮询")
	require.Equal(t, "ABCD-EFGHI", provider.lastUser)
	require.Empty(t, store.accs)
}

// TestCompleteCodexOAuthDeviceLoginValidationBeforePoll 模板/分组校验必须**先于**
// 上游轮询：上游授权码一次性，先换码再拒绝会烧掉用户刚授权的凭据。
func TestCompleteCodexOAuthDeviceLoginValidationBeforePoll(t *testing.T) {
	ctx := context.Background()
	svc, store, provider := codexLoginFixture(t)
	provider.status = domain.CodexLoginAuthorized
	provider.tokens = &domain.CodexLoginTokens{RefreshToken: "rt", AccountID: "a", Email: "e@example.com"}

	_, err := svc.CompleteCodexOAuthDeviceLogin(ctx, "auth-1", "code", 2, nil) // codex-pat 模板
	require.ErrorIs(t, err, ErrInvalidInput)
	require.Equal(t, 0, provider.pollHits, "类型错配不得消费上游授权码")

	missing := int64(99)
	_, err = svc.CompleteCodexOAuthDeviceLogin(ctx, "auth-1", "code", 1, &missing)
	require.ErrorIs(t, err, ErrNotFound)
	require.Equal(t, 0, provider.pollHits, "分组不存在不得消费上游授权码")
	require.Empty(t, store.accs)

	_, err = svc.CompleteCodexOAuthDeviceLogin(ctx, "", "code", 1, nil)
	require.ErrorIs(t, err, ErrInvalidInput)
	_, err = svc.CompleteCodexOAuthDeviceLogin(ctx, "auth-1", "", 1, nil)
	require.ErrorIs(t, err, ErrInvalidInput)

	bare := New(Deps{Store: newFakeStore(), EmailCodeStore: testEmailCodes})
	_, err = bare.CompleteCodexOAuthDeviceLogin(ctx, "auth-1", "code", 1, nil)
	require.ErrorIs(t, err, ErrUnavailable)
}

// TestCompleteCodexOAuthDeviceLoginTerminalStates 过期/拒绝 → 状态枚举；上游故障
// → ErrUpstream（授权码可能仍有效，前端可重试）。
func TestCompleteCodexOAuthDeviceLoginTerminalStates(t *testing.T) {
	ctx := context.Background()
	svc, store, provider := codexLoginFixture(t)

	for _, want := range []domain.CodexLoginStatus{domain.CodexLoginExpired, domain.CodexLoginDenied} {
		provider.status = want
		res, err := svc.CompleteCodexOAuthDeviceLogin(ctx, "auth-1", "code", 1, nil)
		require.NoError(t, err)
		require.Equal(t, want, res.Status)
		require.Empty(t, store.accs)
	}

	provider.pollErr = errors.New("network down")
	_, err := svc.CompleteCodexOAuthDeviceLogin(ctx, "auth-1", "code", 1, nil)
	require.ErrorIs(t, err, ErrUpstream)
}

// TestCompleteCodexOAuthDeviceLoginImports 授权完成 → 复用导入核心（新建 imported /
// 同键 updated），并返回账号定位。
func TestCompleteCodexOAuthDeviceLoginImports(t *testing.T) {
	ctx := context.Background()
	svc, store, provider := codexLoginFixture(t)
	gid := int64(7)
	expires := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	provider.status = domain.CodexLoginAuthorized
	provider.tokens = &domain.CodexLoginTokens{
		AccessToken: "at-1", RefreshToken: "rt-1", AccountID: "acc-1", Email: "a@example.com", ExpiresAt: &expires,
	}

	res, err := svc.CompleteCodexOAuthDeviceLogin(ctx, "auth-1", "code", 1, &gid)
	require.NoError(t, err)
	require.Equal(t, domain.CodexLoginAuthorized, res.Status)
	require.True(t, res.Imported)
	require.NotZero(t, res.AccountID)
	require.Equal(t, "a@example.com", res.AccountName, "新建账号名 = codex_email")
	require.Equal(t, "a@example.com", res.Email)
	require.Equal(t, "acc-1", res.CodexAccountID)

	ext, err := store.FindAccountExtByCodexKey(ctx, "a@example.com", "acc-1")
	require.NoError(t, err)
	require.Equal(t, credential.TypeCodexOAuth, ext.CredentialType)
	require.Equal(t, "at-1", *ext.CodexOAuthToken)
	require.Equal(t, "rt-1", *ext.CodexOAuthRefreshToken)
	require.NotNil(t, ext.CodexOAuthExpiresAt)
	require.True(t, ext.CodexOAuthExpiresAt.Equal(expires))
	require.NotNil(t, ext.CodexIdentity)
	require.NotEmpty(t, ext.CodexIdentity.InstallationID)

	acc, err := store.GetAccount(ctx, ext.AccountID)
	require.NoError(t, err)
	gids, err := store.GetAccountGroups(ctx, acc.ID)
	require.NoError(t, err)
	require.Equal(t, []int64{gid}, gids, "归组沿用完成参数")
	require.Equal(t, 25, acc.MaxConcurrency, "导入面并发缺省 25")
	require.True(t, acc.Enabled, "缺省启用")
	require.Equal(t, int64(0), acc.SupplierUserID, "管理面建号归平台")

	// 同键再次完成（凭据更新路径）：updated、账号行 id 不变、身份沿用。
	provider.tokens = &domain.CodexLoginTokens{
		AccessToken: "at-2", RefreshToken: "rt-2", AccountID: "acc-1", Email: "a@example.com", ExpiresAt: &expires,
	}
	res2, err := svc.CompleteCodexOAuthDeviceLogin(ctx, "auth-1", "code", 1, &gid)
	require.NoError(t, err)
	require.False(t, res2.Imported, "同键 → 凭据更新")
	require.Equal(t, res.AccountID, res2.AccountID)

	ext, err = store.FindAccountExtByCodexKey(ctx, "a@example.com", "acc-1")
	require.NoError(t, err)
	require.Equal(t, "at-2", *ext.CodexOAuthToken)
}

// TestCompleteCodexOAuthDeviceLoginSupplierScope 供应商面完成：账号归属 = JWT 本人
// （作用域经 ctx 注入；无服务端会话，因此不存在跨用户越权面）。
func TestCompleteCodexOAuthDeviceLoginSupplierScope(t *testing.T) {
	svc, store, provider := codexLoginFixture(t)
	provider.status = domain.CodexLoginAuthorized
	provider.tokens = &domain.CodexLoginTokens{AccessToken: "at", RefreshToken: "rt", AccountID: "acc", Email: "s@example.com"}

	ctx := domain.WithAccountScope(context.Background(), domain.SupplierAccountScope(42))
	res, err := svc.CompleteCodexOAuthDeviceLogin(ctx, "auth-1", "code", 1, nil)
	require.NoError(t, err)
	require.Equal(t, domain.CodexLoginAuthorized, res.Status)

	ext, err := store.FindAccountExtByCodexKey(ctx, "s@example.com", "acc")
	require.NoError(t, err)
	acc, err := store.GetAccount(ctx, ext.AccountID)
	require.NoError(t, err)
	require.Equal(t, int64(42), acc.SupplierUserID, "供应商面落库归属恒本人")
}

// TestCompleteCodexOAuthDeviceLoginRejectsIncompleteTokens 设备码登录必须走与批量
// 导入**同一个**行校验：缺 token/refresh/account id 时既不新建账号，也**不得**
// 把既有账号的有效 refresh token 覆盖为空（凭据破坏）。
func TestCompleteCodexOAuthDeviceLoginRejectsIncompleteTokens(t *testing.T) {
	ctx := context.Background()
	svc, store, provider := codexLoginFixture(t)
	tplID := int64(1)

	// 先用批量导入路径预置一个可用账号（RT=rt-good）。
	_, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{{
		CodexEmail: "a@example.com", CodexAccountID: "acc-1",
		CodexOAuthToken: "at-good", CodexOAuthRefreshToken: "rt-good",
	}}, &tplID, nil, domain.CodexImportConfig{})
	require.NoError(t, err)

	provider.status = domain.CodexLoginAuthorized
	cases := map[string]*domain.CodexLoginTokens{
		"missing refresh": {AccessToken: "at-new", AccountID: "acc-1", Email: "a@example.com"},
		"missing access":  {RefreshToken: "rt-new", AccountID: "acc-1", Email: "a@example.com"},
		"missing account id": {
			AccessToken: "at-new", RefreshToken: "rt-new", Email: "a@example.com",
		},
		"invalid email": {AccessToken: "at-new", RefreshToken: "rt-new", AccountID: "acc-1", Email: "not-an-email"},
		"nil bundle":    nil,
	}
	for name, tokens := range cases {
		provider.tokens = tokens
		_, err := svc.CompleteCodexOAuthDeviceLogin(ctx, "auth-1", "code", 1, nil)
		require.ErrorIs(t, err, ErrUpstream, name)

		ext, err := store.FindAccountExtByCodexKey(ctx, "a@example.com", "acc-1")
		require.NoError(t, err, name)
		require.Equal(t, "at-good", *ext.CodexOAuthToken, name+": 既有凭据不得被覆盖")
		require.Equal(t, "rt-good", *ext.CodexOAuthRefreshToken, name+": 有效 RT 不得被空值覆盖")
	}
	require.Len(t, store.accs, 1, "校验失败不得新建账号")
}

// TestCompleteCodexOAuthDeviceLoginSupplierReload 供应商面完成登录后必须触发财务
// 视图换代（§4.6.3 发布屏障与写核心同源）；管理面不触发。
func TestCompleteCodexOAuthDeviceLoginSupplierReload(t *testing.T) {
	store := newFakeStore()
	store.tpls[1] = &domain.Template{ID: 1, Name: "t", CredentialType: credential.TypeCodexOAuth}
	provider := &fakeCodexLoginProvider{status: domain.CodexLoginAuthorized}
	reload := &fakeSupplierViewReloader{}
	svc := New(Deps{Store: store, Invalidate: &invRecorder{}, EmailCodeStore: testEmailCodes, CodexLogin: provider, SupplierViewReload: reload})

	provider.tokens = &domain.CodexLoginTokens{AccessToken: "at", RefreshToken: "rt", AccountID: "acc-1", Email: "a@example.com"}
	_, err := svc.CompleteCodexOAuthDeviceLogin(context.Background(), "auth-1", "code", 1, nil)
	require.NoError(t, err)
	require.Zero(t, reload.calls, "管理面不触发供应商视图换代")

	supplierCtx := domain.WithAccountScope(context.Background(), domain.SupplierAccountScope(42))
	provider.tokens = &domain.CodexLoginTokens{AccessToken: "at2", RefreshToken: "rt2", AccountID: "acc-2", Email: "s@example.com"}
	_, err = svc.CompleteCodexOAuthDeviceLogin(supplierCtx, "auth-1", "code", 1, nil)
	require.NoError(t, err)
	require.Equal(t, 1, reload.calls, "供应商面完成登录须触发财务视图换代")
}

// TestCompleteCodexOAuthDeviceLoginMissingClaims 缺邮箱/账号标识 → 登录成功但无法
// 建号：loud 失败，不落半成品账号。
func TestCompleteCodexOAuthDeviceLoginMissingClaims(t *testing.T) {
	svc, store, provider := codexLoginFixture(t)
	provider.status = domain.CodexLoginAuthorized
	provider.tokens = &domain.CodexLoginTokens{AccessToken: "at", RefreshToken: "rt", AccountID: "acc-1"}

	_, err := svc.CompleteCodexOAuthDeviceLogin(context.Background(), "auth-1", "code", 1, nil)
	require.ErrorIs(t, err, ErrUpstream)
	require.Empty(t, store.accs, "缺 claims 不得落账号")
	require.Empty(t, store.accExts)

	provider.tokens = nil
	_, err = svc.CompleteCodexOAuthDeviceLogin(context.Background(), "auth-1", "code", 1, nil)
	require.ErrorIs(t, err, ErrUpstream)
}

// errTxStore 写事务故障注入（Store 其余方法沿用 fakeStore）。
type errTxStore struct{ *fakeStore }

func (errTxStore) WithTx(ctx context.Context, _ func(repository.TxStore) error) error {
	return errors.New("db down")
}

// TestCompleteCodexOAuthDeviceLoginRowErrorClassification 行级落库失败要区分
// **冲突**（键/软删——可操作，409）与**存储故障**（500，细节只进日志）：上游
// 授权码已被消费，两者都要求重新登录，但状态码不能折叠成一个。
func TestCompleteCodexOAuthDeviceLoginRowErrorClassification(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	store.tpls[1] = &domain.Template{ID: 1, Name: "t", CredentialType: credential.TypeCodexOAuth}
	provider := &fakeCodexLoginProvider{status: domain.CodexLoginAuthorized,
		tokens: &domain.CodexLoginTokens{AccessToken: "at", RefreshToken: "rt", AccountID: "acc-c", Email: "c@example.com"}}
	svc := New(Deps{Store: store, Invalidate: &invRecorder{}, EmailCodeStore: testEmailCodes, CodexLogin: provider})

	// 首次成功（建立同键账号）
	_, err := svc.CompleteCodexOAuthDeviceLogin(ctx, "auth-1", "code", 1, nil)
	require.NoError(t, err)
	ext, err := store.FindAccountExtByCodexKey(ctx, "c@example.com", "acc-c")
	require.NoError(t, err)
	now := time.Now()
	store.mu.Lock()
	store.accs[ext.AccountID].DeletedAt = &now
	store.mu.Unlock()

	// 软删同键 → 冲突（WriteServiceErr 映射 409），不是 500。
	_, err = svc.CompleteCodexOAuthDeviceLogin(ctx, "auth-1", "code", 1, nil)
	require.ErrorIs(t, err, ErrConflict, "软删同键是可操作冲突：%v", err)
	require.NotContains(t, err.Error(), "db down")

	// 存储故障 → 非冲突（500 语义；文案只进日志，不回显给客户端）。
	broken := newFakeStore()
	broken.tpls[1] = &domain.Template{ID: 1, Name: "t", CredentialType: credential.TypeCodexOAuth}
	brokenSvc := New(Deps{Store: errTxStore{broken}, Invalidate: &invRecorder{}, EmailCodeStore: testEmailCodes, CodexLogin: provider})
	_, err = brokenSvc.CompleteCodexOAuthDeviceLogin(ctx, "auth-1", "code", 1, nil)
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrConflict)
}
