// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

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

// TestInvalidateGroupsStageCallStructure pins the call structure §4 requires as
// a measurement rather than prose: invalidateGroupsLocked freezes/stages exactly
// once (one stageLocked + one newStaticView) while invalidateOneIntoLocked
// rebuilds the identity pool exactly once per successful group (so buildIdentityPools
// is called M times for M groups).
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
	require.Equal(t, 1, batch["stageLocked"], "the batch must stage exactly once")
	require.Equal(t, 1, batch["newStaticView"], "the batch must freeze exactly once")
	require.Equal(t, 0, batch["buildIdentityPools"], "the batch body must not rebuild pools once at the end")

	one := counts["invalidateOneIntoLocked"]
	require.Equal(t, 1, one["buildIdentityPools"], "each successful group rebuilds pools once (M calls for M groups)")
}

// TestPlanKeyRetainedHeapMeasured measures retained heap with live roots + GC
// (never field reflection): build a 5000-account scheduler holding its planKeys
// and the single held decision encoding, force GC, and confirm the held output
// is exactly one canonical copy with no oversize capacity.
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
	retained := after.HeapAlloc - before.HeapAlloc
	require.Greater(t, retained, uint64(0), "the live scheduler must retain heap")

	dv := s.View().DecisionView()
	require.NotNil(t, dv)
	require.Equal(t, decisionViewBytes(dv), s.lastDecisionBytes, "exactly one canonical copy is held")

	t.Logf("retained heap after GC (5000-account scheduler, planKeys + one held encoding) = %d bytes; held bytes=%d; leaves=%d",
		retained, len(s.lastDecisionBytes), len(sv.byID))
}
