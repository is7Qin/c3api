// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/is7qin/c3api/internal/domain"
)

var (
	benchmarkSnapshotStatic *snapshotStatic
	benchmarkPlanKey        planKey
	benchmarkStaticKey      staticKey
	benchmarkPayloadKey     payloadKey
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
	for i := range shared {
		sharedAccs = append(sharedAccs, acc(int64(1_000_000+i), tp, 4))
	}
	byGroup := make(map[int64][]*domain.Account, m)
	for gid := 1; gid <= m; gid++ {
		accs := append([]*domain.Account(nil), sharedAccs...)
		for j := range perGroup {
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
	a.Ext = &domain.AccountExt{CodexAccountID: new("acct-up")}
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
		benchmarkSnapshotStatic = newSnapshotStatic(a, tp, gids, directEgress(), nil)
	}
}

// BenchmarkStaticDigestReuse measures planKeyOf on the cached key — **这是
// `planKeyOf` 的缓存读回，不是叶子复用判据的比较路径**。叶子复用判据是
// `staticKeyOf(old) == staticKeyOf(new)`（含载荷摘要与出口摘要重算，见
// BenchmarkStaticKeyOf）；拿本基准当"比较路径现状值"会漏掉两枚子键的装配成本。
func BenchmarkStaticDigestReuse(b *testing.B) {
	a, tp, gids := digestFixture()
	av := newSnapshotStatic(a, tp, gids, directEgress(), nil)
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
		benchmarkSnapshotStatic = newSnapshotStatic(a, tp, gids, directEgress(), nil)
	}
}

// --- T3: payload digest (P3a) ---
//
// 载荷面（OAuth 令牌三元组）在 P3a 里从「三枚明文字段、比较期逐次读」变成「一枚
// 构造期缓存的 [32]byte 摘要」。这里钉的是**成本形状**，两枚判据都不能省（数值判据
// 在 payload_digest_test.go 的 H7/H8 里，本文件只负责打印两列供 benchstat 对照）：
//
//   - 构造期：真实 codex 形状（token 2048B / refresh 256B）下 allocs/op 与 B/op
//     必须与空载荷夹具**逐项相同**——摘要走 4096 的栈上缓冲（零堆分配），缓存位点
//     落进 snapshotStatic 既有的 768 分配类（不抬 B/op）。旧夹具 OAuth 全空，测不到
//     真实成本，这正是"补一个真实形状变体"的原因。
//   - 比较期：staticKeyOf 是叶子复用判据的唯一路径（改前 261–266 ns/op、1 alloc/32B，
//     改后应基本不动——载荷从"逐次读三枚明文字段"退化成"读回一枚 [32]byte"）。

// codexOAuthTokenBytes / codexOAuthRefreshBytes 是真实 codex 载荷的尺寸：访问令牌是
// 一枚 2KB 级 JWT，刷新令牌是 256B 级不透明串。
const (
	codexOAuthTokenBytes   = 2048
	codexOAuthRefreshBytes = 256
)

// codexOAuthTokenShape 造一枚**长度恰为 n** 的访问令牌。JWT 是三段点分 base64url；
// 只有长度与前缀宽度进编码（seg 只吃 len + 内容），字符集不改变成本，故用点分形状
// 表达"这不是普通短串"。
func codexOAuthTokenShape(n int) string {
	const header = "eyJhbGciOiJSUzI1NiIsImtpZCI6ImMzeCJ9" // 36B 的 JWT header
	if n <= len(header)+1 {
		return strings.Repeat("a", max(n, 0))
	}
	return header + "." + strings.Repeat("A", n-len(header)-1)
}

// codexOAuthRefreshShape 造一枚长度恰为 n 的刷新令牌（不透明 base64url 串，无点分）。
func codexOAuthRefreshShape(n int) string {
	const prefix = "rt_"
	if n <= len(prefix) {
		return strings.Repeat("r", max(n, 0))
	}
	return prefix + strings.Repeat("R", n-len(prefix))
}

// digestCodexOAuthFixture 是 digestFixture 的**真实载荷形状**变体：token 2048B、
// refresh 256B、expires 固定 ⇒ 载荷前像 24 + 2048 + 256 + 8 = 2336B（落在 4096 的
// 栈上缓冲内 ⇒ 摘要零堆分配）。
func digestCodexOAuthFixture() (domain.Account, *domain.Template, []int64) {
	a, tp, gids := digestFixture()
	a.Ext.CodexOAuthToken = new(codexOAuthTokenShape(codexOAuthTokenBytes))
	a.Ext.CodexOAuthRefreshToken = new(codexOAuthRefreshShape(codexOAuthRefreshBytes))
	exp := time.Unix(0, payloadFixtureExpires)
	a.Ext.CodexOAuthExpiresAt = &exp
	return a, tp, gids
}

// BenchmarkStaticDigestBuildCodexOAuthShape 用**真实 codex 载荷形状**测构造期成本
// （H7：allocs/op == 10、B/op == 920，与空载荷夹具逐项相同）。
//
// 分配数在计时之外断言（AllocsPerRun，快且精确）；B/op 的那一半由
// TestStaticDigestBuildCodexOAuthShapeCost 读 harness 自己的第二列钉住——基准里读不到
// 自己那一列，硬造一个也不比那条用例更可信。
func BenchmarkStaticDigestBuildCodexOAuthShape(b *testing.B) {
	a, tp, gids := digestCodexOAuthFixture()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkSnapshotStatic = newSnapshotStatic(a, tp, gids, directEgress(), nil)
	}
	b.StopTimer()
	if allocs := testing.AllocsPerRun(500, func() {
		benchmarkSnapshotStatic = newSnapshotStatic(a, tp, gids, directEgress(), nil)
	}); allocs != 10 {
		b.Fatalf("真实载荷形状必须与改前同为 10 allocs/op（实测 %v）：摘要得留在 4096 的栈上缓冲内、缓存位点不得抬分配类", allocs)
	}
}

// BenchmarkStaticKeyOf 是**叶子复用判据**的成本（H7：allocs/op == 1、B/op == 32）：
// `staticKeyOf(old) == staticKeyOf(new)` 每次 reload 对每个账号跑两遍。
//
// 分配数在计时之外断言；B/op 那一半由 TestStaticKeyOfCost 钉住（同上）。
func BenchmarkStaticKeyOf(b *testing.B) {
	a, tp, gids := digestCodexOAuthFixture()
	av := newSnapshotStatic(a, tp, gids, directEgress(), nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkStaticKey = staticKeyOf(av)
	}
	b.StopTimer()
	if allocs := testing.AllocsPerRun(500, func() { benchmarkStaticKey = staticKeyOf(av) }); allocs != 1 {
		b.Fatalf("staticKeyOf 必须 1 alloc/op（实测 %v）：比较期不得再派生任何东西", allocs)
	}
}

// BenchmarkPayloadKeyOf 钉住"比较期不再碰凭据原文"的成本：改前逐次读三枚明文字段
// 并解引用（实测 16.1 ns/op），改后只剩一次 32B 读回（1.8 ns/op、0 alloc）。
func BenchmarkPayloadKeyOf(b *testing.B) {
	a, tp, gids := digestCodexOAuthFixture()
	av := newSnapshotStatic(a, tp, gids, directEgress(), nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkPayloadKey = payloadKeyOf(av)
	}
	b.StopTimer()
	if allocs := testing.AllocsPerRun(500, func() { benchmarkPayloadKey = payloadKeyOf(av) }); allocs != 0 {
		b.Fatalf("payloadKeyOf 必须 0 alloc/op（实测 %v）：它只是读回一枚 [32]byte", allocs)
	}
}
