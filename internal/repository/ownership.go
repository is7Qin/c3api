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

// lockOwnershipUsers 按 **users → accounts** 协议先锁归属相关 users 行（§5.7
// 固定锁序：与「禁用用户 → 连带停账号」（user_repo.UpdateUser 先在 users 上加行
// 锁、再 UPDATE accounts）同序），升序 uid 去重后逐个 FOR UPDATE。
//
// 为什么必须在锁账号之前：反序（先 accounts 后 users）会与禁用路径构成 ABBA 死锁
// 面；且「先读 active ⇒ 禁用提交 ⇒ 再写归属」的 TOCTOU 也只能由「同事务内先锁
// users 行」消灭（禁用方拿不到该行 ⇒ 两边串行化）。
//
// 参数 uids 是本次写入**可能触碰到的全部归属**（当前归属 ∪ 目标归属，见
// UpdateAccountsBatch）。uid<=0（平台自有）不锁——它不是任何 users 行。
//
// 本函数**只加锁**：行不存在也不报错（当前归属失效不该阻塞该账号的其它写入——
// 「禁用供应商后仍可把账号转出/改配置」是合法操作）。能力判定（供应商面可达 +
// active）由调用方按**最终归属**单独复核（validateOwnershipTarget）。
func lockOwnershipUsers(ctx context.Context, driver dialect.Driver, uids []int64) error {
	positive := make([]int64, 0, len(uids))
	for _, uid := range uids {
		if uid > 0 {
			positive = append(positive, uid)
		}
	}
	for _, uid := range sortedUniqueIDs(positive) {
		// lockUserRow 在单条 SELECT ... FOR UPDATE 里完成加锁；缺行不报错。
		if _, _, err := lockUserRow(ctx, driver, uid); err != nil {
			if err == sql.ErrNoRows {
				continue // 当前归属已失效：加锁已是既成事实，能力判定由调用方按最终归属做
			}
			return fmt.Errorf("lock supplier_user_id %d: %w", uid, err)
		}
	}
	return nil
}
