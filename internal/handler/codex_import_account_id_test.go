// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// —— codex 批量导入 account id 缺省补全（完整语义见 sdkbridge/codex_account_id.go；
// 仍空 → service 原文案行级 failed） ——

// TestImportCodexOAuthDerivesAccountID OAuth 行空 account id：JWT claim 可派生 →
// 导入并落库派生值；不可派生 → 行级 failed（既有 "codex_account_id 必填" 原文案）。
func TestImportCodexOAuthDerivesAccountID(t *testing.T) {
	ctx := context.Background()

	t.Run("empty account id derived from token claims", func(t *testing.T) {
		h, store, tplID, _ := codexImportTestAPI(t)
		rec := doImport(t, h, http.MethodPost, "/api/admin/accounts/batch-import-codex-oauth", `{
			"items": [{"codex_email":"d@example.com","codex_account_id":"",
				"codex_oauth_token":"`+extCodexAccountJWT(t, "acc-derived-1")+`","codex_oauth_refresh_token":"rt-1"}],
			"template_id": `+itoa(tplID)+`}`)
		require.Equal(t, 200, rec.Code, "body: %s", rec.Body.String())
		var out ImportResult
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		require.Equal(t, 1, out.Imported)
		require.Empty(t, out.Failed)
		ext, err := store.FindAccountExtByCodexKey(ctx, "d@example.com", "acc-derived-1")
		require.NoError(t, err, "派生值参与幂等键并落库")
		require.Equal(t, "acc-derived-1", *ext.CodexAccountID)
	})

	t.Run("underivable token keeps row-level failure message", func(t *testing.T) {
		h, _, tplID, _ := codexImportTestAPI(t)
		rec := doImport(t, h, http.MethodPost, "/api/admin/accounts/batch-import-codex-oauth", `{
			"items": [{"codex_email":"u@example.com","codex_account_id":"",
				"codex_oauth_token":"opaque-non-jwt","codex_oauth_refresh_token":"rt-1"}],
			"template_id": `+itoa(tplID)+`}`)
		require.Equal(t, 200, rec.Code)
		var out ImportResult
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		require.Zero(t, out.Imported)
		require.Len(t, out.Failed, 1)
		require.Equal(t, 0, out.Failed[0].Index)
		require.Equal(t, "codex_account_id 必填", out.Failed[0].Error, "放宽 ≠ 允许空入库：仍空 → 原文案行级 failed")
	})
}

// TestImportCodexPATDerivesAccountID PAT 行空 account id：whoami 成功 → 导入并
// 落库；whoami 失败 → 行级 failed（既有原文案）；显式提供 id → 零出站。
func TestImportCodexPATDerivesAccountID(t *testing.T) {
	const patTplID = 2 // codexImportTestAPI 预置 pat 模板（id=2）

	t.Run("whoami success imports with derived id", func(t *testing.T) {
		h, store, _, _ := codexImportTestAPI(t)
		ctx := context.Background()
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"email":"w@example.com","chatgpt_account_id":"acc-whoami-2"}`))
		}))
		defer srv.Close()
		t.Setenv("CODEX_AUTHAPI_BASE_URL", srv.URL)

		rec := doImport(t, h, http.MethodPost, "/api/admin/accounts/batch-import-codex-pat", `{
			"items": [{"codex_email":"w@example.com","codex_account_id":"","codex_pat_key":"pat-w-1"}],
			"template_id": `+itoa(patTplID)+`}`)
		require.Equal(t, 200, rec.Code, "body: %s", rec.Body.String())
		var out ImportResult
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		require.Equal(t, 1, out.Imported)
		require.Empty(t, out.Failed)
		require.Equal(t, int32(1), hits.Load(), "缺省行应触发一次 whoami")
		ext, err := store.FindAccountExtByCodexKey(ctx, "w@example.com", "acc-whoami-2")
		require.NoError(t, err, "派生值参与幂等键并落库")
		require.Equal(t, "acc-whoami-2", *ext.CodexAccountID)
	})

	t.Run("whoami failure keeps row-level failure", func(t *testing.T) {
		h, _, _, _ := codexImportTestAPI(t)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer srv.Close()
		t.Setenv("CODEX_AUTHAPI_BASE_URL", srv.URL)

		rec := doImport(t, h, http.MethodPost, "/api/admin/accounts/batch-import-codex-pat", `{
			"items": [{"codex_email":"f@example.com","codex_account_id":"","codex_pat_key":"pat-f-1"}],
			"template_id": `+itoa(patTplID)+`}`)
		require.Equal(t, 200, rec.Code)
		var out ImportResult
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		require.Zero(t, out.Imported)
		require.Len(t, out.Failed, 1)
		require.Equal(t, "codex_account_id 必填", out.Failed[0].Error, "whoami 失败 → 原文案行级 failed")
	})

	t.Run("provided id skips whoami", func(t *testing.T) {
		h, _, _, _ := codexImportTestAPI(t)
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		t.Setenv("CODEX_AUTHAPI_BASE_URL", srv.URL)

		rec := doImport(t, h, http.MethodPost, "/api/admin/accounts/batch-import-codex-pat", `{
			"items": [{"codex_email":"s@example.com","codex_account_id":"acc-given-3","codex_pat_key":"pat-s-1"}],
			"template_id": `+itoa(patTplID)+`}`)
		require.Equal(t, 200, rec.Code, "body: %s", rec.Body.String())
		var out ImportResult
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
		require.Equal(t, 1, out.Imported)
		require.Empty(t, out.Failed)
		require.Zero(t, hits.Load(), "显式 id → 零 whoami 出站")
	})
}
