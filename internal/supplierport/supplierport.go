// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package supplierport 供应商记账链 / 解冻链 / 视图链的**存储契约与行值**。
//
// 为什么单独一个包：这些接口由**消费者**（worker 编排）声明、由**实现者**
// （`internal/repository`）实现——这是正确的依赖倒置方向。但接口定义原先住在
// `internal/supplier`（同时装着 worker 编排、logx、worker 生命周期），
// 于是实现者被迫依赖整个编排包。把"契约 + 行值"抽到中立叶子后：
//
//	internal/supplier        → 只依赖本包（编排 worker）
//	internal/repository      → 只依赖本包（实现接口）
//
// 双方不再互相认识。与 `internal/loadport`（P5-1 同样的做法）同范式。
//
// ⚠ 本包零内部依赖、零逻辑：**只放 interface 与纯数据**。不要在这里加 SQL、
// 重试策略、时钟读取或校验——那些属于实现方或编排方。
package supplierport

import (
	"context"

	"github.com/is7qin/c3api/internal/domain"
)

// CreditStore 记账链持久面（repository 实现；§5.1/§5.2）。
type CreditStore interface {
	// FetchCreditBatch 取批（消费索引①；`ORDER BY id LIMIT`，双谓词
	// `NOT supplier_credited AND supplier_earn_millis > 0`）。返回行携带
	// uid/cost/earn/created_at（§5.1 ①）。
	FetchCreditBatch(ctx context.Context, limit int) ([]domain.BatchRow, error)
	// FreezeHoursByUID 各 uid 生效冻结小时（全局开关优先 + NULL 注入默认，§2.4）。
	FreezeHoursByUID(ctx context.Context, uids []int64) (map[int64]int, error)
	// ApplyCreditTx 单事务（阶段 A chunks → B 按 uid 升序预锁 balances → C 建行/
	// 累加 → D 标记 usage_logs credited）：余额覆盖守卫 + 标记计数守卫 + 金额守恒；
	// 任一失败整事务回滚。封账按源日 upsert（§3.10）。
	ApplyCreditTx(ctx context.Context, batch []domain.BatchRow, freezeByUID map[int64]int) error
	// ReleaseFrozenNoWait freeze_enabled=false 的存量释放（忽略 available_at，
	// 保留 LIMIT 每周期一批）——最终收敛语义（§5.4）。
	ReleaseFrozenNoWait(ctx context.Context, limit int) (int, error)
	// CreditLagHead 头行快速 lag（游标头 id 定位，低成本；高频）。
	CreditLagHead(ctx context.Context) (int64, error)
	// CreditLagFull 完整 backlog COUNT + SUM（O(backlog) 成本；降频）。
	CreditLagFull(ctx context.Context) (rows int64, sumEarn int64, err error)
}

// CreditLocker 记账链串行化（同一时刻只允许一个实例推进游标）。
type CreditLocker interface {
	AcquireSupplierLock(ctx context.Context) (release func(), ok bool, err error)
}

// ThawStore 解冻链持久面（repository 实现：四步独立顺序 SQL + 单连接契约 + 提交前
// 覆盖守卫；§5.3）。ThawDueChunks 返回本批删除的桶行数。
type ThawStore interface {
	ThawDueChunks(ctx context.Context, limit int) (int, error)
	// ThawBucketStats 冻结桶物理观测（§7.2/G7）：bucket_rows（总行数）、
	// bucket_lag（最老未解冻桶 available_at 距今秒数；全部未来/空 = 0）、overdue
	// 行数/金额。物理行数在停摆/多代可超数学上界，故容量以观测保证。
	ThawBucketStats(ctx context.Context) (domain.ThawBucketSnapshot, error)
}

// ViewStore 供应商视图装载持久面（repository 实现：accounts 归属 + supplier_balances
// 分成率，同一一致性视图；§4.3）。
type ViewStore interface {
	LoadSupplierView(ctx context.Context) (owner map[int64]int64, share map[int64]int, err error)
}
