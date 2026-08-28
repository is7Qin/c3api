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
	// cold second Store must fetch from Redis, not L1, to prove string-decimal wire
	s2, err := New(c, "secret-int53-1234567890")
	require.NoError(t, err)
	require.Equal(t, 0, s2.L1Len())
	b, ok, err := s2.Lookup(ctx, 1, 1, rid, "responses", "resp_large")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, largeAcc, b.AccountID)
	require.Equal(t, largeRev, b.Revision)
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
	_ = mr
}

func TestContinuationMalformedFailClosed(t *testing.T) {
	mr, s := newMiniredisStore(t, "secret-malformed-123456")
	rid := routeID(t)
	f := fp(t)
	ctx := t.Context()
	_, err := s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_ok", 10, f, 1)
	require.NoError(t, err)
	rkey, _ := s.RedisKey(1, 1, rid, "responses", "resp_ok")
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
		val, err := s.client.Get(ctx, badKey).Result()
		require.NoError(t, err)
		_, ok := parseBinding([]byte(val))
		require.False(t, ok, "case %d should be invalid: %s", i, bad)
		require.NoError(t, s.client.Set(ctx, rkey, bad, TTL).Err())
		s.l1 = newL1(DefaultMaxEntries, DefaultMaxBytes)
		b, ok, err := s.Lookup(ctx, 1, 1, rid, "responses", "resp_ok")
		require.NoError(t, err)
		require.False(t, ok, "case %d lookup should fail closed", i)
		require.Nil(t, b)
		require.Equal(t, 0, s.L1Len(), "malformed value must not be cached")
	}
	_, err = s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_ok", 10, f, 1)
	require.NoError(t, err)
	_ = mr
}

func TestContinuationMalformedCannotRefreshOrCache(t *testing.T) {
	mr, s := newMiniredisStore(t, "secret-malformed-refresh-1")
	rid := routeID(t)
	f := fp(t)
	ctx := t.Context()
	_, err := s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_mal", 10, f, 1)
	require.NoError(t, err)
	rkey, _ := s.RedisKey(1, 1, rid, "responses", "resp_mal")
	// inject malformed existing value
	require.NoError(t, s.client.Set(ctx, rkey, `not-json`, TTL).Err())
	// clear L1 to force Redis path
	s.l1 = newL1(DefaultMaxEntries, DefaultMaxBytes)
	st, err := s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_mal", 10, f, 1)
	require.NoError(t, err)
	require.Equal(t, "conflict", st, "malformed existing must not refresh, fail closed")
	require.Equal(t, 0, s.L1Len(), "malformed refresh must not cache")
	b, ok, err := s.Lookup(ctx, 1, 1, rid, "responses", "resp_mal")
	require.NoError(t, err)
	require.False(t, ok)
	require.Nil(t, b)
	require.Equal(t, 0, s.L1Len(), "malformed lookup must not enter L1")
	// noncanonical whitespace should also fail to refresh
	canon := encodeWire(Binding{AccountID: 10, Fingerprint: f, Revision: 1, RedisAcked: true})
	// inject reordered + whitespace variant with same logical values
	var w redisWire
	require.NoError(t, json.Unmarshal(canon, &w))
	reordered := `{"revision":"` + w.Revision + `","account_id":"` + w.AccountID + `","fingerprint":"` + w.Fingerprint + `","redis_acked":true}`
	require.NoError(t, s.client.Set(ctx, rkey, reordered, TTL).Err())
	s.l1 = newL1(DefaultMaxEntries, DefaultMaxBytes)
	st, err = s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_mal", 10, f, 1)
	require.NoError(t, err)
	require.Equal(t, "conflict", st, "reordered wire must not refresh")
	s.l1 = newL1(DefaultMaxEntries, DefaultMaxBytes)
	b, ok, err = s.Lookup(ctx, 1, 1, rid, "responses", "resp_mal")
	require.NoError(t, err)
	require.False(t, ok, "reordered wire must fail strict canonical check")
	require.Nil(t, b)
	require.Equal(t, 0, s.L1Len())
	// duplicate key variant
	dup := `{"account_id":"10","account_id":"10","fingerprint":"` + w.Fingerprint + `","revision":"1","redis_acked":true}`
	require.NoError(t, s.client.Set(ctx, rkey, dup, TTL).Err())
	s.l1 = newL1(DefaultMaxEntries, DefaultMaxBytes)
	_, okDup := parseBinding([]byte(dup))
	require.False(t, okDup, "duplicate keys must be rejected")
	b, ok, err = s.Lookup(ctx, 1, 1, rid, "responses", "resp_mal")
	require.NoError(t, err)
	require.False(t, ok)
	require.Equal(t, 0, s.L1Len())
	_ = mr
}

func TestContinuationCanonicalExactRefresh(t *testing.T) {
	_, s := newMiniredisStore(t, "secret-canonical-123456")
	rid := routeID(t)
	f := fp(t)
	ctx := t.Context()
	st, err := s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_canon", 10, f, 1)
	require.NoError(t, err)
	require.Equal(t, "created", st)
	require.Equal(t, 1, s.L1Len())
	// exact canonical payload must refresh
	st, err = s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_canon", 10, f, 1)
	require.NoError(t, err)
	require.Equal(t, "refreshed", st)
	// verify Redis wire is exactly canonical
	rkey, _ := s.RedisKey(1, 1, rid, "responses", "resp_canon")
	val, err := s.client.Get(ctx, rkey).Result()
	require.NoError(t, err)
	b := Binding{AccountID: 10, Fingerprint: f, Revision: 1, RedisAcked: true}
	require.Equal(t, string(encodeWire(b)), val, "stored wire must be canonical exact")
	// whitespace variant must be rejected
	ws := string(encodeWire(b))
	wsSpaced := ws[:1] + " " + ws[1:]
	_, ok := parseBinding([]byte(wsSpaced))
	require.False(t, ok, "whitespace variant must be noncanonical")
}

func TestContinuationRTTDeadlineNoExtension(t *testing.T) {
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
	l1 := newL1(DefaultMaxEntries, DefaultMaxBytes)
	l1.now = nowFn
	s, err := NewWithL1(c, "secret-rtt-1234567890", l1)
	require.NoError(t, err)
	s.now = nowFn
	adv := 80 * time.Millisecond
	h := &advHook{mu: &mu, cur: &cur, d: adv}
	c.AddHook(h)
	// Create path: expiry must be start+TTL, not completion+TTL
	mu.Lock()
	cur = base
	mu.Unlock()
	st, err := s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_rtt_create", 10, f, 1)
	require.NoError(t, err)
	require.Equal(t, "created", st)
	rkey, _ := s.RedisKey(1, 1, rid, "responses", "resp_rtt_create")
	l1.mu.Lock()
	e := l1.entries[rkey]
	l1.mu.Unlock()
	require.NotNil(t, e)
	require.Equal(t, base.Add(TTL), e.expiry, "Create expiry must be start+TTL without RTT extension")
	// Lookup stale case: PTTL < RTT must not cache
	mu.Lock()
	cur = base
	mu.Unlock()
	st, err = s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_rtt_lookup", 10, f, 1)
	require.NoError(t, err)
	require.Equal(t, "created", st)
	rkey2, _ := s.RedisKey(1, 1, rid, "responses", "resp_rtt_lookup")
	require.NoError(t, c.PExpire(ctx, rkey2, 50*time.Millisecond).Err())
	// force cold fetch
	s.l1 = newL1(DefaultMaxEntries, DefaultMaxBytes)
	s.l1.now = nowFn
	l1 = s.l1
	mu.Lock()
	cur = base
	mu.Unlock()
	b, ok, err := s.Lookup(ctx, 1, 1, rid, "responses", "resp_rtt_lookup")
	require.NoError(t, err)
	require.False(t, ok, "PTTL 50ms < RTT 80ms must be stale, not cached")
	require.Nil(t, b)
	require.Equal(t, 0, s.L1Len(), "stale must not enter L1")
	// Lookup non-stale: PTTL 200ms > RTT 80ms must cache with deadline = start+PTTL
	require.NoError(t, c.PExpire(ctx, rkey2, 200*time.Millisecond).Err())
	s.l1 = newL1(DefaultMaxEntries, DefaultMaxBytes)
	s.l1.now = nowFn
	l1 = s.l1
	mu.Lock()
	cur = base
	mu.Unlock()
	b, ok, err = s.Lookup(ctx, 1, 1, rid, "responses", "resp_rtt_lookup")
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, b)
	require.Equal(t, 1, s.L1Len())
	l1.mu.Lock()
	e2 := l1.entries[rkey2]
	l1.mu.Unlock()
	require.NotNil(t, e2)
	// expiry should be start+PTTL (~base+200ms), not completion+PTTL (~base+280ms)
	require.True(t, e2.expiry.Before(base.Add(250*time.Millisecond)), "expiry must be start+PTTL, not completion+PTTL")
	require.True(t, e2.expiry.After(base.Add(120*time.Millisecond)))
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
	require.NoError(t, s.client.PExpire(ctx, rkey, 120*time.Millisecond).Err())
	s.l1 = newL1(DefaultMaxEntries, DefaultMaxBytes)
	b, ok, err := s.Lookup(ctx, 1, 1, rid, "responses", "resp_pttl")
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, b)
	require.Equal(t, 1, s.L1Len())
	// verify L1 expiry is ~PTTL not 24h by using controllable clock
	// instead of sleep, check expiry directly
	s.l1.mu.Lock()
	e := s.l1.entries[rkey]
	s.l1.mu.Unlock()
	require.NotNil(t, e)
	require.True(t, e.expiry.Before(time.Now().Add(500*time.Millisecond)), "L1 expiry should be near PTTL, not 24h")
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
	l1 := newL1(2, 640)
	s, err := NewWithL1(c, "secret-evict-1234567890", l1)
	require.NoError(t, err)
	rid := routeID(t)
	f := fp(t)
	ctx := t.Context()
	h := &countHook{}
	c.AddHook(h)
	_ = luaCAS.Load(ctx, c).Err()
	_ = luaLookup.Load(ctx, c).Err()
	h.n.Store(0)
	for i := 0; i < 3; i++ {
		id := "resp_evict_" + strconv.Itoa(i)
		_, err := s.CreateOrRefresh(ctx, 1, 1, rid, "responses", id, 10, f, 1)
		require.NoError(t, err)
	}
	require.LessOrEqual(t, s.L1Len(), 2)
	targetID := "resp_evict_0"
	targetKey, _ := s.RedisKey(1, 1, rid, "responses", targetID)
	_, ok := s.l1.Get(targetKey)
	if ok {
		found := ""
		for i := 0; i < 3; i++ {
			id := "resp_evict_" + strconv.Itoa(i)
			k, _ := s.RedisKey(1, 1, rid, "responses", id)
			if _, ok2 := s.l1.Get(k); !ok2 {
				found = id
				targetKey = k
				break
			}
		}
		require.NotEmpty(t, found, "one of the 3 must be evicted from original L1")
		targetID = found
		_, ok = s.l1.Get(targetKey)
	}
	require.False(t, ok, "evicted key must be absent from original L1")
	require.False(t, func() bool { _, ok := l1.Get(targetKey); return ok }(), "target must be absent from original L1 instance")
	h.n.Store(0)
	b, ok, err := s.Lookup(ctx, 1, 1, rid, "responses", targetID)
	require.NoError(t, err)
	require.True(t, ok, "evicted key must be re-fetched from Redis via same Store")
	require.NotNil(t, b)
	require.Equal(t, int64(1), h.n.Load(), "re-fetch must be single Redis command")
	require.LessOrEqual(t, s.L1Len(), 2)
	require.LessOrEqual(t, l1.Bytes(), 640)
	_ = mr
	_ = h
}

func TestContinuationFullExpiryReleasesBacking(t *testing.T) {
	base := time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)
	l1 := newL1(100000, 32*1024*1024)
	l1.now = func() time.Time { return base }
	for i := 0; i < 1000; i++ {
		l1.Put("k"+strconv.Itoa(i), Binding{AccountID: 1, Fingerprint: mustFP(), Revision: 1, RedisAcked: true}, time.Second)
	}
	require.Equal(t, 1000, l1.Len())
	capBefore := cap(l1.heap)
	require.Greater(t, capBefore, 500)
	// full expiry: advance beyond all TTL and Put new entry triggers purge that empties
	l1.now = func() time.Time { return base.Add(2 * time.Second) }
	l1.Put("k_new", Binding{AccountID: 1, Fingerprint: mustFP(), Revision: 1, RedisAcked: true}, TTL)
	require.Equal(t, 1, l1.Len())
	require.Equal(t, entryCharged, l1.Bytes())
	l1.mu.Lock()
	capAfter := cap(l1.heap)
	heapLen := len(l1.heap)
	entriesLen := len(l1.entries)
	l1.mu.Unlock()
	require.Equal(t, 1, heapLen)
	require.Equal(t, 1, entriesLen)
	require.Less(t, capAfter, 16, "heap backing must be released after full expiry, got cap %d", capAfter)
	// partial stale compaction: 1000 entries, 900 short TTL, 100 long TTL
	l2 := newL1(100000, 32*1024*1024)
	l2.now = func() time.Time { return base }
	for i := 0; i < 900; i++ {
		l2.Put("sk"+strconv.Itoa(i), Binding{AccountID: 1, Fingerprint: mustFP(), Revision: 1, RedisAcked: true}, time.Second)
	}
	for i := 900; i < 1000; i++ {
		l2.Put("lk"+strconv.Itoa(i), Binding{AccountID: 1, Fingerprint: mustFP(), Revision: 1, RedisAcked: true}, 20*time.Second)
	}
	require.Equal(t, 1000, l2.Len())
	capMid := cap(l2.heap)
	require.Greater(t, capMid, 500)
	l2.now = func() time.Time { return base.Add(2 * time.Second) }
	l2.Put("k_after", Binding{AccountID: 1, Fingerprint: mustFP(), Revision: 1, RedisAcked: true}, TTL)
	// 900 short should be purged, 100 long + 1 new = 101 remains, cap should be compacted
	require.Equal(t, 101, l2.Len())
	l2.mu.Lock()
	capPartial := cap(l2.heap)
	l2.mu.Unlock()
	require.Less(t, capPartial, capMid/2, "partial stale heap must be compacted, before %d after %d", capMid, capPartial)
	require.LessOrEqual(t, capPartial, 2*l2.Len()+64+10, "cap should be close to len after compaction")
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
	require.Equal(t, 0, s2.L1Len())
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
	_ = mr
}

func TestContinuationL1CapsChargedBound(t *testing.T) {
	l1 := newL1(DefaultMaxEntries, DefaultMaxBytes)
	l1.now = func() time.Time { return time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC) }
	for i := 0; i < DefaultMaxEntries+5000; i++ {
		k := "c3api:cont:" + hex.EncodeToString([]byte{byte(i >> 8), byte(i), byte(i >> 16), byte(i >> 24)}) + "xxxxxxxxxxxxxxxxxxxxxxxx"
		l1.Put(k, Binding{AccountID: 1, Fingerprint: mustFP(), Revision: 1, RedisAcked: true}, TTL)
	}
	require.LessOrEqual(t, l1.Len(), DefaultMaxEntries)
	require.LessOrEqual(t, l1.Bytes(), DefaultMaxBytes)
	require.Equal(t, l1.Len()*entryCharged, l1.Bytes(), "charged must be fixed-size * entries")
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
	l1c := newL1(100000, 32*1024*1024)
	now := time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC)
	l1c.now = func() time.Time { return now }
	for i := 0; i < 1000; i++ {
		l1c.Put("k"+strconv.Itoa(i), Binding{AccountID: 1, Fingerprint: mustFP(), Revision: 1, RedisAcked: true}, time.Second)
	}
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
	_ = luaCAS.Load(ctx, c).Err()
	_ = luaLookup.Load(ctx, c).Err()
	h.n.Store(0)
	st, err := s.CreateOrRefresh(ctx, 1, 1, rid, "responses", "resp_cnt", 10, f, 1)
	require.NoError(t, err)
	require.Equal(t, "created", st)
	require.Equal(t, int64(1), h.n.Load(), "CreateOrRefresh must be single EVALSHA")
	s.l1 = newL1(DefaultMaxEntries, DefaultMaxBytes)
	h.n.Store(0)
	b, ok, err := s.Lookup(ctx, 1, 1, rid, "responses", "resp_cnt")
	require.NoError(t, err)
	require.True(t, ok)
	require.NotNil(t, b)
	require.Equal(t, int64(1), h.n.Load(), "cold Lookup must be single EVALSHA (GET+PTTL lua)")
	h.n.Store(0)
	b2, ok, err := s.Lookup(ctx, 1, 1, rid, "responses", "resp_cnt")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, b.Fingerprint, b2.Fingerprint)
	require.Equal(t, int64(0), h.n.Load(), "warm L1 Lookup must not hit Redis")
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
