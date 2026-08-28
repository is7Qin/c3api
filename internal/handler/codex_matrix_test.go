// SPDX-License-Identifier: AGPL-3.0-or-later
package handler

import (
	"encoding/json"
	"net/http"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestAccountMinimalPutWithoutStatus(t *testing.T) {
	_, _, do := newListTestRouter(t)

	rec := do(http.MethodPost, "/api/admin/templates", `{"name":"t1","base_url":"https://api.example.com","supported_formats":["openai-chat"],"credential_type":"api_key"}`)
	require.Equal(t, 200, rec.Code, "create template: %s", rec.Body.String())
	var tpl domain.Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))

	rec = do(http.MethodPost, "/api/admin/accounts", `{"name":"acc1","template_id":`+strconv.FormatInt(tpl.ID, 10)+`,"upstream_key":"sk-x","status":"active"}`)
	require.Equal(t, 200, rec.Code, "create with status: %s", rec.Body.String())
	var acc domain.Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))
	require.Equal(t, domain.StatusActive, acc.Status)

	putID := strconv.FormatInt(acc.ID, 10)
	rec = do(http.MethodPut, "/api/admin/accounts/"+putID, `{"name":"acc1","template_id":`+strconv.FormatInt(tpl.ID, 10)+`,"upstream_key":"sk-x"}`)
	require.Equal(t, 200, rec.Code, "minimal PUT without status must not 500: %s", rec.Body.String())
	var updated domain.Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	require.Equal(t, domain.StatusActive, updated.Status, "omitted status must default to active")

	rec = do(http.MethodGet, "/api/admin/accounts/"+putID, "")
	require.Equal(t, 200, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	require.Equal(t, domain.StatusActive, updated.Status)

	rec = do(http.MethodPut, "/api/admin/accounts/"+putID, `{"name":"acc1","template_id":`+strconv.FormatInt(tpl.ID, 10)+`,"upstream_key":"sk-x","status":"bogus"}`)
	require.Equal(t, 400, rec.Code, "invalid status must 400 not 500: %s", rec.Body.String())
}

func TestCodexTemplateBaseURLMatrix(t *testing.T) {
	for _, cred := range []string{"codex-oauth", "codex-pat"} {
		t.Run(cred, func(t *testing.T) {
			_, _, do := newListTestRouter(t)
			// create nonempty -> 400
			rec := do(http.MethodPost, "/api/admin/templates", `{"name":"t-`+cred+`-bad","base_url":"https://example.com","credential_type":"`+cred+`","supported_formats":["openai-responses"]}`)
			require.Equal(t, 400, rec.Code, "codex template create nonempty base_url must 400: %s", rec.Body.String())
			// whitespace -> 400
			rec = do(http.MethodPost, "/api/admin/templates", `{"name":"t-`+cred+`-ws","base_url":"   ","credential_type":"`+cred+`","supported_formats":["openai-responses"]}`)
			require.Equal(t, 400, rec.Code, "codex template create whitespace base_url must 400: %s", rec.Body.String())
			// empty -> 200
			rec = do(http.MethodPost, "/api/admin/templates", `{"name":"t-`+cred+`-ok","base_url":"","credential_type":"`+cred+`","supported_formats":["openai-responses"]}`)
			require.Equal(t, 200, rec.Code, "codex template create empty base_url must 200: %s", rec.Body.String())
			var tpl domain.Template
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))
			// PUT nonempty -> 400
			rec = do(http.MethodPut, "/api/admin/templates/"+strconv.FormatInt(tpl.ID, 10), `{"name":"t-`+cred+`-ok","base_url":"https://example.com","credential_type":"`+cred+`","supported_formats":["openai-responses"]}`)
			require.Equal(t, 400, rec.Code, "codex template PUT nonempty must 400: %s", rec.Body.String())
			// PUT whitespace -> 400
			rec = do(http.MethodPut, "/api/admin/templates/"+strconv.FormatInt(tpl.ID, 10), `{"name":"t-`+cred+`-ok","base_url":"   ","credential_type":"`+cred+`","supported_formats":["openai-responses"]}`)
			require.Equal(t, 400, rec.Code, "codex template PUT whitespace must 400: %s", rec.Body.String())
			// PUT empty -> 200
			rec = do(http.MethodPut, "/api/admin/templates/"+strconv.FormatInt(tpl.ID, 10), `{"name":"t-`+cred+`-ok","base_url":"","credential_type":"`+cred+`","supported_formats":["openai-responses"]}`)
			require.Equal(t, 200, rec.Code, "codex template PUT empty must 200: %s", rec.Body.String())
			// batch nonempty -> 400 atomic
			rec = do(http.MethodPost, "/api/admin/templates/batch-update", `{"ids":[`+strconv.FormatInt(tpl.ID, 10)+`],"fields":{"base_url":"https://example.com"}}`)
			require.Equal(t, 400, rec.Code, "codex template batch nonempty must 400: %s", rec.Body.String())
			// verify unchanged
			rec = do(http.MethodGet, "/api/admin/templates/"+strconv.FormatInt(tpl.ID, 10), "")
			require.Equal(t, 200, rec.Code)
			var got domain.Template
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
			require.Equal(t, "", got.BaseURL, "failed batch must not mutate")
			// batch whitespace -> 400
			rec = do(http.MethodPost, "/api/admin/templates/batch-update", `{"ids":[`+strconv.FormatInt(tpl.ID, 10)+`],"fields":{"base_url":"   "}}`)
			require.Equal(t, 400, rec.Code, "codex template batch whitespace must 400: %s", rec.Body.String())
			// batch empty -> 200
			rec = do(http.MethodPost, "/api/admin/templates/batch-update", `{"ids":[`+strconv.FormatInt(tpl.ID, 10)+`],"fields":{"base_url":""}}`)
			require.Equal(t, 200, rec.Code, "codex template batch empty must 200: %s", rec.Body.String())
			// null omitted (empty fields with name) -> success (base_url not mutated)
			rec = do(http.MethodPost, "/api/admin/templates/batch-update", `{"ids":[`+strconv.FormatInt(tpl.ID, 10)+`],"fields":{"name":"t-`+cred+`-ok2"}}`)
			require.Equal(t, 200, rec.Code, "codex template batch null base_url must succeed: %s", rec.Body.String())
		})
	}
}

func TestCodexAccountBaseURLMatrix(t *testing.T) {
	for _, cred := range []string{"codex-oauth", "codex-pat"} {
		t.Run(cred, func(t *testing.T) {
			_, _, do := newListTestRouter(t)
			// create codex template
			rec := do(http.MethodPost, "/api/admin/templates", `{"name":"t-`+cred+`","credential_type":"`+cred+`","supported_formats":["openai-responses"]}`)
			require.Equal(t, 200, rec.Code, "create codex template: %s", rec.Body.String())
			var tpl domain.Template
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))
			// account create nonempty -> 400
			rec = do(http.MethodPost, "/api/admin/accounts", `{"name":"a-bad","template_id":`+strconv.FormatInt(tpl.ID, 10)+`,"upstream_key":"sk-x","base_url":"https://example.com"}`)
			require.Equal(t, 400, rec.Code, "codex account create nonempty base_url must 400: %s", rec.Body.String())
			// whitespace nonempty -> 400 (via URL validation)
			rec = do(http.MethodPost, "/api/admin/accounts", `{"name":"a-ws","template_id":`+strconv.FormatInt(tpl.ID, 10)+`,"upstream_key":"sk-x","base_url":"   "}`)
			require.Equal(t, 400, rec.Code, "codex account create whitespace must 400: %s", rec.Body.String())
			// empty -> 200 (inherits)
			rec = do(http.MethodPost, "/api/admin/accounts", `{"name":"a-ok","template_id":`+strconv.FormatInt(tpl.ID, 10)+`,"upstream_key":"sk-x","base_url":""}`)
			require.Equal(t, 200, rec.Code, "codex account create empty must 200: %s", rec.Body.String())
			var acc domain.Account
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))
			require.Nil(t, acc.BaseURL, "empty base_url must be nil (inherit)")
			// null omitted -> 200
			rec = do(http.MethodPost, "/api/admin/accounts", `{"name":"a-null","template_id":`+strconv.FormatInt(tpl.ID, 10)+`,"upstream_key":"sk-x"}`)
			require.Equal(t, 200, rec.Code, "codex account create null must 200: %s", rec.Body.String())
			var acc2 domain.Account
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc2))
			// PUT nonempty -> 400
			rec = do(http.MethodPut, "/api/admin/accounts/"+strconv.FormatInt(acc.ID, 10), `{"name":"a-ok","template_id":`+strconv.FormatInt(tpl.ID, 10)+`,"upstream_key":"sk-x","base_url":"https://example.com"}`)
			require.Equal(t, 400, rec.Code, "codex account PUT nonempty must 400: %s", rec.Body.String())
			// PUT whitespace -> 400
			rec = do(http.MethodPut, "/api/admin/accounts/"+strconv.FormatInt(acc.ID, 10), `{"name":"a-ok","template_id":`+strconv.FormatInt(tpl.ID, 10)+`,"upstream_key":"sk-x","base_url":"   "}`)
			require.Equal(t, 400, rec.Code, "codex account PUT whitespace must 400: %s", rec.Body.String())
			// PUT empty -> 200
			rec = do(http.MethodPut, "/api/admin/accounts/"+strconv.FormatInt(acc.ID, 10), `{"name":"a-ok","template_id":`+strconv.FormatInt(tpl.ID, 10)+`,"upstream_key":"sk-x","base_url":""}`)
			require.Equal(t, 200, rec.Code, "codex account PUT empty must 200: %s", rec.Body.String())
			// PUT null -> 200
			rec = do(http.MethodPut, "/api/admin/accounts/"+strconv.FormatInt(acc.ID, 10), `{"name":"a-ok","template_id":`+strconv.FormatInt(tpl.ID, 10)+`,"upstream_key":"sk-x"}`)
			require.Equal(t, 200, rec.Code, "codex account PUT null must 200: %s", rec.Body.String())
			// batch nonempty -> 400 atomic
			rec = do(http.MethodPost, "/api/admin/accounts/batch-update", `{"ids":[`+strconv.FormatInt(acc.ID, 10)+`],"fields":{"base_url":"https://example.com"}}`)
			require.Equal(t, 400, rec.Code, "codex account batch nonempty must 400: %s", rec.Body.String())
			// batch whitespace -> 400
			rec = do(http.MethodPost, "/api/admin/accounts/batch-update", `{"ids":[`+strconv.FormatInt(acc.ID, 10)+`],"fields":{"base_url":"   "}}`)
			require.Equal(t, 400, rec.Code, "codex account batch whitespace must 400: %s", rec.Body.String())
			// batch empty clear -> 200 (clears to inherit)
			rec = do(http.MethodPost, "/api/admin/accounts/batch-update", `{"ids":[`+strconv.FormatInt(acc.ID, 10)+`],"fields":{"base_url":""}}`)
			require.Equal(t, 200, rec.Code, "codex account batch empty must 200: %s", rec.Body.String())
			// batch null (other field) -> 200
			rec = do(http.MethodPost, "/api/admin/accounts/batch-update", `{"ids":[`+strconv.FormatInt(acc.ID, 10)+`],"fields":{"name":"a-ok2"}}`)
			require.Equal(t, 200, rec.Code, "codex account batch null base_url must 200: %s", rec.Body.String())
		})
	}
}

func TestNonCodexBaseURLAccepted(t *testing.T) {
	_, _, do := newListTestRouter(t)
	// api_key template nonempty accepted
	rec := do(http.MethodPost, "/api/admin/templates", `{"name":"t-api","base_url":"https://api.example.com","supported_formats":["openai-chat"],"credential_type":"api_key"}`)
	require.Equal(t, 200, rec.Code, "api_key template nonempty must 200: %s", rec.Body.String())
	var tpl domain.Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))
	rec = do(http.MethodPut, "/api/admin/templates/"+strconv.FormatInt(tpl.ID, 10), `{"name":"t-api","base_url":"https://api.example.com/v2","credential_type":"api_key","supported_formats":["openai-chat"]}`)
	require.Equal(t, 200, rec.Code, "api_key template PUT nonempty must 200: %s", rec.Body.String())
	rec = do(http.MethodPost, "/api/admin/templates/batch-update", `{"ids":[`+strconv.FormatInt(tpl.ID, 10)+`],"fields":{"base_url":"https://api.example.com/v3"}}`)
	require.Equal(t, 200, rec.Code, "api_key template batch nonempty must 200: %s", rec.Body.String())
	// account with api_key nonempty accepted
	rec = do(http.MethodPost, "/api/admin/accounts", `{"name":"a-api","template_id":`+strconv.FormatInt(tpl.ID, 10)+`,"upstream_key":"sk-x","base_url":"https://override.example.com"}`)
	require.Equal(t, 200, rec.Code, "api_key account nonempty must 200: %s", rec.Body.String())
	var acc domain.Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))
	require.NotNil(t, acc.BaseURL)
	require.Equal(t, "https://override.example.com", *acc.BaseURL)
	rec = do(http.MethodPut, "/api/admin/accounts/"+strconv.FormatInt(acc.ID, 10), `{"name":"a-api","template_id":`+strconv.FormatInt(tpl.ID, 10)+`,"upstream_key":"sk-x","base_url":"https://override2.example.com"}`)
	require.Equal(t, 200, rec.Code, "api_key account PUT nonempty must 200: %s", rec.Body.String())
	rec = do(http.MethodPost, "/api/admin/accounts/batch-update", `{"ids":[`+strconv.FormatInt(acc.ID, 10)+`],"fields":{"base_url":"https://override3.example.com"}}`)
	require.Equal(t, 200, rec.Code, "api_key account batch nonempty must 200: %s", rec.Body.String())
	// responses-special also non-codex
	rec = do(http.MethodPost, "/api/admin/templates", `{"name":"t-special","base_url":"https://api.example.com","credential_type":"responses-special","supported_formats":["openai-responses"]}`)
	require.Equal(t, 200, rec.Code, "responses-special template nonempty must 200: %s", rec.Body.String())
	var tpl2 domain.Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl2))
	rec = do(http.MethodPost, "/api/admin/accounts", `{"name":"a-special","template_id":`+strconv.FormatInt(tpl2.ID, 10)+`,"upstream_key":"sk-x","base_url":"https://override.example.com"}`)
	require.Equal(t, 200, rec.Code, "responses-special account nonempty must 200: %s", rec.Body.String())
}

func TestCodexTransitionsAndMixedBatchAtomic(t *testing.T) {
	_, _, do := newListTestRouter(t)
	// create api_key template with base_url
	rec := do(http.MethodPost, "/api/admin/templates", `{"name":"t-api","base_url":"https://api.example.com","supported_formats":["openai-chat"],"credential_type":"api_key"}`)
	require.Equal(t, 200, rec.Code, "create api template: %s", rec.Body.String())
	var tAPI domain.Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tAPI))
	// create codex template
	rec = do(http.MethodPost, "/api/admin/templates", `{"name":"t-codex","credential_type":"codex-oauth","supported_formats":["openai-responses"]}`)
	require.Equal(t, 200, rec.Code, "create codex template: %s", rec.Body.String())
	var tCodex domain.Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tCodex))
	// create account with api template and base_url override
	rec = do(http.MethodPost, "/api/admin/accounts", `{"name":"a1","template_id":`+strconv.FormatInt(tAPI.ID, 10)+`,"upstream_key":"sk-x","base_url":"https://override.example.com"}`)
	require.Equal(t, 200, rec.Code, "create a1: %s", rec.Body.String())
	var a1 domain.Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &a1))
	// create account with codex (inherits)
	rec = do(http.MethodPost, "/api/admin/accounts", `{"name":"a2","template_id":`+strconv.FormatInt(tCodex.ID, 10)+`,"upstream_key":"sk-x"}`)
	require.Equal(t, 200, rec.Code, "create a2: %s", rec.Body.String())
	var a2 domain.Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &a2))
	// transition: PUT a1 to codex template while keeping base_url -> must 400
	rec = do(http.MethodPut, "/api/admin/accounts/"+strconv.FormatInt(a1.ID, 10), `{"name":"a1","template_id":`+strconv.FormatInt(tCodex.ID, 10)+`,"upstream_key":"sk-x","base_url":"https://override.example.com"}`)
	require.Equal(t, 400, rec.Code, "account transition to codex with base_url must 400: %s", rec.Body.String())
	// verify unchanged
	rec = do(http.MethodGet, "/api/admin/accounts/"+strconv.FormatInt(a1.ID, 10), "")
	require.Equal(t, 200, rec.Code)
	var gotA1 domain.Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &gotA1))
	require.Equal(t, tAPI.ID, gotA1.TemplateID, "failed transition must not mutate")
	require.NotNil(t, gotA1.BaseURL)
	// transition: PUT a2 to api template with base_url -> 200 (non-codex accepts)
	rec = do(http.MethodPut, "/api/admin/accounts/"+strconv.FormatInt(a2.ID, 10), `{"name":"a2","template_id":`+strconv.FormatInt(tAPI.ID, 10)+`,"upstream_key":"sk-x","base_url":"https://override.example.com"}`)
	require.Equal(t, 200, rec.Code, "account transition from codex to api with base_url must 200: %s", rec.Body.String())
	// transition template: api template to codex with nonempty base_url -> 400
	rec = do(http.MethodPut, "/api/admin/templates/"+strconv.FormatInt(tAPI.ID, 10), `{"name":"t-api","base_url":"https://api.example.com","credential_type":"codex-oauth","supported_formats":["openai-responses"]}`)
	require.Equal(t, 400, rec.Code, "template transition api->codex with base_url must 400: %s", rec.Body.String())
	// create second codex template for mixed batch
	rec = do(http.MethodPost, "/api/admin/templates", `{"name":"t-codex2","credential_type":"codex-pat","supported_formats":["openai-responses"]}`)
	require.Equal(t, 200, rec.Code)
	var tCodex2 domain.Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tCodex2))
	// mixed template batch: one codex, one api, set base_url nonempty -> 400 atomic
	rec = do(http.MethodPost, "/api/admin/templates/batch-update", `{"ids":[`+strconv.FormatInt(tAPI.ID, 10)+`,`+strconv.FormatInt(tCodex.ID, 10)+`],"fields":{"base_url":"https://example.com"}}`)
	require.Equal(t, 400, rec.Code, "mixed template batch must 400: %s", rec.Body.String())
	// verify both unchanged (tAPI still has original, tCodex still empty)
	rec = do(http.MethodGet, "/api/admin/templates/"+strconv.FormatInt(tAPI.ID, 10), "")
	require.Equal(t, 200, rec.Code)
	var checkTAPI domain.Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &checkTAPI))
	require.Equal(t, "https://api.example.com", checkTAPI.BaseURL)
	rec = do(http.MethodGet, "/api/admin/templates/"+strconv.FormatInt(tCodex.ID, 10), "")
	require.Equal(t, 200, rec.Code)
	var checkTCodex domain.Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &checkTCodex))
	require.Equal(t, "", checkTCodex.BaseURL)
	// mixed account batch: a1 (api with base) and a2 (now api) but create new codex account
	rec = do(http.MethodPost, "/api/admin/accounts", `{"name":"a3","template_id":`+strconv.FormatInt(tCodex.ID, 10)+`,"upstream_key":"sk-x"}`)
	require.Equal(t, 200, rec.Code, "create a3 codex: %s", rec.Body.String())
	var a3 domain.Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &a3))
	// batch set base_url nonempty across api account a1 and codex account a3 -> 400 atomic
	rec = do(http.MethodPost, "/api/admin/accounts/batch-update", `{"ids":[`+strconv.FormatInt(a1.ID, 10)+`,`+strconv.FormatInt(a3.ID, 10)+`],"fields":{"base_url":"https://example.com"}}`)
	require.Equal(t, 400, rec.Code, "mixed account batch must 400: %s", rec.Body.String())
	// verify unchanged
	rec = do(http.MethodGet, "/api/admin/accounts/"+strconv.FormatInt(a1.ID, 10), "")
	require.Equal(t, 200, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &gotA1))
	require.Equal(t, "https://override.example.com", *gotA1.BaseURL)
	rec = do(http.MethodGet, "/api/admin/accounts/"+strconv.FormatInt(a3.ID, 10), "")
	require.Equal(t, 200, rec.Code)
	var gotA3 domain.Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &gotA3))
	require.Nil(t, gotA3.BaseURL, "codex account must remain nil after failed batch")
	// batch with missing id must not mutate valid ids (atomic)
	rec = do(http.MethodPost, "/api/admin/accounts/batch-update", `{"ids":[`+strconv.FormatInt(a1.ID, 10)+`,999999],"fields":{"name":"hacked"}}`)
	require.Equal(t, 404, rec.Code, "batch with missing id must 404: %s", rec.Body.String())
	rec = do(http.MethodGet, "/api/admin/accounts/"+strconv.FormatInt(a1.ID, 10), "")
	require.Equal(t, 200, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &gotA1))
	require.NotEqual(t, "hacked", gotA1.Name, "failed batch must not mutate valid row")
}
