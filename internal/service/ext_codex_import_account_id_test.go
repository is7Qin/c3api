// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/sdkbridge"
)

// —— codex 导入行 account id 缺省补全（spec §7.0-6：放宽 = 允许缺省提供，不等
// 于允许空入库——补全后仍空 → 行级 failed 原文案不变） ——

// codexAccountIDJWT 构造含 chatgpt_account_id claim 的 JWT（sdkbridge 离线解析
// 只解 payload 不验签）。
func codexAccountIDJWT(t *testing.T, accountID string) string {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." +
		enc([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"`+accountID+`"}}`)) + ".sig"
}

// importAccountIDFixture 导入派生接线：真实 sdkbridge 薄包装注入（离线解析 +
// whoami 在线查询经 httptest + CODEX_AUTHAPI_BASE_URL 由各用例自配）。
func importAccountIDFixture(t *testing.T) (*Service, *fakeStore) {
	t.Helper()
	svc, store, _ := importFixture(t)
	svc.deriveOAuthAccountID = sdkbridge.DeriveCodexAccountID
	svc.fetchPATAccountID = sdkbridge.FetchPATAccountID
	store.tpls[2] = &domain.Template{ID: 2, Name: "tp", CredentialType: credential.TypeCodexPAT}
	return svc, store
}

// TestImportCodexOAuthDeriveAccountID OAuth 行空 account id：JWT claim 可派生 →
// 导入并落库派生值；不可派生 → 行级 failed（既有 "codex_account_id 必填" 原文案）。
func TestImportCodexOAuthDeriveAccountID(t *testing.T) {
	ctx := context.Background()
	svc, store := importAccountIDFixture(t)
	tplID := int64(1)

	t.Run("empty account id derived from token claims", func(t *testing.T) {
		res, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
			{CodexEmail: "d@example.com", CodexAccountID: "",
				CodexOAuthToken: codexAccountIDJWT(t, "acc-derived-1"), CodexOAuthRefreshToken: "rt-1"},
		}, &tplID, nil)
		require.NoError(t, err)
		require.Equal(t, 1, res.Imported)
		require.Empty(t, res.Failed)
		ext, err := store.FindAccountExtByCodexKey(ctx, "d@example.com", "acc-derived-1")
		require.NoError(t, err, "派生值参与幂等键并落库")
		require.Equal(t, "acc-derived-1", *ext.CodexAccountID)
	})

	t.Run("underivable token keeps row-level failure message", func(t *testing.T) {
		res, err := svc.ImportCodexOAuthAccounts(ctx, []domain.CodexOAuthImportItem{
			{CodexEmail: "u@example.com", CodexAccountID: "",
				CodexOAuthToken: "opaque-non-jwt", CodexOAuthRefreshToken: "rt-1"},
		}, &tplID, nil)
		require.NoError(t, err)
		require.Equal(t, 0, res.Imported)
		require.Len(t, res.Failed, 1)
		require.Equal(t, 0, res.Failed[0].Index)
		require.Equal(t, "codex_account_id 必填", res.Failed[0].Error, "放宽 ≠ 允许空入库：仍空 → 原文案行级 failed")
	})
}

// TestImportCodexPATDeriveAccountID PAT 行空 account id：whoami 成功 → 导入并
// 落库；whoami 失败 → 行级 failed（既有原文案）。
func TestImportCodexPATDeriveAccountID(t *testing.T) {
	ctx := context.Background()

	t.Run("whoami success imports with derived id", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"email":"w@example.com","chatgpt_account_id":"acc-whoami-2"}`))
		}))
		defer srv.Close()
		t.Setenv("CODEX_AUTHAPI_BASE_URL", srv.URL)

		svc, store := importAccountIDFixture(t)
		tplID := int64(2)
		res, err := svc.ImportCodexPATAccounts(ctx, []domain.CodexPATImportItem{
			{CodexEmail: "w@example.com", CodexAccountID: "", CodexPATKey: "pat-w-1"},
		}, &tplID, nil)
		require.NoError(t, err)
		require.Equal(t, 1, res.Imported)
		require.Empty(t, res.Failed)
		ext, err := store.FindAccountExtByCodexKey(ctx, "w@example.com", "acc-whoami-2")
		require.NoError(t, err, "whoami 值参与幂等键并落库")
		require.Equal(t, "acc-whoami-2", *ext.CodexAccountID)
	})

	t.Run("whoami failure keeps row-level failure message", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"bad key"}`))
		}))
		defer srv.Close()
		t.Setenv("CODEX_AUTHAPI_BASE_URL", srv.URL)

		svc, _ := importAccountIDFixture(t)
		tplID := int64(2)
		res, err := svc.ImportCodexPATAccounts(ctx, []domain.CodexPATImportItem{
			{CodexEmail: "f@example.com", CodexAccountID: "", CodexPATKey: "pat-bad"},
		}, &tplID, nil)
		require.NoError(t, err)
		require.Equal(t, 0, res.Imported)
		require.Len(t, res.Failed, 1)
		require.Equal(t, "codex_account_id 必填", res.Failed[0].Error)
	})
}
