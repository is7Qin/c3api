// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

// **来源身份版本（K）贯通**的判别用例（§3.3b-2）：fatal / 构造失败 / 轮转回写三条
// 事件回流都必须带着"产生它的那一版 K"走到持久化与内存动作，不得在施加前回读
// "当前"。都是确定性交错（不依赖并发概率，也不用 sleep）。

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// oauthCredWithRev 带来源 K 的 oauth 凭据（生产里由 Selection.Credential 给出）。
func oauthCredWithRev(accountID int64, at, rt string, revision int64) *domain.AccountCredential {
	c := oauthCred(accountID, at, rt)
	c.IdentityRevision = revision
	return c
}

// patCredWithRev 带来源 K 的 PAT 凭据。
func patCredWithRev(accountID int64, pat string, revision int64) *domain.AccountCredential {
	return &domain.AccountCredential{AccountID: accountID, PATKey: pat, IdentityRevision: revision}
}

// TestCodexFatalAfterIdentitySwapDoesNotDisableAccount **C-M1 的屏障用例**：旧条目
// 的来源检查通过之后、失效链首次读库之前，管理面把身份推进到 K2（表指针不变——
// 期间没有任何请求重建条目），旧 fatal 不得禁用新身份。
//
//	请求 A: Prepare(K=1)  → 条目 e1（srcRev=1）
//	管理面换凭据          → 库里 K=2（无新 Prepare ⇒ 表里仍是 e1，isCurrent 仍为真）
//	A: 收到判死帧         → 上报必须携带**来源 K=1**
//
// 失效链以 K=1 作 expected：库中 K=2 ⇒ 陈旧事件（fail-closed），新身份保持可用。
func TestCodexFatalAfterIdentitySwapDoesNotDisableAccount(t *testing.T) {
	store := &configRevisionStore{acct: newCodexAccountForRetry(7, 1)}
	failer := &retryFakeFailer{}
	deps := FailureDeps{Store: store, Failer: failer, Latch: newRetryFakeLatch()}
	a := NewCodex(NewFailureHandler(deps), testEgressPool(nil), nil, RotationDeps{})

	prep, err := a.Prepare(oauthCredWithRev(7, "at-1", "rt-1", 1))
	require.NoError(t, err)
	require.Equal(t, int64(1), a.clients.Get(7).srcRev.Load(), "条目冻结来源 K")

	// 管理面完成身份写入：K 1→2。表里没有任何变化（没有新请求 Prepare）。
	store.acct.IdentityRevision = 2
	require.True(t, a.clients.isCurrent(prep.entry), "旧条目仍是表里的当前条目（这正是本地门挡不住的窗口）")

	fatal := &codexsdk.AuthPermanentlyRevokedError{Code: "token_invalidated"}
	a.FatalPrepared(prep, fatal)

	require.Nil(t, store.acct.FailedAt, "来源 K 的旧判决不得禁用已换身份的新凭据")
	require.Empty(t, failer.calls, "CAS 被围栏拒绝 ⇒ 内存摘除不得先发生")
	require.Empty(t, store.gotExpect, "身份已变 ⇒ 连 CAS 都不该尝试（围栏在读库校验处即拒绝）")
}

// TestHandleFailureDoesNotDisableBeforeCASCommits 顺序判据：内存/锁存动作必须
// **晚于** CAS 成功。CAS 恒定失败（stale）时不得留下"已摘除但库里没失效"的状态。
func TestHandleFailureDoesNotDisableBeforeCASCommits(t *testing.T) {
	store := &configRevisionStore{acct: newCodexAccountForRetry(7, 3), casErr: repository.ErrStaleIdentityRevision}
	failer := &retryFakeFailer{}
	latch := newRetryFakeLatch()
	deps := FailureDeps{Store: store, Failer: failer, Latch: latch}

	err := HandleFailure(context.Background(), deps, failEvent(7, 3, errors.New("fatal")))
	require.ErrorIs(t, err, ErrStaleFailureRevision)
	require.Empty(t, failer.calls, "先摘再判会让陈旧判决留下错误的运行时状态")
	require.Empty(t, latch.m, "锁存动作同样必须晚于 CAS")
}

// TestHandleFailureRejectsUnversionedEvent 来源版本未知的事件不得退化成"读当前 K"
// （那正是旧判决禁用新身份的成因）：拒绝账号级动作并报 ErrMissingExpectedRevision。
func TestHandleFailureRejectsUnversionedEvent(t *testing.T) {
	store := &configRevisionStore{acct: newCodexAccountForRetry(7, 5)}
	failer := &retryFakeFailer{}
	deps := FailureDeps{Store: store, Failer: failer, Latch: newRetryFakeLatch()}

	err := HandleFailure(context.Background(), deps, failEvent(7, 0, errors.New("fatal")))
	require.ErrorIs(t, err, ErrMissingExpectedRevision)
	require.Empty(t, store.gotExpect, "没有来源版本就没有可施加的账号级事实")
	require.Nil(t, store.acct.FailedAt)
	require.Empty(t, failer.calls)
}

// TestCodexStaleMalformedPreparePreservesCurrent **C-M2 的判别用例**（无需并发）：
// 先建有效 OAuth 条目，再 Prepare 同账号**缺 RT 的旧快照** ⇒ 当前条目保持、错误
// 照常上抛；构造失败不得升级成账号级事件。
func TestCodexStaleMalformedPreparePreservesCurrent(t *testing.T) {
	handler := &recordingHandler{}
	a := NewCodex(handler.add, testEgressPool(nil), nil, RotationDeps{})

	current, err := a.Prepare(oauthCredWithRev(7, "at-1", "rt-1", 1))
	require.NoError(t, err)

	// 旧快照：同账号、同来源 K、缺 refresh_token（构造不出 Auth）。
	stale := &domain.AccountCredential{AccountID: 7, OAuthToken: "at-old", IdentityRevision: 1}
	_, err = a.Prepare(stale)
	require.ErrorIs(t, err, errCredentialIncomplete, "错误照常上抛（该请求自身失败）")

	require.Empty(t, handler.snapshot(), "陈旧快照的构造失败不得升级为账号级事件")
	require.Same(t, current.entry, a.clients.Get(7), "当前条目必须保留（管理员刚修好的条目不得被删）")
}

// TestCodexCurrentMalformedPrepareReportsAndDropsOlder 来源门是双向的：来源快照确实
// **更新**（K 更大）且构造失败 ⇒ 照常上报（带来源 K），并且只摘除更旧的条目。
func TestCodexCurrentMalformedPrepareReportsAndDropsOlder(t *testing.T) {
	handler := &recordingHandler{}
	a := NewCodex(handler.add, testEgressPool(nil), nil, RotationDeps{})

	old, err := a.Prepare(oauthCredWithRev(7, "at-1", "rt-1", 1))
	require.NoError(t, err)

	// 管理员换上的新配置是坏的（K=2、缺 RT），来源比表里的条目更新。
	broken := &domain.AccountCredential{AccountID: 7, OAuthToken: "at-2", IdentityRevision: 2}
	_, err = a.Prepare(broken)
	require.ErrorIs(t, err, errCredentialIncomplete)

	calls := handler.snapshot()
	require.Len(t, calls, 1, "当前版本的坏凭据仍按一次账号级事件上报")
	require.Equal(t, int64(2), calls[0].sourceRevision, "上报携带来源 K（不是回读的当前值）")
	require.Nil(t, a.clients.Get(7), "更旧的条目被版本化摘除（invalidateOlder 只对更旧版本生效）")
	require.NotSame(t, old.entry, a.clients.Get(7))
}

// TestCodexMalformedPrepareWithSameRevisionKeepsEntry 同一代身份下迟到的坏快照
// 不得删掉另一份构造成功的凭据（同一 K 只有一份权威内容）。
func TestCodexMalformedPrepareWithSameRevisionKeepsEntry(t *testing.T) {
	handler := &recordingHandler{}
	a := NewCodex(handler.add, testEgressPool(nil), nil, RotationDeps{})

	valid, err := a.Prepare(&domain.AccountCredential{AccountID: 9, PATKey: "pat-good", IdentityRevision: 4})
	require.NoError(t, err)

	// 同 K、不同内容：PAT / OAuth 混搭的坏快照。
	_, err = a.Prepare(&domain.AccountCredential{AccountID: 9, OAuthToken: "at", IdentityRevision: 4})
	require.ErrorIs(t, err, errCredentialIncomplete)
	require.Empty(t, handler.snapshot())
	require.Same(t, valid.entry, a.clients.Get(9))
}

// TestStaleRotationWritebackIsDropped **C-M3 的本地门**：替换条目后调用旧条目的
// rotateWriteback ⇒ 存储零调用（旧 Auth 的在途轮转不得覆盖新凭据）。
func TestStaleRotationWritebackIsDropped(t *testing.T) {
	store := &fakeRotationStore{}
	invals := 0
	a := NewCodex(nil, testEgressPool(nil), nil, RotationDeps{
		Store:              store,
		InvalidateSnapshot: func(int64) { invals++ },
	})

	oldEntry, err := a.entryFor(oauthCredWithRev(7, "at-old", "rt-old", 1))
	require.NoError(t, err)
	_, err = a.entryFor(oauthCredWithRev(7, "at-new", "rt-new", 2))
	require.NoError(t, err)

	a.rotateWriteback(oldEntry, 1, "at-rotated", "rt-rotated")
	require.Empty(t, store.snapshot(), "旧条目的轮转不得写库")
	require.Zero(t, invals, "被丢弃的轮转也不得回写调度器快照")
}

// TestRotationWritebackCarriesSourceRevision 回写携带**来源 K**（存储围栏谓词）。
func TestRotationWritebackCarriesSourceRevision(t *testing.T) {
	store := &fakeRotationStore{}
	a := NewCodex(nil, testEgressPool(nil), nil, RotationDeps{Store: store})

	e, err := a.entryFor(oauthCredWithRev(7, "at-old", "rt-old", 3))
	require.NoError(t, err)
	a.rotateWriteback(e, 3, "at-new-r", "rt-new-r")

	calls := store.snapshot()
	require.Len(t, calls, 1)
	require.Equal(t, int64(3), calls[0].sourceRevision, "expectedRevision = 来源 K，不是回读的当前 K")
}

// TestRotationFencedByStorageDoesNotDisableAccount 存储提交围栏拒绝（K 已被管理面推进）
// 属于**陈旧事件**：不 panic（否则 SDK 会重试到 CallbackDeliveryError 并摘账号）、
// 不回写快照、不反过来禁用账号。
func TestRotationFencedByStorageDoesNotDisableAccount(t *testing.T) {
	store := &fakeRotationStore{err: repository.ErrStaleIdentityRevision}
	handler := &recordingHandler{}
	invals := 0
	a := NewCodex(handler.add, testEgressPool(nil), nil, RotationDeps{
		Store:              store,
		InvalidateSnapshot: func(int64) { invals++ },
	})

	e, err := a.entryFor(oauthCredWithRev(7, "at-old", "rt-old", 1))
	require.NoError(t, err)

	require.NotPanics(t, func() { a.rotateWriteback(e, 1, "at-rotated", "rt-rotated") },
		"围栏拒绝是陈旧事件、不是投递失败：不得 panic 触发 SDK 重试链")
	require.Len(t, store.snapshot(), 1, "尝试过一次（由存储评判）")
	require.Zero(t, invals, "被拒的轮转不得回写快照")
	require.Empty(t, handler.snapshot(), "也不得升级为账号级失效")
	require.Same(t, e, a.clients.Get(7), "条目不被摘除")
}

// TestRotationWritebackStillFailsClosedOnStoreError 非围栏错误仍是投递失败
// （fail-closed）：panic 交给 SDK 的 pending/重试链。
func TestRotationWritebackStillFailsClosedOnStoreError(t *testing.T) {
	store := &fakeRotationStore{err: errors.New("db down")}
	a := NewCodex(nil, testEgressPool(nil), nil, RotationDeps{Store: store})

	e, err := a.entryFor(oauthCredWithRev(7, "at-old", "rt-old", 1))
	require.NoError(t, err)
	require.Panics(t, func() { a.rotateWriteback(e, 1, "at-rotated", "rt-rotated") })
}

// TestEntrySourceRevisionIsMonotone 条目按**签名**复用，来源 K 随之单调抬升：
//   - 不动凭据内容的身份写入（K 抬升、同签名）不重建 Auth/连接池；
//   - 无版本调用面先建出条目后，首个带 K 的请求把围栏补上（而不是永久"来源未知"）。
func TestEntrySourceRevisionIsMonotone(t *testing.T) {
	a := NewCodex(nil, testEgressPool(nil), nil, RotationDeps{})

	// 管理面额度快照先建条目（无 Selection ⇒ K=0）。
	e0, err := a.Prepare(&domain.AccountCredential{AccountID: 5, PATKey: "pat"})
	require.NoError(t, err)
	require.Zero(t, e0.entry.srcRev.Load())

	// 首个请求（同签名、K=2）复用同一条目并把围栏抬到 2。
	e1, err := a.Prepare(patCredWithRev(5, "pat", 2))
	require.NoError(t, err)
	require.Same(t, e0.entry, e1.entry, "同签名不重建（不动凭据内容的身份写入不该关连接池）")
	require.Equal(t, int64(2), e1.entry.srcRev.Load())

	// 更旧的来源（在途请求的旧快照）不得把围栏回退。
	e2, err := a.Prepare(patCredWithRev(5, "pat", 1))
	require.NoError(t, err)
	require.Equal(t, int64(2), e2.entry.srcRev.Load(), "来源 K 单调：旧快照不得回退围栏")
}

// TestFatalPreparedReportsSourceRevisionOfEntry 事件来源版本的端到端形态：上报的 K
// 就是构造条目那一版（而不是上报时刻库里的值）。
func TestFatalPreparedReportsSourceRevisionOfEntry(t *testing.T) {
	handler := &recordingHandler{}
	a := NewCodex(handler.add, testEgressPool(nil), nil, RotationDeps{})
	prep, err := a.Prepare(oauthCredWithRev(7, "at-1", "rt-1", 6))
	require.NoError(t, err)

	a.FatalPrepared(prep, &codexsdk.AuthPermanentlyRevokedError{Code: "token_invalidated"})
	calls := handler.snapshot()
	require.Len(t, calls, 1)
	require.Equal(t, int64(6), calls[0].sourceRevision)
}

// TestNonCodexCredentialStillRejected 防御：非 codex 凭据的上报仍被拒绝（不因新增
// 来源版本而放宽既有类型门）。
func TestNonCodexCredentialStillRejected(t *testing.T) {
	store := &configRevisionStore{acct: &domain.Account{
		ID: 1, TemplateID: 10, UpstreamKey: "k", LifecycleRevision: 1, IdentityRevision: 1,
		Template: &domain.Template{CredentialType: credential.TypeAPIKey},
	}}
	failer := &retryFakeFailer{}
	deps := FailureDeps{Store: store, Failer: failer, Latch: newRetryFakeLatch(), Log: nil}

	require.NoError(t, HandleFailure(context.Background(), deps, failEvent(1, 1, errors.New("fatal"))))
	require.Empty(t, failer.calls)
	require.Nil(t, store.acct.FailedAt)
}
