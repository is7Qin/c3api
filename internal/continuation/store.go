// SPDX-License-Identifier: AGPL-3.0-or-later
package continuation

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
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
	AccountID   int64  `json:"account_id"`
	Fingerprint string `json:"fingerprint"` // hex 64
	Revision    int64  `json:"revision"`
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
}

func New(client *redis.Client, authSecret string) (*Store, error) {
	key, err := deriveHMACKey(authSecret)
	if err != nil {
		return nil, err
	}
	return &Store{client: client, hmacKey: key, l1: newL1(DefaultMaxEntries, DefaultMaxBytes)}, nil
}

func NewWithL1(client *redis.Client, authSecret string, l1 *l1Cache) (*Store, error) {
	key, err := deriveHMACKey(authSecret)
	if err != nil {
		return nil, err
	}
	if l1 == nil {
		l1 = newL1(DefaultMaxEntries, DefaultMaxBytes)
	}
	return &Store{client: client, hmacKey: key, l1: l1}, nil
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
local ok, cur = pcall(cjson.decode, v)
if not ok or type(cur) ~= 'table' then
  return 'conflict'
end
if cur.account_id == tonumber(ARGV[3]) and cur.fingerprint == ARGV[4] and cur.revision == tonumber(ARGV[5]) then
  redis.call('EXPIRE', KEYS[1], ARGV[2])
  return 'refreshed'
else
  return 'conflict'
end
`)

func (s *Store) CreateOrRefresh(ctx context.Context, userID, groupID int64, routeClassID domain.RouteClassIDVal, protocolTag, continuationID string, accountID int64, fingerprint domain.CandidateFingerprintVal, revision int64) (string, error) {
	rkey, err := s.RedisKey(userID, groupID, routeClassID, protocolTag, continuationID)
	if err != nil {
		return "", err
	}
	b := Binding{AccountID: accountID, Fingerprint: hex.EncodeToString(fingerprint[:]), Revision: revision}
	payload, err := json.Marshal(b)
	if err != nil {
		return "", err
	}
	res, err := luaCAS.Run(ctx, s.client, []string{rkey}, string(payload), ttlSeconds, accountID, b.Fingerprint, revision).Text()
	if err != nil {
		return "", err
	}
	switch res {
	case "created", "refreshed":
		s.l1.Put(rkey, b, TTL)
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
		cp := b
		return &cp, true, nil
	}
	val, err := s.client.Get(ctx, rkey).Result()
	if err == redis.Nil {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	var b Binding
	if err := json.Unmarshal([]byte(val), &b); err != nil {
		return nil, false, nil
	}
	s.l1.Put(rkey, b, TTL)
	cp := b
	return &cp, true, nil
}

func (s *Store) L1Len() int   { return s.l1.Len() }
func (s *Store) L1Bytes() int { return s.l1.Bytes() }
