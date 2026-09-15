// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSyncStrictRevMalformedFreezes(t *testing.T) {
	_, c := newHealthTestRedis(t)
	fakeNow := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)
	h := NewRuntimeHealth(c, "self-a", nil, nil)
	h.now = func() time.Time { return fakeNow }
	key := healthKeyFor(30, "q-strict-rev", 1)
	_, err := h.Throttle(context.Background(), key, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))
	snapBefore := h.View()
	genBefore := h.curGen.Load()
	require.Contains(t, snapBefore, key)
	cases := []string{"12abc", "1.5", "9223372036854775808", "-1", ""}
	for _, bad := range cases {
		recKey := healthRecordPrefix + key.String()
		field := key.String()
		if bad == "" {
			require.NoError(t, c.HDel(context.Background(), recKey, "rev").Err())
		} else {
			require.NoError(t, c.HSet(context.Background(), recKey, "rev", bad).Err())
		}
		err = h.Sync(context.Background())
		require.Error(t, err, "malformed rev %q must error", bad)
		require.Equal(t, genBefore, h.curGen.Load(), "curGen freeze on rev %q", bad)
		require.Equal(t, snapBefore[key].Generation, h.View()[key].Generation, "view freeze on rev %q", bad)
		// restore valid rev for next case
		require.NoError(t, c.HSet(context.Background(), recKey, "rev", "1").Err())
		if bad == "" {
			// need field still in ZSET and hash
		}
		_ = field
	}
}

func TestSyncStrictTTLMalformedFreezes(t *testing.T) {
	_, c := newHealthTestRedis(t)
	fakeNow := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC)
	h := NewRuntimeHealth(c, "self-a", nil, nil)
	h.now = func() time.Time { return fakeNow }
	key := healthKeyFor(31, "q-strict-ttl", 1)
	_, err := h.Throttle(context.Background(), key, StateOPEN, 5*time.Second)
	require.NoError(t, err)
	require.NoError(t, h.Sync(context.Background()))
	snapBefore := h.View()
	genBefore := h.curGen.Load()
	require.Contains(t, snapBefore, key)
	cases := []string{"12abc", "1.5", "9223372036854775808", "-5", "0", ""}
	for _, bad := range cases {
		recKey := healthRecordPrefix + key.String()
		if bad == "" {
			require.NoError(t, c.HDel(context.Background(), recKey, "ttl").Err())
		} else {
			require.NoError(t, c.HSet(context.Background(), recKey, "ttl", bad).Err())
		}
		err = h.Sync(context.Background())
		require.Error(t, err, "malformed ttl %q must error", bad)
		require.Equal(t, genBefore, h.curGen.Load(), "curGen freeze on ttl %q", bad)
		require.Equal(t, snapBefore[key].Generation, h.View()[key].Generation, "view freeze on ttl %q", bad)
		require.NoError(t, c.HSet(context.Background(), recKey, "ttl", "5000").Err())
	}
}
