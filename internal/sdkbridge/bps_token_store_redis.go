// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

// ErrBPSTokenConflict is returned when a refresh winner has already advanced
// the token revision. The bps-sdk adapter translates it to
// bps.ErrTokenStoreConflict so TokenAuth reloads the winner instead of treating
// a normal cross-instance race as account-fatal.
var ErrBPSTokenConflict = errors.New("sdkbridge: BPS token revision conflict")

// ErrBPSTokenRecordCorrupt means a Redis record exists but does not contain a
// complete, internally consistent owner/token tuple. Reads fail closed rather
// than returning a partially trusted token set.
var ErrBPSTokenRecordCorrupt = errors.New("sdkbridge: corrupt BPS token record")

const bpsTokenRedisPrefix = "bps:token:"

const bpsTokenCASLua = `
local key = KEYS[1]
local expected = tonumber(ARGV[1])
local c3 = ARGV[2]
local identity = ARGV[3]
local owner = ARGV[4]
local upstream = ARGV[5]
local access = ARGV[6]
local refresh = ARGV[7]
local expires = ARGV[8]

if redis.call('EXISTS', key) == 0 then
  if expected ~= 0 then
    return -1
  end
  redis.call('HSET', key,
    'c3_account_id', c3,
    'identity_revision', identity,
    'owner', owner,
    'upstream_account_id', upstream,
    'token_revision', '1',
    'access_token', access,
    'refresh_token', refresh,
    'expires_at_unix_nano', expires)
  return 1
end

local currentC3 = redis.call('HGET', key, 'c3_account_id')
local currentIdentity = tonumber(redis.call('HGET', key, 'identity_revision'))
local currentOwner = redis.call('HGET', key, 'owner')
local currentUpstream = redis.call('HGET', key, 'upstream_account_id')
if currentC3 ~= c3 or currentOwner ~= owner or currentUpstream ~= upstream or not currentIdentity then
  return -2
end
-- A strictly newer identity revision may seed a replacement record only from
-- revision zero. Older adapters can never overwrite the newer K.
if currentIdentity < tonumber(identity) then
  if expected ~= 0 then
    return -1
  end
  redis.call('HSET', key,
    'c3_account_id', c3,
    'identity_revision', identity,
    'owner', owner,
    'upstream_account_id', upstream,
    'token_revision', '1',
    'access_token', access,
    'refresh_token', refresh,
    'expires_at_unix_nano', expires)
  return 1
end
if currentIdentity ~= tonumber(identity) then
  return -2
end

local current = tonumber(redis.call('HGET', key, 'token_revision'))
if not current then
  return -3
end
if current ~= expected then
  return -1
end
local next = current + 1
redis.call('HSET', key,
  'token_revision', tostring(next),
  'access_token', access,
  'refresh_token', refresh,
  'expires_at_unix_nano', expires)
return next
`

// RedisBPSTokenStore is the cross-instance CAS winner store for BPS OAuth
// rotation. The Redis key is based only on the verified upstream ChatGPT
// account ID; c3 account ID, identity revision K, and owner are fields guarded
// atomically by the Lua CAS script. This preserves the bps-sdk account-claim
// contract without concatenating c3/K into its accountID argument.
type RedisBPSTokenStore struct {
	client redis.UniversalClient
	prefix string
}

func NewRedisBPSTokenStore(client redis.UniversalClient) *RedisBPSTokenStore {
	return &RedisBPSTokenStore{client: client, prefix: bpsTokenRedisPrefix}
}

func (s *RedisBPSTokenStore) Load(ctx context.Context, key BPSTokenKey) (BPSTokenSet, int64, error) {
	if err := validateBPSTokenKey(key); err != nil {
		return BPSTokenSet{}, 0, err
	}
	if s == nil || s.client == nil {
		return BPSTokenSet{}, 0, ErrBPSStoreUnavailable
	}
	values, err := s.client.HGetAll(ctx, s.redisKey(key.UpstreamAccountID)).Result()
	if err != nil {
		return BPSTokenSet{}, 0, err
	}
	if len(values) == 0 {
		return BPSTokenSet{}, 0, nil
	}
	storedIdentity, err := validateBPSTokenOwner(values, key)
	if err != nil {
		return BPSTokenSet{}, 0, err
	}
	if storedIdentity < key.IdentityRevision {
		// The current caller is a newer K. Treat the old record as
		// unreachable and let TokenAuth seed it through SaveCAS(0).
		return BPSTokenSet{}, 0, nil
	}
	if storedIdentity != key.IdentityRevision {
		return BPSTokenSet{}, 0, ErrBPSOwnerMismatch
	}
	if err := validateBPSTokenRecord(values, key); err != nil {
		return BPSTokenSet{}, 0, err
	}
	revision, err := parsePositiveInt(values, "token_revision")
	if err != nil {
		return BPSTokenSet{}, 0, err
	}
	expires, err := parsePositiveInt(values, "expires_at_unix_nano")
	if err != nil {
		return BPSTokenSet{}, 0, err
	}
	return BPSTokenSet{
		AccessToken:  values["access_token"],
		RefreshToken: values["refresh_token"],
		ExpiresAt:    time.Unix(0, expires),
	}, revision, nil
}

func (s *RedisBPSTokenStore) SaveCAS(ctx context.Context, key BPSTokenKey, expectedRevision int64, set BPSTokenSet) (int64, error) {
	if err := validateBPSTokenKey(key); err != nil {
		return 0, err
	}
	if expectedRevision < 0 || set.AccessToken == "" || set.RefreshToken == "" || set.ExpiresAt.IsZero() {
		return 0, ErrBPSStoreUnavailable
	}
	if s == nil || s.client == nil {
		return 0, ErrBPSStoreUnavailable
	}
	result, err := s.client.Eval(ctx, bpsTokenCASLua, []string{s.redisKey(key.UpstreamAccountID)},
		expectedRevision, strconv.FormatInt(key.AccountID, 10),
		strconv.FormatInt(key.IdentityRevision, 10), key.Owner, key.UpstreamAccountID,
		set.AccessToken, set.RefreshToken, strconv.FormatInt(set.ExpiresAt.UnixNano(), 10)).Int64()
	if err != nil {
		return 0, err
	}
	switch result {
	case -1:
		return 0, fmt.Errorf("%w: expected revision %d", ErrBPSTokenConflict, expectedRevision)
	case -2:
		return 0, ErrBPSOwnerMismatch
	case -3:
		return 0, ErrBPSTokenRecordCorrupt
	case 0:
		return 0, ErrBPSTokenRecordCorrupt
	default:
		return result, nil
	}
}

func (s *RedisBPSTokenStore) redisKey(upstreamAccountID string) string {
	prefix := s.prefix
	if prefix == "" {
		prefix = bpsTokenRedisPrefix
	}
	return prefix + upstreamAccountID
}

func validateBPSTokenKey(key BPSTokenKey) error {
	if key.AccountID <= 0 || key.IdentityRevision <= 0 || key.Owner == "" || key.UpstreamAccountID == "" {
		return ErrBPSOwnerMismatch
	}
	return nil
}

func validateBPSTokenOwner(values map[string]string, key BPSTokenKey) (int64, error) {
	if values["c3_account_id"] != strconv.FormatInt(key.AccountID, 10) ||
		values["owner"] != key.Owner ||
		values["upstream_account_id"] != key.UpstreamAccountID {
		return 0, ErrBPSOwnerMismatch
	}
	return parsePositiveInt(values, "identity_revision")
}

func validateBPSTokenRecord(values map[string]string, key BPSTokenKey) error {
	if values["c3_account_id"] != strconv.FormatInt(key.AccountID, 10) ||
		values["identity_revision"] != strconv.FormatInt(key.IdentityRevision, 10) ||
		values["owner"] != key.Owner ||
		values["upstream_account_id"] != key.UpstreamAccountID ||
		values["access_token"] == "" || values["refresh_token"] == "" {
		return ErrBPSOwnerMismatch
	}
	if _, err := parsePositiveInt(values, "token_revision"); err != nil {
		return err
	}
	if _, err := parsePositiveInt(values, "expires_at_unix_nano"); err != nil {
		return err
	}
	return nil
}

func parsePositiveInt(values map[string]string, field string) (int64, error) {
	raw := values[field]
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%w: invalid %s", ErrBPSTokenRecordCorrupt, field)
	}
	return n, nil
}

var _ BPSTokenStore = (*RedisBPSTokenStore)(nil)
