// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

func TestImportCodexOAuthImportsDeduplicatesAndUpdates(t *testing.T) {
	store := newFakeStore()
	store.tpls[1] = &domain.Template{
		ID: 1, Name: "codex", BaseURL: "https://codex.example",
		CredentialType:   credential.TypeCodexOAuth,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
	}
	service := &Service{store: store, inv: NopInvalidator{}}

	result, err := service.ImportCodexOAuth(context.Background(), []byte(`{"credentials":[
		{"access_token":"at-a","refresh_token":"rt-a","account_id":"external-a","email":"a@example.com"},
		{"access_token":"at-a-duplicate","refresh_token":"rt-a-duplicate","account_id":"external-a"},
		{"accessToken":"at-b","refreshToken":"rt-b","accountId":"external-b","expires_in":3600}
	]}`), CodexOAuthImportOptions{TemplateID: 1})
	require.NoError(t, err)
	require.Equal(t, 2, result.Imported)
	require.Equal(t, 1, result.Skipped)
	require.Zero(t, result.Updated)
	require.Zero(t, result.Failed)
	require.Len(t, store.accs, 2)
	require.Len(t, store.accExts, 2)

	var importedID int64
	for id, ext := range store.accExts {
		if ext.CodexAccountID != nil && *ext.CodexAccountID == "external-a" {
			importedID = id
		}
	}
	require.NotZero(t, importedID)

	result, err = service.ImportCodexOAuth(context.Background(), []byte(`{"accounts":[{"access_token":"at-new","refresh_token":"rt-new","account_id":"external-a"}]}`), CodexOAuthImportOptions{TemplateID: 1})
	require.NoError(t, err)
	require.Equal(t, 1, result.Updated)
	require.Equal(t, "at-new", *store.accExts[importedID].OAuthToken)
	require.Equal(t, "rt-new", *store.accExts[importedID].OAuthRefreshToken)
}

func TestParseCodexOAuthJSONUsesJWTClaimsAndBoundsExpiry(t *testing.T) {
	token := codexTestJWT(t, `{"https://api.openai.com/auth.chatgpt_account_id":"jwt-account","email":"jwt@example.com"}`)
	inputs, err := parseCodexOAuthJSON([]byte(`[
		{"access_token":"` + token + `","refresh_token":"rt-jwt"},
		{"tokens":{"access_token":"at-nested","refreshToken":"rt-nested"},"chatgpt_account_id":"nested-account","expiresIn":999999999999999}
	]`))
	require.NoError(t, err)
	require.Len(t, inputs, 2)
	require.Equal(t, "jwt-account", inputs[0].AccountID)
	require.Equal(t, "jwt@example.com", inputs[0].Email)
	require.Equal(t, "nested-account", inputs[1].AccountID)
	require.NotNil(t, inputs[1].ExpiresAt)
	require.WithinDuration(t, time.Now().Add(codexMaxExpiresIn), *inputs[1].ExpiresAt, 2*time.Second)
}

func codexTestJWT(t *testing.T, payload string) string {
	t.Helper()
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return "eyJhbGciOiJub25lIn0." + encoded + ".signature"
}
