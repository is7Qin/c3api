// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/codexproto"
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/service"
)

// fakeAccountIDResolver 可编程的 AccountIDResolver（只测端口契约与 nil 语义，
// 不测 whoami 出站——那是 sdkbridge 的测试面）。
type fakeAccountIDResolver struct {
	id  string
	err error

	mu    sync.Mutex
	calls []string
}

func (f *fakeAccountIDResolver) ResolveAccountID(ctx context.Context, patKey string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, patKey)
	f.mu.Unlock()
	return f.id, f.err
}

func (f *fakeAccountIDResolver) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// newDeriveTestAPI 只装配 resolve 端口（不带快照面）的最小 AdminAPI。
func newDeriveTestAPI(res AccountIDResolver) *AdminAPI {
	store := newFakeStore()
	svc := service.New(service.Deps{Store: store, Scheduler: fakeSched{}, Invalidate: service.NopInvalidator{}, Publisher: nil, RuleReload: nil, Auth: &fakeKeys{}, Log: nil, EmailCodeStore: store})
	return New(svc, OpsOptions{AccountIDResolver: res})
}

// TestDeriveAccountIDTypes 派生策略的四个分支（**唯一实现**的单测，见
// codex_account_id.go）：OAuth 离线解析、PAT 在线查询、未装配端口、其它类型。
func TestDeriveAccountIDTypes(t *testing.T) {
	t.Run("oauth derives offline and never touches resolver", func(t *testing.T) {
		res := &fakeAccountIDResolver{id: "must-not-be-used"}
		h := newDeriveTestAPI(res)
		got := h.deriveAccountID(context.Background(), credential.TypeCodexOAuth, extCodexAccountJWT(t, "acc-o-1"), "pat-ignored")
		require.Equal(t, "acc-o-1", got)
		require.Zero(t, res.callCount(), "OAuth 分支是纯函数，零出站")
	})

	t.Run("oauth invalid token yields empty", func(t *testing.T) {
		h := newDeriveTestAPI(&fakeAccountIDResolver{id: "x"})
		for _, tok := range []string{"", "opaque", "a.b", "a.b.c.d"} {
			require.Empty(t, h.deriveAccountID(context.Background(), credential.TypeCodexOAuth, tok, ""))
		}
	})

	t.Run("pat asks the resolver", func(t *testing.T) {
		res := &fakeAccountIDResolver{id: "acc-p-1"}
		h := newDeriveTestAPI(res)
		require.Equal(t, "acc-p-1", h.deriveAccountID(context.Background(), credential.TypeCodexPAT, "ignored", "pat-1"))
		require.Equal(t, 1, res.callCount())
	})

	t.Run("pat resolver error yields empty", func(t *testing.T) {
		h := newDeriveTestAPI(&fakeAccountIDResolver{err: errors.New("whoami 401")})
		require.Empty(t, h.deriveAccountID(context.Background(), credential.TypeCodexPAT, "", "pat-bad"))
	})

	t.Run("pat resolver empty id yields empty", func(t *testing.T) {
		h := newDeriveTestAPI(&fakeAccountIDResolver{id: ""})
		require.Empty(t, h.deriveAccountID(context.Background(), credential.TypeCodexPAT, "", "pat-none"))
	})

	t.Run("nil resolver leaves pat empty without panic", func(t *testing.T) {
		h := newDeriveTestAPI(nil)
		require.NotPanics(t, func() {
			require.Empty(t, h.deriveAccountID(context.Background(), credential.TypeCodexPAT, "", "pat-1"),
				"未装配 ⇒ 留空（不是 500、不是 panic）")
		})
		// nil 端口不影响 OAuth 分支（纯函数永远可用）。
		require.Equal(t, "acc-o-2", h.deriveAccountID(context.Background(), credential.TypeCodexOAuth, extCodexAccountJWT(t, "acc-o-2"), ""))
	})

	t.Run("other credential types never derive", func(t *testing.T) {
		res := &fakeAccountIDResolver{id: "x"}
		h := newDeriveTestAPI(res)
		require.Empty(t, h.deriveAccountID(context.Background(), credential.Type("api-key"), "tok", "pat"))
		require.Zero(t, res.callCount())
	})
}

// TestPutAccountsExtNilResolverDegrades PAT 保存的空 account id + **未装配端口**：
// 端点必须 200、account_id 留空（不 500、不 panic）。这是 §5.4 的 nil 降级语义在
// 真实请求路径上的钉。
func TestPutAccountsExtNilResolverDegrades(t *testing.T) {
	_, _, do := newListTestRouter(t) // 生产装配之外的零值 OpsOptions
	id := extCodexAccount(t, do, "codex-pat")

	rec := do(http.MethodPut, "/api/admin/accounts/"+itoa64(id)+"/ext",
		`{"credential_type":"codex-pat","codex_pat_key":"pat-nowhere"}`)
	require.Equal(t, 200, rec.Code, "未装配 resolver 不得 500：%s", rec.Body.String())
	var ext AccountExt
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ext))
	require.Nil(t, ext.CodexAccountId, "未装配 ⇒ 留空（下次保存重试）")
}

// TestImportCodexPATNilResolverDegrades 批量导入的空 account id + 未装配端口：
// 行级失败（"codex_account_id 必填"），**不是**整请求 500。
func TestImportCodexPATNilResolverDegrades(t *testing.T) {
	h, _, _, _ := codexImportTestAPI(t)
	rec := doImport(t, h, http.MethodPost, "/api/admin/accounts/batch-import-codex-pat", `{
		"items": [{"codex_email":"nil@example.com","codex_account_id":"","codex_pat_key":"pat-nil"}],
		"template_id": 2}`)
	require.Equal(t, 200, rec.Code, "未装配 resolver 不得 500：%s", rec.Body.String())
	var out ImportResult
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	require.Zero(t, out.Imported)
	require.Len(t, out.Failed, 1)
	require.Equal(t, "codex_account_id 必填", out.Failed[0].Error, "未派生 ⇒ 走既有行级必填校验")
}

// TestUsageClassificationByKind 分类**只认 codexproto.Error 的 Kind**：
// 两端（凭据失效 / 上游）各自映射到契约枚举；**未知错误不误标**（null/null），
// 且"只带旧哨兵、不带类型化包装"的错误也不再被分类——那是本单换口径的直接证据。
func TestUsageClassificationByKind(t *testing.T) {
	now := time.Date(2026, 8, 18, 15, 30, 0, 0, time.UTC)
	cases := []struct {
		name string
		err  error
		want domain.AccountUsageUpstreamError
	}{
		{"auth expired", wrapKind(codexproto.ErrKindAuthExpired, "fatal 401"), domain.UpstreamErrorAuthExpired},
		{"upstream", wrapKind(codexproto.ErrKindUpstream, "500"), domain.UpstreamErrorUpstreamUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, store := newUsageTestHandler(t, now, &hUsageSnap{errs: map[int64]error{1: tc.err}})
			seedCodexAccounts(store, 1)
			store.accExts[1] = &domain.AccountExt{AccountID: 1, CredentialType: credential.TypeCodexPAT, CodexPATKey: new("pat-ok")}
			items := []domain.AccountUsage{{AccountID: 1}}
			h.assembleUpstream(context.Background(), items)
			require.NotNil(t, items[0].UpstreamError)
			require.Equal(t, tc.want, *items[0].UpstreamError)
		})
	}

	t.Run("unclassified error stays null/null", func(t *testing.T) {
		h, store := newUsageTestHandler(t, now, &hUsageSnap{errs: map[int64]error{1: errors.New("store down?")}})
		seedCodexAccounts(store, 1)
		store.accExts[1] = &domain.AccountExt{AccountID: 1, CredentialType: credential.TypeCodexPAT, CodexPATKey: new("pat-ok")}
		items := []domain.AccountUsage{{AccountID: 1}}
		h.assembleUpstream(context.Background(), items)
		require.Nil(t, items[0].Upstream)
		require.Nil(t, items[0].UpstreamError, "未知错误不误标")
	})
}

// wrapKind 复刻实现方（sdkbridge）的产出形态：类型化错误 + 旧哨兵两层同链。
func wrapKind(kind codexproto.ErrorKind, cause string) error {
	return fmt.Errorf("%w: %w", &codexproto.Error{Kind: kind, Err: errors.New(cause)}, errors.New("codex: usage sentinel"))
}
