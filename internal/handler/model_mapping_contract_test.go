// SPDX-License-Identifier: AGPL-3.0-or-later
package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestModelMappingHTTPRejectsInvalidEntries(t *testing.T) {
	_, _, do := newListTestRouter(t)
	created := do(http.MethodPost, "/api/admin/templates", `{"name":"mapping-invalid-base","supported_formats":["openai-chat"]}`)
	require.Equal(t, http.StatusOK, created.Code, created.Body.String())
	var template Template
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &template))

	cases := []struct {
		name    string
		mapping string
	}{
		{"legacy string", `{"alias":"legacy"}`},
		{"missing mode", `{"alias":{"mapped_model":"target"}}`},
		{"missing mapped model", `{"alias":{"mode":"explicit"}}`},
		{"null entry", `{"alias":null}`},
		{"null mode", `{"alias":{"mapped_model":"target","mode":null}}`},
		{"unknown mode", `{"alias":{"mapped_model":"target","mode":"other"}}`},
		{"whitespace alias", `{" alias":{"mapped_model":"target","mode":"explicit"}}`},
		{"whitespace target", `{"alias":{"mapped_model":" target","mode":"explicit"}}`},
	}
	for index, tc := range cases {
		body := fmt.Sprintf(`{"name":"mapping-invalid-%d","supported_formats":["openai-chat"],"model_mapping":%s}`, index, tc.mapping)
		t.Run(tc.name+" post", func(t *testing.T) {
			response := do(http.MethodPost, "/api/admin/templates", body)
			require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
		})
		t.Run(tc.name+" put", func(t *testing.T) {
			response := do(http.MethodPut, "/api/admin/templates/"+itoa(template.ID), body)
			require.Equal(t, http.StatusBadRequest, response.Code, response.Body.String())
		})
	}
}

func TestModelMappingHTTPRoundTripsExplicitAndImplicitIdentity(t *testing.T) {
	_, _, do := newListTestRouter(t)
	response := do(http.MethodPost, "/api/admin/templates", `{
		"name":"mapping-roundtrip","supported_formats":["openai-chat"],
		"model_mapping":{
			"explicit-id":{"mapped_model":"explicit-id","mode":"explicit"},
			"implicit-id":{"mapped_model":"implicit-id","mode":"implicit"}
		}}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var created Template
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &created))
	require.NotNil(t, created.ModelMapping)
	require.Equal(t, Explicit, (*created.ModelMapping)["explicit-id"].Mode)
	require.Equal(t, Implicit, (*created.ModelMapping)["implicit-id"].Mode)

	response = do(http.MethodGet, "/api/admin/templates/"+itoa(created.ID), "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var fetched Template
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &fetched))
	require.Equal(t, created.ModelMapping, fetched.ModelMapping)

	response = do(http.MethodPut, "/api/admin/templates/"+itoa(created.ID), `{
		"name":"mapping-roundtrip","supported_formats":["openai-chat"],
		"model_mapping":{
			"explicit-next":{"mapped_model":"explicit-next","mode":"explicit"},
			"implicit-next":{"mapped_model":"implicit-next","mode":"implicit"}
		}}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var updated Template
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &updated))
	require.NotNil(t, updated.ModelMapping)
	require.Equal(t, Explicit, (*updated.ModelMapping)["explicit-next"].Mode)
	require.Equal(t, Implicit, (*updated.ModelMapping)["implicit-next"].Mode)
	require.NotContains(t, *updated.ModelMapping, "explicit-id")

	response = do(http.MethodGet, "/api/admin/templates/"+itoa(created.ID), "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var refetched Template
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &refetched))
	require.Equal(t, updated.ModelMapping, refetched.ModelMapping)
}

func TestModelMappingHTTPOmittedCreateAndPutReturnCanonicalEmpty(t *testing.T) {
	_, _, do := newListTestRouter(t)
	response := do(http.MethodPost, "/api/admin/templates", `{"name":"mapping-empty","supported_formats":["openai-chat"]}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var created Template
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &created))
	require.NotNil(t, created.ModelMapping)
	require.Empty(t, *created.ModelMapping)

	response = do(http.MethodPut, "/api/admin/templates/"+itoa(created.ID), `{"name":"mapping-empty","supported_formats":["openai-chat"]}`)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var updated Template
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &updated))
	require.NotNil(t, updated.ModelMapping)
	require.Empty(t, *updated.ModelMapping)

	response = do(http.MethodGet, "/api/admin/templates/"+itoa(created.ID), "")
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var fetched Template
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &fetched))
	require.NotNil(t, fetched.ModelMapping)
	require.Empty(t, *fetched.ModelMapping)
}

func TestModelMappingHTTPBatchPreserveReplaceAndClear(t *testing.T) {
	cases := []struct {
		name   string
		fields string
		want   map[string]ModelMappingEntry
	}{
		{"omitted preserves", `{"name":"mapping-batch-renamed"}`, map[string]ModelMappingEntry{"before": {MappedModel: "target-before", Mode: Explicit}}},
		{"object replaces", `{"model_mapping":{"after":{"mapped_model":"target-after","mode":"implicit"}}}`, map[string]ModelMappingEntry{"after": {MappedModel: "target-after", Mode: Implicit}}},
		{"empty clears", `{"model_mapping":{}}`, map[string]ModelMappingEntry{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, do := newListTestRouter(t)
			response := do(http.MethodPost, "/api/admin/templates", `{
				"name":"mapping-batch","supported_formats":["openai-chat"],
				"model_mapping":{"before":{"mapped_model":"target-before","mode":"explicit"}}}`)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var created Template
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &created))

			response = do(http.MethodPost, "/api/admin/templates/batch-update", fmt.Sprintf(`{"ids":[%d],"fields":%s}`, created.ID, tc.fields))
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			response = do(http.MethodGet, "/api/admin/templates/"+itoa(created.ID), "")
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			var fetched Template
			require.NoError(t, json.Unmarshal(response.Body.Bytes(), &fetched))
			require.NotNil(t, fetched.ModelMapping)
			require.Equal(t, tc.want, *fetched.ModelMapping)
		})
	}
}

func TestModelMappingResponseConversionDoesNotRepairNil(t *testing.T) {
	require.Nil(t, toAPIModelMapping(nil))
}

func TestModelMappingResponseConversionInvalidModeUsesZeroValue(t *testing.T) {
	mode := domainModeToAPIMode(domain.ModelMappingMode(99))
	require.Empty(t, mode)
	require.NotEqual(t, Explicit, mode)
	require.NotEqual(t, Implicit, mode)
}
