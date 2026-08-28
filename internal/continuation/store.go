// SPDX-License-Identifier: AGPL-3.0-or-later
package continuation

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/hkdf"

	"github.com/is7qin/c3api/internal/domain"
)

const (
	HKDFLabel   = "c3api/routing/continuation/v1"
	RedisPrefix = "c3api:cont:"
	TTL         = 24 * time.Hour
	ttlSeconds  = int(TTL / time.Second)
)

type Binding struct {
	AccountID   int64
	Fingerprint domain.CandidateFingerprintVal
	Revision    int64
	RedisAcked  bool
}

type redisWire struct {
	AccountID   string `json:"account_id"`
	Fingerprint string `json:"fingerprint"`
	Revision    string `json:"revision"`
	RedisAcked  bool   `json:"redis_acked"`
}

func (b Binding) valid() bool {
	if b.AccountID <= 0 || b.Revision <= 0 || !b.RedisAcked {
		return false
	}
	if b.Fingerprint == (domain.CandidateFingerprintVal{}) {
		return false
	}
	return true
}

func encodeWire(b Binding) []byte {
	w := redisWire{
		AccountID:   strconv.FormatInt(b.AccountID, 10),
		Fingerprint: hex.EncodeToString(b.Fingerprint[:]),
		Revision:    strconv.FormatInt(b.Revision, 10),
		RedisAcked:  true,
	}
	data, _ := json.Marshal(w)
	return data
}

func parseBinding(data []byte) (Binding, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return Binding{}, false
	}
	if len(raw) != 4 {
		return Binding{}, false
	}
	if _, ok := raw["account_id"]; !ok {
		return Binding{}, false
	}
	if _, ok := raw["fingerprint"]; !ok {
		return Binding{}, false
	}
	if _, ok := raw["revision"]; !ok {
		return Binding{}, false
	}
	if _, ok := raw["redis_acked"]; !ok {
		return Binding{}, false
	}
	var w redisWire
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return Binding{}, false
	}
	if dec.InputOffset() < int64(len(data)) {
		if len(bytes.TrimSpace(data[dec.InputOffset():])) != 0 {
			return Binding{}, false
		}
	}
	if !w.RedisAcked {
		return Binding{}, false
	}
	acc, err := strconv.ParseInt(w.AccountID, 10, 64)
	if err != nil || acc <= 0 || strconv.FormatInt(acc, 10) != w.AccountID {
		return Binding{}, false
	}
	rev, err := strconv.ParseInt(w.Revision, 10, 64)
	if err != nil || rev <= 0 || strconv.FormatInt(rev, 10) != w.Revision {
		return Binding{}, false
	}
	if len(w.Fingerprint) != 64 {
		return Binding{}, false
	}
	fb, err := hex.DecodeString(w.Fingerprint)
	if err != nil || len(fb) != 32 {
		return Binding{}, false
	}
	var fp domain.CandidateFingerprintVal
	copy(fp[:], fb)
	if fp == (domain.CandidateFingerprintVal{}) {
		return Binding{}, false
	}
	b := Binding{AccountID: acc, Fingerprint: fp, Revision: rev, RedisAcked: true}
	if !bytes.Equal(encodeWire(b), data) {
		return Binding{}, false
	}
	return b, true
}

func deriveHMACKey(authSecret string) ([]byte, error) {
	if authSecret == "" {
		return nil, fmt.Errorf("continuation: empty auth secret")
	}
	r := hkdf.New(sha256.New, []byte(authSecret), nil, []byte(HKDFLabel))
	key := make([]byte, 32)
	if _, err := io.ReadFull(r, key); err != nil {
		return nil, err
	}
	return key, nil
}

func encodeUvarint(n uint64) []byte {
	var buf [10]byte
	l := binary.PutUvarint(buf[:], n)
	return buf[:l]
}

func canonicalTuple(userID, groupID int64, routeClassID domain.RouteClassIDVal, protocolTag, continuationID string) []byte {
	var out []byte
	out = append(out, 1)
	for _, f := range [][]byte{
		func() []byte { var b [8]byte; binary.BigEndian.PutUint64(b[:], uint64(userID)); return b[:] }(),
		func() []byte { var b [8]byte; binary.BigEndian.PutUint64(b[:], uint64(groupID)); return b[:] }(),
		routeClassID[:],
		[]byte(protocolTag),
		[]byte(continuationID),
	} {
		out = append(out, encodeUvarint(uint64(len(f)))...)
		out = append(out, f...)
	}
	return out
}

func hmacTruncate128(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write(data)
	sum := m.Sum(nil)
	return sum[:16]
}

type Store struct {
	client  *redis.Client
	hmacKey []byte
	l1      *l1Cache
	now     func() time.Time
}

func New(client *redis.Client, authSecret string) (*Store, error) {
	key, err := deriveHMACKey(authSecret)
	if err != nil {
		return nil, err
	}
	return &Store{client: client, hmacKey: key, l1: newL1(DefaultMaxEntries, DefaultMaxBytes), now: time.Now}, nil
}

func NewWithL1(client *redis.Client, authSecret string, l1 *l1Cache) (*Store, error) {
	key, err := deriveHMACKey(authSecret)
	if err != nil {
		return nil, err
	}
	if l1 == nil {
		l1 = newL1(DefaultMaxEntries, DefaultMaxBytes)
	}
	return &Store{client: client, hmacKey: key, l1: l1, now: time.Now}, nil
}

func (s *Store) RedisKey(userID, groupID int64, routeClassID domain.RouteClassIDVal, protocolTag, continuationID string) (string, error) {
	if continuationID == "" {
		return "", fmt.Errorf("continuation: empty continuation_id")
	}
	if protocolTag == "" {
		return "", fmt.Errorf("continuation: empty protocol tag")
	}
	data := canonicalTuple(userID, groupID, routeClassID, protocolTag, continuationID)
	trunc := hmacTruncate128(s.hmacKey, data)
	return RedisPrefix + hex.EncodeToString(trunc), nil
}

var luaCAS = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if not v then
  redis.call('SET', KEYS[1], ARGV[1], 'EX', ARGV[2])
  return 'created'
end
if v == ARGV[1] then
  redis.call('EXPIRE', KEYS[1], ARGV[2])
  return 'refreshed'
else
  return 'conflict'
end
`)

var luaLookup = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if not v then
  return {nil, -2}
end
local ttl = redis.call('PTTL', KEYS[1])
return {v, ttl}
`)

func (s *Store) CreateOrRefresh(ctx context.Context, userID, groupID int64, routeClassID domain.RouteClassIDVal, protocolTag, continuationID string, accountID int64, fingerprint domain.CandidateFingerprintVal, revision int64) (string, error) {
	if accountID <= 0 {
		return "", fmt.Errorf("continuation: invalid account_id %d", accountID)
	}
	if revision <= 0 {
		return "", fmt.Errorf("continuation: invalid revision %d", revision)
	}
	if fingerprint == (domain.CandidateFingerprintVal{}) {
		return "", fmt.Errorf("continuation: zero fingerprint")
	}
	rkey, err := s.RedisKey(userID, groupID, routeClassID, protocolTag, continuationID)
	if err != nil {
		return "", err
	}
	b := Binding{AccountID: accountID, Fingerprint: fingerprint, Revision: revision, RedisAcked: true}
	payload := encodeWire(b)
	start := s.now()
	res, err := luaCAS.Run(ctx, s.client, []string{rkey}, string(payload), strconv.Itoa(ttlSeconds)).Text()
	completion := s.now()
	if err != nil {
		return "", err
	}
	switch res {
	case "created", "refreshed":
		deadline := start.Add(TTL)
		if !deadline.After(completion) {
			return res, nil
		}
		s.l1.putAt(rkey, b, deadline, completion)
		return res, nil
	case "conflict":
		return res, nil
	default:
		return res, fmt.Errorf("continuation: unexpected lua result %q", res)
	}
}

func (s *Store) Lookup(ctx context.Context, userID, groupID int64, routeClassID domain.RouteClassIDVal, protocolTag, continuationID string) (*Binding, bool, error) {
	rkey, err := s.RedisKey(userID, groupID, routeClassID, protocolTag, continuationID)
	if err != nil {
		return nil, false, err
	}
	if b, ok := s.l1.Get(rkey); ok {
		if !b.valid() {
			return nil, false, nil
		}
		cp := b
		return &cp, true, nil
	}
	start := s.now()
	raw, err := luaLookup.Run(ctx, s.client, []string{rkey}).Slice()
	completion := s.now()
	if err != nil {
		if err == redis.Nil {
			return nil, false, nil
		}
		return nil, false, err
	}
	if len(raw) != 2 {
		return nil, false, nil
	}
	if raw[0] == nil {
		return nil, false, nil
	}
	val, ok := raw[0].(string)
	if !ok {
		return nil, false, nil
	}
	var pttl int64
	switch v := raw[1].(type) {
	case int64:
		pttl = v
	case int:
		pttl = int64(v)
	default:
		return nil, false, nil
	}
	if pttl <= 0 {
		return nil, false, nil
	}
	deadline := start.Add(time.Duration(pttl) * time.Millisecond)
	if !deadline.After(completion) {
		return nil, false, nil
	}
	b, valid := parseBinding([]byte(val))
	if !valid {
		return nil, false, nil
	}
	s.l1.putAt(rkey, b, deadline, completion)
	cp := b
	return &cp, true, nil
}

func (s *Store) L1Len() int   { return s.l1.Len() }
func (s *Store) L1Bytes() int { return s.l1.Bytes() }
