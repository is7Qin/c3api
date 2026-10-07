// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package supplier

import (
	"errors"

	"github.com/jackc/pgx/v5/pgconn"
)

// pgCodeDeadlock/pgCodeLockNotAvailable 死锁与锁不可用（§5.7）：三阶段锁序大幅
// 降低死锁，但在集合 SQL + 多实例下不构成无环的形式证明 ⇒ 显式接受这两种整事务
// 回滚 + 公平有界重试。
const (
	pgCodeDeadlock         = "40P01"
	pgCodeLockNotAvailable = "55P03"
)

// IsRetryableTxErr 报告错误是否为可重试的事务级冲突（死锁/锁不可用）。
func IsRetryableTxErr(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == pgCodeDeadlock || pgErr.Code == pgCodeLockNotAvailable
	}
	return false
}
