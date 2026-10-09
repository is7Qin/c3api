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
	// IdentityRevision 是**身份代际 K**（identity_revision），不是客户端 CAS
	// 令牌 C（lifecycle_revision）。绑定按 (I=指纹, K) 围栏在途续跑；
	// C 只围栏管理员写入，与绑定身份无关。
	IdentityRevision int64
	RedisAcked       bool
}

type redisWire struct {
	AccountID        string `json:"account_id"`
	Fingerprint      string `json:"fingerprint"`
	IdentityRevision string `json:"identity_revision"`
	RedisAcked       bool   `json:"redis_acked"`
}

func encodeWire(b Binding) []byte {
	w := redisWire{
		AccountID:        strconv.FormatInt(b.AccountID, 10),
		Fingerprint:      hex.EncodeToString(b.Fingerprint[:]),
		IdentityRevision: strconv.FormatInt(b.IdentityRevision, 10),
		RedisAcked:       true,
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
	if _, ok := raw["identity_revision"]; !ok {
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
	rev, err := strconv.ParseInt(w.IdentityRevision, 10, 64)
	if err != nil || rev <= 0 || strconv.FormatInt(rev, 10) != w.IdentityRevision {
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
	b := Binding{AccountID: acc, Fingerprint: fp, IdentityRevision: rev, RedisAcked: true}
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
	now     func() time.Time
}

func New(client *redis.Client, authSecret string) (*Store, error) {
	key, err := deriveHMACKey(authSecret)
	if err != nil {
		return nil, err
	}
	return &Store{client: client, hmacKey: key, now: time.Now}, nil
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

// luaCASSource 是绑定 CAS 脚本源：同步路径经 luaCAS（Script.Run：EVALSHA +
// NOSCRIPT 冷启回退）执行；批量路径把它作为**普通 EVAL** 排进 pipeline（不依赖
// Script.Run 排队期 fallback——pipeline 内 EVALSHA 的 NOSCRIPT 在 Exec 时才出现，
// 回退无法生效）。两条路径同源脚本、同 source 常量。
const luaCASSource = `
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
`

var luaCAS = redis.NewScript(luaCASSource)

var luaLookup = redis.NewScript(`
local v = redis.call('GET', KEYS[1])
if not v then
  return {nil, -2}
end
local ttl = redis.call('PTTL', KEYS[1])
return {v, ttl}
`)

// validateBind 是绑定写入的共享前置校验（同步 CreateOrRefresh 与批量
// CreateOrRefreshBatch 逐条同源）：accountID/identityRevision 必须为正、
// fingerprint 非零。RedisKey 的非空校验（continuationID/protocolTag）在各调用
// 内由 RedisKey 自身完成。
func validateBind(accountID, identityRevision int64, fingerprint domain.CandidateFingerprintVal) error {
	if accountID <= 0 {
		return fmt.Errorf("continuation: invalid account_id %d", accountID)
	}
	if identityRevision <= 0 {
		return fmt.Errorf("continuation: invalid identity revision %d", identityRevision)
	}
	if fingerprint == (domain.CandidateFingerprintVal{}) {
		return fmt.Errorf("continuation: zero fingerprint")
	}
	return nil
}

func (s *Store) CreateOrRefresh(ctx context.Context, userID, groupID int64, routeClassID domain.RouteClassIDVal, protocolTag, continuationID string, accountID int64, fingerprint domain.CandidateFingerprintVal, identityRevision int64) (string, error) {
	if err := validateBind(accountID, identityRevision, fingerprint); err != nil {
		return "", err
	}
	rkey, err := s.RedisKey(userID, groupID, routeClassID, protocolTag, continuationID)
	if err != nil {
		return "", err
	}
	b := Binding{AccountID: accountID, Fingerprint: fingerprint, IdentityRevision: identityRevision, RedisAcked: true}
	payload := encodeWire(b)
	res, err := luaCAS.Run(ctx, s.client, []string{rkey}, string(payload), strconv.Itoa(ttlSeconds)).Text()
	if err != nil {
		return "", err
	}
	switch res {
	case "created", "refreshed", "conflict":
		return res, nil
	default:
		return res, fmt.Errorf("continuation: unexpected lua result %q", res)
	}
}

// BindRequest 是批量绑定的单条输入（异步 worker 的首帧快照）：与同步
// CreateOrRefresh 的参数一一对应，但整条以值快照携带——worker 不得持有
// dispatch/请求 ctx/relay 切片。
type BindRequest struct {
	UserID           int64
	GroupID          int64
	RouteClassID     domain.RouteClassIDVal
	ProtocolTag      string
	ContinuationID   string
	AccountID        int64
	Fingerprint      domain.CandidateFingerprintVal
	IdentityRevision int64
}

// BindResult 是批量绑定的单条结果：Status ∈ {"created","refreshed","conflict"}；
// 该条在排队前的校验/编码错误（此时 Status 为空，Err 非 nil）逐条隔离，不阻断
// 其余条目。
type BindResult struct {
	Status string
	Err    error
}

// CreateOrRefreshBatch 与 CreateOrRefresh 同源校验/同源 key/payload，但把每个
// EVAL 排进同一 pipeline，**一次 Exec** 完成（一次 Redis 往返）。逐条结果区分
// created/refreshed/conflict 与逐条 I/O/编码错误；pipeline Exec 级错误（连接/
// 协议）→ 外层 error（整批失败），此时返回 nil 切片。
func (s *Store) CreateOrRefreshBatch(ctx context.Context, reqs []BindRequest) ([]BindResult, error) {
	results := make([]BindResult, len(reqs))
	if len(reqs) == 0 {
		return results, nil
	}
	pipe := s.client.Pipeline()
	cmds := make([]*redis.Cmd, len(reqs))
	queued := make([]bool, len(reqs))
	for i := range reqs {
		req := &reqs[i]
		if err := validateBind(req.AccountID, req.IdentityRevision, req.Fingerprint); err != nil {
			results[i] = BindResult{Err: err}
			continue
		}
		rkey, err := s.RedisKey(req.UserID, req.GroupID, req.RouteClassID, req.ProtocolTag, req.ContinuationID)
		if err != nil {
			results[i] = BindResult{Err: err}
			continue
		}
		b := Binding{AccountID: req.AccountID, Fingerprint: req.Fingerprint, IdentityRevision: req.IdentityRevision, RedisAcked: true}
		payload := encodeWire(b)
		cmds[i] = pipe.Eval(ctx, luaCASSource, []string{rkey}, string(payload), strconv.Itoa(ttlSeconds))
		queued[i] = true
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return nil, err
	}
	for i := range reqs {
		if !queued[i] {
			continue
		}
		res, err := cmds[i].Text()
		if err != nil {
			results[i] = BindResult{Err: err}
			continue
		}
		switch res {
		case "created", "refreshed", "conflict":
			results[i] = BindResult{Status: res}
		default:
			results[i] = BindResult{Err: fmt.Errorf("continuation: unexpected lua result %q", res)}
		}
	}
	return results, nil
}

func (s *Store) Lookup(ctx context.Context, userID, groupID int64, routeClassID domain.RouteClassIDVal, protocolTag, continuationID string) (*Binding, bool, error) {
	rkey, err := s.RedisKey(userID, groupID, routeClassID, protocolTag, continuationID)
	if err != nil {
		return nil, false, err
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
	// Freshness: the key must still be alive at command completion, measured
	// from the pre-command clock sample (no RTT extension).
	deadline := start.Add(time.Duration(pttl) * time.Millisecond)
	if !deadline.After(completion) {
		return nil, false, nil
	}
	b, valid := parseBinding([]byte(val))
	if !valid {
		return nil, false, nil
	}
	cp := b
	return &cp, true, nil
}
