// SPDX-License-Identifier: AGPL-3.0-or-later
package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestModelMappingTopLevelNullRejected(t *testing.T) {
	_, _, do := newListTestRouter(t)
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		path := "/api/admin/templates"
		body := `{"name":"mapping-null-top","supported_formats":["openai-chat"],"model_mapping":null}`
		if method == http.MethodPut {
			created := do(http.MethodPost, "/api/admin/templates", `{"name":"mapping-null-base","supported_formats":["openai-chat"]}`)
			require.Equal(t, http.StatusOK, created.Code)
			var tpl Template
			require.NoError(t, json.Unmarshal(created.Body.Bytes(), &tpl))
			path = "/api/admin/templates/" + itoa(tpl.ID)
		}
		rec := do(method, path, body)
		require.Equal(t, http.StatusBadRequest, rec.Code, "%s null must be 400: %s", method, rec.Body.String())
		require.Contains(t, rec.Body.String(), "model_mapping")
	}
}

func TestModelMappingBatchNullRejected(t *testing.T) {
	_, _, do := newListTestRouter(t)
	created := do(http.MethodPost, "/api/admin/templates", `{"name":"mapping-batch-null","supported_formats":["openai-chat"],"model_mapping":{"before":{"mapped_model":"target-before","mode":"explicit"}}}`)
	require.Equal(t, http.StatusOK, created.Code)
	var tpl Template
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &tpl))
	rec := do(http.MethodPost, "/api/admin/templates/batch-update", `{"ids":[`+itoa(tpl.ID)+`],"fields":{"model_mapping":null}}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, "batch null must be 400: %s", rec.Body.String())
	require.Contains(t, rec.Body.String(), "model_mapping")
	fetched := do(http.MethodGet, "/api/admin/templates/"+itoa(tpl.ID), "")
	require.Equal(t, http.StatusOK, fetched.Code)
	var after Template
	require.NoError(t, json.Unmarshal(fetched.Body.Bytes(), &after))
	require.NotNil(t, after.ModelMapping)
	require.Contains(t, *after.ModelMapping, "before")
}

func TestModelMappingOmittedAndEmptyBoundary(t *testing.T) {
	_, _, do := newListTestRouter(t)
	created := do(http.MethodPost, "/api/admin/templates", `{"name":"mapping-omitted","supported_formats":["openai-chat"]}`)
	require.Equal(t, http.StatusOK, created.Code)
	var c Template
	require.NoError(t, json.Unmarshal(created.Body.Bytes(), &c))
	require.NotNil(t, c.ModelMapping)
	require.Empty(t, *c.ModelMapping)
	put := do(http.MethodPut, "/api/admin/templates/"+itoa(c.ID), `{"name":"mapping-omitted","supported_formats":["openai-chat"]}`)
	require.Equal(t, http.StatusOK, put.Code)
	var u Template
	require.NoError(t, json.Unmarshal(put.Body.Bytes(), &u))
	require.NotNil(t, u.ModelMapping)
	require.Empty(t, *u.ModelMapping)

	batchBase := do(http.MethodPost, "/api/admin/templates", `{"name":"mapping-batch-boundary","supported_formats":["openai-chat"],"model_mapping":{"keep":{"mapped_model":"target-keep","mode":"explicit"}}}`)
	require.Equal(t, http.StatusOK, batchBase.Code)
	var b Template
	require.NoError(t, json.Unmarshal(batchBase.Body.Bytes(), &b))
	omitted := do(http.MethodPost, "/api/admin/templates/batch-update", `{"ids":[`+itoa(b.ID)+`],"fields":{"name":"renamed"}}`)
	require.Equal(t, http.StatusOK, omitted.Code)
	fetched := do(http.MethodGet, "/api/admin/templates/"+itoa(b.ID), "")
	require.Equal(t, http.StatusOK, fetched.Code)
	var preserved Template
	require.NoError(t, json.Unmarshal(fetched.Body.Bytes(), &preserved))
	require.Contains(t, *preserved.ModelMapping, "keep")

	cleared := do(http.MethodPost, "/api/admin/templates/batch-update", `{"ids":[`+itoa(b.ID)+`],"fields":{"model_mapping":{}}}`)
	require.Equal(t, http.StatusOK, cleared.Code)
	fetched2 := do(http.MethodGet, "/api/admin/templates/"+itoa(b.ID), "")
	require.Equal(t, http.StatusOK, fetched2.Code)
	var empty Template
	require.NoError(t, json.Unmarshal(fetched2.Body.Bytes(), &empty))
	require.NotNil(t, empty.ModelMapping)
	require.Empty(t, *empty.ModelMapping)
}
