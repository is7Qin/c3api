// SPDX-License-Identifier: AGPL-3.0-or-later
package quality

// Fold-at-source sync handoff and seal (v3 F2): the versioned
// snapshot/ack/lease handshake the sync flush settles through, plus the
// shutdown seal. Sync never owns rows; it snapshots one dirty minute at a
// time under the owner lock, writes the full cumulative snapshot with
// replacement UpsertFlowSnapshot, and settles through the token. Failure
// means no ack and no refill: owner state stays dirty and retries next
// cycle. Only the expansion source changed (cells, not chains).

import (
	"encoding/json"
	"sort"
)

// flowPayloadState 是 redisPayload 的返回态：无内容 / rows 载荷 / 空快照标记。
type flowPayloadState uint8

const (
	flowPayloadNone flowPayloadState = iota
	flowPayloadRows
	flowPayloadEmpty
)

// redisPayload 返回某分钟 Redis flow 发布的载荷片段，并按 (minute, shell
// version) 缓存 rows 数组 JSON：同一版本重发布（保留分钟的每 tick 序号心跳）
// 只重拼小 wrapper，不再重物化/重编码。state 语义与旧调用面一致：
// flowPayloadNone = 无行且无空标记（跳过），flowPayloadRows = blob 为 rows
// JSON，flowPayloadEmpty = 空快照标记。ok=false = 已 seal 或分钟未知（无载荷
// 可发布）。只读：无 lease、无 ack，owner 保留态不动。
//
// Caller-facing。编码在锁外进行：materialize 必须在锁内读 shell.counts，
// 但 json 编码只依赖物化快照；仅当版本未变才写回缓存（否则返回本次编码，
// 内容与快照时点一致）。
func (o *FlowOwner) redisPayload(minute int64) (blob []byte, state flowPayloadState, ok bool) {
	o.mu.Lock()
	o.drainFoldLocked()
	o.cells.growIfNeeded()
	if o.sealed.Load() {
		o.mu.Unlock()
		return nil, flowPayloadNone, false
	}
	shell, found := o.shells[minute]
	if !found {
		o.mu.Unlock()
		return nil, flowPayloadNone, false
	}
	switch {
	case shell.emptyMarked && len(shell.counts) == 0:
		o.mu.Unlock()
		return nil, flowPayloadEmpty, true
	case len(shell.counts) == 0:
		o.mu.Unlock()
		return nil, flowPayloadNone, true
	case shell.redisBlob != nil && shell.redisBlobVersion == shell.version:
		blob = shell.redisBlob
		o.mu.Unlock()
		return blob, flowPayloadRows, true
	}
	version := shell.version
	fm := materializeShell(shell)
	o.mu.Unlock()
	encoded, err := json.Marshal(fm.FlowRows())
	if err != nil {
		// 纯结构体数组编码不会失败；fail closed 为"无可发布"。
		return nil, flowPayloadNone, true
	}
	o.mu.Lock()
	if shell.version == version && !o.sealed.Load() {
		shell.redisBlob = encoded
		shell.redisBlobVersion = version
	}
	o.mu.Unlock()
	return encoded, flowPayloadRows, true
}

// pgCandidateMinutes returns dirty, unleased, PG-open minute IDs in
// deterministic oldest-first order (draining first, so every folded fact is
// visible). Each candidate is attempted at most once per flush cycle.
// Caller-facing; takes o.mu via the method itself.
func (o *FlowOwner) pgCandidateMinutes() []int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.drainFoldLocked()
	if o.sealed.Load() {
		return nil
	}
	var out []int64
	for minute, shell := range o.shells {
		if shell.dirty && !shell.leased {
			out = append(out, minute)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// redisCandidateMinutes returns PG-open pre-seal retained minute IDs in
// deterministic oldest-first order with no row work beyond the fold.
// Read-only: no lease, no ack. Caller-facing.
func (o *FlowOwner) redisCandidateMinutes(curMinute int64) []int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.drainFoldLocked()
	if o.sealed.Load() {
		return nil
	}
	var out []int64
	for minute := range o.shells {
		if minute <= curMinute {
			out = append(out, minute)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// snapshotForPG atomically acquires a per-minute lease with
// leaseID = ++owner.nextLeaseID, captures the token, and returns one
// deep-owned payload (the tick scratch). Same-minute folds arriving while the
// lease is active land in the live shell (bump version, stay dirty, exceed
// the captured watermark) and never mutate the materialized payload. At most
// one payload is live per call; callers consume it synchronously before the
// next snapshot. Tick expansion grows here. Caller-facing.
func (o *FlowOwner) snapshotForPG(minute int64) (*FlowMinute, foldSnapshotToken, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.drainFoldLocked()
	o.cells.growIfNeeded()
	if o.sealed.Load() {
		return nil, foldSnapshotToken{}, false
	}
	shell, ok := o.shells[minute]
	if !ok || !shell.dirty || shell.leased {
		return nil, foldSnapshotToken{}, false
	}
	o.nextLeaseID++
	shell.leased = true
	shell.leaseID = o.nextLeaseID
	tok := foldSnapshotToken{
		minute:            minute,
		leaseID:           shell.leaseID,
		capturedVersion:   shell.version,
		acceptedWatermark: shell.acceptedContrib,
	}
	return materializeShell(shell), tok, true
}

// matchLeaseLocked is the two-field identity match (minute plus activeLeaseID
// with an active lease). The captured version is only for the clean-vs-dirty
// decision, never for identity, so a stale token from any prior lease is a
// no-op even when the version is unchanged.
func (o *FlowOwner) matchLeaseLocked(tok foldSnapshotToken) (*foldShell, bool) {
	shell, ok := o.shells[tok.minute]
	if !ok || !shell.leased || shell.leaseID != tok.leaseID {
		return nil, false
	}
	return shell, true
}

// ackPG settles a lease on success: advances the persisted watermark to the
// captured value, marks everPersisted (including empty zero-row snapshots),
// clears the lease, and marks clean only when the version is unchanged and
// PG is not sealed. Ack never overwrites new state; any mismatch is a no-op.
// Caller-facing.
func (o *FlowOwner) ackPG(tok foldSnapshotToken) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	shell, ok := o.matchLeaseLocked(tok)
	if !ok {
		return false
	}
	if tok.acceptedWatermark > shell.persistedWatermark {
		shell.persistedWatermark = tok.acceptedWatermark
	}
	shell.everPersisted = true
	shell.leased = false
	shell.leaseID = 0
	if shell.version == tok.capturedVersion && !o.sealed.Load() {
		shell.dirty = false
	}
	return true
}

// releasePG settles a lease on failure, deferral, expiry, or unwind: clears
// the lease and leaves the minute dirty with watermarks unchanged for
// next-cycle retry. A stale token from any prior lease is a no-op and clears
// nothing. Caller-facing.
func (o *FlowOwner) releasePG(tok foldSnapshotToken) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	shell, ok := o.matchLeaseLocked(tok)
	if !ok {
		return false
	}
	shell.leased = false
	shell.leaseID = 0
	return true
}

// sealPG revokes PG candidacy at shutdown: it folds every undrained cell
// first (landing residual under seal), sets sealed, revokes every active
// lease so every late ack/release is a stale-token no-op, and moves ALL
// unconfirmed accepted credits (acceptedContrib - persistedWatermark) from
// accepted to residual exactly once for every minute including leased ones.
// Sealed minutes retain cumulative rows for diagnostics only and are never
// published after seal. Idempotent; the sweep runs exactly once per owner
// lifetime. Called by SyncWorker.Close on every terminal path.
func (o *FlowOwner) sealPG() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.sealed.Load() {
		return
	}
	o.sealed.Store(true)
	o.sealedGen.Store(1)
	o.drainFoldLocked()
	for _, shell := range o.shells {
		shell.leased = false
		shell.leaseID = 0
		if unconfirmed := shell.acceptedContrib - shell.persistedWatermark; unconfirmed > 0 {
			o.edgeRowsAccepted.Add(-unconfirmed)
			o.residualRows.Add(unconfirmed)
			shell.persistedWatermark = shell.acceptedContrib
		}
	}
}

// pgWorkTotal is the bounded Close-drain and PendingFlow signal: dirty or
// leased PG-open shells, plus one when undrained cells remain. Clean retained
// reconstruction state contributes nothing, so it cannot block Close or
// inflate pending work. Sealed owners report zero.
func (o *FlowOwner) pgWorkTotal() int {
	if o.sealed.Load() {
		return 0
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	total := 0
	for _, shell := range o.shells {
		if shell.leased || shell.dirty {
			total++
		}
	}
	if o.cells.pending.Load() > 0 {
		total++
	}
	return total
}
