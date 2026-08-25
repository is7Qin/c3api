// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

// batch_controller.go 结算批规模自适应控制（safe_batch = 时间预算 / 实测每行成本）：
// 固定大批次的生产事故——50000 行/批在千万行脏可见性地图上单语句 >settleTimeout(10s)
// → 毒行梯子重试恒超时 → 该车道永久停摆。控制器以实测语句时长反馈调节批规模：
// 快（d < 预算/3，健康余量 ≥3 倍）倍增逼近吞吐上限、慢减半退避、超时立即减半、
// 他错保持（错误归因不明时不盲调）。稳态落在预算边界附近：单批时长 ≈ budget/3
// ≈ 2.7s（对比 8000 定批的 1.3-2.6s）。硬上界澄清：单语句由 repo settleTimeout
// (10s) 兜底而非本预算——控制器是事后反应者，首个超预算语句仍会跑满到超时；
// 且 consumeBatch 含双车道顺序执行，最坏一轮超出 ≈ 2×settleTimeout+sweep。
// 真正的收益在自愈性：数据变慢时自动折半逼近安全批，而非固定值静默滑向停摆
// ——与 drainCycleBudget 的「预算到期收尾、剩余积压下周期续排」语义兼容（drain.go）。

import (
	"context"
	"errors"
	"sync"
	"time"
)

const (
	// settleTimeBudget 批控时间预算 = 0.8 × repository.settleTimeout（10s，
	// internal/repository/billing_repo.go:30）。跨包耦合刻意不导出——复制常量
	// 并以此注释锚定；repo 端调整超时须同步复核本值。
	settleTimeBudget = 8 * time.Second
	// maxBatchLimit 倍增封顶（8000 种子的 8 倍上限——防病态快路径无界膨胀）。
	maxBatchLimit = 64000
	// minBatchLimit 减半托底（保底吞吐下限——防反复减半归零饿死游标）。
	minBatchLimit = 500
)

// batchController 结算批规模控制器（Flusher 私有态）：cur 当前批规模，mu 保护
// ——settleLaneParallel 的 K 个桶 goroutine 并发 limit()/observe()。
type batchController struct {
	mu  sync.Mutex
	cur int
}

// newBatchController 构造：种子取 settleBatchLimit（F2-opt W2 实测安全值）。
// 指针返回——内含互斥量不可拷贝。
func newBatchController() *batchController {
	return &batchController{cur: settleBatchLimit}
}

// limit 当前批规模。
func (c *batchController) limit() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur
}

// observe 反馈一次结算观测（d = 单车道单桶整次 settle 调用时长，err = 其错误）：
// 超时立即减半（errors.Is 全链匹配包装）；成功按时长二分逼近 budget/3 边界；
// 其他错误保持现状。调用方无需持锁。
func (c *batchController) observe(d time.Duration, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		c.cur /= 2 // 超时即减半，与时长无关（重试恒超时 = 停摆前兆）
	case err == nil && d < settleTimeBudget/3:
		c.cur *= 2
	case err == nil:
		c.cur /= 2
	}
	if c.cur < minBatchLimit {
		c.cur = minBatchLimit
	} else if c.cur > maxBatchLimit {
		c.cur = maxBatchLimit
	}
}
