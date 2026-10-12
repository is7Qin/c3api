// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	userapi "github.com/is7qin/c3api/internal/handler/user"
)

// TestAdminGroupDescription: POST /groups echoes Description for non-empty,
// omitted, "" and null inputs (the latter three all yield ""); GET /groups
// carries Description; PUT /groups/{id} is three-state (override / keep on
// omitted+null / clear on ""), each re-checked via a follow-up GET.
func TestAdminGroupDescription(t *testing.T) {
	doAdmin, _, _ := newSharedRouters(t)

	create := func(body string) Group {
		rec := doAdmin(http.MethodPost, "/api/admin/groups", body, "")
		require.Equal(t, http.StatusOK, rec.Code, "create %s: %s", body, rec.Body.String())
		var g Group
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
		require.NotNil(t, g.Description, "response must always carry Description")
		return g
	}

	// POST: non-empty / omitted / "" / null.
	require.Equal(t, "desc-a", *create(`{"name":"d-a","description":"desc-a"}`).Description)
	require.Equal(t, "", *create(`{"name":"d-b"}`).Description)
	require.Equal(t, "", *create(`{"name":"d-c","description":""}`).Description)
	require.Equal(t, "", *create(`{"name":"d-d","description":null}`).Description)

	// GET /groups list carries Description.
	rec := doAdmin(http.MethodGet, "/api/admin/groups", "", "")
	require.Equal(t, http.StatusOK, rec.Code, "list: %s", rec.Body.String())
	var list GroupListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &list))
	var sawA bool
	for _, row := range list.Rows {
		if row.Name != nil && *row.Name == "d-a" {
			sawA = true
			require.NotNil(t, row.Description)
			require.Equal(t, "desc-a", *row.Description)
		}
	}
	require.True(t, sawA, "d-a must appear in the list")

	// PUT three-state on d-d.
	var target Group
	for _, row := range list.Rows {
		if row.Name != nil && *row.Name == "d-d" {
			target = row
		}
	}
	require.NotNil(t, target.ID, "d-d present")
	id := itoa(*target.ID)

	get := func() Group {
		rec := doAdmin(http.MethodGet, "/api/admin/groups/"+id, "", "")
		require.Equal(t, http.StatusOK, rec.Code, "get: %s", rec.Body.String())
		var g Group
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
		require.NotNil(t, g.Description)
		return g
	}
	put := func(body string) Group {
		rec := doAdmin(http.MethodPut, "/api/admin/groups/"+id, body, "")
		require.Equal(t, http.StatusOK, rec.Code, "put %s: %s", body, rec.Body.String())
		var g Group
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))
		require.NotNil(t, g.Description)
		return g
	}

	// Override.
	require.Equal(t, "set-1", *put(`{"name":"d-d","description":"set-1"}`).Description)
	require.Equal(t, "set-1", *get().Description)
	// Keep on omitted.
	require.Equal(t, "set-1", *put(`{"name":"d-d"}`).Description)
	require.Equal(t, "set-1", *get().Description)
	// Keep on null.
	require.Equal(t, "set-1", *put(`{"name":"d-d","description":null}`).Description)
	require.Equal(t, "set-1", *get().Description)
	// Clear on "".
	require.Equal(t, "", *put(`{"name":"d-d","description":""}`).Description)
	require.Equal(t, "", *get().Description)
}

// TestUserGroupsExposeDescriptionAndMultiplier pins the C1 regression guard:
// GET /api/user/groups must include Description (both empty and non-empty) and
// PriceMultiplier (user-facing toAPIGroup), covering 1.5 and 0 (free).
func TestUserGroupsExposeDescriptionAndMultiplier(t *testing.T) {
	doAdmin, doUser, _ := newSharedRouters(t)

	rec := doAdmin(http.MethodPost, "/api/admin/groups",
		`{"name":"u-desc","description":"user-visible-desc","price_multiplier":1.5}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "create u-desc: %s", rec.Body.String())
	rec = doAdmin(http.MethodPost, "/api/admin/groups",
		`{"name":"u-free","description":"","price_multiplier":0}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "create u-free: %s", rec.Body.String())

	token, _ := registerAndGet(t, doUser, "udesc@example.com")
	rec = doUser(http.MethodGet, "/api/user/groups", "", token)
	require.Equal(t, http.StatusOK, rec.Code, "list user groups: %s", rec.Body.String())
	var groups []userapi.Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &groups))

	byName := map[string]userapi.Group{}
	for _, g := range groups {
		require.NotNil(t, g.Name)
		require.NotNil(t, g.Description, "user面 Group 必须含 Description")
		byName[*g.Name] = g
	}

	d, ok := byName["u-desc"]
	require.True(t, ok, "u-desc must be listed")
	require.Equal(t, "user-visible-desc", *d.Description)
	require.NotNil(t, d.PriceMultiplier)
	require.Equal(t, 1.5, *d.PriceMultiplier)

	f, ok := byName["u-free"]
	require.True(t, ok, "u-free must be listed")
	require.Equal(t, "", *f.Description)
	require.NotNil(t, f.PriceMultiplier)
	require.Equal(t, 0.0, *f.PriceMultiplier)
}
