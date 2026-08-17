// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

func codexUsageFixture(t *testing.T, upstream http.Handler) (*Service, *fakeStore) {
	t.Helper()
	server := httptest.NewServer(upstream)
	t.Cleanup(server.Close)
	store := newFakeStore()
	store.tpls[1] = &domain.Template{
		ID: 1, Name: "codex", BaseURL: server.URL,
		CredentialType:   credential.TypeCodexOAuth,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
	}
	store.accs[2] = &domain.Account{ID: 2, Name: "account", TemplateID: 1, Status: domain.StatusActive, MaxConcurrency: 8}
	accessToken, refreshToken, accountID := "at-old", "rt-old", "external-2"
	store.accExts[2] = &domain.AccountExt{
		AccountID: 2, CredentialType: credential.TypeCodexOAuth, CodexAccountID: &accountID,
		InstallationID: "installation", OAuthToken: &accessToken, OAuthRefreshToken: &refreshToken,
	}
	service := &Service{store: store, inv: NopInvalidator{}}
	service.SetCodexUsageHTTPClient(server.Client())
	return service, store
}

func TestCodexUsageCachesAndRefreshesUnauthorizedToken(t *testing.T) {
	var mu sync.Mutex
	var usageCalls, resetCalls int
	var accountHeader string
	refresh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/oauth/token", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at-new","refresh_token":"rt-new","expires_in":3600}`))
	}))
	t.Cleanup(refresh.Close)

	service, store := codexUsageFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		accountHeader = r.Header.Get("chatgpt-account-id")
		switch r.URL.Path {
		case "/backend-api/wham/usage":
			usageCalls++
			if r.Header.Get("Authorization") == "Bearer at-old" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"plan_type":"plus","rate_limit":{"primary_window":{"used_percent":12}}}`))
		case "/backend-api/wham/rate-limit-reset-credits":
			resetCalls++
			_, _ = w.Write([]byte(`{"available_count":3}`))
		default:
			http.NotFound(w, r)
		}
	}))
	service.getCodexUsageManager().refreshURL = func() string { return refresh.URL + "/oauth/token" }

	first, err := service.GetCodexUsage(context.Background(), 2)
	require.NoError(t, err)
	require.JSONEq(t, `{"available_count":3}`, string(first.ResetCredits))
	_, err = service.GetCodexUsage(context.Background(), 2)
	require.NoError(t, err)
	mu.Lock()
	require.Equal(t, 2, usageCalls, "one unauthorized request and one retry are cached")
	require.Equal(t, 1, resetCalls)
	require.Equal(t, "external-2", accountHeader)
	mu.Unlock()

	store.mu.Lock()
	require.Equal(t, "at-new", *store.accExts[2].OAuthToken)
	require.Equal(t, "rt-new", *store.accExts[2].OAuthRefreshToken)
	store.mu.Unlock()
}
