// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

// Fold-at-source counter cells (v3 F1, spec §4 FlowCell table): the single
// request-path fold point. Each attempt contributes ONE atomic add per edge;
// ticks expand cells into today's identical rows (fold_expand.go).
//
// OWNERSHIP (normative §4, one owner + lifecycle per structure): the table is
// constructed by newFlowOwner at recorder construction (the same construction
// site, new structure). Request paths only Add (never grow, never read-modify
// -write a row); ONLY the tick owner drains (Swap-collect, tombstones old
// minutes, grows) — all under the shard write lock. Lifecycle: construction
// (preallocated slots) -> request Adds -> tick drain/fold/grow/tombstone ->
// seal (drain-all + revoke) -> process exit. No eviction, no pooling,
// no sampling.
//
// SHARDING (normative): 64 shards (pow2), shard = accountID low 6 bits (no
// hash — direct), each shard cache-line-padded so concurrent Adds on adjacent
// shards never false-share.
// CAPACITY (normative): 4096 cells per shard max (256k total), NO eviction.
// An Add that finds its shard full lands in the per-shard overflow counter
// (provisioning FAIL if nonzero at any gate, never a drop class).
// HOT-ACCOUNT BOUND (normative): worst case is the full load on one shard
// (30k rps x <=8 edges = 240k adds/s); repeat-edge Adds are a read-lock probe
// plus one atomic add — single-core atomic-add throughput is O(10M)/s, i.e.
// >40x headroom. New-identity claims take the shard write lock briefly; the
// tick drain takes it per shard for microseconds. Adds on other shards never
// block.
//
// Placement within a shard is open-addressed (linear probe) over the packed
// comparable key, with tombstones: empty slots terminate probes, tombstoned
// (drained old-minute) slots stay probed but reusable, live slots are never
// overwritten (that would be eviction). The placement hash is FNV-1a over
// stack key bytes (~30ns, zero-alloc): NOT a string key, NOT hex/sha256 —
// those stay off the path entirely. A linear scan per Add would be O(cap) per
// request and is forbidden by invariant 1; the placement hash is the minimal
// mechanism the normative open-addressed shape requires.

import (
	"sync"
	"sync/atomic"
)

const (
	foldShardBits         = 6
	foldShardCount        = 1 << foldShardBits
	foldShardMask         = foldShardCount - 1
	foldCellShardCapStart = 512
	foldCellShardCapMax   = 4096
	foldCellGrowLoad      = 0.75
)

const (
	foldSlotEmpty uint32 = iota
	foldSlotLive
	foldSlotTomb
)

// foldSlot is one counter cell: the packed key plus its exact event count.
// state/seq are tick-owned (claims and tombstones happen under the shard
// write lock, always tick- or insert-side; requests only Add to count).
// A live slot is never overwritten; a tombstoned slot is reusable.
type foldSlot struct {
	key   attemptFact
	count atomic.Int64
	seq   atomic.Uint64
	state uint32
}

type foldShard struct {
	mu       sync.RWMutex
	slots    []foldSlot
	mask     uint64
	overflow atomic.Int64 // per-shard table-full counter (normative §5.1)
	_        [64]byte     // cache-line pad: adjacent shards never false-share
}

// foldCellTable is the sharded open-addressed counter store.
type foldCellTable struct {
	shards  [foldShardCount]foldShard
	pending atomic.Int64 // accepted-not-yet-folded events (stats/work bound)
	grown   atomic.Uint64
	_       [64]byte
}

func newFoldCellTable() *foldCellTable {
	t := &foldCellTable{}
	for i := range t.shards {
		t.shards[i].slots = make([]foldSlot, foldCellShardCapStart)
		t.shards[i].mask = foldCellShardCapStart - 1
	}
	return t
}

// resetCellsForTest rebuilds every shard at the given (pow2) capacity.
// Single-threaded setup only — the deterministic shard-full probe.
func (t *foldCellTable) resetCellsForTest(shardCap int) {
	for i := range t.shards {
		t.shards[i].slots = make([]foldSlot, shardCap)
		t.shards[i].mask = uint64(shardCap - 1)
		t.shards[i].overflow.Store(0)
	}
	t.pending.Store(0)
}

func foldShardOf(accountID int64) int { return int(uint64(accountID) & foldShardMask) }

// foldHash is hand-rolled FNV-1a over the packed key bytes — deliberately NOT
// hash/fnv: the digest would round-trip through the hash.Hash64 interface
// (a New64a + a Write per field + Sum64, with per-call interface dispatch),
// while this inlined loop mixes each field while encoding it, single-pass
// over fixed-size stack bytes (no strings, no crypto). Measured zero-alloc
// either way (TestTmpFoldHashZeroAlloc, since removed); the hand-rolled form
// stays for the inlined single pass on the fold hot path, not for an alloc
// delta.
func foldHash(f attemptFact) uint64 {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	h := uint64(offset)
	mix := func(b byte) { h ^= uint64(b); h *= prime }
	mixBytes := func(p []byte) {
		for _, b := range p {
			mix(b)
		}
	}
	mixU64 := func(v uint64) {
		for i := 0; i < 8; i++ {
			mix(byte(v >> (8 * i)))
		}
	}
	mixBytes(f.route[:])
	mixBytes(f.fingerprint[:])
	mixU64(uint64(f.accountID))
	mixU64(uint64(f.prevAccount))
	mixU64(uint64(f.generation))
	mixU64(uint64(f.minuteBucket))
	mix(f.ordinal)
	mix(f.lane)
	mix(f.outcome)
	mix(f.prevOutcome)
	mix(f.transition)
	mix(f.identityVersion)
	if f.isTerminal {
		mix(1)
	} else {
		mix(0)
	}
	if f.hasPrev {
		mix(1)
	} else {
		mix(0)
	}
	if f.hasPrevOutcome {
		mix(1)
	} else {
		mix(0)
	}
	if f.residual {
		mix(1)
	} else {
		mix(0)
	}
	return h
}

// add lands one delta for f. True = landed (counted by the caller);
// false = shard full (caller counts the provisioning overflow — never
// silent). The landed slot persists (slots are tombstoned, never freed), so
// the seal-split move-one fixup always finds its slot or claims a reusable
// one; only a completely full shard refuses it, in which case the caller
// keeps the at-Add classification (still exact).
func (t *foldCellTable) add(f attemptFact, delta int64) bool {
	s := &t.shards[foldShardOf(f.accountID)]
	h := foldHash(f)
	s.mu.RLock()
	for i := uint64(0); i <= s.mask; i++ {
		sl := &s.slots[(h+i)&s.mask]
		st := sl.state
		if st == foldSlotEmpty {
			break // terminator: no match beyond (claims always fill before empty)
		}
		if sl.key == f {
			sl.count.Add(delta)
			s.mu.RUnlock()
			t.pending.Add(delta)
			return true
		}
	}
	s.mu.RUnlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	for i := uint64(0); i <= s.mask; i++ {
		sl := &s.slots[(h+i)&s.mask]
		if sl.state != foldSlotEmpty && sl.key == f {
			sl.count.Add(delta)
			t.pending.Add(delta)
			return true
		}
	}
	for i := uint64(0); i <= s.mask; i++ {
		sl := &s.slots[(h+i)&s.mask]
		if sl.state != foldSlotLive {
			sl.key = f
			sl.state = foldSlotLive
			sl.count.Add(delta)
			t.pending.Add(delta)
			return true
		}
	}
	s.overflow.Add(1)
	flowChainEnqueueOverflow.Add(1)
	return false
}

// drain swaps every nonzero count out exactly once and hands (key, count) to
// fn after releasing the shard lock; slots older than curMinute are
// tombstoned (reusable, still probed). Current-minute slots stay live so the
// hot keys keep their lock-free fast path. The caller must hold o.mu (drain
// is tick-owned).
func (t *foldCellTable) drain(curMinute int64, seq uint64, fn func(f attemptFact, count int64)) int64 {
	type drained struct {
		key   attemptFact
		count int64
	}
	var total int64
	for i := range t.shards {
		s := &t.shards[i]
		var out []drained
		s.mu.Lock()
		for j := range s.slots {
			sl := &s.slots[j]
			if sl.state == foldSlotEmpty {
				continue
			}
			if c := sl.count.Swap(0); c != 0 {
				sl.seq.Store(seq)
				out = append(out, drained{key: sl.key, count: c})
				total += c
			}
			if sl.state != foldSlotEmpty && sl.key.minuteBucket < curMinute {
				sl.state = foldSlotTomb
			}
		}
		s.mu.Unlock()
		for _, d := range out {
			fn(d.key, d.count)
		}
	}
	t.pending.Add(-total)
	return total
}

// growIfNeeded discards stale probe runs by reallocating shards that
// overflowed or exceed the load factor, up to the normative max. Post-drain
// every count is already folded, so reallocation drops keys freely (nothing
// to conserve). TICK-ONLY: called from the two tick expansions
// (snapshotForPG/snapshotForRedis), never from diagnostic reads and never
// from the request path.
func (t *foldCellTable) growIfNeeded() {
	for i := range t.shards {
		s := &t.shards[i]
		if s.overflow.Load() == 0 && t.loadPct(s) < foldCellGrowLoad {
			continue
		}
		s.mu.Lock()
		cur := len(s.slots)
		if cur < foldCellShardCapMax && (s.overflow.Load() > 0 || t.loadPctLocked(s) >= foldCellGrowLoad) {
			next := cur * 2
			if next > foldCellShardCapMax {
				next = foldCellShardCapMax
			}
			s.slots = make([]foldSlot, next)
			s.mask = uint64(next - 1)
			t.grown.Add(1)
		}
		s.mu.Unlock()
	}
}

func (t *foldCellTable) loadPct(s *foldShard) float64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return t.loadPctLocked(s)
}

func (t *foldCellTable) loadPctLocked(s *foldShard) float64 {
	if len(s.slots) == 0 {
		return 0
	}
	var n int
	for j := range s.slots {
		if s.slots[j].state == foldSlotLive {
			n++
		}
	}
	return float64(n) / float64(len(s.slots))
}

// overflowTotal sums the per-shard table-full counters for one table.
func (t *foldCellTable) overflowTotal() int64 {
	var total int64
	for i := range t.shards {
		total += t.shards[i].overflow.Load()
	}
	return total
}

// flowChainEnqueueOverflow is the process-wide table-full tally feeding the
// service loss seam (Capacity+Enqueue). Bumped beside the per-shard counter
// (full shards are provisioning failures — off the hot path by definition).
var flowChainEnqueueOverflow atomic.Int64

func FlowChainEnqueueOverflow() int64 { return flowChainEnqueueOverflow.Load() }
