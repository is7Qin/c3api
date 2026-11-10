// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// mock 替身（§6）：可编程回调（记录 accountID/fatal 序列）+ 可编程
// FailureStore / AccountFailer。
// ---------------------------------------------------------------------------

// failureCall 一次回调记录（含**来源身份代际**——事件必须带它，见 CredentialFailure）。
type failureCall struct {
	accountID      int64
	sourceRevision int64
	fatal          error
}

// recordingHandler mock 回调替身：记录 (accountID, 来源 K, fatal) 序列（适配层回调的
// 测试替身形态；同时用于验证 NewFailureHandler 装配的调用面）。
type recordingHandler struct {
	mu    sync.Mutex
	calls []failureCall
}

func (r *recordingHandler) add(ev CredentialFailure) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, failureCall{accountID: ev.AccountID, sourceRevision: ev.SourceRevision, fatal: ev.Cause})
}

func (r *recordingHandler) snapshot() []failureCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]failureCall, len(r.calls))
	copy(out, r.calls)
	return out
}

type fakeStore struct {
	mu        sync.Mutex
	accountID int64
	failedAt  time.Time
	reason    string
	calls     int
	err       error // 注入错误：模拟 DB 写失败
}

func (f *fakeStore) SetAccountFailed(ctx context.Context, accountID int64, failedAt time.Time, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accountID, f.failedAt, f.reason = accountID, failedAt, reason
	f.calls++
	return f.err
}

type fakeFailer struct {
	mu        sync.Mutex
	accountID int64
	calls     int
}

func (f *fakeFailer) FailAccount(accountID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accountID = accountID
	f.calls++
}

func newDeps(store *fakeStore, failer *fakeFailer) FailureDeps {
	return FailureDeps{Store: store, Failer: failer}
}

// failEvent 构造一次凭据版本事件。**来源 K 必须由用例显式给出**——这正是修复
// 的形状：来源版本不再是"调用时再读一次当前账号"补出来的。
func failEvent(accountID, sourceRevision int64, cause error) CredentialFailure {
	return CredentialFailure{AccountID: accountID, SourceRevision: sourceRevision, Cause: cause}
}

func TestHandleFailure(t *testing.T) {
	store, failer := &fakeStore{}, &fakeFailer{}
	fatal := errors.New("auth permanently revoked")

	err := HandleFailure(context.Background(), newDeps(store, failer), failEvent(7, 1, fatal))
	require.NoError(t, err)

	store.mu.Lock()
	require.Equal(t, int64(7), store.accountID)
	require.Equal(t, fatal.Error(), store.reason)
	require.False(t, store.failedAt.IsZero())
	store.mu.Unlock()

	failer.mu.Lock()
	require.Equal(t, int64(7), failer.accountID)
	failer.mu.Unlock()
}

// TestHandleFailureNilFatal 防御：nil 错误不上报（无任何副作用）。
func TestHandleFailureNilFatal(t *testing.T) {
	store, failer := &fakeStore{}, &fakeFailer{}
	require.NoError(t, HandleFailure(context.Background(), newDeps(store, failer), failEvent(7, 1, nil)))
	require.Equal(t, 0, store.calls)
	require.Equal(t, 0, failer.calls)
}

// TestHandleFailureTruncatesReason 失效原因域内截断 500（失效原因复用既有
// last_error，与既有错误文本共用截断语义）。
func TestHandleFailureTruncatesReason(t *testing.T) {
	store, failer := &fakeStore{}, &fakeFailer{}
	long := strings.Repeat("x", 600)
	require.NoError(t, HandleFailure(context.Background(), newDeps(store, failer), failEvent(7, 1, errors.New(long))))

	store.mu.Lock()
	require.Len(t, store.reason, 500)
	store.mu.Unlock()
}

// TestHandleFailureStoreErrorFailClosed DB 写失败不阻断摘除（fail-closed）：
// 返回错误，FailAccount 恒执行。
func TestHandleFailureStoreErrorFailClosed(t *testing.T) {
	store := &fakeStore{err: errors.New("db down")}
	failer := &fakeFailer{}

	err := HandleFailure(context.Background(), newDeps(store, failer), failEvent(7, 1, errors.New("fatal")))
	require.Error(t, err, "DB 写失败返回错误供日志")
	require.Equal(t, 1, failer.calls, "摘除恒执行——DB 故障内存摘除先生效")
}

// TestNewFailureHandlerWiring 装配：NewFailureHandler 产出的回调把上报翻译成
// 处理链（mock 回调替身 recordingHandler 验证调用面形态——accountID/fatal 序列）。
func TestNewFailureHandlerWiring(t *testing.T) {
	store, failer := &fakeStore{}, &fakeFailer{}
	handler := NewFailureHandler(newDeps(store, failer))
	rec := &recordingHandler{}
	rec.add(failEvent(7, 3, errors.New("boom"))) // 模拟适配层把 SDK 判死翻译成回调
	handler(failEvent(7, 3, errors.New("boom")))

	require.Equal(t, 1, store.calls)
	require.Equal(t, 1, failer.calls)
	require.Len(t, rec.snapshot(), 1, "回调序列完整记录")
	require.Equal(t, int64(7), rec.snapshot()[0].accountID)
	require.Equal(t, int64(3), rec.snapshot()[0].sourceRevision, "事件必须携带来源身份代际 K")
	require.EqualError(t, rec.snapshot()[0].fatal, "boom")
}
