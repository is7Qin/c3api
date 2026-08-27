// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"errors"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notification"
)

func newBalanceWarningCooldown(t *testing.T) *notification.Cooldown {
	t.Helper()
	server, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(server.Close)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	return notification.NewCooldown(client)
}

func TestUpdateBalanceWarningThreshold_DisableClearsOldCooldown(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newBWService(t)
	created, err := store.CreateUser(ctx, &domain.User{
		Email:                   "cooldown-disable@example.com",
		PasswordHash:            "hash",
		Role:                    domain.RoleUser,
		Status:                  domain.UserStatusActive,
		BalanceWarningThreshold: 100_000,
	})
	require.NoError(t, err)

	cooldown := newBalanceWarningCooldown(t)
	svc.SetBalanceWarningCooldownCleaner(cooldown.Clear)
	event := domain.BalanceWarningEvent{EntityID: created.ID, ThresholdMillis: 100_000}
	_, claimed, err := cooldown.TryClaim(ctx, event)
	require.NoError(t, err)
	require.True(t, claimed)

	_, err = svc.UpdateBalanceWarningThreshold(ctx, created.ID, 0)
	require.NoError(t, err)
	_, claimed, err = cooldown.TryClaim(ctx, event)
	require.NoError(t, err)
	require.True(t, claimed)
}

func TestUpdateBalanceWarningThreshold_ChangeClearsOnlyOldCooldown(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newBWService(t)
	created, err := store.CreateUser(ctx, &domain.User{
		Email:                   "cooldown-change@example.com",
		PasswordHash:            "hash",
		Role:                    domain.RoleUser,
		Status:                  domain.UserStatusActive,
		BalanceWarningThreshold: 100_000,
	})
	require.NoError(t, err)
	cooldown := newBalanceWarningCooldown(t)
	svc.SetBalanceWarningCooldownCleaner(cooldown.Clear)
	oldEvent := domain.BalanceWarningEvent{EntityID: created.ID, ThresholdMillis: 100_000}
	newEvent := domain.BalanceWarningEvent{EntityID: created.ID, ThresholdMillis: 200_000}
	_, claimed, err := cooldown.TryClaim(ctx, oldEvent)
	require.NoError(t, err)
	require.True(t, claimed)
	_, claimed, err = cooldown.TryClaim(ctx, newEvent)
	require.NoError(t, err)
	require.True(t, claimed)

	_, err = svc.UpdateBalanceWarningThreshold(ctx, created.ID, 2)
	require.NoError(t, err)
	_, claimed, err = cooldown.TryClaim(ctx, oldEvent)
	require.NoError(t, err)
	require.True(t, claimed)
	_, claimed, err = cooldown.TryClaim(ctx, newEvent)
	require.NoError(t, err)
	require.False(t, claimed)
}

func TestUpdateBalanceWarningThreshold_SameValueKeepsCooldown(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newBWService(t)
	created, err := store.CreateUser(ctx, &domain.User{
		Email:                   "cooldown-same@example.com",
		PasswordHash:            "hash",
		Role:                    domain.RoleUser,
		Status:                  domain.UserStatusActive,
		BalanceWarningThreshold: 100_000,
	})
	require.NoError(t, err)
	cooldown := newBalanceWarningCooldown(t)
	svc.SetBalanceWarningCooldownCleaner(cooldown.Clear)
	event := domain.BalanceWarningEvent{EntityID: created.ID, ThresholdMillis: 100_000}
	_, claimed, err := cooldown.TryClaim(ctx, event)
	require.NoError(t, err)
	require.True(t, claimed)

	_, err = svc.UpdateBalanceWarningThreshold(ctx, created.ID, 1)
	require.NoError(t, err)
	_, claimed, err = cooldown.TryClaim(ctx, event)
	require.NoError(t, err)
	require.False(t, claimed)
}

func TestUpdateBalanceWarningThreshold_CleanupFailureDoesNotFailUpdate(t *testing.T) {
	ctx := context.Background()
	svc, store, _ := newBWService(t)
	created, err := store.CreateUser(ctx, &domain.User{
		Email:                   "cooldown-failure@example.com",
		PasswordHash:            "hash",
		Role:                    domain.RoleUser,
		Status:                  domain.UserStatusActive,
		BalanceWarningThreshold: 100_000,
	})
	require.NoError(t, err)
	var cleanupCalls int
	svc.SetBalanceWarningCooldownCleaner(func(context.Context, int64, int64) error {
		cleanupCalls++
		return errors.New("redis unavailable")
	})

	updated, err := svc.UpdateBalanceWarningThreshold(ctx, created.ID, 0)
	require.NoError(t, err)
	require.Zero(t, updated.BalanceWarningThreshold)
	require.Equal(t, 1, cleanupCalls)
	stored, err := store.GetUser(ctx, created.ID)
	require.NoError(t, err)
	require.Zero(t, stored.BalanceWarningThreshold)
}
