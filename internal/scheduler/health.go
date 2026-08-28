// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
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
type HealthKey struct {
	AccountID int64
	Quality   string // hex or "*"
	Revision  int64
}

func (k HealthKey) String() string {
	return fmt.Sprintf("%d:%s:%d", k.AccountID, k.Quality, k.Revision)
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
	return HealthKey{AccountID: acc, Quality: parts[1], Revision: rev}, true
}

// healthEntry is the immutable per-key record stored in Redis HASH and view.
type healthEntry struct {
	Key       HealthKey   `json:"key"`
	State     HealthState `json:"state"`
	Generation int64      `json:"gen"`
	Revision  int64       `json:"rev"`
	UpdatedAt int64       `json:"updated_at"` // unix milli
	TTLms     int64       `json:"ttl_ms"`
	ExpiresAt int64       `json:"expires_at"` // explicit until deadline: UpdatedAt+TTLms
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
)

// ProbeFunc is injected probe function for a single health key.
// Returns nil on success, error on failure (failure reopen).
type ProbeFunc func(context.Context, HealthKey) error

// RuntimeHealth is the atomic RuntimeHealth Redis/view/probe core; no Rule/SDK integration.
// One immutable atomic.Pointer view; key account+quality|*+revision; states OPEN>RETRY_AFTER>PROBING>READY.
// Lua throttle/READY atomic generation/revision/record HASH/active ZSET/tombstone/TTL.
// Sync INFO run_id + gen-before/records/gen-after; run_id/expiry stored in immutable view, explicit OPEN->until->PROBING->probe retention.
// Uses worker.GoLoop for loops; probe injected selfID/rendezvous/ProbeFunc, one permit, two current-gen successes READY, failure reopen.
type RuntimeHealth struct {
	client *redis.Client
	selfID string
	members func() []string
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

	now      func() time.Time
	syncHook func(stage string)
	runIDHook func(context.Context) (string, error)

	startOnce sync.Once
	stopOnce  sync.Once
	cancel    context.CancelFunc
	done      chan struct{}
}

// NewRuntimeHealth constructs the core. members may be nil (single instance).
// rendezvous may be nil (defaults to simple hash). probeFn may be nil (probe no-op).
func NewRuntimeHealth(client *redis.Client, selfID string, members func() []string, probeFn ProbeFunc, log *logx.Logger) *RuntimeHealth {
	h := &RuntimeHealth{
		client:       client,
		selfID:       selfID,
		members:      members,
		probeFn:      probeFn,
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
func (h *RuntimeHealth) Start(ctx context.Context) error {
	h.startOnce.Do(func() {
		c, cancel := context.WithCancel(ctx)
		h.cancel = cancel
		h.done = make(chan struct{})
		close(h.done)
		_ = worker.GoLoop(c, "runtime-health-sync", h.log, h.syncLoop)
		_ = worker.GoLoop(c, "runtime-health-probe", h.log, h.probeLoop)
	})
	return nil
}

// Close stops loops.
func (h *RuntimeHealth) Close(_ context.Context) error {
	h.stopOnce.Do(func() {
		if h.cancel != nil {
			h.cancel()
		}
	})
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
			_ = h.Sync(ctx)
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
	res, err := h.client.Eval(ctx, throttleLua, []string{healthGenKey, healthActiveZSet, healthTombstoneHash}, field, stateStr, fmt.Sprintf("%d", key.Revision), fmt.Sprintf("%d", ttlMs), healthRecordPrefix, healthTombstonePrefix).Result()
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
	res, err := h.client.Eval(ctx, readyLua, []string{healthGenKey, healthActiveZSet, healthTombstoneHash}, field, fmt.Sprintf("%d", expectedGen), fmt.Sprintf("%d", key.Revision), fmt.Sprintf("%d", ttlMs), healthRecordPrefix, healthTombstonePrefix).Result()
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

// EffectiveState returns the severity-most health for account+quality+revision.
func (h *RuntimeHealth) EffectiveState(accountID int64, quality string, revision int64) HealthState {
	view := h.view.Load()
	if view == nil {
		return StateReady
	}
	specific := view.entries[HealthKey{AccountID: accountID, Quality: quality, Revision: revision}]
	wildcard := view.entries[HealthKey{AccountID: accountID, Quality: "*", Revision: revision}]
	best := StateReady
	bestSev := best.severity()
	if s, ok := view.entries[HealthKey{AccountID: accountID, Quality: quality, Revision: revision}]; ok {
		if s.State.severity() > bestSev {
			best = s.State
			bestSev = s.State.severity()
		}
		_ = specific
	}
	if w, ok := view.entries[HealthKey{AccountID: accountID, Quality: "*", Revision: revision}]; ok {
		if w.State.severity() > bestSev {
			best = w.State
		}
		_ = wildcard
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
		var rev, ttlMsVal int64
		_, _ = fmt.Sscanf(m["rev"], "%d", &rev)
		_, _ = fmt.Sscanf(m["ttl"], "%d", &ttlMsVal)
		ttlMs := ttlMsVal
		if ttlMs <= 0 {
			ttlMs = 30000
		}
		expiresAt := nowMs + ttlMs
		records[k] = healthEntry{Key: k, State: st, Generation: gen, Revision: rev, UpdatedAt: nowMs, TTLms: ttlMs, ExpiresAt: expiresAt}
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
func (h *RuntimeHealth) probeTick(ctx context.Context) {
	view := h.view.Load()
	if view == nil {
		return
	}
	members := h.members()
	for key, entry := range view.entries {
		if entry.State != StateOPEN && entry.State != StateRetryAfter && entry.State != StateProbing {
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
			_, _ = h.Throttle(ctx, key, StateOPEN, 30*time.Second)
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
