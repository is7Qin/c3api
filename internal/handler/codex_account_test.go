// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCodexOAuthImportAndUsageEndpoints(t *testing.T) {
	var mu sync.Mutex
	var accountHeaders []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "Bearer access-secret", r.Header.Get("Authorization"))
		require.Equal(t, "codex_cli_rs", r.Header.Get("originator"))
		mu.Lock()
		accountHeaders = append(accountHeaders, r.Header.Get("chatgpt-account-id"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			_, _ = w.Write([]byte(`{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":25}}}`))
		case "/backend-api/wham/rate-limit-reset-credits":
			_, _ = w.Write([]byte(`{"available_count":3}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)

	h, _, do := newListTestRouter(t)
	h.svc.SetCodexUsageHTTPClient(upstream.Client())
	rec := do(http.MethodPost, "/admin/templates", `{"name":"codex-import","base_url":"`+upstream.URL+`","credential_type":"codex-oauth","supported_formats":["openai-responses"]}`)
	require.Equal(t, http.StatusOK, rec.Code, "create template: %s", rec.Body.String())
	var template Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &template))

	body := `{"template_id":` + strconv.FormatInt(template.ID, 10) + `,"name_prefix":"company","credentials":{"accounts":[{"access_token":"access-secret","refresh_token":"refresh-secret","account_id":"external-company","email":"ops@example.com"}]}}`
	rec = do(http.MethodPost, "/admin/accounts/import/codex-oauth", body)
	require.Equal(t, http.StatusOK, rec.Code, "import: %s", rec.Body.String())
	require.NotContains(t, rec.Body.String(), "access-secret")
	require.NotContains(t, rec.Body.String(), "refresh-secret")
	var imported CodexOAuthImportResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &imported))
	require.Equal(t, 1, imported.Imported)
	require.Len(t, imported.Items, 1)
	require.NotNil(t, imported.Items[0].AccountId)
	accountID := *imported.Items[0].AccountId

	rec = do(http.MethodGet, "/admin/accounts/"+strconv.FormatInt(accountID, 10)+"/codex/usage", "")
	require.Equal(t, http.StatusOK, rec.Code, "get usage: %s", rec.Body.String())
	var usage CodexUsageResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &usage))
	require.Equal(t, accountID, usage.AccountId)
	require.Equal(t, "external-company", usage.CodexAccountId)
	require.NotNil(t, usage.ResetCredits)

	rec = do(http.MethodPost, "/admin/accounts/codex/refresh-usage", `{"account_ids":[`+strconv.FormatInt(accountID, 10)+`,`+strconv.FormatInt(accountID, 10)+`]}`)
	require.Equal(t, http.StatusOK, rec.Code, "batch refresh: %s", rec.Body.String())
	var batch CodexUsageBatchResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &batch))
	require.Len(t, batch.Items, 1, "duplicate ids are normalized")
	require.Equal(t, CodexUsageBatchItemStatusRefreshed, batch.Items[0].Status)

	mu.Lock()
	require.NotEmpty(t, accountHeaders)
	for _, accountID := range accountHeaders {
		require.Equal(t, "external-company", accountID)
	}
	mu.Unlock()
}
