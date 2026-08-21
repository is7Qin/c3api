// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// EnsureNoLegacyTransmitColumn 启动期哨兵：探测 rules 表是否仍携带旧 transmit 列
// （ent schema 已在指针化重构中移除该列）。若存在 → fail-fast，要求 fresh setup
// （项目哲学：无迁移路径）。查询走 information_schema.columns 参数化-free 的字面量
// 条件（table_name='rules' AND column_name='transmit'），当表尚未创建时（全新库、
// migrate 前）仅返回 0 行 → 通过，不对缺表报错。
//
// Placement: internal/repository（与分区 bootstrap 等启动期 DDL 探测同层），调用点
// 在 cmd/server/main.go 分区 bootstrap 附近（单次启动执行，热路径零触及）。
func EnsureNoLegacyTransmitColumn(ctx context.Context, pool *pgxpool.Pool) error {
	var n int64
	// 参数-free 字面量查询（spec 约束）：仅查当前库 information_schema，不触业务表
	// 本身，表不存在时返回 0 不报错。
	if err := pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM information_schema.columns WHERE table_name='rules' AND column_name='transmit'`,
	).Scan(&n); err != nil {
		return fmt.Errorf("check legacy transmit column: %w", err)
	}
	if n > 0 {
		return fmt.Errorf(
			`legacy column "transmit" still exists on table "rules": database predates rule-engine pointer redesign (ResponseCode/CustomMessage pointer semantics); fresh setup required (no migration path) — please provision a new database and re-import configuration`,
		)
	}
	return nil
}
