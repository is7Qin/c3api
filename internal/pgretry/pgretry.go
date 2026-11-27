// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package pgretry 报告 PostgreSQL 事务级可重试错误（死锁 / 锁不可用）。
//
// 为什么单独一个包：这是**基础设施事实**，不是领域规则。判据完全由 SQLSTATE
// 决定（40P01 / 55P03），与业务语义无关；放在领域或规则包里会让"纯领域"包
// 反向依赖 pgx 驱动。任何需要在事务冲突上重试的调用方都可依赖本包，
// 而本包只依赖 pgconn。
//
// 本包零内部依赖：不 import 仓库内任何包（领域包不得知道 pgx，这是同一条原则的另一面）。
package pgretry

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// pgCodeDeadlock/pgCodeLockNotAvailable 死锁与锁不可用：三阶段锁序大幅降低死锁，
// 但在集合 SQL + 多实例下不构成无环的形式证明 ⇒ 显式接受这两种整事务回滚 +
// 公平有界重试。
const (
	pgCodeDeadlock         = "40P01"
	pgCodeLockNotAvailable = "55P03"
)

// IsRetryableTxErr 报告错误是否为可重试的事务级冲突（死锁 / 锁不可用）。
//
// 只认 pgconn.PgError 的 SQLSTATE；nil 与其它错误一律 false（不吞错、不误重试）。
func IsRetryableTxErr(err error) bool {
	if err == nil {
		return false
	}
	if pgErr, ok := errors.AsType[*pgconn.PgError](err); ok {
		return pgErr.Code == pgCodeDeadlock || pgErr.Code == pgCodeLockNotAvailable
	}
	return false
}
