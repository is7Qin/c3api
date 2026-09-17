// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// —— 单账号 ext 保存 account id 后置补全（spec §7.0-6 / §7.2：可留空，保存后
// 自动识别——派生失败不阻塞保存，留空 → 下次保存重试） ——

// extCodexAccountJWT 构造含 chatgpt_account_id claim 的 JWT（离线解析只解
// payload 不验签）。
func extCodexAccountJWT(t *testing.T, accountID string) string {
	t.Helper()
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." +
		enc([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"`+accountID+`"}}`)) + ".sig"
}

// extCodexNameSeq 单测内模板/账号名唯一后缀（同 store 多次建模板防重名 409）。
var extCodexNameSeq atomic.Int64

// extCodexOAuthAccount 建 oauth 模板 + 账号，返回账号 id。
func extCodexOAuthAccount(t *testing.T, do func(method, path, body string) *httptest.ResponseRecorder) int64 {
	t.Helper()
	n := extCodexNameSeq.Add(1)
	rec := do(http.MethodPost, "/api/admin/templates", fmt.Sprintf(`{
		"name":"t-codex-acctid-%d","base_url":"",
		"credential_type":"codex-oauth","supported_formats":["openai-responses"]}`, n))
	require.Equal(t, 200, rec.Code, "create template: %s", rec.Body.String())
	var tpl Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))
	rec = do(http.MethodPost, "/api/admin/accounts",
		`{"name":"acc-acctid-`+itoa64(n)+`","template_id":`+itoa64(tpl.ID)+`,"upstream_key":"sk-x"}`)
	require.Equal(t, 200, rec.Code, "create account: %s", rec.Body.String())
	var acc Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))
	return *acc.ID
}

// TestPutAccountsExtDerivesOAuthAccountID OAuth 保存空 account id：请求凭据可派
// 生 → 补全并持久化；不可派生 → 保存成功且留空；请求凭据不可派生但存量行可 →
// 存量回退补全。
func TestPutAccountsExtDerivesOAuthAccountID(t *testing.T) {
	_, _, do := newListTestRouter(t)
	id := extCodexOAuthAccount(t, do)

	t.Run("request token derives and persists", func(t *testing.T) {
		rec := do(http.MethodPut, "/api/admin/accounts/"+itoa64(id)+"/ext",
			`{"credential_type":"codex-oauth","codex_oauth_token":"`+extCodexAccountJWT(t, "acc-save-1")+`","codex_oauth_refresh_token":"rt"}`)
		require.Equal(t, 200, rec.Code, "put ext: %s", rec.Body.String())
		var ext AccountExt
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ext))
		require.NotNil(t, ext.CodexAccountId)
		require.Equal(t, "acc-save-1", *ext.CodexAccountId)

		rec = do(http.MethodGet, "/api/admin/accounts/"+itoa64(id)+"/ext", "")
		require.Equal(t, 200, rec.Code, "get ext: %s", rec.Body.String())
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ext))
		require.NotNil(t, ext.CodexAccountId)
		require.Equal(t, "acc-save-1", *ext.CodexAccountId, "派生值持久化")
	})

	t.Run("stored row backs up underivable request token", func(t *testing.T) {
		// 存量行持有可派生 JWT-A；请求换不可派生 token → 存量回退补全 id。
		rec := do(http.MethodPut, "/api/admin/accounts/"+itoa64(id)+"/ext",
			`{"credential_type":"codex-oauth","codex_oauth_token":"opaque-rotated","codex_oauth_refresh_token":"rt2"}`)
		require.Equal(t, 200, rec.Code, "put ext: %s", rec.Body.String())
		var ext AccountExt
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ext))
		require.NotNil(t, ext.CodexAccountId)
		require.Equal(t, "acc-save-1", *ext.CodexAccountId, "请求凭据不可派生 → 存量行回退")
	})

	t.Run("derivation failure keeps save successful and empty", func(t *testing.T) {
		id2 := extCodexOAuthAccount(t, do)
		rec := do(http.MethodPut, "/api/admin/accounts/"+itoa64(id2)+"/ext",
			`{"credential_type":"codex-oauth","codex_oauth_token":"opaque","codex_oauth_refresh_token":"rt"}`)
		require.Equal(t, 200, rec.Code, "put ext: %s", rec.Body.String())
		var ext AccountExt
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ext))
		require.Nil(t, ext.CodexAccountId, "派生失败不阻塞保存，留空 → 下次保存重试")
	})
}

// TestPutAccountsExtDerivesPATAccountID PAT 保存空 account id：whoami 成功 →
// 补全并持久化；whoami 失败 → 保存成功且留空。
func TestPutAccountsExtDerivesPATAccountID(t *testing.T) {
	newPATAccount := func(t *testing.T, do func(method, path, body string) *httptest.ResponseRecorder) int64 {
		t.Helper()
		n := extCodexNameSeq.Add(1)
		rec := do(http.MethodPost, "/api/admin/templates", fmt.Sprintf(`{
			"name":"t-codex-pat-acctid-%d","base_url":"",
			"credential_type":"codex-pat","supported_formats":["openai-responses"]}`, n))
		require.Equal(t, 200, rec.Code, "create template: %s", rec.Body.String())
		var tpl Template
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))
		rec = do(http.MethodPost, "/api/admin/accounts",
			`{"name":"acc-pat-acctid-`+itoa64(n)+`","template_id":`+itoa64(tpl.ID)+`,"upstream_key":"sk-x"}`)
		require.Equal(t, 200, rec.Code, "create account: %s", rec.Body.String())
		var acc Account
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))
		return *acc.ID
	}

	t.Run("whoami success derives and persists", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"email":"u@example.com","chatgpt_account_id":"acc-pat-save-1"}`))
		}))
		defer srv.Close()
		t.Setenv("CODEX_AUTHAPI_BASE_URL", srv.URL)

		_, _, do := newListTestRouter(t)
		id := newPATAccount(t, do)
		rec := do(http.MethodPut, "/api/admin/accounts/"+itoa64(id)+"/ext",
			`{"credential_type":"codex-pat","codex_pat_key":"pat-save-1"}`)
		require.Equal(t, 200, rec.Code, "put ext: %s", rec.Body.String())
		var ext AccountExt
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ext))
		require.NotNil(t, ext.CodexAccountId)
		require.Equal(t, "acc-pat-save-1", *ext.CodexAccountId)

		rec = do(http.MethodGet, "/api/admin/accounts/"+itoa64(id)+"/ext", "")
		require.Equal(t, 200, rec.Code, "get ext: %s", rec.Body.String())
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ext))
		require.Equal(t, "acc-pat-save-1", *ext.CodexAccountId, "派生值持久化")
	})

	t.Run("whoami failure keeps save successful and empty", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"bad key"}`))
		}))
		defer srv.Close()
		t.Setenv("CODEX_AUTHAPI_BASE_URL", srv.URL)

		_, _, do := newListTestRouter(t)
		id := newPATAccount(t, do)
		rec := do(http.MethodPut, "/api/admin/accounts/"+itoa64(id)+"/ext",
			`{"credential_type":"codex-pat","codex_pat_key":"pat-bad"}`)
		require.Equal(t, 200, rec.Code, "put ext: %s", rec.Body.String())
		var ext AccountExt
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ext))
		require.Nil(t, ext.CodexAccountId, "whoami 失败不阻塞保存，留空")
	})
}
