// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCodexOAuthImportEndpointUsesEmailAndSpaceIdentity(t *testing.T) {
	_, _, do := newListTestRouter(t)
	rec := do(http.MethodPost, "/admin/templates", `{"name":"codex-import","base_url":"https://codex.example","credential_type":"codex-oauth","supported_formats":["openai-responses"]}`)
	require.Equal(t, http.StatusOK, rec.Code, "create template: %s", rec.Body.String())
	var template Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &template))

	body := `{"template_id":` + strconv.FormatInt(template.ID, 10) + `,"name_prefix":"company","credentials":{"accounts":[` +
		`{"access_token":"access-secret-a","refresh_token":"refresh-secret-a","account_id":"shared-space","email":"ops-a@example.com"},` +
		`{"access_token":"access-secret-b","refresh_token":"refresh-secret-b","account_id":"shared-space","email":"ops-b@example.com"}]}}`
	rec = do(http.MethodPost, "/admin/accounts/import/codex-oauth", body)
	require.Equal(t, http.StatusOK, rec.Code, "import: %s", rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "access-secret")
	assert.NotContains(t, rec.Body.String(), "refresh-secret")
	var imported CodexOAuthImportResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &imported))
	assert.Equal(t, 2, imported.Imported)
	assert.Zero(t, imported.Updated)
	require.Len(t, imported.Items, 2)
	assert.Equal(t, "shared-space", deref(imported.Items[0].SpaceId))
	assert.Equal(t, "shared-space", deref(imported.Items[1].SpaceId))
	require.NotNil(t, imported.Items[0].AccountId)
	require.NotNil(t, imported.Items[1].AccountId)
	assert.NotEqual(t, *imported.Items[0].AccountId, *imported.Items[1].AccountId)
}
