// SPDX-License-Identifier: AGPL-3.0-or-later
package continuation

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"sync"
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

type advHook struct {
	mu  *sync.Mutex
	cur *time.Time
	d   time.Duration
}

func (h *advHook) DialHook(next redis.DialHook) redis.DialHook { return next }
func (h *advHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		err := next(ctx, cmd)
		h.mu.Lock()
		*h.cur = h.cur.Add(h.d)
		h.mu.Unlock()
		return err
	}
}
func (h *advHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		err := next(ctx, cmds)
		h.mu.Lock()
		*h.cur = h.cur.Add(time.Duration(len(cmds)) * h.d)
		h.mu.Unlock()
		return err
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
	require.Equal(t, int64(5), b.IdentityRevision)
	require.True(t, b.RedisAcked)
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
	_, err = s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_conf2", 0, f, 1)
	require.Error(t, err)
	_, err = s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_conf2", 10, f, 0)
	require.Error(t, err)
	var zero domain.CandidateFingerprintVal
	_, err = s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_conf2", 10, zero, 1)
	require.Error(t, err)
}

func TestContinuationInt64Beyond53(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })
	s1, err := New(c, "secret-int53-1234567890")
	require.NoError(t, err)
	rid := routeID(t)
	f := fp(t)
	ctx := t.Context()
	largeAcc := int64(9007199254740993)
	largeRev := int64(9007199254740995)
	st, err := s1.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_large", largeAcc, f, largeRev)
	require.NoError(t, err)
	require.Equal(t, "created", st)
	// second Store must fetch from Redis to prove string-decimal wire
	s2, err := New(c, "secret-int53-1234567890")
	require.NoError(t, err)
	b, ok, err := s2.Lookup(ctx, 1, 1, rid, "responses", "resp_large")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, largeAcc, b.AccountID)
	require.Equal(t, largeRev, b.IdentityRevision)
	st, err = s1.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_large", largeAcc+1, f, largeRev)
	require.NoError(t, err)
	require.Equal(t, "conflict", st)
	st, err = s1.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_large", largeAcc, f, largeRev+1)
	require.NoError(t, err)
	require.Equal(t, "conflict", st)
	rkey, _ := s1.RedisKey(1, 1, rid, "responses", "resp_large")
	val, err := s1.client.Get(ctx, rkey).Result()
	require.NoError(t, err)
	require.Contains(t, val, strconv.FormatInt(largeAcc, 10))
	require.Contains(t, val, strconv.FormatInt(largeRev, 10))
	require.NotContains(t, val, "9007199254740992")
}

func TestContinuationMalformedFailClosed(t *testing.T) {
	_, s := newMiniredisStore(t, "secret-malformed-123456")
	rid := routeID(t)
	f := fp(t)
	ctx := t.Context()
	_, err := s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_ok", 10, f, 1)
	require.NoError(t, err)
	rkey, _ := s.RedisKey(1, 1, rid, "responses", "resp_ok")
	rkeyBad, _ := s.RedisKey(1, 1, rid, "responses", "resp_bad")
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
		val, err := s.client.Get(ctx, badKey).Result()
		require.NoError(t, err)
		_, ok := parseBinding([]byte(val))
		require.False(t, ok, "case %d should be invalid: %s", i, bad)
		require.NoError(t, s.client.Set(ctx, rkey, bad, TTL).Err())
		b, ok, err := s.Lookup(ctx, 1, 1, rid, "responses", "resp_ok")
		require.NoError(t, err)
		require.False(t, ok, "case %d lookup should fail closed", i)
		require.Nil(t, b)
	}
}

func TestContinuationMalformedCannotRefresh(t *testing.T) {
	_, s := newMiniredisStore(t, "secret-malformed-refresh-1")
	rid := routeID(t)
	f := fp(t)
	ctx := t.Context()
	_, err := s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_mal", 10, f, 1)
	require.NoError(t, err)
	rkey, _ := s.RedisKey(1, 1, rid, "responses", "resp_mal")
	// inject malformed existing value
	require.NoError(t, s.client.Set(ctx, rkey, `not-json`, TTL).Err())
	st, err := s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_mal", 10, f, 1)
	require.NoError(t, err)
	require.Equal(t, "conflict", st, "malformed existing must not refresh, fail closed")
	b, ok, err := s.Lookup(ctx, 1, 1, rid, "responses", "resp_mal")
	require.NoError(t, err)
	require.False(t, ok)
	require.Nil(t, b)
	// noncanonical reordered variant with same logical values
	canon := encodeWire(Binding{AccountID: 10, Fingerprint: f, IdentityRevision: 1, RedisAcked: true})
	var w redisWire
	require.NoError(t, json.Unmarshal(canon, &w))
	reordered := `{"identity_revision":"` + w.IdentityRevision + `","account_id":"` + w.AccountID + `","fingerprint":"` + w.Fingerprint + `","redis_acked":true}`
	require.NoError(t, s.client.Set(ctx, rkey, reordered, TTL).Err())
	st, err = s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_mal", 10, f, 1)
	require.NoError(t, err)
	require.Equal(t, "conflict", st, "reordered wire must not refresh")
	b, ok, err = s.Lookup(ctx, 1, 1, rid, "responses", "resp_mal")
	require.NoError(t, err)
	require.False(t, ok, "reordered wire must fail strict canonical check")
	require.Nil(t, b)
	// duplicate key variant
	dup := `{"account_id":"10","account_id":"10","fingerprint":"` + w.Fingerprint + `","identity_revision":"1","redis_acked":true}`
	require.NoError(t, s.client.Set(ctx, rkey, dup, TTL).Err())
	_, okDup := parseBinding([]byte(dup))
	require.False(t, okDup, "duplicate keys must be rejected")
	b, ok, err = s.Lookup(ctx, 1, 1, rid, "responses", "resp_mal")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestContinuationCanonicalExactRefresh(t *testing.T) {
	_, s := newMiniredisStore(t, "secret-canonical-123456")
	rid := routeID(t)
	f := fp(t)
	ctx := t.Context()
	st, err := s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_canon", 10, f, 1)
	require.NoError(t, err)
	require.Equal(t, "created", st)
	// exact canonical payload must refresh
	st, err = s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_canon", 10, f, 1)
	require.NoError(t, err)
	require.Equal(t, "refreshed", st)
	// verify Redis wire is exactly canonical
	rkey, _ := s.RedisKey(1, 1, rid, "responses", "resp_canon")
	val, err := s.client.Get(ctx, rkey).Result()
	require.NoError(t, err)
	b := Binding{AccountID: 10, Fingerprint: f, IdentityRevision: 1, RedisAcked: true}
	require.Equal(t, string(encodeWire(b)), val, "stored wire must be canonical exact")
	// whitespace variant must be rejected
	ws := string(encodeWire(b))
	wsSpaced := ws[:1] + " " + ws[1:]
	_, ok := parseBinding([]byte(wsSpaced))
	require.False(t, ok, "whitespace variant must be noncanonical")
}

func TestContinuationRTTStaleLookup(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := redisx.Open(redisx.Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisx.Close(c) })
	rid := routeID(t)
	f := fp(t)
	ctx := t.Context()
	base := time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	cur := base
	nowFn := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return cur
	}
	s, err := New(c, "secret-rtt-1234567890")
	require.NoError(t, err)
	s.now = nowFn
	adv := 80 * time.Millisecond
	h := &advHook{mu: &mu, cur: &cur, d: adv}
	c.AddHook(h)
	// PTTL < RTT: the key dies mid-command, lookup must report not-found
	st, err := s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_rtt", 10, f, 1)
	require.NoError(t, err)
	require.Equal(t, "created", st)
	rkey, _ := s.RedisKey(1, 1, rid, "responses", "resp_rtt")
	require.NoError(t, c.PExpire(ctx, rkey, 50*time.Millisecond).Err())
	mu.Lock()
	cur = base
	mu.Unlock()
	b, ok, err := s.Lookup(ctx, 1, 1, rid, "responses", "resp_rtt")
	require.NoError(t, err)
	require.False(t, ok, "PTTL 50ms < RTT 80ms must be stale, not found")
	require.Nil(t, b)
	// PTTL > RTT: still alive at completion, must be found
	require.NoError(t, c.PExpire(ctx, rkey, 200*time.Millisecond).Err())
	mu.Lock()
	cur = base
	mu.Unlock()
	b, ok, err = s.Lookup(ctx, 1, 1, rid, "responses", "resp_rtt")
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, b)
}

func TestContinuationPTTLNearExpiry(t *testing.T) {
	mr, s := newMiniredisStore(t, "secret-pttl-1234567890")
	rid := routeID(t)
	f := fp(t)
	ctx := t.Context()
	_, err := s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_pttl", 10, f, 1)
	require.NoError(t, err)
	rkey, _ := s.RedisKey(1, 1, rid, "responses", "resp_pttl")
	require.NoError(t, s.client.PExpire(ctx, rkey, 120*time.Millisecond).Err())
	b, ok, err := s.Lookup(ctx, 1, 1, rid, "responses", "resp_pttl")
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, b)
	mr.FastForward(300 * time.Millisecond)
	_, ok, err = s.Lookup(ctx, 1, 1, rid, "responses", "resp_pttl")
	require.NoError(t, err)
	require.False(t, ok, "after PTTL expiry, lookup must miss")
}

func TestContinuationRedisRestart(t *testing.T) {
	mr, s := newMiniredisStore(t, "secret-restart-123456")
	rid := routeID(t)
	f := fp(t)
	ctx := t.Context()
	_, err := s.CreateOrRefresh(ctx, 5, 2, rid, "responses", "resp_restart", 10, f, 1)
	require.NoError(t, err)
	s2, err := New(s.client, "secret-restart-123456")
	require.NoError(t, err)
	b, ok, err := s2.Lookup(ctx, 5, 2, rid, "responses", "resp_restart")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, int64(10), b.AccountID)
	mr.FlushDB()
	s3, err := New(s.client, "secret-restart-123456")
	require.NoError(t, err)
	_, ok, err = s3.Lookup(ctx, 5, 2, rid, "responses", "resp_restart")
	require.NoError(t, err)
	require.False(t, ok, "after Redis restart/flush, binding gone")
	st, err := s3.CreateOrRefresh(ctx, 5, 2, rid, "responses", "resp_restart", 10, f, 1)
	require.NoError(t, err)
	require.Equal(t, "created", st)
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
	_ = luaCAS.Load(ctx, c).Err()
	_ = luaLookup.Load(ctx, c).Err()
	h.n.Store(0)
	st, err := s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_cnt", 10, f, 1)
	require.NoError(t, err)
	require.Equal(t, "created", st)
	require.Equal(t, int64(1), h.n.Load(), "CreateOrRefresh must be single EVALSHA")
	h.n.Store(0)
	b, ok, err := s.Lookup(ctx, 1, 1, rid, "responses", "resp_cnt")
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, b)
	require.Equal(t, int64(1), h.n.Load(), "Lookup must be single EVALSHA (GET+PTTL lua)")
	h.n.Store(0)
	b2, ok, err := s.Lookup(ctx, 1, 1, rid, "responses", "resp_cnt")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, b.Fingerprint, b2.Fingerprint)
	require.Equal(t, int64(1), h.n.Load(), "repeat Lookup must re-check Redis, never serve from local state")
	h.n.Store(0)
	_, ok, err = s.Lookup(ctx, 1, 1, rid, "responses", "resp_missing")
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, int64(1), h.n.Load())
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
	sOld, _ := New(s.client, "old-secret-123456789012")
	sNew, _ := New(s.client, "new-secret-654321098765")
	_, err = sOld.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_rot", 10, f, 1)
	require.NoError(t, err)
	_, ok, err := sNew.Lookup(ctx, 1, 1, rid, "responses", "resp_rot")
	require.NoError(t, err)
	require.False(t, ok, "rotated secret must not find old binding")
}

func TestContinuationLookupWarmRedisDownFailClosed(t *testing.T) {
	mr, s := newMiniredisStore(t, "secret-outage-123456789")
	rid := routeID(t)
	f := fp(t)
	ctx := t.Context()
	_, err := s.CreateOrRefresh(ctx, 7, 3, rid, "responses", "resp_outage", 10, f, 1)
	require.NoError(t, err)
	b, ok, err := s.Lookup(ctx, 7, 3, rid, "responses", "resp_outage")
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, b)
	mr.Close()
	b2, ok2, err := s.Lookup(ctx, 7, 3, rid, "responses", "resp_outage")
	require.Error(t, err, "warm lookup must still consult Redis and fail closed on outage")
	require.Nil(t, b2)
	require.False(t, ok2)
}

func TestContinuationLookupKeyDeletedReturnsNotFound(t *testing.T) {
	mr, s := newMiniredisStore(t, "secret-deleted-12345678")
	rid := routeID(t)
	f := fp(t)
	ctx := t.Context()
	_, err := s.CreateOrRefresh(ctx, 8, 4, rid, "responses", "resp_del", 10, f, 1)
	require.NoError(t, err)
	_, ok, err := s.Lookup(ctx, 8, 4, rid, "responses", "resp_del")
	require.NoError(t, err)
	require.True(t, ok)
	rkey, _ := s.RedisKey(8, 4, rid, "responses", "resp_del")
	require.True(t, mr.Del(rkey))
	b, ok, err := s.Lookup(ctx, 8, 4, rid, "responses", "resp_del")
	require.NoError(t, err)
	require.False(t, ok, "deleted Redis key must return not-found even after a warm lookup")
	require.Nil(t, b)
}
