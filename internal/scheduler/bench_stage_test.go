// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// Final delivery scope (record only; the spec's frozen thresholds are NOT
// changed here): the branch ships T1 (folded group-invalidation batch, shared
// deadline, sticky retry) + T2 (single snapshotStatic constructor / cached
// planKey) + T4 (candidate-build + cache-domain alloc trims). T3 (single-copy
// decision encoding) was REVERTED under the spec's "未达即回退" rule (commit
// 6c89167e): no T3 production change ships and NO encoder retained-buffer
// reduction is claimed. The frozen T3 thresholds remain equal ≤1.2× /
// changed ≤2.5× (recorded for history only; T3 acceptance no longer applies).
// The retained-heap benchmark below therefore measures the BASE reusable
// encoder buffer + the inline planKey retention, not any "de-duplicated"
// encoding.

// BenchmarkInvalidateSingleGroupStage is the M-times single-group CONTRAST for
// BenchmarkInvalidateGroupsStage: the same topology driven as M separate
// single-group batches, so the one-freeze batch's stage/newStaticView saving is
// visible against M separate stages.
func BenchmarkInvalidateSingleGroupStage(b *testing.B) {
	for _, m := range []int{1, 10, 100} {
		a := m*50 + 10
		b.Run(fmt.Sprintf("M%d_A%d", m, a), func(b *testing.B) {
			s, ldr := batchStageFixture(b, m)
			ids := make([]int64, 0, m)
			for gid := 1; gid <= m; gid++ {
				ids = append(ids, int64(gid))
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ldr.reset()
				for _, gid := range ids {
					s.InvalidateGroups([]int64{gid})
				}
			}
			b.StopTimer()
			if got := ldr.groupCallsCopy(); len(got) != m {
				b.Fatalf("LoadGroupAccounts calls = %d, want %d (one per single-group batch)", len(got), m)
			}
		})
	}
}

// BenchmarkInvalidateGroupsStageCodex is the codex-ratio>0 counterpart of
// BenchmarkInvalidateGroupsStage: every group carries codex (OAuth) accounts
// with live identity pools, so the step-wise pool conversion (syncIdentityPool
// reuse vs resize) is actually exercised. It reports ns/op, B/op and allocs/op
// and validates the pool ledger (reuse/created/resize counts) after the run.
func BenchmarkInvalidateGroupsStageCodex(b *testing.B) {
	const m = 10
	b.Run(fmt.Sprintf("M%d_codex", m), func(b *testing.B) {
		s, ldr := codexBatchStageFixture(b, m)
		prev := s.View().StaticView().identityPools
		ids := make([]int64, 0, m)
		for gid := 1; gid <= m; gid++ {
			ids = append(ids, int64(gid))
		}
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			ldr.reset()
			s.InvalidateGroups(ids)
		}
		b.StopTimer()
		if got := ldr.groupCallsCopy(); len(got) != m {
			b.Fatalf("LoadGroupAccounts calls = %d, want %d", len(got), m)
		}
		reused, created, resized := poolLedger(prev, s.publisher.pending.identityPools)
		b.Logf("codex pool ledger: reused=%d created=%d resized=%d (pools before=%d)", reused, created, resized, len(prev.pools))
	})
}

// codexBatchStageFixture builds `m` groups, each with perGroup=50 codex accounts
// plus 10 codex accounts shared across every group, all with live identity
// pools — so codex ratio > 0 and the step-wise pool conversion does real work.
func codexBatchStageFixture(b *testing.B, m int) (*Scheduler, *t1Loader) {
	b.Helper()
	const (
		perGroup = 50
		shared   = 10
	)
	sharedAccs := make([]*domain.Account, 0, shared)
	for i := 0; i < shared; i++ {
		sharedAccs = append(sharedAccs, codexAcc(int64(1_000_000+i), domain.FormatOpenAIResponses, "gpt-5", 2, fmt.Sprintf("inst-shared-%d", i)))
	}
	byGroup := make(map[int64][]*domain.Account, m)
	for gid := 1; gid <= m; gid++ {
		accs := append([]*domain.Account(nil), sharedAccs...)
		for j := 0; j < perGroup; j++ {
			accs = append(accs, codexAcc(int64(gid*1000+j), domain.FormatOpenAIResponses, "gpt-5", 2, fmt.Sprintf("inst-%d-%d", gid, j)))
		}
		byGroup[int64(gid)] = accs
	}
	ldr := newT1Loader(byGroup)
	s := New(testCfg(), ldr, newTestRuleEngine(b), nil, nil, nil, nil)
	if err := s.reload(context.Background()); err != nil {
		b.Fatal(err)
	}
	wireSources(s, nil, nil)
	s.compileOnce()
	return s, ldr
}

// poolLedger classifies each pool in next by its relationship to prev: reused
// (same pointer), created (no previous pool for the account) or resized
// (present before but a fresh pointer).
func poolLedger(prev, next *identityRegistry) (reused, created, resized int) {
	if next == nil {
		return
	}
	for id, np := range next.pools {
		var op *identityPool
		if prev != nil {
			op = prev.pools[id]
		}
		switch {
		case op == nil:
			created++
		case op == np:
			reused++
		default:
			resized++
		}
	}
	return
}

// TestInvalidateGroupsCodexPoolLedger asserts the step-wise pool conversion
// ledger: an unchanged batch reuses every pool by pointer; a K change resizes
// exactly the changed account's pool; a newly-added codex account creates one.
func TestInvalidateGroupsCodexPoolLedger(t *testing.T) {
	shared := codexAcc(1000, domain.FormatOpenAIResponses, "gpt-5", 2, "inst-shared")
	g1 := codexAcc(1, domain.FormatOpenAIResponses, "gpt-5", 3, "inst-1")
	g2 := codexAcc(2, domain.FormatOpenAIResponses, "gpt-5", 4, "inst-2")
	ldr := newT1Loader(map[int64][]*domain.Account{1: {shared, g1}, 2: {shared, g2}})
	s := New(testCfg(), ldr, newTestRuleEngine(t), nil, nil, nil, nil)
	require.NoError(t, s.reload(context.Background()))
	prev := s.publisher.pending.identityPools
	require.Len(t, prev.pools, 3)

	s.InvalidateGroups([]int64{1, 2})
	reused, created, resized := poolLedger(prev, s.publisher.pending.identityPools)
	require.Equal(t, 3, reused, "unchanged pools must be reused by pointer")
	require.Zero(t, created)
	require.Zero(t, resized)

	prev = s.publisher.pending.identityPools
	ldr.mu.Lock()
	g1.MaxConcurrency = 5
	ldr.mu.Unlock()
	s.InvalidateGroups([]int64{1})
	reused, created, resized = poolLedger(prev, s.publisher.pending.identityPools)
	require.Equal(t, 1, resized, "the changed-K account's pool must be resized")
	require.Equal(t, 2, reused)
	require.Zero(t, created)

	prev = s.publisher.pending.identityPools
	newAcc := codexAcc(3, domain.FormatOpenAIResponses, "gpt-5", 2, "inst-3")
	ldr.mu.Lock()
	ldr.byGroup[1] = []*domain.Account{shared, g1, newAcc}
	ldr.mu.Unlock()
	s.InvalidateGroups([]int64{1})
	reused, created, resized = poolLedger(prev, s.publisher.pending.identityPools)
	require.Equal(t, 1, created, "a newly-added codex account must create a pool")
	require.Equal(t, 3, reused)
	require.Zero(t, resized)
}

// TestInvalidateGroupsStageCallStructure pins the call structure §4 requires as
// a STRUCTURAL PROXY (a STATIC call-SITE count — NOT a runtime call count):
// invalidateGroupsLocked freezes/stages exactly once (one stageLocked call site +
// one newStaticView call site) while invalidateOneIntoLocked rebuilds the
// identity pool exactly once per successful group (so buildIdentityPools is
// called M times for M groups). A call moved into a loop would still pass, so
// this is explicitly a structural proxy; the dynamic side is covered by the
// runtime ledger (poolLedger) and the M-times single-group contrast benchmark.
func TestInvalidateGroupsStageCallStructure(t *testing.T) {
	root := gateRoot(t)
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filepath.Join(root, "internal", "scheduler", "invalidate_batch.go"), nil, parser.SkipObjectResolution)
	require.NoError(t, err)

	counts := map[string]map[string]int{}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		c := map[string]int{}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			ce, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := ce.Fun.(type) {
			case *ast.SelectorExpr:
				c[fun.Sel.Name]++
			case *ast.Ident:
				c[fun.Name]++
			}
			return true
		})
		counts[fn.Name.Name] = c
	}

	batch := counts["invalidateGroupsLocked"]
	require.Equal(t, 1, batch["stageLocked"], "structural proxy: the batch body has exactly one stageLocked call site")
	require.Equal(t, 1, batch["newStaticView"], "structural proxy: the batch body has exactly one newStaticView call site")
	require.Equal(t, 0, batch["buildIdentityPools"], "structural proxy: the batch body does not rebuild pools once at the end")

	one := counts["invalidateOneIntoLocked"]
	require.Equal(t, 1, one["buildIdentityPools"], "structural proxy: each successful group has one buildIdentityPools call site (M calls for M groups)")
}

// TestPlanKeyRetainedHeapMeasured measures retained heap with live roots + GC
// (never field reflection): build a 5000-account scheduler holding its inline
// planKeys and the BASE reusable encoder's retained bytes, force GC, and assert
// the retained bytes equal the canonical decision encoding. Byte equality only
// proves the held cache matches the canonical encoding — it does NOT prove how
// many copies are held (the BASE encoder's reusable buffer remains). The
// before/after delta is computed as a SIGNED difference so a bucket that shrank
// (GC released earlier garbage) can never underflow into a bogus large positive
// value.
func TestPlanKeyRetainedHeapMeasured(t *testing.T) {
	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	s := schedulerWithAccounts(t, 5000, domain.ModelMapping{})
	sv := s.View().StaticView()
	require.NotNil(t, sv)

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)

	// Keep the live roots alive across the sampling reads.
	runtime.KeepAlive(s)
	runtime.KeepAlive(sv)

	retained := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	require.GreaterOrEqual(t, after.HeapAlloc, before.HeapAlloc,
		"live-root heap must not shrink below the pre-GC baseline")
	require.Greater(t, retained, int64(0), "the live scheduler must retain heap")

	dv := s.View().DecisionView()
	require.NotNil(t, dv)
	require.Equal(t, decisionViewBytes(dv), s.lastDecisionBytes,
		"held cache must equal the canonical encoding (byte equality only; the BASE reusable encoder buffer remains)")

	// ESTIMATED inline planKey bytes: planKey is stored inline in each
	// snapshotStatic leaf, so its logical contribution is sizeof(planKey) per
	// leaf. This is an ESTIMATE from the field layout, NOT an isolated measured
	// increment; it is logged so a BASE/HEAD retained differential can be read.
	planKeyBytes := int(unsafe.Sizeof(planKey{})) * len(sv.byID)
	t.Logf("retained heap after GC (5000-account scheduler) = %d bytes; estimated inline planKey bytes = %d (%d leaves × %d B); held bytes=%d; leaves=%d",
		retained, planKeyBytes, len(sv.byID), unsafe.Sizeof(planKey{}), len(s.lastDecisionBytes), len(sv.byID))
}

// sleepLoader blocks one group's load for a fixed duration under publisher.mu,
// so a concurrent compile/publish observes the batch lock hold.
type sleepLoader struct {
	*t1Loader
	blockGroup int64
	entered    chan struct{}
	hold       time.Duration
}

func (l *sleepLoader) LoadGroupAccounts(ctx context.Context, id int64) ([]*domain.Account, error) {
	if id == l.blockGroup {
		select {
		case l.entered <- struct{}{}:
		default:
		}
		time.Sleep(l.hold)
	}
	return l.t1Loader.LoadGroupAccounts(ctx, id)
}

// TestInvalidateGroupsBlocksCompilePublish measures the compile/publish latency a
// batch imposes on the lane: a loader stalled `hold` under publisher.mu makes a
// concurrent compileOnce wait for the same lock, so its completion latency is at
// least half the hold.
func TestInvalidateGroupsBlocksCompilePublish(t *testing.T) {
	const hold = 60 * time.Millisecond
	tp := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	base := newT1Loader(map[int64][]*domain.Account{10: {acc(1, tp, 4)}})
	ldr := &sleepLoader{t1Loader: base, blockGroup: 10, entered: make(chan struct{}, 1), hold: hold}
	s := New(testCfg(), ldr, newTestRuleEngine(t), nil, nil, nil, nil)
	require.NoError(t, s.reload(context.Background()))
	wireSources(s, nil, nil)
	s.compileOnce()

	batchDone := make(chan struct{})
	go func() {
		s.InvalidateGroups([]int64{10})
		close(batchDone)
	}()
	select {
	case <-ldr.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("batch never entered the stalled load")
	}

	start := time.Now()
	s.compileOnce() // waits for publisher.mu held by the batch
	elapsed := time.Since(start)
	<-batchDone
	require.GreaterOrEqual(t, elapsed, hold/2, "compile/publish latency must reflect the batch lock hold")
	t.Logf("batch lock hold=%v, compile/publish wait=%v", hold, elapsed)
}
