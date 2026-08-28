// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"sync"
)

type latchKey struct {
	AccountID int64
	Fingerprint string
	Revision int64
}

type latchStore struct {
	mu sync.RWMutex
	m map[int64]latchKey
}

func newLatchStore() *latchStore {
	return &latchStore{m: make(map[int64]latchKey)}
}

func (l *latchStore) TryAcquire(accountID int64, fingerprint string, revision int64) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur, ok := l.m[accountID]; ok {
		if cur.Fingerprint == fingerprint && cur.Revision == revision {
			return false
		}
	}
	l.m[accountID] = latchKey{AccountID: accountID, Fingerprint: fingerprint, Revision: revision}
	return true
}

func (l *latchStore) IsLatched(accountID int64, fingerprint string) bool {
	l.mu.RLock()
	defer l.mu.RUnlock()
	cur, ok := l.m[accountID]
	if !ok {
		return false
	}
	if fingerprint != "" && cur.Fingerprint != "" && cur.Fingerprint != fingerprint {
		return false
	}
	return true
}

func (l *latchStore) Clear(accountID int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, accountID)
}

func (l *latchStore) ClearIfRevisionGreater(accountID int64, currentRevision int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur, ok := l.m[accountID]; ok && currentRevision > cur.Revision {
		delete(l.m, accountID)
	}
}

func (l *latchStore) ClearIfFingerprintChanged(accountID int64, currentFingerprint string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if cur, ok := l.m[accountID]; ok && cur.Fingerprint != currentFingerprint {
		delete(l.m, accountID)
	}
}

func (l *latchStore) ClearIfMissing(accountID int64, exists bool) {
	if !exists {
		l.mu.Lock()
		delete(l.m, accountID)
		l.mu.Unlock()
	}
}
