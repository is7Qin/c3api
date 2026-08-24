// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// billing_repo.go 计费仓库载体（F2-opt v2 三车道拓扑，spec-f2opt-settlement）：
// legacy 逐组扣减面（DeductOnlyAndMark/deductOnlyCore/deductTx 接口族/chunk 合并
// 事务）已整体退役（D8）——扣减与标记由结算语句一体完成（每窗口一次往返），见
// billing_settle.go（SettleBalanceBatch/SettleFefoBatch）。游标取批/纯标记/lag/
// 会话锁面见 billing_cursor.go。usage_logs 明细的唯一写者是 usage flusher
// （InsertBatch）；本包只做标记/消费，不插日志。

import (
	"time"

	"entgo.io/ent/dialect"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/is7qin/c3api/internal/ent"
)

// settleTimeout 结算事务 per-query 超时（F-P2-4 降级形态）：会话级
// statement_timeout=10s 与 admin 面 ScanStats 大窗口聚合实测冲突（720 万行/30
// 天 → 57014，见 f1-impl-report.md 副作用核实）→ 按 spec 授权降级为计费路径
// per-query 超时：结算事务整体 10s 上限（执行时长 + 锁等待双有界；锁等待另有
// 池级 lock_timeout=5s 会话 GUC 先行兜底——SELECT 不取行锁不受其影响；含并发
// 标记重放一次的预算）。超时 → 事务取消回滚 → 行保持 unbilled，游标下周期
// 重放（不丢不重）。
const settleTimeout = 10 * time.Second

// BillingRepo 计费仓库：三车道结算语句载体（SettleBalanceBatch/SettleFefoBatch，
// 全毫分直接扣减——1 USD = 100,000 毫分，零换算零取整误差）。
//
// 双事务载体（形态沿袭热点修复 A，载体差异收敛到 settleTx 双适配）：
//   - pool != nil（生产 NewWithPG 装配）→ pgx 直连事务
//   - pool == nil（mock/无池装配、WithTx 事务内）→ ent txDriver 事务回落
//
// 两载体共用 runSettleStmt 单一编排（防漂移）。
type BillingRepo struct {
	client *ent.Client
	// driver 为 raw SQL（结算语句）用：与 txDriver 组合保证 raw SQL 与 ent
	// 构建器同事务连接（WithTx 同构，评审 I-1）。
	driver dialect.Driver
	// pool 为 pgx 连接池（NewWithPG 注入；New 构造的仓库为 nil）：非 nil →
	// 结算走 pgx 直连事务；nil → ent 事务路径回落。WithTx 的 tx 版仓库恒传
	// nil（事务内不挂池，见 repository.go WithTx）。
	pool *pgxpool.Pool
}
