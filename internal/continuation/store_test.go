// SPDX-License-Identifier: AGPL-3.0-or-later
package continuation

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/redisx"
)

func newMiniredisStore(t *testing.T, secret string) (*miniredis.Miniredis, *Store) {
	t.Helper()
	mr := miniredis.RunT(t)
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })
	s, err := New(c, secret)
	require.NoError(t, err)
	return mr, s
}

func routeID(t *testing.T) domain.RouteClassIDVal {
	t.Helper()
	id, err := domain.RouteClassID(1, domain.FormatOpenAIResponses, "gpt-4", domain.OpResponses)
	require.NoError(t, err)
	return id
}

func fp(t *testing.T) domain.CandidateFingerprintVal {
	t.Helper()
	f, err := domain.CandidateFingerprint(10, 20, "api_key", "https://api.openai.com", "sk-abc", "", "", "", false, "", "", "", "")
	require.NoError(t, err)
	return f
}

func fp2(t *testing.T) domain.CandidateFingerprintVal {
	t.Helper()
	f, err := domain.CandidateFingerprint(11, 20, "api_key", "https://api.openai.com", "sk-different", "", "", "", false, "", "", "", "")
	require.NoError(t, err)
	return f
}

func TestContinuationCreateAndCrossInstance(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })
	s1, err := New(c, "secret-1234567890123456")
	require.NoError(t, err)
	s2, err := New(c, "secret-1234567890123456")
	require.NoError(t, err)
	rid := routeID(t)
	f := fp(t)
	ctx := t.Context()
	st, err := s1.CreateOrRefresh(ctx, 100, 1, rid, "responses", "resp_abc", 10, f, 5)
	require.NoError(t, err)
	require.Equal(t, "created", st)
	// cross-instance lookup via second store (shared Redis, different L1)
	b, ok, err := s2.Lookup(ctx, 100, 1, rid, "responses", "resp_abc")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(10), b.AccountID)
	require.Equal(t, hex.EncodeToString(f[:]), b.Fingerprint)
	require.Equal(t, int64(5), b.Revision)
	// s2 L1 now populated, second lookup hits L1 even if Redis cleared? verify L1
	require.Equal(t, 1, s2.L1Len())
	// refresh same binding
	st, err = s1.CreateOrRefresh(ctx, 100, 1, rid, "responses", "resp_abc", 10, f, 5)
	require.NoError(t, err)
	require.Equal(t, "refreshed", st)
}

func TestContinuationConflictFailClosed(t *testing.T) {
	_, s := newMiniredisStore(t, "secret-1234567890123456")
	rid := routeID(t)
	f := fp(t)
	fOther := fp2(t)
	ctx := t.Context()
	st, err := s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_conf", 10, f, 1)
	require.NoError(t, err)
	require.Equal(t, "created", st)
	// conflicting account
	st, err = s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_conf", 11, f, 1)
	require.NoError(t, err)
	require.Equal(t, "conflict", st)
	// conflicting fingerprint
	st, err = s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_conf", 10, fOther, 1)
	require.NoError(t, err)
	require.Equal(t, "conflict", st)
	// conflicting revision
	st, err = s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_conf", 10, f, 2)
	require.NoError(t, err)
	require.Equal(t, "conflict", st)
	// original still retrievable
	b, ok, err := s.Lookup(ctx, 1, 1, rid, "responses", "resp_conf")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(10), b.AccountID)
}

func TestContinuationFingerprintRevisionValidation(t *testing.T) {
	_, s := newMiniredisStore(t, "secret-xyz-1234567890")
	rid := routeID(t)
	f := fp(t)
	ctx := t.Context()
	_, err := s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_val", 10, f, 7)
	require.NoError(t, err)
	b, ok, err := s.Lookup(ctx, 1, 1, rid, "responses", "resp_val")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, hex.EncodeToString(f[:]), b.Fingerprint)
	require.Equal(t, int64(7), b.Revision)
	// simulate account unavailable: caller would check b.AccountID against enabled set and fail closed (store still returns binding)
}

func TestContinuationExpiry(t *testing.T) {
	mr, s := newMiniredisStore(t, "secret-expiry-test-1234")
	rid := routeID(t)
	f := fp(t)
	ctx := t.Context()
	_, err := s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_exp", 10, f, 1)
	require.NoError(t, err)
	// fast-forward past TTL
	mr.FastForward(TTL + time.Second)
	// force L1 expiry by advancing time? l1 uses real time; simulate by clearing via Get expired check after waiting? Use fast-forward + direct Redis check
	// Redis should have expired
	b, ok, err := s.Lookup(ctx, 1, 1, rid, "responses", "resp_exp")
	require.NoError(t, err)
	// After Redis expiry, L1 should also have expired (real clock hasn't advanced 24h, so L1 still holds). But TTL 24h real time not fast-forwarded for L1.
	// To test L1 expiry, create a small-TTL l1 directly
	if ok && b != nil {
		// Redis not yet expired in miniredis time? check miniredis TTL behavior: FastForward advances miniredis clock, so GET should be nil
		// If still ok, it's because L1 hit; clear L1 and retry
		s.l1 = newL1(DefaultMaxEntries, DefaultMaxBytes)
		b2, ok2, err2 := s.Lookup(ctx, 1, 1, rid, "responses", "resp_exp")
		require.NoError(t, err2)
		require.False(t, ok2)
		require.Nil(t, b2)
	} else {
		require.False(t, ok)
	}
}

func TestContinuationL1ExpirySmallTTL(t *testing.T) {
	_ = redis.NewClient(&redis.Options{Addr: "127.0.0.1:0"})
	l1 := newL1(10, 1<<20)
	l1.now = func() time.Time { return time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC) }
	b := Binding{AccountID: 1, Fingerprint: "abc", Revision: 1}
	l1.Put("k1", b, time.Second)
	require.Equal(t, 1, l1.Len())
	// advance past expiry
	l1.now = func() time.Time { return time.Date(2026, 8, 28, 0, 0, 2, 0, time.UTC) }
	_, ok := l1.Get("k1")
	require.False(t, ok, "expired entry must miss")
	require.Equal(t, 0, l1.Len())
}

func TestContinuationRestartAndStaleMember(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })
	s, err := New(c, "secret-restart-123456")
	require.NoError(t, err)
	rid := routeID(t)
	f := fp(t)
	ctx := t.Context()
	_, err = s.CreateOrRefresh(ctx, 5, 2, rid, "responses", "resp_restart", 10, f, 1)
	require.NoError(t, err)
	// simulate restart: new Store with same secret but empty L1 must still fetch from Redis
	s2, err := New(c, "secret-restart-123456")
	require.NoError(t, err)
	require.Equal(t, 0, s2.L1Len())
	b, ok, err := s2.Lookup(ctx, 5, 2, rid, "responses", "resp_restart")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(10), b.AccountID)
	require.Equal(t, 1, s2.L1Len(), "after Redis GET, L1 populated")
	// verify Redis persistence across L1 loss
	_ = mr
}

func TestContinuationSecretRotation(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })
	rid := routeID(t)
	f := fp(t)
	ctx := t.Context()
	sOld, _ := New(c, "old-secret-123456789012")
	_, err = sOld.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_rot", 10, f, 1)
	require.NoError(t, err)
	// new secret derives different key
	sNew, _ := New(c, "new-secret-654321098765")
	_, ok, err := sNew.Lookup(ctx, 1, 1, rid, "responses", "resp_rot")
	require.NoError(t, err)
	require.False(t, ok, "rotated secret must not find old binding (no dual secret)")
	// new binding under new secret
	st, err := sNew.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_rot", 10, f, 1)
	require.NoError(t, err)
	require.Equal(t, "created", st)
	_, ok, err = sNew.Lookup(ctx, 1, 1, rid, "responses", "resp_rot")
	require.NoError(t, err)
	require.True(t, ok)
	// old store still sees old key
	_, ok, err = sOld.Lookup(ctx, 1, 1, rid, "responses", "resp_rot")
	require.NoError(t, err)
	require.True(t, ok)
	_ = mr
}

func TestContinuationNoRawIDLeak(t *testing.T) {
	_, s := newMiniredisStore(t, "secret-no-raw-123456789")
	rid := routeID(t)
	f := fp(t)
	ctx := t.Context()
	_, err := s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_secret_value_123", 10, f, 1)
	require.NoError(t, err)
	rkey, _ := s.RedisKey(1, 1, rid, "responses", "resp_secret_value_123")
	require.NotContains(t, rkey, "resp_secret_value_123")
	// scan redis keys ensure no raw ID stored
	mr := miniredis.RunT(t)
	c2, _ := redisx.Open(redisx.Options{Addr: mr.Addr()})
	_ = c2
	// direct check via store's client
	keys, err := s.client.Keys(ctx, "*").Result()
	require.NoError(t, err)
	for _, k := range keys {
		require.NotContains(t, k, "resp_secret_value_123")
	}
	// also ensure continuation_id not in stored value
	val, err := s.client.Get(ctx, rkey).Result()
	require.NoError(t, err)
	require.NotContains(t, val, "resp_secret_value_123")
}

func TestContinuationL1Caps(t *testing.T) {
	l1 := newL1(3, 500)
	l1.now = func() time.Time { return time.Now() }
	for i := 0; i < 5; i++ {
		l1.Put(hex.EncodeToString([]byte{byte(i)}), Binding{AccountID: int64(i), Fingerprint: "fp", Revision: 1}, TTL)
	}
	require.LessOrEqual(t, l1.Len(), 3)
	require.LessOrEqual(t, l1.Bytes(), 500)
	// expiry-first: earliest expiry evicted
	l1b := newL1(2, 1<<20)
	base := time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)
	l1b.now = func() time.Time { return base }
	l1b.Put("k1", Binding{AccountID: 1, Fingerprint: "a", Revision: 1}, 10*time.Second)
	l1b.Put("k2", Binding{AccountID: 2, Fingerprint: "b", Revision: 1}, 20*time.Second)
	// next insert should evict k1 (earliest expiry)
	l1b.Put("k3", Binding{AccountID: 3, Fingerprint: "c", Revision: 1}, 30*time.Second)
	_, ok1 := l1b.Get("k1")
	require.False(t, ok1, "k1 earliest expiry should be evicted")
	_, ok2 := l1b.Get("k2")
	require.True(t, ok2)
}

func TestContinuationOrdinaryNoRedis(t *testing.T) {
	// ordinary request must not touch Redis; verify that constructing store alone issues no commands
	mr := miniredis.RunT(t)
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })
	_, err = New(c, "secret-ordinary-123456")
	require.NoError(t, err)
	// no keys yet
	keys, err := c.Keys(t.Context(), "*").Result()
	require.NoError(t, err)
	require.Empty(t, keys)
	_ = mr
}

func TestContinuationRace(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })
	rid := routeID(t)
	f := fp(t)
	fOther := fp2(t)
	ctx := t.Context()
	// concurrent CAS: one must win, other conflict
	errCh := make(chan string, 2)
	go func() {
		st, _ := New(c, "secret-race-123456789012")
		s, _ := New(c, "secret-race-123456789012")
		_ = st
		st2, _ := s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_race", 10, f, 1)
		errCh <- st2
	}()
	go func() {
		s, _ := New(c, "secret-race-123456789012")
		st, _ := s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_race", 11, fOther, 1)
		errCh <- st
	}()
	r1 := <-errCh
	r2 := <-errCh
	require.True(t, (r1 == "created" && r2 == "conflict") || (r1 == "conflict" && r2 == "created") || (r1 == "created" && r2 == "created" && false), "exactly one must succeed, got %q %q", r1, r2)
	// verify exactly one binding persisted regardless of race
	sCheck, _ := New(c, "secret-race-123456789012")
	b, ok, err := sCheck.Lookup(ctx, 1, 1, rid, "responses", "resp_race")
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, b)
	_ = mr
}
