// SPDX-License-Identifier: AGPL-3.0-or-later
package continuation

import (
	"context"
	"encoding/hex"
	"strconv"
	"sync/atomic"
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

func mustFP() domain.CandidateFingerprintVal {
	f, err := domain.CandidateFingerprint(10, 20, "api_key", "https://api.openai.com", "sk-abc", "", "", "", false, "", "", "", "")
	if err != nil {
		panic(err)
	}
	return f
}

type countHook struct {
	n atomic.Int64
}

func (h *countHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *countHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		h.n.Add(1)
		return next(ctx, cmd)
	}
}
func (h *countHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		h.n.Add(int64(len(cmds)))
		return next(ctx, cmds)
	}
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
	b, ok, err := s2.Lookup(ctx, 100, 1, rid, "responses", "resp_abc")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(10), b.AccountID)
	require.Equal(t, f, b.Fingerprint)
	require.Equal(t, int64(5), b.Revision)
	require.True(t, b.RedisAcked)
	require.Equal(t, 1, s2.L1Len())
	st, err = s1.CreateOrRefresh(ctx, 100, 1, rid, "responses", "resp_abc", 10, f, 5)
	require.NoError(t, err)
	require.Equal(t, "refreshed", st)
	_ = mr
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
	st, err = s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_conf", 11, f, 1)
	require.NoError(t, err)
	require.Equal(t, "conflict", st)
	st, err = s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_conf", 10, fOther, 1)
	require.NoError(t, err)
	require.Equal(t, "conflict", st)
	st, err = s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_conf", 10, f, 2)
	require.NoError(t, err)
	require.Equal(t, "conflict", st)
	b, ok, err := s.Lookup(ctx, 1, 1, rid, "responses", "resp_conf")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(10), b.AccountID)
	require.Equal(t, f, b.Fingerprint)
	// invalid inputs fail
	_, err = s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_conf2", 0, f, 1)
	require.Error(t, err)
	_, err = s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_conf2", 10, f, 0)
	require.Error(t, err)
	var zero domain.CandidateFingerprintVal
	_, err = s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_conf2", 10, zero, 1)
	require.Error(t, err)
}

func TestContinuationInt64Beyond53(t *testing.T) {
	_, s := newMiniredisStore(t, "secret-int53-1234567890")
	rid := routeID(t)
	f := fp(t)
	ctx := t.Context()
	largeAcc := int64(9007199254740993)
	largeRev := int64(9007199254740995)
	st, err := s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_large", largeAcc, f, largeRev)
	require.NoError(t, err)
	require.Equal(t, "created", st)
	// lookup with same large values via cross-instance
	b, ok, err := s.Lookup(ctx, 1, 1, rid, "responses", "resp_large")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, largeAcc, b.AccountID)
	require.Equal(t, largeRev, b.Revision)
	// conflicting nearby value must not be treated as equal due to double precision
	st, err = s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_large", largeAcc+1, f, largeRev)
	require.NoError(t, err)
	require.Equal(t, "conflict", st)
	st, err = s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_large", largeAcc, f, largeRev+1)
	require.NoError(t, err)
	require.Equal(t, "conflict", st)
	// verify stored wire uses decimal strings
	rkey, _ := s.RedisKey(1, 1, rid, "responses", "resp_large")
	val, err := s.client.Get(ctx, rkey).Result()
	require.NoError(t, err)
	require.Contains(t, val, strconv.FormatInt(largeAcc, 10))
	require.Contains(t, val, strconv.FormatInt(largeRev, 10))
	require.NotContains(t, val, "9007199254740992")
}

func TestContinuationMalformedFailClosed(t *testing.T) {
	mr, s := newMiniredisStore(t, "secret-malformed-123456")
	rid := routeID(t)
	f := fp(t)
	ctx := t.Context()
	_, err := s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_ok", 10, f, 1)
	require.NoError(t, err)
	rkey, _ := s.RedisKey(1, 1, rid, "responses", "resp_ok")
	// create a second continuation key for malformed injection
	rid2 := rid
	rkeyBad, _ := s.RedisKey(1, 1, rid2, "responses", "resp_bad")
	cases := []string{
		`not-json`,
		`{"account_id":"10","fingerprint":"` + hex.EncodeToString(f[:]) + `","revision":"1"}`,
		`{"account_id":"10","fingerprint":"` + hex.EncodeToString(f[:]) + `","revision":"1","redis_acked":false}`,
		`{"account_id":10,"fingerprint":"` + hex.EncodeToString(f[:]) + `","revision":"1","redis_acked":true}`,
		`{"account_id":"10","fingerprint":"` + hex.EncodeToString(f[:]) + `","revision":"1","redis_acked":true,"extra":"x"}`,
		`{"account_id":"0","fingerprint":"` + hex.EncodeToString(f[:]) + `","revision":"1","redis_acked":true}`,
		`{"account_id":"10","fingerprint":"0000000000000000000000000000000000000000000000000000000000000000","revision":"1","redis_acked":true}`,
		`{"account_id":"10","fingerprint":"zzzz","revision":"1","redis_acked":true}`,
		`{"account_id":"abc","fingerprint":"` + hex.EncodeToString(f[:]) + `","revision":"1","redis_acked":true}`,
	}
	for i, bad := range cases {
		badKey := rkeyBad + strconv.Itoa(i)
		require.NoError(t, s.client.Set(ctx, badKey, bad, TTL).Err())
		// use direct Lookup via raw key injection: bypass RKey, test parseBinding via manual Get
		val, err := s.client.Get(ctx, badKey).Result()
		require.NoError(t, err)
		_, ok := parseBinding([]byte(val))
		require.False(t, ok, "case %d should be invalid: %s", i, bad)
		// also test via Store.Lookup on a derived continuation that maps to this bad key
		// inject by overwriting the good key with bad value and ensuring Lookup fails closed and not cached
		require.NoError(t, s.client.Set(ctx, rkey, bad, TTL).Err())
		// clear L1 to force Redis fetch
		s.l1 = newL1(DefaultMaxEntries, DefaultMaxBytes)
		b, ok, err := s.Lookup(ctx, 1, 1, rid, "responses", "resp_ok")
		require.NoError(t, err)
		require.False(t, ok, "case %d lookup should fail closed", i)
		require.Nil(t, b)
		require.Equal(t, 0, s.L1Len(), "malformed value must not be cached")
	}
	// restore good value and ensure re-fetch works
	_, err = s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_ok", 10, f, 1)
	require.NoError(t, err)
	_ = mr
}

func TestContinuationPTTLNearExpiry(t *testing.T) {
	mr, s := newMiniredisStore(t, "secret-pttl-1234567890")
	rid := routeID(t)
	f := fp(t)
	ctx := t.Context()
	_, err := s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_pttl", 10, f, 1)
	require.NoError(t, err)
	rkey, _ := s.RedisKey(1, 1, rid, "responses", "resp_pttl")
	// force near-expiry via PExpire
	require.NoError(t, s.client.PExpire(ctx, rkey, 120*time.Millisecond).Err())
	// clear L1 to force fetch with remaining TTL
	s.l1 = newL1(DefaultMaxEntries, DefaultMaxBytes)
	b, ok, err := s.Lookup(ctx, 1, 1, rid, "responses", "resp_pttl")
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, b)
	require.Equal(t, 1, s.L1Len())
	// remaining TTL should be ~120ms, not 24h; wait expiry
	time.Sleep(200 * time.Millisecond)
	_, ok = s.l1.Get(rkey)
	require.False(t, ok, "L1 should have expired with remaining TTL, not extended authority")
	// Redis should also be expired (miniredis FastForward equivalent via sleep)
	// allow miniredis to expire
	mr.FastForward(300 * time.Millisecond)
	s.l1 = newL1(DefaultMaxEntries, DefaultMaxBytes)
	_, ok, err = s.Lookup(ctx, 1, 1, rid, "responses", "resp_pttl")
	require.NoError(t, err)
	require.False(t, ok, "after PTTL expiry, lookup must miss")
	_ = mr
}

func TestContinuationReFetchAfterEviction(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })
	// tiny L1 to force eviction
	l1 := newL1(2, 640)
	s, err := NewWithL1(c, "secret-evict-1234567890", l1)
	require.NoError(t, err)
	rid := routeID(t)
	f := fp(t)
	ctx := t.Context()
	for i := 0; i < 3; i++ {
		id := "resp_evict_" + strconv.Itoa(i)
		_, err := s.CreateOrRefresh(ctx, 1, 1, rid, "responses", id, 10, f, 1)
		require.NoError(t, err)
	}
	require.LessOrEqual(t, s.L1Len(), 2)
	// earliest should have been evicted (expiry-first); re-fetch must hit Redis
	s2l1 := s.l1
	// find an evicted key: first one
	b, ok, err := s.Lookup(ctx, 1, 1, rid, "responses", "resp_evict_0")
	require.NoError(t, err)
	require.True(t, ok, "evicted key must be re-fetched from Redis")
	require.NotNil(t, b)
	require.LessOrEqual(t, s.L1Len(), 2)
	// still expiry-first: after re-fetch, L1 size stays bounded
	require.LessOrEqual(t, s2l1.Bytes(), 640)
	_ = mr
}

func TestContinuationRedisRestart(t *testing.T) {
	mr, s := newMiniredisStore(t, "secret-restart-123456")
	rid := routeID(t)
	f := fp(t)
	ctx := t.Context()
	_, err := s.CreateOrRefresh(ctx, 5, 2, rid, "responses", "resp_restart", 10, f, 1)
	require.NoError(t, err)
	// restart: new Store with same secret but empty L1 must still fetch
	s2, err := New(s.client, "secret-restart-123456")
	require.NoError(t, err)
	require.Equal(t, 0, s2.L1Len())
	b, ok, err := s2.Lookup(ctx, 5, 2, rid, "responses", "resp_restart")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(10), b.AccountID)
	// simulate Redis restart: flush
	mr.FlushDB()
	s3, err := New(s.client, "secret-restart-123456")
	require.NoError(t, err)
	_, ok, err = s3.Lookup(ctx, 5, 2, rid, "responses", "resp_restart")
	require.NoError(t, err)
	require.False(t, ok, "after Redis restart/flush, binding gone")
	// can recreate after restart
	st, err := s3.CreateOrRefresh(ctx, 5, 2, rid, "responses", "resp_restart", 10, f, 1)
	require.NoError(t, err)
	require.Equal(t, "created", st)
	_ = mr
}

func TestContinuationL1CapsChargedBound(t *testing.T) {
	// default caps
	l1 := newL1(DefaultMaxEntries, DefaultMaxBytes)
	l1.now = func() time.Time { return time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC) }
	// fill beyond caps
	for i := 0; i < DefaultMaxEntries+5000; i++ {
		k := "c3api:cont:" + hex.EncodeToString([]byte{byte(i >> 8), byte(i), byte(i >> 16), byte(i >> 24)}) + "xxxxxxxxxxxxxxxxxxxxxxxx"
		l1.Put(k, Binding{AccountID: 1, Fingerprint: mustFP(), Revision: 1, RedisAcked: true}, TTL)
	}
	require.LessOrEqual(t, l1.Len(), DefaultMaxEntries)
	require.LessOrEqual(t, l1.Bytes(), DefaultMaxBytes)
	require.Equal(t, l1.Len()*entryCharged, l1.Bytes(), "charged must be fixed-size * entries")
	// expiry-first: earliest expiry evicted
	l1b := newL1(2, 1<<20)
	base := time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)
	l1b.now = func() time.Time { return base }
	l1b.Put("k1", Binding{AccountID: 1, Fingerprint: mustFP(), Revision: 1, RedisAcked: true}, 10*time.Second)
	l1b.Put("k2", Binding{AccountID: 2, Fingerprint: mustFP(), Revision: 1, RedisAcked: true}, 20*time.Second)
	l1b.Put("k3", Binding{AccountID: 3, Fingerprint: mustFP(), Revision: 1, RedisAcked: true}, 30*time.Second)
	_, ok1 := l1b.Get("k1")
	require.False(t, ok1, "k1 earliest expiry should be evicted")
	_, ok2 := l1b.Get("k2")
	require.True(t, ok2)
	// large batch expiry and compaction: mass expiry should not leak heap capacity
	l1c := newL1(100000, 32*1024*1024)
	now := time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)
	l1c.now = func() time.Time { return now }
	for i := 0; i < 1000; i++ {
		l1c.Put("k"+strconv.Itoa(i), Binding{AccountID: 1, Fingerprint: mustFP(), Revision: 1, RedisAcked: true}, time.Second)
	}
	// fast-forward past expiry and trigger purge via Put
	l1c.now = func() time.Time { return now.Add(2 * time.Second) }
	l1c.Put("k_new", Binding{AccountID: 1, Fingerprint: mustFP(), Revision: 1, RedisAcked: true}, TTL)
	require.LessOrEqual(t, l1c.Len(), 100000)
	require.LessOrEqual(t, l1c.Bytes(), DefaultMaxBytes)
}

func TestContinuationCommandCount(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })
	h := &countHook{}
	c.AddHook(h)
	s, err := New(c, "secret-count-1234567890")
	require.NoError(t, err)
	rid := routeID(t)
	f := fp(t)
	ctx := t.Context()
	// preload scripts so first measured call is single EVALSHA (go-redis does EVALSHA fallback to EVAL on cache miss)
	_ = luaCAS.Load(ctx, c).Err()
	_ = luaLookup.Load(ctx, c).Err()
	h.n.Store(0)
	// CreateOrRefresh should issue exactly 1 EVALSHA
	st, err := s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_cnt", 10, f, 1)
	require.NoError(t, err)
	require.Equal(t, "created", st)
	require.Equal(t, int64(1), h.n.Load(), "CreateOrRefresh must be single EVALSHA")
	// Lookup cold (L1 hit after create, so 0). Clear L1 then lookup = 1 EVALSHA
	s.l1 = newL1(DefaultMaxEntries, DefaultMaxBytes)
	h.n.Store(0)
	b, ok, err := s.Lookup(ctx, 1, 1, rid, "responses", "resp_cnt")
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, b)
	require.Equal(t, int64(1), h.n.Load(), "cold Lookup must be single EVALSHA (GET+PTTL lua)")
	// warm L1 hit must be 0 commands
	h.n.Store(0)
	b2, ok, err := s.Lookup(ctx, 1, 1, rid, "responses", "resp_cnt")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, b.Fingerprint, b2.Fingerprint)
	require.Equal(t, int64(0), h.n.Load(), "warm L1 Lookup must not hit Redis")
	// verify ordinary request still zero: new continuation miss still 1
	h.n.Store(0)
	_, ok, err = s.Lookup(ctx, 1, 1, rid, "responses", "resp_missing")
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, int64(1), h.n.Load())
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
	errCh := make(chan string, 2)
	go func() {
		s, _ := New(c, "secret-race-123456789012")
		st, _ := s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_race", 10, f, 1)
		errCh <- st
	}()
	go func() {
		s, _ := New(c, "secret-race-123456789012")
		st, _ := s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_race", 11, fOther, 1)
		errCh <- st
	}()
	r1 := <-errCh
	r2 := <-errCh
	require.True(t, (r1 == "created" && r2 == "conflict") || (r1 == "conflict" && r2 == "created"), "exactly one must succeed, got %q %q", r1, r2)
	sCheck, _ := New(c, "secret-race-123456789012")
	b, ok, err := sCheck.Lookup(ctx, 1, 1, rid, "responses", "resp_race")
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, b)
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
	keys, err := s.client.Keys(ctx, "*").Result()
	require.NoError(t, err)
	for _, k := range keys {
		require.NotContains(t, k, "resp_secret_value_123")
	}
	val, err := s.client.Get(ctx, rkey).Result()
	require.NoError(t, err)
	require.NotContains(t, val, "resp_secret_value_123")
	// secret rotation: new secret derives different key
	mr := miniredis.RunT(t)
	c, _ := redisx.Open(redisx.Options{Addr: mr.Addr()})
	_ = c
	_ = mr
	sOld, _ := New(s.client, "old-secret-123456789012")
	sNew, _ := New(s.client, "new-secret-654321098765")
	_, err = sOld.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_rot", 10, f, 1)
	require.NoError(t, err)
	_, ok, err := sNew.Lookup(ctx, 1, 1, rid, "responses", "resp_rot")
	require.NoError(t, err)
	require.False(t, ok, "rotated secret must not find old binding")
}
