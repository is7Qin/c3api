// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	bpssdk "github.com/is7Qin/bps-sdk"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestRedisBPSTokenStoreOwnerAndCASFence(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })

	store := NewRedisBPSTokenStore(client)
	key := BPSTokenKey{AccountID: 42, IdentityRevision: 9, Owner: "c3-account:42", UpstreamAccountID: "upstream-acct"}
	set := BPSTokenSet{AccessToken: "at-1", RefreshToken: "rt-1", ExpiresAt: time.Unix(4102444800, 0)}

	loaded, rev, err := store.Load(context.Background(), key)
	require.NoError(t, err)
	require.Zero(t, rev)
	require.Empty(t, loaded.AccessToken)

	require.Equal(t, int64(1), mustSaveBPSToken(t, store, key, 0, set))
	loaded, rev, err = store.Load(context.Background(), key)
	require.NoError(t, err)
	require.Equal(t, int64(1), rev)
	require.Equal(t, set, loaded)

	_, err = store.SaveCAS(context.Background(), key, 0, set)
	require.ErrorIs(t, err, ErrBPSTokenConflict)
	_, err = (&bpsTokenStoreAdapter{store: store, key: key}).SaveCAS(context.Background(), "upstream-acct", 1, bpssdk.TokenSet{
		AccessToken: "at-2", RefreshToken: "rt-2", ExpiresAt: set.ExpiresAt,
	})
	require.NoError(t, err)

	wrongK := key
	wrongK.IdentityRevision++
	loaded, rev, err = store.Load(context.Background(), wrongK)
	require.NoError(t, err, "new K sees old record as unreachable and seeds from its DB snapshot")
	require.Zero(t, rev)
	require.Empty(t, loaded.AccessToken)
	require.Equal(t, int64(1), mustSaveBPSToken(t, store, wrongK, 0, BPSTokenSet{
		AccessToken: "at-new", RefreshToken: "rt-new", ExpiresAt: set.ExpiresAt,
	}))
	_, _, err = store.Load(context.Background(), key)
	require.ErrorIs(t, err, ErrBPSOwnerMismatch, "old K must not read the new record")
}

func TestRedisBPSTokenStoreCorruptRecordFailsClosed(t *testing.T) {
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = client.Close() })
	store := NewRedisBPSTokenStore(client)
	key := BPSTokenKey{AccountID: 7, IdentityRevision: 2, Owner: "c3-account:7", UpstreamAccountID: "upstream-7"}

	require.NoError(t, client.HSet(context.Background(), "bps:token:upstream-7", "c3_account_id", "7").Err())
	_, _, err := store.Load(context.Background(), key)
	require.True(t, errors.Is(err, ErrBPSOwnerMismatch) || errors.Is(err, ErrBPSTokenRecordCorrupt), "err=%v", err)
}

func mustSaveBPSToken(t *testing.T, store *RedisBPSTokenStore, key BPSTokenKey, expected int64, set BPSTokenSet) int64 {
	t.Helper()
	next, err := store.SaveCAS(context.Background(), key, expected, set)
	require.NoError(t, err)
	return next
}
