// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/logx"
)

// HealthState enumerates runtime health projection states.
// Severity order: OPEN > RETRY_AFTER > PROBING > READY.
type HealthState int

const (
	StateOPEN HealthState = iota
	StateRetryAfter
	StateProbing
	StateReady
)

func (s HealthState) String() string {
	switch s {
	case StateOPEN:
		return "OPEN"
	case StateRetryAfter:
		return "RETRY_AFTER"
	case StateProbing:
		return "PROBING"
	case StateReady:
		return "READY"
	default:
		return "UNKNOWN"
	}
}

// severity returns numeric severity for EffectiveState comparison.
// Higher value = more severe, consistent with OPEN>RETRY_AFTER>PROBING>READY.
func (s HealthState) severity() int {
	switch s {
	case StateOPEN:
		return 4
	case StateRetryAfter:
		return 3
	case StateProbing:
		return 2
	case StateReady:
		return 1
	default:
		return 0
	}
}

// HealthKey is the immutable composite key account+quality|*+revision.
// quality is either a QualityClassID hex or "*" for wildcard (account scope).
// IdentityRevision 是**身份代际 K**（identity_revision），不是客户端 CAS 令牌
// C（lifecycle_revision）。健康记录按 K 隔离：K 推进 ⇒ 旧记录不再被查询。
// 命名承重——泛化的 Revision 会让调用点把 C 静默传进来（SetProbing 曾如此，
// 导致 recover 写的 PROBING 记录在 EffectiveState 的 K 查询下永不命中）。
type HealthKey struct {
	AccountID        int64
	Quality          string // hex or "*"
	IdentityRevision int64
}

func (k HealthKey) String() string {
	return fmt.Sprintf("%d:%s:%d", k.AccountID, k.Quality, k.IdentityRevision)
}

func parseHealthKey(s string) (HealthKey, bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return HealthKey{}, false
	}
	var acc, rev int64
	_, err := fmt.Sscanf(parts[0], "%d", &acc)
	if err != nil {
		return HealthKey{}, false
	}
	_, err = fmt.Sscanf(parts[2], "%d", &rev)
	if err != nil {
		return HealthKey{}, false
	}
	return HealthKey{AccountID: acc, Quality: parts[1], IdentityRevision: rev}, true
}

// healthEntry is the immutable per-key record stored in Redis HASH and view.
type healthEntry struct {
	Key        HealthKey   `json:"key"`
	State      HealthState `json:"state"`
	Generation int64       `json:"gen"`
	Revision   int64       `json:"rev"`
	UpdatedAt  int64       `json:"updated_at"` // unix milli
	TTLms      int64       `json:"ttl_ms"`
	ExpiresAt  int64       `json:"expires_at"` // explicit until deadline: UpdatedAt+TTLms
}

// healthView is the single immutable view published via atomic.Pointer.
// One immutable atomic.Pointer view; whole map replaced on sync. Stores runID and per-entry expiry.
type healthView struct {
	entries map[HealthKey]healthEntry
	gen     int64
	runID   string
}

func parseGenStrict(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("health: empty generation")
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("health: malformed generation %q: %w", s, err)
	}
	if v < 0 {
		return 0, fmt.Errorf("health: negative generation %d", v)
	}
	return v, nil
}

const (
	healthGenKey          = "c3api:health:generation"
	healthRecordsHash     = "c3api:health:records"
	healthActiveZSet      = "c3api:health:active"
	healthTombstoneHash   = "c3api:health:tombstone"
	healthTombstonePrefix = "c3api:health:tombstone:"
	healthRecordPrefix    = "c3api:health:record:"
	healthSyncInterval    = 500 * time.Millisecond
	healthProbeInterval   = 1 * time.Second
	healthCleanupBound    = 100
)

var (
	ErrProbeStaleRevision = errors.New("health probe: stale revision")

	// Lua throttle/READY atomic generation/revision/record HASH/active ZSET/tombstone/TTL
	throttleLua = `
local genKey = KEYS[1]
local recPrefix = ARGV[5]
local activeKey = KEYS[2]
local tombHash = KEYS[3]
local tombPrefix = ARGV[6]
local field = ARGV[1]
local state = ARGV[2]
local rev = ARGV[3]
local ttl = ARGV[4]
local gen = redis.call('INCR', genKey)
local recKey = recPrefix .. field
redis.call('HSET', recKey, 'state', state, 'gen', gen, 'rev', rev, 'ttl', ttl)
redis.call('PEXPIRE', recKey, ttl)
redis.call('ZADD', activeKey, gen, field)
redis.call('HDEL', tombHash, field)
redis.call('DEL', tombPrefix .. field)
return gen
`
	// clearAccountLua 原子清掉**一个账号全部 quality 字段**的健康记录，与
	// readyLua 同一 4 段纪律（逐字段：DEL 记录 + ZREM 活动 ZSET + HSET 墓碑
	// 哈希 + SET 墓碑前缀）。逐字段墓碑是**强制**的而非可选：Sync 按活动 ZSET
	// 重建（health.go Sync 的 ZRANGE），未过期的 OPEN 记录**只有被墓碑标注**才
	// 会被丢弃——只清内存视图而不写 Redis 侧墓碑，Sync 会把记录原样装回来。
	//
	// 字段格式为 accountID:quality:revision，故按 accId .. ":" 前缀匹配，
	// 结构上不可能清到别的账号（不做子串/数字前缀匹配）。
	clearAccountLua = `
local genKey = KEYS[1]
local activeKey = KEYS[2]
local tombHash = KEYS[3]
local accId = ARGV[1]
local recPrefix = ARGV[2]
local tombPrefix = ARGV[3]
local ttl = ARGV[4]
local gen = redis.call('INCR', genKey)
local prefix = accId .. ':'
local plen = string.len(prefix)
local cleared = 0
local members = redis.call('ZRANGE', activeKey, 0, -1)
for i = 1, #members do
  local field = members[i]
  if string.sub(field, 1, plen) == prefix then
    redis.call('DEL', recPrefix .. field)
    redis.call('ZREM', activeKey, field)
    redis.call('HSET', tombHash, field, gen)
    redis.call('SET', tombPrefix .. field, gen, 'PX', ttl)
    cleared = cleared + 1
  end
end
return cleared
`
	// readyLua atomically validates HASH record exists and matches expected account/revision/current generation before READY.
	readyLua = `
local genKey = KEYS[1]
local activeKey = KEYS[2]
local tombHash = KEYS[3]
local field = ARGV[1]
local expectedGen = ARGV[2]
local expectedRev = ARGV[3]
local ttl = ARGV[4]
local recPrefix = ARGV[5]
local tombPrefix = ARGV[6]
local curGen = redis.call('GET', genKey)
if not curGen then curGen = 0 else curGen = tonumber(curGen) end
if tonumber(expectedGen) ~= curGen then
  return 0
end
local recKey = recPrefix .. field
if redis.call('EXISTS', recKey) == 0 then
  return 0
end
local recGen = redis.call('HGET', recKey, 'gen')
if not recGen or tonumber(recGen) ~= tonumber(expectedGen) then
  return 0
end
local recRev = redis.call('HGET', recKey, 'rev')
if not recRev or tonumber(recRev) ~= tonumber(expectedRev) then
  return 0
end
local gen = redis.call('INCR', genKey)
redis.call('DEL', recKey)
redis.call('ZREM', activeKey, field)
redis.call('HSET', tombHash, field, gen)
redis.call('SET', tombPrefix .. field, gen, 'PX', ttl)
return gen
`
	// cleanupLua is one Lua script for expiry cleanup: re-reads global generation and per-record generation/revision before each ZREM/HDEL.
	// Mode "active": validates global gen unchanged and per-record HASH still absent before ZREM; Mode "tomb": validates global gen and per-key tomb still absent before HDEL.
	// Errors propagate via EVAL error; stale generation or recreated record returns 0 (no delete) without error.
	cleanupLua = `
local genKey = KEYS[1]
local activeKey = KEYS[2]
local tombHash = KEYS[3]
local mode = ARGV[1]
local field = ARGV[2]
local expectedGen = ARGV[3]
local recPrefix = ARGV[4]
local tombPrefix = ARGV[5]
local curGen = redis.call('GET', genKey)
if not curGen then curGen = 0 else curGen = tonumber(curGen) end
if tonumber(curGen) ~= tonumber(expectedGen) then return 0 end
if mode == "active" then
  local recKey = recPrefix .. field
  if redis.call('EXISTS', recKey) == 1 then return 0 end
  return redis.call('ZREM', activeKey, field)
else
  local tombKey = tombPrefix .. field
  if redis.call('EXISTS', tombKey) == 1 then return 0 end
  return redis.call('HDEL', tombHash, field)
end
`
	fenceLua = `
local genKey = KEYS[1]
local expected = ARGV[1]
local cur = redis.call('GET', genKey)
if not cur then cur = 0 else cur = tonumber(cur) end
if tonumber(cur) ~= tonumber(expected) then return 0 end
return 1
`
)

// ProbeFunc is injected probe function for a single health key.
// Returns nil on success, error on failure (failure reopen).
type ProbeFunc func(context.Context, HealthKey) error

// RuntimeHealth is the atomic RuntimeHealth Redis/view/probe core; no Rule/SDK integration.
// One immutable atomic.Pointer view; key account+quality|*+revision; states OPEN>RETRY_AFTER>PROBING>READY.
// Lua throttle/READY atomic generation/revision/record HASH/active ZSET/tombstone/TTL.
// Sync INFO run_id + gen-before/records/gen-after; run_id/expiry stored in immutable view, explicit OPEN->until->PROBING->probe retention.
// Uses worker.GoLoop for loops; selfID/rendezvous injected at construction, probe
// injected once at Start (loop spawn 前单次交接——循环启动后只读，无回填读写竞态）;
// one permit, two current-gen successes READY, failure reopen.
// 探针不变量：probeTick 只探测 StateProbing 条目；OPEN/RETRY_AFTER 在其窗口内永不
// 被探测（窗口跑满 TTL，到期处理权在 Sync retention 转换逻辑，本循环不碰）；
// PROBING 条目只来自 recover 链路 SetProbing 与 Sync 保留转换。
type RuntimeHealth struct {
	client     *redis.Client
	selfID     string
	members    func() []string
	rendezvous func(key string, members []string) string
	probeFn    ProbeFunc
	log        *logx.Logger

	view atomic.Pointer[healthView]

	mu           sync.Mutex
	successCount map[string]int
	successGen   map[string]int64

	permit chan struct{}

	runID   string
	curGen  atomic.Int64
	lastGen atomic.Int64

	now       func() time.Time
	syncHook  func(stage string)
	runIDHook func(context.Context) (string, error)

	startOnce sync.Once
	cancel    context.CancelFunc
	// syncDone/probeDone GoLoop 完成信号（Start 存入，Close join——同
	// conc-sync/quality-sync 停机纪律：Close 返回后不再有循环在跑）。
	// lastSyncOkMs/syncErrors/lastTickOk 编译道外的健康道新鲜度观测
	//（Stats 冷路径原子读）。
	syncDone     <-chan struct{}
	probeDone    <-chan struct{}
	lastSyncOkMs atomic.Int64
	syncErrors   atomic.Int64
	lastTickOk   atomic.Bool
}

// NewRuntimeHealth constructs the core. members may be nil (single instance).
// rendezvous may be nil (defaults to simple hash). probe 不在此处注入——它是
// Start 期依赖（组合根在 sched/codex 就绪后构造真 probe，Start 期一次性交接）；
// Start 前 nil probe fail-closed（doProbe 恒失败，记录停在 OPEN/PROBING，绝不 READY）。
func NewRuntimeHealth(client *redis.Client, selfID string, members func() []string, log *logx.Logger) *RuntimeHealth {
	h := &RuntimeHealth{
		client:       client,
		selfID:       selfID,
		members:      members,
		log:          log,
		permit:       make(chan struct{}, 1),
		successCount: make(map[string]int),
		successGen:   make(map[string]int64),
		now:          time.Now,
	}
	if h.members == nil {
		h.members = func() []string { return []string{selfID} }
	}
	if h.rendezvous == nil {
		h.rendezvous = rendezvousOwner
	}
	empty := &healthView{entries: make(map[HealthKey]healthEntry)}
	h.view.Store(empty)
	h.permit <- struct{}{}
	return h
}

func (h *RuntimeHealth) currentMs() int64 {
	if h.now != nil {
		return h.now().UnixMilli()
	}
	return time.Now().UnixMilli()
}

// rendezvousOwner is default rendezvous (highest FNV weight) for probe owner election.
func rendezvousOwner(key string, members []string) string {
	if len(members) == 0 {
		return ""
	}
	var best string
	var bestScore uint64
	for i, m := range members {
		var hash uint64 = 1469598103934665603
		for _, b := range []byte(m) {
			hash ^= uint64(b)
			hash *= 1099511628211
		}
		hash ^= 0
		hash *= 1099511628211
		for _, b := range []byte(key) {
			hash ^= uint64(b)
			hash *= 1099511628211
		}
		if i == 0 || hash > bestScore {
			bestScore = hash
			best = m
		}
	}
	return best
}

// Name satisfies worker.Worker.
func (h *RuntimeHealth) Name() string { return "runtime-health" }

// Start launches sync and probe loops via worker.GoLoop, no raw go/Sleep/Background.
// probe 是 Start 期依赖：循环 spawn 之前单次交接（h.probeFn = probe），循环
// 启动后只读——Start 后不存在回填写入，消除回填读写竞态。nil probe 保持
// fail-closed（doProbe 恒失败）。
func (h *RuntimeHealth) Start(ctx context.Context, probe ProbeFunc) error {
	h.startOnce.Do(func() {
		h.probeFn = probe
		c, cancel := context.WithCancel(ctx)
		h.cancel = cancel
		h.syncDone = worker.GoLoop(c, "runtime-health-sync", h.log, h.syncLoop)
		h.probeDone = worker.GoLoop(c, "runtime-health-probe", h.log, h.probeLoop)
	})
	return nil
}

// Close stops loops and joins their completion signals (bounded by ctx):
// orderly shutdown — a nil return guarantees no sync/probe tick can still run.
// context.CancelFunc is idempotent, so every caller (concurrent or retry)
// independently joins the same done signals: a deadline-limited Close reports
// the incomplete join as an error without permanently suppressing a later
// successful join (no stopOnce — the once would be consumed by the timeout).
func (h *RuntimeHealth) Close(ctx context.Context) error {
	if h.cancel == nil {
		return nil // 未 Start：Close 安全 no-op（worker 契约）
	}
	h.cancel()
	for _, d := range []<-chan struct{}{h.syncDone, h.probeDone} {
		select {
		case <-d:
		default:
			select {
			case <-d:
			case <-ctx.Done():
				if h.log != nil {
					h.log.Warn("runtime-health close timeout, loop still running", logx.Error(ctx.Err()))
				}
				return fmt.Errorf("health: close join incomplete: %w", ctx.Err())
			}
		}
	}
	return nil
}

func (h *RuntimeHealth) syncLoop(ctx context.Context) {
	t := time.NewTicker(healthSyncInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := h.Sync(ctx); err != nil {
				h.syncErrors.Add(1)
				h.lastTickOk.Store(false)
				continue
			}
			h.lastSyncOkMs.Store(h.currentMs())
			h.lastTickOk.Store(true)
		}
	}
}

func (h *RuntimeHealth) probeLoop(ctx context.Context) {
	t := time.NewTicker(healthProbeInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			h.probeTick(ctx)
		}
	}
}

// Throttle performs Lua throttle atomic generation/revision/record HASH/active ZSET/tombstone/TTL.
func (h *RuntimeHealth) Throttle(ctx context.Context, key HealthKey, state HealthState, ttl time.Duration) (int64, error) {
	if h.client == nil {
		return 0, fmt.Errorf("health: no redis")
	}
	field := key.String()
	ttlMs := ttl.Milliseconds()
	if ttlMs <= 0 {
		ttlMs = 30000
	}
	stateStr := state.String()
	res, err := h.client.Eval(ctx, throttleLua, []string{healthGenKey, healthActiveZSet, healthTombstoneHash}, field, stateStr, fmt.Sprintf("%d", key.IdentityRevision), fmt.Sprintf("%d", ttlMs), healthRecordPrefix, healthTombstonePrefix).Result()
	if err != nil {
		return 0, err
	}
	gen, _ := res.(int64)
	if gen == 0 {
		if v, ok := res.(int); ok {
			gen = int64(v)
		}
	}
	h.lastGen.Store(gen)
	return gen, nil
}

// MarkReady attempts Lua READY atomic generation/revision/record HASH/active ZSET/tombstone/TTL
func (h *RuntimeHealth) MarkReady(ctx context.Context, key HealthKey, expectedGen int64, ttl time.Duration) (int64, error) {
	if h.client == nil {
		return 0, fmt.Errorf("health: no redis")
	}
	field := key.String()
	ttlMs := ttl.Milliseconds()
	if ttlMs <= 0 {
		ttlMs = 30000
	}
	res, err := h.client.Eval(ctx, readyLua, []string{healthGenKey, healthActiveZSet, healthTombstoneHash}, field, fmt.Sprintf("%d", expectedGen), fmt.Sprintf("%d", key.IdentityRevision), fmt.Sprintf("%d", ttlMs), healthRecordPrefix, healthTombstonePrefix).Result()
	if err != nil {
		return 0, err
	}
	var gen int64
	switch v := res.(type) {
	case int64:
		gen = v
	case int:
		gen = int64(v)
	case int32:
		gen = int64(v)
	}
	if gen == 0 {
		return 0, fmt.Errorf("health: stale generation")
	}
	h.lastGen.Store(gen)
	return gen, nil
}

// ClearAccount 原子清掉该账号**全部**健康记录（所有 quality / 所有代际），返回
// 清掉的字段数。用于**身份代际 K 推进**之后：旧 K 下的记录对新 K 不可达，但
// Sync 会按活动 ZSET 把它们重新装回视图（未过期 OPEN 仅在被墓碑标注时才丢弃），
// 故必须连 Redis 侧一并清（见 clearAccountLua 的 4 段纪律）。
//
// client 未装配（测试/降级）时 no-op 返回 0：健康面本就不可用，无记录可清。
func (h *RuntimeHealth) ClearAccount(ctx context.Context, accountID int64) (int64, error) {
	if h.client == nil {
		return 0, nil
	}
	res, err := h.client.Eval(ctx, clearAccountLua,
		[]string{healthGenKey, healthActiveZSet, healthTombstoneHash},
		strconv.FormatInt(accountID, 10), healthRecordPrefix, healthTombstonePrefix,
		strconv.FormatInt((30*time.Second).Milliseconds(), 10)).Int64()
	if err != nil {
		return 0, fmt.Errorf("health: clear account %d: %w", accountID, err)
	}
	return res, nil
}

// SetProbing writes the wildcard PROBING record for (account, newRevision)
// after a successful recover CAS (satisfies the recover-side health prober
// contract). Rides the standard Throttle Lua path so generation bump, record
// HASH, active ZSET membership and tombstone clearing stay atomic. Non-positive
// revision is rejected fail-closed; old-revision records stay isolated by key.
func (h *RuntimeHealth) SetProbing(ctx context.Context, accountID int64, identityRevision int64) error {
	if identityRevision <= 0 {
		return fmt.Errorf("health: invalid probing identity revision %d for account %d", identityRevision, accountID)
	}
	_, err := h.Throttle(ctx, HealthKey{AccountID: accountID, Quality: "*", IdentityRevision: identityRevision}, StateProbing, 30*time.Second)
	return err
}

// EffectiveState returns the severity-most health for account+quality+revision.
func (h *RuntimeHealth) EffectiveState(accountID int64, quality string, identityRevision int64) HealthState {
	view := h.view.Load()
	if view == nil {
		return StateReady
	}
	best := StateReady
	bestSev := best.severity()
	if s, ok := view.entries[HealthKey{AccountID: accountID, Quality: quality, IdentityRevision: identityRevision}]; ok {
		if s.State.severity() > bestSev {
			best = s.State
			bestSev = s.State.severity()
		}
	}
	if w, ok := view.entries[HealthKey{AccountID: accountID, Quality: "*", IdentityRevision: identityRevision}]; ok {
		if w.State.severity() > bestSev {
			best = w.State
		}
	}
	return best
}

// View returns a copy of current immutable view for testing.
func (h *RuntimeHealth) View() map[HealthKey]healthEntry {
	v := h.view.Load()
	if v == nil {
		return nil
	}
	out := make(map[HealthKey]healthEntry, len(v.entries))
	for k, e := range v.entries {
		out[k] = e
	}
	return out
}

// ViewRunID returns current view runID for testing.
func (h *RuntimeHealth) ViewRunID() string {
	v := h.view.Load()
	if v == nil {
		return ""
	}
	return v.runID
}

// Sync performs INFO run_id + gen-before/records/gen-after atomic read.
// Fail-closed on INFO/global-generation/record errors and never treats error as empty.
// Expiry cleanup uses one Lua script that re-reads global generation and per-record generation/revision before each ZREM/HDEL; errors propagate and freeze view.
// Generation strings parsed strictly; malformed/overflow/negative returns error and freezes, never become zero.
// Candidate generation stored only after all cleanup and view publication succeed; cleanup failure leaves old curGen/view unchanged.
// Read actual Redis run_id and compare with previous immutable view runID; retention/probe transition based on run-change event, not test-mutated field.
// Same-run empty may clear READY/missing; run-change repeated empty retains OPEN/RETRY until original ExpiresAt then PROBING until real probe result.
func (h *RuntimeHealth) Sync(ctx context.Context) error {
	if h.client == nil {
		return nil
	}
	genBeforeStr, err := h.client.Get(ctx, healthGenKey).Result()
	var genBefore int64
	if err != nil {
		if err == redis.Nil {
			genBefore = 0
		} else {
			return err
		}
	} else {
		genBefore, err = parseGenStrict(genBeforeStr)
		if err != nil {
			return err
		}
	}
	var runID string
	if h.runIDHook != nil {
		runID, err = h.runIDHook(ctx)
		if err != nil {
			return err
		}
	} else {
		infoStr, err := h.client.Info(ctx, "replication").Result()
		if err != nil {
			fallback, ferr := h.client.Info(ctx).Result()
			if ferr != nil {
				return err
			}
			infoStr = fallback
		}
		for _, line := range strings.Split(infoStr, "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "run_id:") {
				runID = strings.TrimSpace(strings.TrimPrefix(line, "run_id:"))
				break
			}
			if strings.HasPrefix(line, "master_replid:") && runID == "" {
				runID = strings.TrimSpace(strings.TrimPrefix(line, "master_replid:"))
			}
		}
	}
	members, err := h.client.ZRange(ctx, healthActiveZSet, 0, -1).Result()
	if err != nil {
		return err
	}
	records := make(map[HealthKey]healthEntry)
	var staleActive []string
	nowMs := h.currentMs()
	prevForExpiry := h.view.Load()
	for _, field := range members {
		recKey := healthRecordPrefix + field
		m, err := h.client.HGetAll(ctx, recKey).Result()
		if err != nil {
			return err
		}
		if len(m) == 0 {
			staleActive = append(staleActive, field)
			continue
		}
		k, ok := parseHealthKey(field)
		if !ok {
			staleActive = append(staleActive, field)
			continue
		}
		var st HealthState
		switch m["state"] {
		case "OPEN":
			st = StateOPEN
		case "RETRY_AFTER":
			st = StateRetryAfter
		case "PROBING":
			st = StateProbing
		case "READY":
			st = StateReady
		default:
			st = StateOPEN
		}
		genStr := m["gen"]
		if genStr == "" {
			return fmt.Errorf("health: missing generation for %s", field)
		}
		gen, err := parseGenStrict(genStr)
		if err != nil {
			return err
		}
		revStr := strings.TrimSpace(m["rev"])
		if revStr == "" {
			return fmt.Errorf("health: missing rev for %s", field)
		}
		rev, err := parseGenStrict(revStr)
		if err != nil {
			return fmt.Errorf("health: malformed rev for %s: %w", field, err)
		}
		ttlStr := strings.TrimSpace(m["ttl"])
		if ttlStr == "" {
			return fmt.Errorf("health: missing ttl for %s", field)
		}
		ttlMs, err := parseGenStrict(ttlStr)
		if err != nil {
			return fmt.Errorf("health: malformed ttl for %s: %w", field, err)
		}
		if ttlMs <= 0 {
			return fmt.Errorf("health: invalid ttl %d for %s", ttlMs, field)
		}
		expiresAt := nowMs + ttlMs
		updatedAt := nowMs
		if prevForExpiry != nil {
			if prevEntry, ok := prevForExpiry.entries[k]; ok && prevEntry.Generation == gen {
				expiresAt = prevEntry.ExpiresAt
				updatedAt = prevEntry.UpdatedAt
				ttlMs = prevEntry.TTLms
			}
		}
		records[k] = healthEntry{Key: k, State: st, Generation: gen, Revision: rev, UpdatedAt: updatedAt, TTLms: ttlMs, ExpiresAt: expiresAt}
	}
	tombMembers, err := h.client.HGetAll(ctx, healthTombstoneHash).Result()
	if err != nil && err != redis.Nil {
		return err
	}
	var staleTomb []string
	for field := range tombMembers {
		tombKey := healthTombstonePrefix + field
		exists, err := h.client.Exists(ctx, tombKey).Result()
		if err != nil {
			return err
		}
		if exists == 0 {
			staleTomb = append(staleTomb, field)
		}
	}
	if h.syncHook != nil {
		h.syncHook("beforeGenAfter")
	}
	genAfterStr, err := h.client.Get(ctx, healthGenKey).Result()
	var genAfter int64
	if err != nil {
		if err == redis.Nil {
			genAfter = 0
		} else {
			return err
		}
	} else {
		genAfter, err = parseGenStrict(genAfterStr)
		if err != nil {
			return err
		}
	}
	if genBefore != genAfter {
		return fmt.Errorf("health: stale generation %d != %d", genBefore, genAfter)
	}
	candidateGen := genAfter

	if h.syncHook != nil {
		h.syncHook("beforeCleanup")
	}
	if len(staleActive) > 0 {
		if len(staleActive) > healthCleanupBound {
			staleActive = staleActive[:healthCleanupBound]
		}
		for _, field := range staleActive {
			_, err := h.client.Eval(ctx, cleanupLua, []string{healthGenKey, healthActiveZSet, healthTombstoneHash}, "active", field, fmt.Sprintf("%d", candidateGen), healthRecordPrefix, healthTombstonePrefix).Result()
			if err != nil && err != redis.Nil {
				return err
			}
		}
	}
	if len(staleTomb) > 0 {
		if len(staleTomb) > healthCleanupBound {
			staleTomb = staleTomb[:healthCleanupBound]
		}
		for _, field := range staleTomb {
			_, err := h.client.Eval(ctx, cleanupLua, []string{healthGenKey, healthActiveZSet, healthTombstoneHash}, "tomb", field, fmt.Sprintf("%d", candidateGen), healthRecordPrefix, healthTombstonePrefix).Result()
			if err != nil && err != redis.Nil {
				return err
			}
		}
	}

	if h.syncHook != nil {
		h.syncHook("beforePublish")
	}
	fenceRes, err := h.client.Eval(ctx, fenceLua, []string{healthGenKey}, fmt.Sprintf("%d", candidateGen)).Result()
	if err != nil {
		if err == redis.Nil {
			fenceRes = int64(0)
		} else {
			return err
		}
	}
	var fenceOk int64
	switch v := fenceRes.(type) {
	case int64:
		fenceOk = v
	case int:
		fenceOk = int64(v)
	case int32:
		fenceOk = int64(v)
	default:
		fenceOk = 0
	}
	if fenceOk != 1 {
		return fmt.Errorf("health: stale generation fence %d", candidateGen)
	}

	prevView := h.view.Load()
	prevRunID := ""
	if prevView != nil {
		prevRunID = prevView.runID
	}
	isRunChange := prevRunID != "" && runID != "" && runID != prevRunID
	if len(records) == 0 {
		if prevView != nil && len(prevView.entries) > 0 {
			if !isRunChange {
				hasProbing := false
				for _, e := range prevView.entries {
					if e.State == StateProbing {
						hasProbing = true
						break
					}
				}
				if !hasProbing {
					newView := &healthView{entries: make(map[HealthKey]healthEntry), gen: candidateGen, runID: runID}
					h.view.Store(newView)
					h.curGen.Store(candidateGen)
					if runID != "" {
						h.runID = runID
					}
					return nil
				}
			}
			staleTombSet := make(map[string]struct{}, len(staleTomb))
			for _, f := range staleTomb {
				staleTombSet[f] = struct{}{}
			}
			retained := make(map[HealthKey]healthEntry)
			for k, e := range prevView.entries {
				fieldStr := k.String()
				if _, ok := tombMembers[fieldStr]; ok {
					if _, isStale := staleTombSet[fieldStr]; isStale {
						continue
					}
					continue
				}
				if e.State == StateOPEN || e.State == StateRetryAfter {
					until := e.ExpiresAt
					if until == 0 {
						until = e.UpdatedAt + e.TTLms
					}
					if until == 0 {
						until = nowMs + 30000
					}
					if nowMs < until {
						retained[k] = e
					} else {
						e.State = StateProbing
						retained[k] = e
					}
				} else if e.State == StateProbing {
					retained[k] = e
				}
			}
			if len(retained) > 0 {
				viewRunID := prevRunID
				if viewRunID == "" {
					viewRunID = runID
				}
				newView := &healthView{entries: retained, gen: candidateGen, runID: viewRunID}
				h.view.Store(newView)
				h.curGen.Store(candidateGen)
				if runID != "" {
					h.runID = runID
				}
				return nil
			}
		}
		newView := &healthView{entries: make(map[HealthKey]healthEntry), gen: candidateGen, runID: runID}
		h.view.Store(newView)
		h.curGen.Store(candidateGen)
		if runID != "" {
			h.runID = runID
		}
		return nil
	}
	newView := &healthView{entries: records, gen: candidateGen, runID: runID}
	h.view.Store(newView)
	h.curGen.Store(candidateGen)
	if runID != "" {
		h.runID = runID
	}
	return nil
}

// probeTick performs one probe cycle: owner check via rendezvous, one permit, two current-gen successes READY, failure reopen.
// 只探测 StateProbing（OPEN/RETRY_AFTER 窗口内永不探测——跑满 TTL，到期由 Sync 转换；本函数不做到期处理）。
func (h *RuntimeHealth) probeTick(ctx context.Context) {
	view := h.view.Load()
	if view == nil {
		return
	}
	members := h.members()
	for key, entry := range view.entries {
		// 窗口 honored：OPEN/RETRY_AFTER 跑满 TTL，不经探针提前清除。
		if entry.State != StateProbing {
			continue
		}
		field := key.String()
		owner := h.rendezvous(field, members)
		if owner != h.selfID {
			continue
		}
		select {
		case <-h.permit:
		default:
			continue
		}
		curGen := h.curGen.Load()
		if h.client != nil {
			if gStr, err := h.client.Get(ctx, healthGenKey).Result(); err == nil {
				if v, err := parseGenStrict(gStr); err == nil {
					curGen = v
				}
			}
		}
		err := h.doProbe(ctx, key)
		if err == nil {
			h.mu.Lock()
			gen0, ok := h.successGen[field]
			if !ok || gen0 != curGen {
				h.successGen[field] = curGen
				h.successCount[field] = 1
			} else {
				h.successCount[field]++
			}
			cnt := h.successCount[field]
			h.mu.Unlock()
			if cnt >= 2 {
				_, _ = h.MarkReady(ctx, key, curGen, 30*time.Second)
				h.mu.Lock()
				delete(h.successCount, field)
				delete(h.successGen, field)
				h.mu.Unlock()
			}
		} else {
			h.mu.Lock()
			delete(h.successCount, field)
			delete(h.successGen, field)
			h.mu.Unlock()
			if !errors.Is(err, ErrProbeStaleRevision) {
				_, _ = h.Throttle(ctx, key, StateOPEN, 30*time.Second)
			}
		}
		h.permit <- struct{}{}
	}
}

func (h *RuntimeHealth) doProbe(ctx context.Context, key HealthKey) error {
	if h.probeFn == nil {
		return fmt.Errorf("no probe")
	}
	return h.probeFn(ctx, key)
}
