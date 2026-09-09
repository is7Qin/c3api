// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type auditEntry struct {
	Seq      int64     `json:"seq"`
	Path     string    `json:"path"`
	Key      string    `json:"key"`
	Model    string    `json:"model,omitempty"`
	CacheKey string    `json:"prompt_cache_key,omitempty"`
	Status   int       `json:"status"`
	At       time.Time `json:"at"`
}

const auditCap = 4096

type auditLog struct {
	mu    sync.Mutex
	seq   int64
	items []auditEntry
}

func (a *auditLog) add(r *http.Request, body map[string]any, status int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq++
	e := auditEntry{Seq: a.seq, Path: r.URL.Path, Key: bearerKey(r), Status: status, At: time.Now()}
	if body != nil {
		e.Model, _ = body["model"].(string)
		e.CacheKey, _ = body["prompt_cache_key"].(string)
	}
	a.items = append(a.items, e)
	if n := len(a.items) - auditCap; n > 0 {
		a.items = a.items[n:]
	}
}

func (a *auditLog) snapshot() []auditEntry {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]auditEntry{}, a.items...)
}

func bearerKey(r *http.Request) string {
	return strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
}

func upstreamKey(r *http.Request) string {
	if key := bearerKey(r); key != "" {
		return key
	}
	return strings.TrimSpace(r.Header.Get("x-api-key"))
}

var audit auditLog
var responseSequence atomic.Int64
var responsePrefix = fmt.Sprintf("rsp_%d", os.Getpid())

func nextResponseID() string {
	return fmt.Sprintf("%s_%d", responsePrefix, responseSequence.Add(1))
}
