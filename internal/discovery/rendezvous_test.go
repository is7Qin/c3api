// SPDX-License-Identifier: AGPL-3.0-or-later
package discovery

import (
	"context"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/pkg/redisx"
)

func TestMemberSnapshotTwoInstances(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })
	a := New(c, "inst-a", nil)
	b := New(c, "inst-b", nil)
	require.NoError(t, a.Start(t.Context()))
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	require.NoError(t, b.Start(t.Context()))
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	require.Eventually(t, func() bool { return len(a.LiveMembers()) == 2 && len(b.LiveMembers()) == 2 }, 3*time.Second, 20*time.Millisecond)
	require.Equal(t, []string{"inst-a", "inst-b"}, a.LiveMembers())
	require.Equal(t, []string{"inst-a", "inst-b"}, b.LiveMembers())
	// immutability
	snap := a.LiveMembers()
	snap[0] = "tamper"
	require.Equal(t, []string{"inst-a", "inst-b"}, a.LiveMembers(), "snapshot must be copy")
}

func TestMemberSnapshotStaleMember(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })
	a := New(c, "inst-a", nil)
	require.NoError(t, a.Start(t.Context()))
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	now := float64(time.Now().UnixMilli())
	require.NoError(t, c.ZAdd(t.Context(), MembersKey, redis.Z{Score: now, Member: "stale"}).Err())
	require.Eventually(t, func() bool { return len(a.LiveMembers()) == 2 }, 3*time.Second, 20*time.Millisecond)
	stale := float64(time.Now().UnixMilli()) - float64((memberTTL+time.Second)/time.Millisecond)
	require.NoError(t, c.ZAdd(t.Context(), MembersKey, redis.Z{Score: stale, Member: "stale"}).Err())
	require.Eventually(t, func() bool {
		m := a.LiveMembers()
		return len(m) == 1 && m[0] == "inst-a"
	}, 3*time.Second, 20*time.Millisecond)
}

func TestMemberSnapshotFreezeAndZREM(t *testing.T) {
	mr := miniredis.NewMiniRedis()
	require.NoError(t, mr.StartAddr("127.0.0.1:0"))
	t.Cleanup(func() { mr.Close() })
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })
	a := New(c, "inst-a", nil)
	b := New(c, "inst-b", nil)
	require.NoError(t, a.Start(t.Context()))
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	require.NoError(t, b.Start(t.Context()))
	require.Eventually(t, func() bool { return len(a.LiveMembers()) == 2 }, 3*time.Second, 20*time.Millisecond)
	addr := mr.Addr()
	mr.Close()
	// freeze keeps last snapshot
	require.Eventually(t, func() bool { return a.Stats().(Stats).ConsecutiveErrors > 0 }, 8*time.Second, 20*time.Millisecond)
	require.Equal(t, []string{"inst-a", "inst-b"}, a.LiveMembers(), "freeze must preserve last snapshot")
	require.Equal(t, 2, a.ClusterInstances())
	// ZREM on graceful close
	restarted := miniredis.NewMiniRedis()
	require.NoError(t, restarted.StartAddr(addr))
	t.Cleanup(func() { restarted.Close() })
	require.Eventually(t, func() bool { return a.Stats().(Stats).LastTickOk }, 8*time.Second, 20*time.Millisecond)
	require.NoError(t, b.Close(context.Background()))
	require.Eventually(t, func() bool {
		m := a.LiveMembers()
		return len(m) == 1 && m[0] == "inst-a"
	}, 3*time.Second, 20*time.Millisecond)
}

func TestProbeRendezvousDeterministic(t *testing.T) {
	members := []string{"inst-a", "inst-b", "inst-c"}
	k1 := RendezvousOwner("user:42:routeX", members)
	k2 := RendezvousOwner("user:42:routeX", members)
	require.Equal(t, k1, k2, "rendezvous must be deterministic")
	require.Contains(t, members, k1)
	shuffled := []string{"inst-c", "inst-a", "inst-b"}
	require.Equal(t, k1, RendezvousOwner("user:42:routeX", shuffled), "order independent")
	require.Equal(t, "", RendezvousOwner("any", nil))
	require.Equal(t, "", RendezvousOwner("any", []string{}))
	// discovery method delegates
	mr := miniredis.RunT(t)
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })
	d := New(c, "inst-a", nil)
	require.NoError(t, d.Start(t.Context()))
	t.Cleanup(func() { _ = d.Close(context.Background()) })
	require.Eventually(t, func() bool { return len(d.LiveMembers()) == 1 }, 2*time.Second, 20*time.Millisecond)
	owner := d.RendezvousOwner("probe-key")
	require.Equal(t, "inst-a", owner)
}

func TestProbeRendezvousCrossInstance(t *testing.T) {
	mr := miniredis.RunT(t)
	c, _ := redisx.Open(redisx.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = redisx.Close(c) })
	a := New(c, "inst-a", nil)
	b := New(c, "inst-b", nil)
	require.NoError(t, a.Start(t.Context()))
	t.Cleanup(func() { _ = a.Close(context.Background()) })
	require.NoError(t, b.Start(t.Context()))
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	require.Eventually(t, func() bool { return len(a.LiveMembers()) == 2 }, 3*time.Second, 20*time.Millisecond)
	keys := []string{"key1", "key2", "key3", "key4", "key5"}
	for _, k := range keys {
		require.Equal(t, a.RendezvousOwner(k), b.RendezvousOwner(k), "cross-instance rendezvous must agree")
	}
}
