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
	f.accs[2] = &domain.Account{ID: 2, Enabled: true, IdentityRevision: 3}
	f.accExts[2] = &domain.AccountExt{
		AccountID: 2, CredentialType: credential.TypeCodexOAuth,
		CodexOAuthToken: new("at"), CodexOAuthRefreshToken: new("rt"), CodexOAuthExpiresAt: &exp,
	}
	cred, err = svc.AccountUsageCredential(ctx, 2)
	require.NoError(t, err)
	require.NotNil(t, cred)
	require.Equal(t, int64(2), cred.AccountID)
	require.Equal(t, "at", cred.OAuthToken)
	require.Equal(t, "rt", cred.OAuthRefreshToken, "oauth 列组派生 cred")
	require.Equal(t, int64(3), cred.IdentityRevision, "来源 K 从账号行补齐（ext 行不带它）")

	// codex-pat：pat 列组派生 cred
	f.accs[3] = &domain.Account{ID: 3, Enabled: true, IdentityRevision: 1}
	f.accExts[3] = &domain.AccountExt{AccountID: 3, CredentialType: credential.TypeCodexPAT, CodexPATKey: new("pat-x")}
	cred, err = svc.AccountUsageCredential(ctx, 3)
	require.NoError(t, err)
	require.NotNil(t, cred)
	require.Equal(t, "pat-x", cred.PATKey, "pat 列组派生 cred")

	// ext 行存在但凭据全空 → nil/nil（防御分支）
	f.accs[4] = &domain.Account{ID: 4, Enabled: true, IdentityRevision: 1}
	f.accExts[4] = &domain.AccountExt{AccountID: 4, CredentialType: credential.TypeCodexPAT}
	cred, err = svc.AccountUsageCredential(ctx, 4)
	require.NoError(t, err)
	require.Nil(t, cred, "凭据全空 → 无上游能力")
}

// TestUsageCredentialCarriesIdentityRevision 额度面（无 Selection）的凭据必须带**来源
// 身份代际 K**：ext 行不带 `identity_revision`（它在父行），K=0 的凭据建出的条目上
// 任何轮转回写都会被存储围栏当成陈旧事件丢掉（只 Warn、不落库）。
//
// 取 K 不违反 §2.5：它是**第二条 owner-scoped 单读**的投影（`GetAccount` 把归属谓词
// AND 进自己的 WHERE），本函数不做任何归属比较；账号行读不到 = 越域/刚被删除 ⇒
// 与 ext 缺失同处理（无上游能力），不退化成 K=0。
func TestUsageCredentialCarriesIdentityRevision(t *testing.T) {
	svc := &Service{store: newFakeStore()}
	f := svc.store.(*fakeStore)
	f.accs[11] = &domain.Account{ID: 11, Enabled: true, IdentityRevision: 41}
	f.accExts[11] = &domain.AccountExt{
		AccountID: 11, CredentialType: credential.TypeCodexOAuth,
		CodexOAuthToken: new("at-11"), CodexOAuthRefreshToken: new("rt-11"),
	}

	cred, err := svc.AccountUsageCredential(context.Background(), 11)
	require.NoError(t, err)
	require.NotNil(t, cred)
	require.Equal(t, int64(41), cred.IdentityRevision, "凭据携带父行 identity_revision")

	// 账号行不可见（越域/刚被删除）⇒ 没有可用的来源版本 ⇒ 如实报"无上游能力"。
	f.accs[12] = &domain.Account{ID: 12, Enabled: true, IdentityRevision: 5}
	f.accExts[12] = &domain.AccountExt{
		AccountID: 12, CredentialType: credential.TypeCodexOAuth,
		CodexOAuthToken: new("at-12"), CodexOAuthRefreshToken: new("rt-12"),
	}
	delete(f.accs, 12)
	cred, err = svc.AccountUsageCredential(context.Background(), 12)
	require.NoError(t, err)
	require.Nil(t, cred, "取不到账号行 ⇒ nil/nil（不得退化成 K=0 的凭据）")
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

// TestUsageCredentialReadsCredentialAndRevisionInOneRead 额度面的凭据内容与父行 K
// 必须**一次**读出：分两次读（先 ext 取内容、再 accounts 取 K）时，两条查询之间可能
// 夹进一次管理面凭据替换 ⇒ 得到"旧内容 + 新 K"的凭据，它建出的条目上旧 Auth 的轮转/
// 判死就能通过新身份的围栏。
//
// 判据是 store 读次数：一次组装只允许发生一次读（真实 repo 是一条 JOIN SQL，越域/
// 缺失同样在这一条语句里判掉）。
func TestUsageCredentialReadsCredentialAndRevisionInOneRead(t *testing.T) {
	svc := &Service{store: newFakeStore()}
	f := svc.store.(*fakeStore)
	f.accs[21] = &domain.Account{ID: 21, Enabled: true, IdentityRevision: 77}
	f.accExts[21] = &domain.AccountExt{
		AccountID: 21, CredentialType: credential.TypeCodexOAuth,
		CodexOAuthToken: new("at-21"), CodexOAuthRefreshToken: new("rt-21"),
	}

	before := f.usageCredReads
	cred, err := svc.AccountUsageCredential(context.Background(), 21)
	require.NoError(t, err)
	require.NotNil(t, cred)
	require.Equal(t, 1, f.usageCredReads-before,
		"凭据与父行 K 必须一次读出（分两次读会让这里是 2：\"旧内容 + 新 K\" 的窗口回来）")
	require.Equal(t, int64(77), cred.IdentityRevision, "同一次读出的 K")
	require.Equal(t, "rt-21", cred.OAuthRefreshToken)
}

// TestAccountUsageFrozen failed_at 判定（Q1 冻结范围）+ 失效来源透出（Q2）：
// failed_at 置位 → frozen=true 且透出 failure_source；未失效 / enabled=false
// （手动禁用）→ false；缺 id → false 不报错（无对象可冻结）。
func TestAccountUsageFrozen(t *testing.T) {
	svc := &Service{store: newFakeStore()}
	ctx := context.Background()
	f := svc.store.(*fakeStore)

	failedAt := time.Now()
	sdkSrc := domain.FailureSourceSDK
	ruleSrc := domain.FailureSourceRule
	f.accs[1] = &domain.Account{ID: 1, Enabled: true, FailedAt: &failedAt, FailureSource: &sdkSrc}
	f.accs[2] = &domain.Account{ID: 2, Enabled: true}
	f.accs[3] = &domain.Account{ID: 3, Enabled: false} // 手动禁用：语义分离，不冻结
	f.accs[4] = &domain.Account{ID: 4, Enabled: true, FailedAt: &failedAt, FailureSource: &ruleSrc}
	f.accs[5] = &domain.Account{ID: 5, Enabled: true, FailedAt: &failedAt} // 旧路径：无来源

	frozen, source, err := svc.AccountUsageFrozen(ctx, 1)
	require.NoError(t, err)
	require.True(t, frozen, "failed_at 置位 → 冻结")
	require.Equal(t, domain.FailureSourceSDK, source, "透出失效来源（SDK 判死）")

	frozen, source, err = svc.AccountUsageFrozen(ctx, 2)
	require.NoError(t, err)
	require.False(t, frozen, "未失效 → 不冻结")
	require.Empty(t, source)

	frozen, _, err = svc.AccountUsageFrozen(ctx, 3)
	require.NoError(t, err)
	require.False(t, frozen, "enabled=false（手动禁用）不冻结——只有 failed_at 冻结")

	frozen, source, err = svc.AccountUsageFrozen(ctx, 4)
	require.NoError(t, err)
	require.True(t, frozen)
	require.Equal(t, domain.FailureSourceRule, source, "透出失效来源（规则判死）")

	frozen, source, err = svc.AccountUsageFrozen(ctx, 5)
	require.NoError(t, err)
	require.True(t, frozen)
	require.Empty(t, source, "无来源 → 空串（调用方按不标处理）")

	frozen, source, err = svc.AccountUsageFrozen(ctx, 999)
	require.NoError(t, err, "缺 id → 无对象可冻结，不报错")
	require.False(t, frozen)
	require.Empty(t, source)
}
