// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/domain"
)

// ---------------------------------------------------------------------------
// T5 §1：WithOnTokenRotated → account_ext 部分更新回写（RotationStore 落库面）
// ---------------------------------------------------------------------------

// fakeRotationStore 轮转回写替身：记录 (accountID, at, rt, expiresAt) 调用序
// 列；err 注入失败（D4 链接管测试）。
type fakeRotationStore struct {
	mu    sync.Mutex
	calls []rotationCall
	err   error
}

type rotationCall struct {
	accountID int64
	at, rt    string
	expiresAt *time.Time
}

func (f *fakeRotationStore) WriteOAuthRotation(ctx context.Context, accountID int64, at, rt string, expiresAt *time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, rotationCall{accountID: accountID, at: at, rt: rt, expiresAt: expiresAt})
	return f.err
}

func (f *fakeRotationStore) snapshot() []rotationCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]rotationCall(nil), f.calls...)
}

// rotationUpstream 401 轮转 mock：按 Authorization 值区分——旧 at → 401（非
// 判死 token_expired，触发 SDK 自动轮转）；其余 → 200 成功响应。与
// TestCodex401RotationSuccess 的计数法区别：并发测试下按凭据判别确定性。
type rotationUpstream struct {
	mu    sync.Mutex
	calls int
	auths []string
	URL   string
}

func newRotationUpstream(t *testing.T, atOld string) *rotationUpstream {
	t.Helper()
	u := &rotationUpstream{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		u.mu.Lock()
		u.calls++
		u.auths = append(u.auths, auth)
		u.mu.Unlock()
		if auth != "Bearer "+atOld {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(okImageResponse))
			return
		}
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"code":"token_expired"}}`))
	}))
	u.URL = srv.URL
	t.Cleanup(srv.Close)
	return u
}

func (u *rotationUpstream) auth(i int) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.auths[i]
}

// TestCodexRotationWritebackPersists 轮转回写落库面：401 非判死 → SDK 自动轮
// 转 → 回调 → RotationStore 收到 (at-new, rt-new, **旧 expiry 保旧**——构造
// 时凭据携带值)；不重建缓存（条目实例保留——回调写回的是本 Auth 内部已更新
// 的状态）；后续请求直接用新 at 不重复 refresh。
func TestCodexRotationWritebackPersists(t *testing.T) {
	up := newRotationUpstream(t, "at-old")
	refresh := newCodexMockRefresh(t, codexUpstreamStep{status: 200, body: `{"access_token":"at-new","refresh_token":"rt-new"}`})
	store := &fakeRotationStore{}
	a := NewCodex(nil)
	a.SetRotationDeps(RotationDeps{Store: store})

	cred := oauthCred(7, "at-old", "rt-1")
	cred.BaseURL = up.URL + "/images/generations"
	expires := cred.OAuthExpiresAt

	img, err := a.GenerateImage(context.Background(), cred, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
	require.NoError(t, err)
	require.Len(t, img.Data, 2)

	calls := store.snapshot()
	require.Len(t, calls, 1, "每次轮转恰一次回写")
	require.Equal(t, int64(7), calls[0].accountID)
	require.Equal(t, "at-new", calls[0].at)
	require.Equal(t, "rt-new", calls[0].rt)
	require.NotNil(t, calls[0].expiresAt, "expiry 保旧：回调无 expiry，携带构造时旧值")
	require.True(t, calls[0].expiresAt.Equal(*expires), "codex_oauth_expires_at 保旧——防 ClearX 清空回归")

	// 不重建缓存：条目保留（回调写回的是本 Auth 内部已更新状态）
	a.mu.Lock()
	e := a.entries[7]
	a.mu.Unlock()
	require.NotNil(t, e, "条目保留（不重建不剔除）")

	// 后续请求直接用新 at（零额外 refresh）
	_, err = a.GenerateImage(context.Background(), cred, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
	require.NoError(t, err)
	require.Equal(t, "Bearer at-new", up.auth(1), "轮转后新 at 生效（内存缓存）")
	require.Equal(t, 1, refresh.callsN(), "不重建缓存——新 at 直接复用，零额外 refresh")
	require.Len(t, store.snapshot(), 1, "第二次请求无轮转 → 无新回写")
}

// TestCodexRotationWritebackMissingRefreshKeepsOldRT 响应缺 refresh_token →
// SDK 回调保留旧 rt（doRefresh 仅 rt 非空才覆盖——auth_oauth.go:257-267）→
// 盲写不落空（store 收到旧 rt，非空）。
func TestCodexRotationWritebackMissingRefreshKeepsOldRT(t *testing.T) {
	up := newRotationUpstream(t, "at-old")
	// refresh 响应缺 refresh_token（auth_oauth.go:402-406 字段均可选）
	newCodexMockRefresh(t, codexUpstreamStep{status: 200, body: `{"access_token":"at-new"}`})
	store := &fakeRotationStore{}
	a := NewCodex(nil)
	a.SetRotationDeps(RotationDeps{Store: store})

	cred := oauthCred(7, "at-old", "rt-1")
	cred.BaseURL = up.URL + "/images/generations"

	_, err := a.GenerateImage(context.Background(), cred, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
	require.NoError(t, err)
	calls := store.snapshot()
	require.Len(t, calls, 1)
	require.Equal(t, "at-new", calls[0].at)
	require.Equal(t, "rt-1", calls[0].rt, "缺 refresh_token → 回调收到保留旧 rt——盲写不落空")
}

// rotationUpstream401Always 恒 401 mock（新旧 at 一律 401 非判死码）——每次
// 请求都触发 refresh（D4 重试投递测试用）。
type rotationUpstream401Always struct {
	mu    sync.Mutex
	calls int
	URL   string
}

func newRotationUpstream401Always(t *testing.T) *rotationUpstream401Always {
	t.Helper()
	u := &rotationUpstream401Always{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		u.calls++
		u.mu.Unlock()
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"code":"token_expired"}}`))
	}))
	u.URL = srv.URL
	t.Cleanup(srv.Close)
	return u
}

func (u *rotationUpstream401Always) callsN() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.calls
}

// TestCodexRotationWritebackSingleFlight 并发单飞不重复回写：N 并发请求共享
// 一次 SDK 单飞 refresh（同账号轮转回调串行——auth_oauth.go:217-244）→
// RotationStore 恰一次写入。
func TestCodexRotationWritebackSingleFlight(t *testing.T) {
	up := newRotationUpstream(t, "at-old")
	// refresh 慢响应（100ms）扩大单飞窗口：全部 N 请求在 leader refresh 完成
	// 前命中 401 并加入单飞（等待者共享 leader 轮转结果）
	var refreshCalls atomic.Int64
	rsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		time.Sleep(100 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"access_token":"at-new","refresh_token":"rt-new"}`))
	}))
	t.Cleanup(rsrv.Close)
	t.Setenv("CODEX_REFRESH_TOKEN_URL_OVERRIDE", rsrv.URL)
	store := &fakeRotationStore{}
	a := NewCodex(nil)
	a.SetRotationDeps(RotationDeps{Store: store})

	cred := oauthCred(7, "at-old", "rt-1")
	cred.BaseURL = up.URL + "/images/generations"

	const n = 8
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = a.GenerateImage(context.Background(), cred, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		require.NoError(t, err, "请求 %d 必须成功（单飞共享轮转结果）", i)
	}
	require.Equal(t, int64(1), refreshCalls.Load(), "单飞恰一次 refresh")
	require.Len(t, store.snapshot(), 1, "并发单飞不重复回写——同账号轮转回调串行")
}

// TestCodexRotationWritebackFailureD4Fatal 回写失败 → D4 链接管：回调 panic
// → SDK recover → pending 重试投递（同一 (at, rt) 幂等重试）→ 连续失败达阈
// 值（默认 3）→ CallbackDeliveryError fatal → 统一回调单次上报（fail-closed：
// 令牌无法持久化 = 账号失效信号）。
func TestCodexRotationWritebackFailureD4Fatal(t *testing.T) {
	up := newRotationUpstream401Always(t) // 恒 401：每次请求首拨都触发 refresh → deliverPendingRotate 重试投递
	newCodexMockRefresh(t,
		codexUpstreamStep{status: 200, body: `{"access_token":"at-new","refresh_token":"rt-new"}`},
		codexUpstreamStep{status: 200, body: `{"access_token":"at-new2","refresh_token":"rt-new2"}`},
	)
	handler := &recordingHandler{}
	store := &fakeRotationStore{err: errors.New("db down")}
	a := NewCodex(handler.add)
	a.SetRotationDeps(RotationDeps{Store: store})

	cred := oauthCred(7, "at-old", "rt-1")
	cred.BaseURL = up.URL + "/images/generations"

	// R1：refresh run1 回调失败（fail#1，pending，本次 at 放行）→ 401 重试
	// 防重试风暴不再 refresh → HTTPError 401（回写失败不阻塞请求——D4 语义）
	_, err := a.GenerateImage(context.Background(), cred, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
	require.Error(t, err)
	var he *codexsdk.HTTPError
	require.True(t, errors.As(err, &he), "R1 未达阈值：HTTPError 401 透传（回调失败不阻塞请求）")
	require.Len(t, store.snapshot(), 1, "R1 回调失败一次（pending 未交付）")
	require.Empty(t, handler.snapshot(), "R1 不触发 fatal 上报")

	// R2：refresh run2 先 deliverPendingRotate（fail#2）→ 新回调失败（fail#3
	// ≥ 阈值）→ CallbackDeliveryError fatal → 统一回调单次上报
	_, err = a.GenerateImage(context.Background(), cred, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
	require.Error(t, err)
	var cd *codexsdk.CallbackDeliveryError
	require.True(t, errors.As(err, &cd), "达阈值 → CallbackDeliveryError 透传")
	require.Equal(t, 3, cd.Attempts)

	calls := handler.snapshot()
	require.Len(t, calls, 1, "D4 fatal 统一上报恰一次（双源去重——OnAuthFatal 与 errors.As 同 fatal）")
	var cd2 *codexsdk.CallbackDeliveryError
	require.True(t, errors.As(calls[0].fatal, &cd2))
	require.Equal(t, int64(7), calls[0].accountID)
	require.Len(t, store.snapshot(), 3, "回调三次失败（run1 新回调 + run2 pending 重试 + run2 新回调）——同一 (at, rt) 幂等重试")
}

// TestCodexRotationWritebackUnwired 回写面未装配（SetRotationDeps 未调用——
// 测试/旧装配形态）：轮转正常进行，回写 no-op 不 panic。
func TestCodexRotationWritebackUnwired(t *testing.T) {
	up := newRotationUpstream(t, "at-old")
	newCodexMockRefresh(t, codexUpstreamStep{status: 200, body: `{"access_token":"at-new","refresh_token":"rt-new"}`})
	a := NewCodex(nil) // 未装配 rotate

	cred := oauthCred(7, "at-old", "rt-1")
	cred.BaseURL = up.URL + "/images/generations"
	_, err := a.GenerateImage(context.Background(), cred, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
	require.NoError(t, err, "未装配回写面不阻断轮转")
	require.Equal(t, "Bearer at-new", up.auth(1))
}

// TestRotationCallExpiryNilPreserved 构造时凭据无 expiry（nil——未知过期）→
// 回写携带 nil（写 NULL 与"未知过期"语义一致，不发明新值）。
func TestRotationCallExpiryNilPreserved(t *testing.T) {
	up := newRotationUpstream(t, "at-old")
	newCodexMockRefresh(t, codexUpstreamStep{status: 200, body: `{"access_token":"at-new","refresh_token":"rt-new"}`})
	store := &fakeRotationStore{}
	a := NewCodex(nil)
	a.SetRotationDeps(RotationDeps{Store: store})

	cred := &domain.AccountCredential{AccountID: 7, OAuthToken: "at-old", OAuthRefreshToken: "rt-1"}
	cred.BaseURL = up.URL + "/images/generations"
	_, err := a.GenerateImage(context.Background(), cred, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
	require.NoError(t, err)
	calls := store.snapshot()
	require.Len(t, calls, 1)
	require.Nil(t, calls[0].expiresAt, "无过期信息 → 回写 nil（不发明新值）")
}

// ---------------------------------------------------------------------------
// T5 §3：FatalAuth（WS 业务判死帧——毒化 + 单次上报 + 双源去重）
// ---------------------------------------------------------------------------

// TestCodexFatalAuthPoisonAndDedup 帧判死接线：FatalAuth → Auth 毒化（后续
// Authorization 恒返回该错误——SDK 显式终止不触发 OnAuthFatal）+ 单次上报
// + 不剔除（毒化保留——外部凭据变更才重建）；同一 fatal 再经 errors.As 二次
// 命中（translateError 路径）→ 仍单次上报（CAS 双源去重）。
func TestCodexFatalAuthPoisonAndDedup(t *testing.T) {
	up, _ := newCodexUpstream(t, codexUpstreamStep{status: 200, body: okImageResponse})
	defer up.Close()
	handler := &recordingHandler{}
	a := NewCodex(handler.add)

	cred := oauthCred(7, "at-1", "rt-1")
	cred.BaseURL = up.URL + "/images/generations"
	_, err := a.GenerateImage(context.Background(), cred, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
	require.NoError(t, err)

	// 帧判死：token_invalidated 业务事件 → Auth.Fatal + 上报
	fatal := &codexsdk.AuthPermanentlyRevokedError{Code: "token_invalidated", Raw: []byte(`{"type":"error","error":{"code":"token_invalidated"}}`)}
	a.FatalAuth(7, fatal)
	require.Len(t, handler.snapshot(), 1, "帧判死上报一次")

	// Auth 已毒化：同账号请求 → 恒返回判死错误（errors.As 二次命中——CAS 已置 → 不上报）
	_, err = a.GenerateImage(context.Background(), cred, &domain.ImageGenParams{Model: "gpt-image-2", Prompt: "cat"})
	require.Error(t, err)
	var ap *codexsdk.AuthPermanentlyRevokedError
	require.True(t, errors.As(err, &ap), "毒化 Auth → 请求面恒返回同一 fatal（errors.As 可命中）")
	require.Len(t, handler.snapshot(), 1, "双源去重：errors.As 二次命中同一 fatal → 仍单次上报")

	// 不剔除：毒化条目保留（外部凭据变更 → sig 变化才重建）
	a.mu.Lock()
	e := a.entries[7]
	a.mu.Unlock()
	require.NotNil(t, e, "FatalAuth 不剔除条目——毒化保留")
}

// TestCodexFatalAuthNilAndMissing FatalAuth 防御：nil 错误 / 未知账号 →
// no-op 不 panic（条目不存在 = 并发 fatal 已上报剔除，上报已由胜者完成）。
func TestCodexFatalAuthNilAndMissing(t *testing.T) {
	handler := &recordingHandler{}
	a := NewCodex(handler.add)
	require.NotPanics(t, func() {
		a.FatalAuth(7, nil)
		a.FatalAuth(999, &codexsdk.AuthPermanentlyRevokedError{Code: "token_revoked"})
	})
	require.Empty(t, handler.snapshot(), "nil/未知账号不上报")
}
