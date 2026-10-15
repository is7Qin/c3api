// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

// Routing-plan-memory retained-heap probe (spec §6). BASE-COMPATIBLE: copied
// verbatim into the BASE worktree so both sides measure the same fixture and
// hold equivalent internal roots. Independent process, post-GC HeapAlloc delta.
//
// Run per side (serial, no concurrent perf measurements):
//   RPM_MEM_PROBE=1 RPM_MODELS=693 go test ./internal/scheduler -run TestRPMMemoryProbe -count=1 -v
//
// Reports, on one line prefixed "RPM_MEM":
//   models routes unique_nodes_backings node_payload_bytes retained_heap_after_gc
// node_payload_bytes = Σ_i cap(Nodes_i) × sizeof(CacheDomainNode) over the
// UNIQUE internal Nodes backings (one per actual domain set), exactly the
// spec's capacity formula. It excludes slice headers, Domains backing, maps and
// string storage, which the separate retained_heap_after_gc covers physically.

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"
)

func rpmProbeModels() int {
	if v := os.Getenv("RPM_MODELS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return 693
}

// rpmNodeBackingCaps maps each unique internal Nodes backing to its capacity.
func rpmNodeBackingCaps(dv *DecisionView) map[uintptr]int {
	out := map[uintptr]int{}
	for _, rd := range dv.routes {
		if rd == nil || len(rd.CacheDomainRing.Nodes) == 0 {
			continue
		}
		p := uintptr(unsafe.Pointer(unsafe.SliceData(rd.CacheDomainRing.Nodes)))
		out[p] = cap(rd.CacheDomainRing.Nodes)
	}
	return out
}

// rpmUniqueDomainSets counts distinct actual domain sets without relying on
// HEAD-only key helpers (BASE-compatible).
func rpmUniqueDomainSets(dv *DecisionView) int {
	seen := map[string]struct{}{}
	for _, rd := range dv.routes {
		if rd == nil {
			continue
		}
		seen[fmt.Sprintf("%v", rd.CacheDomainRing.Domains)] = struct{}{}
	}
	return len(seen)
}

// TestRPMMemoryProbe measures the retained heap of the internal published root
// for the homogeneous max fixture, post-GC, against a pre-build baseline. Gated
// behind RPM_MEM_PROBE so it never runs as part of the normal fast suite.
func TestRPMMemoryProbe(t *testing.T) {
	if os.Getenv("RPM_MEM_PROBE") == "" {
		t.Skip("set RPM_MEM_PROBE=1 to run the retained-heap probe")
	}
	models := rpmModels(rpmProbeModels())

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	s, _ := rpmHomogeneousScheduler(t, models)

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(s)

	require.GreaterOrEqual(t, after.HeapAlloc, before.HeapAlloc,
		"live-root heap must not shrink below the pre-GC baseline")
	retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)

	dv := s.View().DecisionView()
	require.NotNil(t, dv)
	caps := rpmNodeBackingCaps(dv)
	nodePayload := 0
	for _, c := range caps {
		nodePayload += c * int(unsafe.Sizeof(CacheDomainNode{}))
	}
	t.Logf("RPM_MEM models=%d routes=%d unique_nodes_backings=%d unique_domain_sets=%d node_payload_bytes=%d retained_heap_after_gc=%d",
		len(models), len(dv.routes), len(caps), rpmUniqueDomainSets(dv), nodePayload, retained)
}
