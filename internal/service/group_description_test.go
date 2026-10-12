// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestGroupDescriptionPassthrough: CreateGroup forwards the description onto the
// stored group, and UpdateGroup (whole-group copy) writes then clears it. The
// fake store keeps the full *domain.Group, so this pins the service boundary.
func TestGroupDescriptionPassthrough(t *testing.T) {
	fs := newFakeStore()
	svc := &Service{store: fs, inv: &invRecorder{}, log: nil}
	ctx := context.Background()

	g, err := svc.CreateGroup(ctx, "d-g", "hello-desc", domain.GroupVisibilityPublic, nil, nil)
	require.NoError(t, err)
	require.Equal(t, "hello-desc", g.Description)
	got, err := svc.GetGroup(ctx, g.ID)
	require.NoError(t, err)
	require.Equal(t, "hello-desc", got.Description)

	// Empty description round-trips as the empty string (no three-state storage).
	g2, err := svc.CreateGroup(ctx, "d-g2", "", domain.GroupVisibilityPublic, nil, nil)
	require.NoError(t, err)
	require.Equal(t, "", g2.Description)
	got2, err := svc.GetGroup(ctx, g2.ID)
	require.NoError(t, err)
	require.Equal(t, "", got2.Description)

	// Update overwrites.
	got.Description = "updated-desc"
	upd, err := svc.UpdateGroup(ctx, got)
	require.NoError(t, err)
	require.Equal(t, "updated-desc", upd.Description)
	again, err := svc.GetGroup(ctx, g.ID)
	require.NoError(t, err)
	require.Equal(t, "updated-desc", again.Description)

	// Update clears.
	again.Description = ""
	upd, err = svc.UpdateGroup(ctx, again)
	require.NoError(t, err)
	require.Equal(t, "", upd.Description)
	again, err = svc.GetGroup(ctx, g.ID)
	require.NoError(t, err)
	require.Equal(t, "", again.Description)
}
