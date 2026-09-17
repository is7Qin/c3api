// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/domain"
)

// —— codex account id 接线（spec 2026-09-17 §7.0 1/2/3：管理面派生薄包装 +
// buildAuth 透传 + credSig 重建语义） ——

// craftAccountIDToken 构造含 chatgpt_account_id claim 的 JWT（alg:none 未签名——
// AccountIDFromToken 只解 payload 不验签，与真客户端 decode_jwt_payload 同语义）。
func craftAccountIDToken(t *testing.T, accountID string) string {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	header := enc([]byte(`{"alg":"none"}`))
	payload := enc([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"` + accountID + `"}}`))
	return header + "." + payload + ".sig"
}

// TestDeriveCodexAccountID 离线派生：合法 JWT 命中；claim 缺失/非 JWT/空串 →
// 未命中（不 panic）。
func TestDeriveCodexAccountID(t *testing.T) {
	id, ok := DeriveCodexAccountID(craftAccountIDToken(t, "acc-derive-1"))
	require.True(t, ok)
	require.Equal(t, "acc-derive-1", id)

	enc := base64.RawURLEncoding.EncodeToString
	noClaim := enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(`{"sub":"user-1"}`)) + ".sig"
	_, ok = DeriveCodexAccountID(noClaim)
	require.False(t, ok, "claim 缺失 → 未命中")

	for _, tok := range []string{"", "not-a-jwt", "a.b", "rt.1.opaque"} {
		_, ok = DeriveCodexAccountID(tok)
		require.False(t, ok, "非 JWT %q → 未命中", tok)
	}
}

// TestFetchPATAccountID whoami 在线查询（httptest + CODEX_AUTHAPI_BASE_URL 覆盖）：
// 200 → account id；非 2xx → 带错；空 key → 零出站 ("", nil)。
func TestFetchPATAccountID(t *testing.T) {
	var hits atomic.Int64
	var gotAuth atomic.Value
	var gotPath atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotAuth.Store(r.Header.Get("Authorization"))
		gotPath.Store(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"email":"p@example.com","chatgpt_user_id":"user-1","chatgpt_account_id":"acc-whoami-1"}`))
	}))
	defer srv.Close()
	t.Setenv("CODEX_AUTHAPI_BASE_URL", srv.URL)

	id, err := FetchPATAccountID(context.Background(), "pat-test-key")
	require.NoError(t, err)
	require.Equal(t, "acc-whoami-1", id)
	require.Equal(t, int64(1), hits.Load())
	require.Equal(t, "Bearer pat-test-key", gotAuth.Load())
	require.Equal(t, "/v1/user-auth-credential/whoami", gotPath.Load())

	id, err = FetchPATAccountID(context.Background(), "   ")
	require.NoError(t, err)
	require.Empty(t, id)
	require.Equal(t, int64(1), hits.Load(), "空 key 零出站")
}

// TestFetchPATAccountIDFailure 非 2xx → error（调用方 best-effort 按失败处理）。
func TestFetchPATAccountIDFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer srv.Close()
	t.Setenv("CODEX_AUTHAPI_BASE_URL", srv.URL)

	_, err := FetchPATAccountID(context.Background(), "pat-bad")
	require.Error(t, err)
	require.Contains(t, err.Error(), "401")
}

// TestCredSigAccountID credSig：account id 变更 → 签名变更（客户端重建）；
// 全等凭据 → 同签名。
func TestCredSigAccountID(t *testing.T) {
	base := &domain.AccountCredential{AccountID: 7, OAuthToken: "at", OAuthRefreshToken: "rt"}
	same := &domain.AccountCredential{AccountID: 7, OAuthToken: "at", OAuthRefreshToken: "rt"}
	changed := &domain.AccountCredential{AccountID: 7, OAuthToken: "at", OAuthRefreshToken: "rt", CodexAccountID: "acc-1"}
	require.Equal(t, credSig(base), credSig(same))
	require.NotEqual(t, credSig(base), credSig(changed), "account id 变更是账号身份变更，必须触发重建")

	patBase := &domain.AccountCredential{AccountID: 9, PATKey: "pat-1"}
	patChanged := &domain.AccountCredential{AccountID: 9, PATKey: "pat-1", CodexAccountID: "acc-9"}
	require.NotEqual(t, credSig(patBase), credSig(patChanged))
}

// TestBuildAuthAccountID buildAuth：DB account id 经 WithOAuthAccountID/
// WithPATAccountID 透传进 Auth（AccountIDProvider 可选接口）；空 = 不发头。
func TestBuildAuthAccountID(t *testing.T) {
	a := NewCodex(nil, nil, RotationDeps{})

	oauth := oauthCred(7, "at-7", "rt-7")
	oauth.CodexAccountID = "acc-oauth-7"
	e, err := a.entryFor(oauth)
	require.NoError(t, err)
	p, ok := e.auth.(codexsdk.AccountIDProvider)
	require.True(t, ok, "OAuth Auth 应实现 AccountIDProvider")
	require.Equal(t, "acc-oauth-7", p.AccountID())

	pat := &domain.AccountCredential{AccountID: 9, PATKey: "pat-9", CodexAccountID: "acc-pat-9"}
	pe, err := a.entryFor(pat)
	require.NoError(t, err)
	pp, ok := pe.auth.(codexsdk.AccountIDProvider)
	require.True(t, ok, "PAT Auth 应实现 AccountIDProvider")
	require.Equal(t, "acc-pat-9", pp.AccountID())

	bare := oauthCred(11, "at-11", "rt-11")
	be, err := a.entryFor(bare)
	require.NoError(t, err)
	bp, ok := be.auth.(codexsdk.AccountIDProvider)
	require.True(t, ok)
	require.Empty(t, bp.AccountID(), "空 account id = 不发头（向后兼容）")
}
