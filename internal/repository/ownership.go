// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

// ownership.go 账号归属（supplier_user_id）写入的值域校验与行锁串行化
// （spec 2026-10-09 §2.5）。
//
//   - 目标用户必须**能访问供应商面**（role ∈ 可达集 且 status = active），否则
//     400（ErrInvalidInput）——判据引用门控同一可达集（domain.CanAccessSupplierSurface），
//     不抄一遍角色名。
//   - 「归属写入」与「禁用该用户」两条路径在 DB 内**锁同一 users 行**后再验证
//     status/role，消灭 TOCTOU（否则「先读 active ⇒ 禁用提交 ⇒ 再写归属」会产生
//     disabled 供应商的账号）。锁在账号写事务内进行，故与账号变更同事务提交/回滚。

import (
	"context"
	"database/sql"
	"fmt"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/is7qin/c3api/internal/domain"
)

// lockUserRow 锁定目标 users 行并回读 role/status（FOR UPDATE；须在事务驱动上调用）。
// 缺行 ⇒ sql.ErrNoRows 语义错误，由调用方归类为 ErrInvalidInput。
func lockUserRow(ctx context.Context, driver dialect.Driver, uid int64) (domain.Role, domain.UserStatus, error) {
	const q = `SELECT role, status FROM users WHERE id = $1 FOR UPDATE`
	rows := &entsql.Rows{}
	if err := driver.Query(ctx, q, []any{uid}, rows); err != nil {
		return "", "", err
	}
	defer rows.Close() // nolint:errcheck // Rows.Err reports iteration failures.
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", "", err
		}
		return "", "", sql.ErrNoRows
	}
	var role, status string
	if err := rows.Scan(&role, &status); err != nil {
		return "", "", err
	}
	return domain.Role(role), domain.UserStatus(status), nil
}

// validateOwnershipTarget 锁定目标 users 行并校验其为「供应商面可达且 active」的
// 用户（§2.5 能力谓词）。任一不满足（缺行 / 角色不可达 / 非 active）⇒
// ErrInvalidInput（service 映射 400）。
func validateOwnershipTarget(ctx context.Context, driver dialect.Driver, uid int64) error {
	if uid <= 0 {
		return fmt.Errorf("%w: supplier_user_id must be positive", ErrInvalidInput)
	}
	role, status, err := lockUserRow(ctx, driver, uid)
	if err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("%w: supplier_user_id %d not found", ErrInvalidInput, uid)
		}
		return err
	}
	if !domain.CanAccessSupplierSurface(role) || status != domain.UserStatusActive {
		return fmt.Errorf("%w: supplier_user_id %d must be an active supplier-surface user (role=%s status=%s)", ErrInvalidInput, uid, role, status)
	}
	return nil
}
