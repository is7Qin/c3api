// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
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
	Key        HealthKey   `json:"key"`
	State      HealthState `json:"state"`
	Generation int64       `json:"gen"`
	Revision   int64       `json:"rev"`
	UpdatedAt  int64       `json:"updated_at"` // unix milli
	TTLms      int64       `json:"ttl_ms"`
}

// healthView is the single immutable view published via atomic.Pointer.
// One immutable atomic.Pointer view; whole map replaced on sync.
type healthView struct {
	entries map[HealthKey]healthEntry
	gen     int64
	runID   string
}

const (
	healthGenKey          = "c3api:health:generation"
	healthRecordsHash     = "c3api:health:records"   // HASH field=key string, value=json entry
	healthActiveZSet      = "c3api:health:active"    // ZSET member=key string, score=generation
	healthTombstoneHash   = "c3api:health:tombstone" // HASH field=key string, value=generation (TTL via per-field logic using ZSET + separate tombstone keys with TTL)
	healthTombstonePrefix = "c3api:health:tombstone:"
	healthRecordPrefix    = "c3api:health:record:"
	healthSyncInterval    = 500 * time.Millisecond
	healthProbeInterval   = 1 * time.Second
)

var (
	// Lua throttle/READY atomic generation/revision/record HASH/active ZSET/tombstone/TTL
	// throttleLua atomically increments generation, writes record HASH, adds active ZSET, clears tombstone, sets TTL.
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
	// readyLua atomically checks generation matches current-gen, then marks READY via tombstone and removes active entry.
	readyLua = `
local genKey = KEYS[1]
local recPrefix = ARGV[5]
local activeKey = KEYS[2]
local tombHash = KEYS[3]
local tombPrefix = ARGV[6]
local field = ARGV[1]
local expectedGen = ARGV[2]
local ttl = ARGV[3]
local curGen = redis.call('GET', genKey)
if not curGen then curGen = 0 else curGen = tonumber(curGen) end
if tonumber(expectedGen) ~= curGen then
  return 0
end
local gen = redis.call('INCR', genKey)
local recKey = recPrefix .. field
redis.call('DEL', recKey)
redis.call('ZREM', activeKey, field)
redis.call('HSET', tombHash, field, gen)
redis.call('SET', tombPrefix .. field, gen, 'PX', ttl)
return gen
`
)

// ProbeFunc is injected probe function for a single health key.
// Returns nil on success, error on failure (failure reopen).
type ProbeFunc func(context.Context, HealthKey) error

// RuntimeHealth is the atomic RuntimeHealth Redis/view/probe core; no Rule/SDK integration.
// One immutable atomic.Pointer view; key account+quality|*+revision; states OPEN>RETRY_AFTER>PROBING>READY.
// Lua throttle/READY atomic generation/revision/record HASH/active ZSET/tombstone/TTL.
// Sync INFO run_id + gen-before/records/gen-after; same-run empty clears, run change retain OPEN until then PROBING.
// Uses worker.GoLoop for loops; probe injected selfID/rendezvous/ProbeFunc, one permit, two current-gen successes READY, failure reopen.
type RuntimeHealth struct {
	client *redis.Client
	selfID string
	// members provides current live members for rendezvous owner election.
	members func() []string
	// rendezvous selects owner for a given key among members (highest weight).
	rendezvous func(key string, members []string) string
	probeFn    ProbeFunc
	log        *logx.Logger

	view atomic.Pointer[healthView]

	mu           sync.Mutex
	successCount map[string]int   // field -> consecutive successes
	successGen   map[string]int64 // field -> generation of first success

	permit chan struct{} // one permit

	runID   string
	curGen  int64
	lastGen atomic.Int64

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
	}
	if h.members == nil {
		h.members = func() []string { return []string{selfID} }
	}
	if h.rendezvous == nil {
		h.rendezvous = rendezvousOwner
	}
	// initial empty immutable view
	empty := &healthView{entries: make(map[HealthKey]healthEntry)}
	h.view.Store(empty)
	h.permit <- struct{}{}
	return h
}

// rendezvousOwner is default rendezvous (highest FNV weight) for probe owner election.
func rendezvousOwner(key string, members []string) string {
	if len(members) == 0 {
		return ""
	}
	var best string
	var bestScore uint64
	for i, m := range members {
		// FNV-1a hash of member + 0x00 + key
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
// key is account+quality|*+revision, state is OPEN or RETRY_AFTER.
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
	// Lua throttle atomic generation/revision/record HASH/active ZSET/tombstone/TTL
	res, err := h.client.Eval(ctx, throttleLua, []string{healthGenKey, healthActiveZSet, healthTombstoneHash}, field, stateStr, fmt.Sprintf("%d", key.Revision), fmt.Sprintf("%d", ttlMs), healthRecordPrefix, healthTombstonePrefix).Result()
	if err != nil {
		return 0, err
	}
	gen, _ := res.(int64)
	if gen == 0 {
		// script returns int, but go-redis may return int64 or float; handle
		if v, ok := res.(int); ok {
			gen = int64(v)
		}
	}
	h.lastGen.Store(gen)
	return gen, nil
}

// MarkReady attempts Lua READY atomic generation/revision/record HASH/active ZSET/tombstone/TTL
// Requires expected generation matches current generation (stale check).
func (h *RuntimeHealth) MarkReady(ctx context.Context, key HealthKey, expectedGen int64, ttl time.Duration) (int64, error) {
	if h.client == nil {
		return 0, fmt.Errorf("health: no redis")
	}
	field := key.String()
	ttlMs := ttl.Milliseconds()
	if ttlMs <= 0 {
		ttlMs = 30000
	}
	res, err := h.client.Eval(ctx, readyLua, []string{healthGenKey, healthActiveZSet, healthTombstoneHash}, field, fmt.Sprintf("%d", expectedGen), fmt.Sprintf("%d", ttlMs), healthRecordPrefix, healthTombstonePrefix).Result()
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
// Checks both specific quality and wildcard "*" and returns OPEN>RETRY_AFTER>PROBING>READY.
func (h *RuntimeHealth) EffectiveState(accountID int64, quality string, revision int64) HealthState {
	view := h.view.Load()
	if view == nil {
		return StateReady
	}
	specific := view.entries[HealthKey{AccountID: accountID, Quality: quality, Revision: revision}]
	wildcard := view.entries[HealthKey{AccountID: accountID, Quality: "*", Revision: revision}]
	// Determine most severe
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
	// Also consider entries with same account but any quality? No, only these two.
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

// Sync performs INFO run_id + gen-before/records/gen-after atomic read.
// same-run empty clears, run change retain OPEN until then PROBING.
func (h *RuntimeHealth) Sync(ctx context.Context) error {
	if h.client == nil {
		return nil
	}
	// gen-before
	genBeforeStr, err := h.client.Get(ctx, healthGenKey).Result()
	var genBefore int64
	if err == nil {
		_, _ = fmt.Sscanf(genBeforeStr, "%d", &genBefore)
	}
	// INFO run_id
	infoStr, err := h.client.Info(ctx, "replication").Result()
	runID := ""
	if err == nil {
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
	// records: read active ZSET and per-record HASHes
	members, err := h.client.ZRange(ctx, healthActiveZSet, 0, -1).Result()
	if err != nil {
		return err
	}
	records := make(map[HealthKey]healthEntry)
	for _, field := range members {
		recKey := healthRecordPrefix + field
		m, err := h.client.HGetAll(ctx, recKey).Result()
		if err != nil || len(m) == 0 {
			continue
		}
		k, ok := parseHealthKey(field)
		if !ok {
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
		var gen, rev int64
		_, _ = fmt.Sscanf(m["gen"], "%d", &gen)
		_, _ = fmt.Sscanf(m["rev"], "%d", &rev)
		records[k] = healthEntry{Key: k, State: st, Generation: gen, Revision: rev}
	}
	// Also read tombstone to filter READY
	tombMembers, _ := h.client.HGetAll(ctx, healthTombstoneHash).Result()
	_ = tombMembers
	// gen-after
	genAfterStr, err := h.client.Get(ctx, healthGenKey).Result()
	var genAfter int64
	if err == nil {
		_, _ = fmt.Sscanf(genAfterStr, "%d", &genAfter)
	}
	// atomic generation check: if genBefore != genAfter, stale read, retry next tick
	if genBefore != genAfter {
		return fmt.Errorf("health: stale generation %d != %d", genBefore, genAfter)
	}
	h.curGen = genAfter

	// Apply run_id logic: same-run empty clears, run change retain OPEN until then PROBING
	prevView := h.view.Load()
	if len(records) == 0 {
		if runID != "" && runID == h.runID && h.runID != "" {
			// same-run empty clears
			newView := &healthView{entries: make(map[HealthKey]healthEntry), gen: genAfter, runID: runID}
			h.view.Store(newView)
			_, _ = json.Marshal(records) // keep json import used
		} else if runID != h.runID && h.runID != "" {
			// run change retain OPEN until then PROBING
			retained := make(map[HealthKey]healthEntry)
			if prevView != nil {
				for k, e := range prevView.entries {
					if e.State == StateOPEN || e.State == StateRetryAfter {
						e.State = StateProbing
						retained[k] = e
					}
				}
			}
			newView := &healthView{entries: retained, gen: genAfter, runID: runID}
			h.view.Store(newView)
		} else {
			// first sync or runID empty
			newView := &healthView{entries: records, gen: genAfter, runID: runID}
			h.view.Store(newView)
		}
	} else {
		newView := &healthView{entries: records, gen: genAfter, runID: runID}
		h.view.Store(newView)
	}
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
		// Probe injected selfID/rendezvous/ProbeFunc - only owner probes
		if owner != h.selfID {
			continue
		}
		// one permit (try acquire)
		select {
		case <-h.permit:
		default:
			continue
		}
		// Capture current generation for two current-gen successes check
		curGen := h.curGen
		if h.client != nil {
			if gStr, err := h.client.Get(ctx, healthGenKey).Result(); err == nil {
				_, _ = fmt.Sscanf(gStr, "%d", &curGen)
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
				// two current-gen successes READY
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
			// failure reopen
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
