// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestQuotaQueueDropsWhenFull：队列满时 Enqueue 立即返回（非阻塞）且计 dropped；
// Stats 的 queued 反映当前积压。
func TestQuotaQueueDropsWhenFull(t *testing.T) {
	q := NewQuotaQueue(1)
	task := QuotaTask{AccountID: 1, IdentityRevision: 1, Fingerprint: "fp", Quality: "*", State: StateRetryAfter}

	q.Enqueue(task) // 占满容量 1
	enqueued, dropped, queued := q.Stats()
	require.Equal(t, int64(1), enqueued)
	require.Zero(t, dropped)
	require.Equal(t, 1, queued, "积压必须反映已入队未消费的一条")

	start := time.Now()
	q.Enqueue(task) // 满 → 立即丢弃
	elapsed := time.Since(start)
	require.Less(t, elapsed, 100*time.Millisecond, "队列满时 Enqueue 必须立即返回（实测 %v）", elapsed)

	enqueued, dropped, queued = q.Stats()
	require.Equal(t, int64(1), enqueued, "丢弃不得计入入队")
	require.Equal(t, int64(1), dropped, "满 ⇒ dropped+1")
	require.Equal(t, 1, queued, "丢弃不改变积压")
}

// TestQuotaQueueCloseLeavesNoResidue 关闭与并发投递的终态（N4 的确定性版本）：
// Close 与生产者并发跑，**Close 返回且生产者退出后** queued 必须归零，且
// enqueued + dropped == Enqueue 调用次数。
//
// 两条都是同一把锁给的结论（closed 判定与投递同锁 ⇒ 置位后不可能再有投递，
// 锁外排空看到的就是终态剩余），不需要测试注入点去钉时序，也不依赖调度概率。
// 红点：把 closed 判定移回锁外（旧实现）⇒ 已通过判定的投递在排空之后落进
// channel，queued != 0；把排空改成重复计数（旧实现的"收回"）⇒
// enqueued+dropped > 调用次数。
func TestQuotaQueueCloseLeavesNoResidue(t *testing.T) {
	const (
		producers   = 8
		perProducer = 4
	)
	attempts := int64(producers * perProducer)
	task := QuotaTask{AccountID: 1, IdentityRevision: 1, Fingerprint: "fp", Quality: "*", State: StateRetryAfter}
	// 容量给足：本轮考「关闭」，不是「满」（满丢弃见上一条用例）。
	q := NewQuotaQueue(producers * perProducer)

	var wg sync.WaitGroup
	for i := 0; i < producers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perProducer; j++ {
				q.Enqueue(task)
			}
		}()
	}
	// 与生产者并发关闭：关闭点由调度决定，先关闭与先投递两种交错都被覆盖。
	q.Close()
	wg.Wait()

	enqueued, dropped, queued := q.Stats()
	require.Zero(t, queued, "关闭返回后不得残留幽灵任务（判定+投递同锁 ⇒ 排空即终态）")
	require.Equal(t, attempts, enqueued+dropped, "守恒：enqueued + dropped == Enqueue 调用次数")

	// 关闭后的调用一律计 dropped 且不入队——守恒与归零同时对终态成立。
	q.Enqueue(task)
	enqueued2, dropped2, queued2 := q.Stats()
	require.Zero(t, queued2, "关闭后 Enqueue 不得入队")
	require.Equal(t, dropped+1, dropped2, "关闭后 Enqueue ⇒ dropped+1")
	require.Equal(t, attempts+1, enqueued2+dropped2, "守恒对关闭后的调用同样成立")
}

// TestQuotaQueueCloseTransfersResidue 确定性钉住「关闭排空把剩余从 enqueued 转入
// dropped」：先入队 K 条且不关闭，断言积压与计数；再 Close，断言积压归零、
// enqueued 归零、剩余全部落入 dropped，且 enqueued+dropped 守恒。
//
// 这是结构不变量（Close 里 enqueued.Add(-1) 与 dropped.Add(1) 的配对）的必然
// 证伪点——删除任一行动即红，且不依赖调度与并发交错。
func TestQuotaQueueCloseTransfersResidue(t *testing.T) {
	const k = 4
	q := NewQuotaQueue(8)
	task := QuotaTask{AccountID: 1, IdentityRevision: 1, Fingerprint: "fp", Quality: "*", State: StateRetryAfter}

	for i := 0; i < k; i++ {
		q.Enqueue(task)
	}
	enqueued, dropped, queued := q.Stats()
	require.Equal(t, int64(k), enqueued, "入队后 enqueued == K")
	require.Zero(t, dropped, "未关闭且未满，不应有丢弃")
	require.Equal(t, k, queued, "入队后积压 == K")

	q.Close()

	enqueued, dropped, queued = q.Stats()
	require.Zero(t, queued, "关闭排空后积压必须归零")
	require.Zero(t, enqueued, "关闭排空后 enqueued 必须归零（剩余全部转出）")
	require.Equal(t, int64(k), dropped, "关闭排空后 dropped == K（剩余全部转入）")
	require.Equal(t, int64(k), enqueued+dropped, "守恒：enqueued + dropped == Enqueue 调用次数")
}

// TestQuotaQueueEnqueueLockProtocol 用 go/parser 钉住 QuotaQueue 的锁协议结构。
//
// 「把 if q.closed 移到 q.mu.Lock() 之前」没有确定性行为探针：是否越过排空取决于
// Close 与生产者的先后，并发用例可能仍然通过，故这里退而用 AST 结构钉：
//  1. Enqueue：q.mu.Lock() 先于任何 q.closed 读取；解锁（defer 或 select 之后的
//     直接调用）覆盖投递 select；
//  2. Close：q.closed = true 的赋值位于 q.mu.Lock() 与 q.mu.Unlock() 之间（置位在锁内）。
//
// 解析失败或任一锚点缺失一律 t.Fatalf——绝不静默通过。
func TestQuotaQueueEnqueueLockProtocol(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "quota_queue.go", nil, 0)
	require.NoError(t, err, "必须能解析同包的 quota_queue.go")

	enqueue := findFuncDecl(t, file, "Enqueue")
	closeFn := findFuncDecl(t, file, "Close")

	// --- Enqueue ---
	lockPos, ok := findMethodCall(enqueue, "q", "Lock", false)
	require.True(t, ok, "Enqueue 必须调用 q.mu.Lock()")

	readPos, ok := firstFieldSelector(enqueue, "q", "closed")
	require.True(t, ok, "Enqueue 必须读取 q.closed")
	require.Less(t, int64(lockPos), int64(readPos),
		"q.closed 读取必须在 q.mu.Lock() 之后（把判定移出锁 ⇒ 本用例红）")

	selectPos, ok := firstSelect(enqueue)
	require.True(t, ok, "Enqueue 必须包含投递 select")

	if deferUnlock, ok := findMethodCall(enqueue, "q", "Unlock", true); ok {
		// defer 语句位于 closed 判定之前，但解锁在函数返回时执行（必然覆盖 select 与
		// closed 判定），故这里只校验它紧随 Lock 之后。
		require.Greater(t, int64(deferUnlock), int64(lockPos),
			"defer q.mu.Unlock() 必须在 q.mu.Lock() 之后")
	} else {
		directUnlock, ok := findMethodCall(enqueue, "q", "Unlock", false)
		require.True(t, ok, "Enqueue 必须在 defer 或 select 之后执行 q.mu.Unlock()")
		require.Greater(t, int64(directUnlock), int64(selectPos),
			"q.mu.Unlock() 必须在投递 select 之后")
	}

	// --- Close ---
	lockPos, ok = findMethodCall(closeFn, "q", "Lock", false)
	require.True(t, ok, "Close 必须调用 q.mu.Lock()")
	unlockPos, ok := findMethodCall(closeFn, "q", "Unlock", false)
	require.True(t, ok, "Close 必须调用 q.mu.Unlock()")
	assignPos, ok := findFieldAssign(closeFn, "q", "closed")
	require.True(t, ok, "Close 必须赋值 q.closed")
	require.Less(t, int64(lockPos), int64(assignPos),
		"q.closed = true 必须在 q.mu.Lock() 之后（移出锁 ⇒ 本用例红）")
	require.Less(t, int64(assignPos), int64(unlockPos),
		"q.closed = true 必须在 q.mu.Unlock() 之前（移出锁 ⇒ 本用例红）")
}

// findFuncDecl 返回顶层函数声明；找不到即 Fatalf（锚点缺失不许静默通过）。
func findFuncDecl(t *testing.T, file *ast.File, name string) *ast.FuncDecl {
	t.Helper()
	for _, decl := range file.Decls {
		if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name {
			return fn
		}
	}
	t.Fatalf("quota_queue.go 中找不到函数 %s", name)
	return nil
}

// methodCallPos 判断 ce 是否是 recv.mu.<method>() 形态的调用。
func methodCallPos(ce *ast.CallExpr, recv, method string) (token.Pos, bool) {
	sel, ok := ce.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != method {
		return 0, false
	}
	inner, ok := sel.X.(*ast.SelectorExpr)
	if !ok || inner.Sel.Name != "mu" {
		return 0, false
	}
	id, ok := inner.X.(*ast.Ident)
	if !ok || id.Name != recv {
		return 0, false
	}
	return ce.Pos(), true
}

// findMethodCall 返回 recv.mu.<method>() 调用的位置：wantDefer 选 defer 形态，
// 否则选直接调用形态。同一文件中 token.Pos 单调，可直接比较先后。
func findMethodCall(fn *ast.FuncDecl, recv, method string, wantDefer bool) (token.Pos, bool) {
	var direct, deferred []token.Pos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if ds, ok := n.(*ast.DeferStmt); ok {
			if p, ok := methodCallPos(ds.Call, recv, method); ok {
				deferred = append(deferred, p)
			}
			return false // 已记录，别把同一调用重复计入 direct
		}
		if ce, ok := n.(*ast.CallExpr); ok {
			if p, ok := methodCallPos(ce, recv, method); ok {
				direct = append(direct, p)
			}
		}
		return true
	})
	ps := direct
	if wantDefer {
		ps = deferred
	}
	if len(ps) == 0 {
		return 0, false
	}
	first := ps[0]
	for _, p := range ps[1:] {
		if p < first {
			first = p
		}
	}
	return first, true
}

// firstFieldSelector 返回 fn 体内第一个 recv.field 选择子的位置。
func firstFieldSelector(fn *ast.FuncDecl, recv, field string) (token.Pos, bool) {
	var first token.Pos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != field {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); !ok || id.Name != recv {
			return true
		}
		if first == 0 || sel.Pos() < first {
			first = sel.Pos()
		}
		return true
	})
	return first, first != 0
}

// firstSelect 返回 fn 体内第一个 select 语句的位置。
func firstSelect(fn *ast.FuncDecl) (token.Pos, bool) {
	var first token.Pos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if ss, ok := n.(*ast.SelectStmt); ok {
			if first == 0 || ss.Pos() < first {
				first = ss.Pos()
			}
		}
		return true
	})
	return first, first != 0
}

// findFieldAssign 返回 fn 体内第一个 recv.field = ... 赋值语句的位置。
func findFieldAssign(fn *ast.FuncDecl, recv, field string) (token.Pos, bool) {
	var first token.Pos
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for _, lhs := range as.Lhs {
			sel, ok := lhs.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != field {
				continue
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == recv {
				if first == 0 || as.Pos() < first {
					first = as.Pos()
				}
			}
		}
		return true
	})
	return first, first != 0
}
