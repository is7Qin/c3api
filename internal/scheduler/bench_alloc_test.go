// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"fmt"
	"testing"

	"github.com/is7qin/c3api/internal/domain"
)

var (
	benchmarkSnapshotStatic *snapshotStatic
	benchmarkPlanKey        planKey
	benchmarkCompareEqual   bool
)

// --- T1: batch-stage benchmark (M groups folded into one stage/freeze) ---

// batchStageFixture builds a scheduler over `m` groups, each carrying
// perGroup=50 own accounts plus shared=10 accounts shared across EVERY group.
// Setup (mem loader + full reload + compile) stays outside the timer; the
// benchmark measures only InvalidateGroups (per-group loads + fold + one freeze).
// It uses api_key accounts (codex ratio = 0 → no identity pools); the ratio>0
// counterpart with a pool reuse/created/resize ledger is
// codexBatchStageFixture + BenchmarkInvalidateGroupsStageCodex in bench_stage_test.go.
func batchStageFixture(b *testing.B, m int) (*Scheduler, *t1Loader) {
	b.Helper()
	tp := tpl(1, domain.FormatOpenAIChat, []string{"m"})
	const (
		perGroup = 50
		shared   = 10
	)
	sharedAccs := make([]*domain.Account, 0, shared)
	for i := 0; i < shared; i++ {
		sharedAccs = append(sharedAccs, acc(int64(1_000_000+i), tp, 4))
	}
	byGroup := make(map[int64][]*domain.Account, m)
	for gid := 1; gid <= m; gid++ {
		accs := append([]*domain.Account(nil), sharedAccs...)
		for j := 0; j < perGroup; j++ {
			accs = append(accs, acc(int64(gid*1000+j), tp, 4))
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

// BenchmarkInvalidateGroupsStage reports ns/op, B/op and allocs/op for folding
// M groups into ONE stage/freeze. A total-account A = m*50 + 10 is listed per
// sub-benchmark name; the codex ratio is 0 (api_key accounts → no identity
// pools to reuse/resize).
func BenchmarkInvalidateGroupsStage(b *testing.B) {
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
				s.InvalidateGroups(ids)
			}
			b.StopTimer()
			if got := ldr.groupCallsCopy(); len(got) != m {
				b.Fatalf("LoadGroupAccounts calls = %d, want %d (one per group in a single batch)", len(got), m)
			}
		})
	}
}

// --- T2: snapshot digest first-build / reuse / changed ---

func digestFixture() (domain.Account, *domain.Template, []int64) {
	tp := tpl(1, domain.FormatOpenAIChat, []string{"m-a", "m-b"})
	a := *acc(7, tp, 4)
	a.Ext = &domain.AccountExt{CodexAccountID: strPtr("acct-up")}
	return a, tp, []int64{3, 9}
}

// BenchmarkStaticDigestBuild measures the one-shot planKey derivation now done
// by the unique constructor newSnapshotStatic (with -benchmem for the new
// retained-key allocation).
func BenchmarkStaticDigestBuild(b *testing.B) {
	a, tp, gids := digestFixture()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkSnapshotStatic = newSnapshotStatic(a, tp, gids)
	}
}

// BenchmarkStaticDigestReuse measures planKeyOf on the cached key (should be
// allocation-free: the digest is derived once and held).
func BenchmarkStaticDigestReuse(b *testing.B) {
	a, tp, gids := digestFixture()
	av := newSnapshotStatic(a, tp, gids)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkPlanKey = planKeyOf(av)
	}
}

// BenchmarkStaticDigestChanged measures reconstructing a leaf whose inputs
// changed (key recomputed) — the cost paid only on a real static change.
func BenchmarkStaticDigestChanged(b *testing.B) {
	a, tp, gids := digestFixture()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		a.IdentityRevision = int64(i)
		benchmarkSnapshotStatic = newSnapshotStatic(a, tp, gids)
	}
}
