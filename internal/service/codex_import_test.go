// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

func TestImportCodexOAuthUsesEmailAndSpaceIdentity(t *testing.T) {
	store := newFakeStore()
	store.tpls[1] = &domain.Template{
		ID: 1, Name: "codex", BaseURL: "https://codex.example",
		CredentialType:   credential.TypeCodexOAuth,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
	}
	svc := &Service{store: store, inv: NopInvalidator{}}

	result, err := svc.ImportCodexOAuth(context.Background(), []byte(`{"credentials":[
		{"access_token":"at-a","refresh_token":"rt-a","account_id":" shared-space ","email":" A@example.com "},
		{"access_token":"at-a-duplicate","refresh_token":"rt-a-duplicate","account_id":"shared-space","email":"a@example.com"},
		{"accessToken":"at-b","refreshToken":"rt-b","accountId":"shared-space","email":"b@example.com","expires_in":3600},
		{"access_token":"at-a-other-space","refresh_token":"rt-a-other-space","space_id":"other-space","email":"a@example.com"}
	]}`), CodexOAuthImportOptions{TemplateID: 1})
	require.NoError(t, err)
	assert.Equal(t, 3, result.Imported)
	assert.Equal(t, 1, result.Skipped)
	assert.Zero(t, result.Updated)
	assert.Zero(t, result.Failed)
	require.Len(t, store.accs, 3)
	require.Len(t, store.accExts, 3)

	var importedID int64
	identities := make(map[string]bool, len(store.accExts))
	for id, ext := range store.accExts {
		require.NotNil(t, ext.CodexEmail)
		require.NotNil(t, ext.CodexAccountID)
		require.NotNil(t, ext.CodexIdentity)
		identities[*ext.CodexEmail+"\x00"+*ext.CodexAccountID] = true
		if *ext.CodexEmail == "a@example.com" && *ext.CodexAccountID == "shared-space" {
			importedID = id
		}
	}
	assert.True(t, identities["a@example.com\x00shared-space"])
	assert.True(t, identities["b@example.com\x00shared-space"])
	assert.True(t, identities["a@example.com\x00other-space"])
	require.NotZero(t, importedID)

	result, err = svc.ImportCodexOAuth(context.Background(), []byte(`{"accounts":[{"access_token":"at-new","refresh_token":"rt-new","account_id":"shared-space","email":"A@example.com"}]}`), CodexOAuthImportOptions{TemplateID: 1})
	require.NoError(t, err)
	assert.Equal(t, 1, result.Updated)
	require.Len(t, result.Items, 1)
	require.NotNil(t, result.Items[0].AccountID)
	assert.Equal(t, importedID, *result.Items[0].AccountID)
	assert.Equal(t, "at-new", *store.accExts[importedID].CodexOAuthToken)
	assert.Equal(t, "rt-new", *store.accExts[importedID].CodexOAuthRefreshToken)
}

func TestParseCodexOAuthJSONUsesAuthJSONAndJWTClaims(t *testing.T) {
	accessToken := codexTestJWT(t, `{"https://api.openai.com/auth.chatgpt_account_id":"jwt-space","email":"JWT@example.com"}`)
	idToken := codexTestJWT(t, `{"https://api.openai.com/profile":{"email":"Codex.User@example.com"}}`)
	inputs, err := parseCodexOAuthJSON([]byte(`[
		{"access_token":"` + accessToken + `","refresh_token":"rt-jwt"},
		{"tokens":{"id_token":"` + idToken + `","access_token":"at-auth-json","refresh_token":"rt-auth-json","account_id":"auth-json-space"}},
		{"tokens":{"access_token":"at-nested","refreshToken":"rt-nested"},"chatgpt_account_id":"nested-space","email":"nested@example.com","expiresIn":999999999999999}
	]`))
	require.NoError(t, err)
	require.Len(t, inputs, 3)
	assert.Equal(t, "jwt-space", inputs[0].SpaceID)
	assert.Equal(t, "jwt@example.com", inputs[0].Email)
	assert.Equal(t, "auth-json-space", inputs[1].SpaceID)
	assert.Equal(t, "codex.user@example.com", inputs[1].Email)
	assert.Equal(t, "at-auth-json", inputs[1].AccessToken)
	assert.Equal(t, "rt-auth-json", inputs[1].RefreshToken)
	assert.Equal(t, "nested-space", inputs[2].SpaceID)
	require.NotNil(t, inputs[2].ExpiresAt)
	assert.WithinDuration(t, time.Now().Add(codexMaxExpiresIn), *inputs[2].ExpiresAt, 2*time.Second)
}

func codexTestJWT(t *testing.T, payload string) string {
	t.Helper()
	encoded := base64.RawURLEncoding.EncodeToString([]byte(payload))
	return "eyJhbGciOiJub25lIn0." + encoded + ".signature"
}
