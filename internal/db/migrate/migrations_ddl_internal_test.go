// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package migrate

// 0003_attempt_logs 的「迁移文本 == 运行时事实源」锚。
//
// 为什么需要它：attempt_logs 是**分区父表**，被 migrateHookExcludesPartitioned
// 从 ent 生成的迁移里排除，而生成器（internal/db/migrate/gen）只认 0001 那三个
// 文件名——`gen -check` 对 0003 完全无感。也就是说：「改了运行时列定义忘了改
// 迁移」与「改了迁移忘了改运行时」这两类漂移，**没有任何生成器会报**。本文件就是
// 那个唯一的守卫，所以比较必须是逐字的（改一个列名的一个字符就必须红）。

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/repository"
)

// attemptLogMigrationVersion 0003 的版本号（错误信息里点名，便于定位）。
const attemptLogMigrationVersion int64 = 3

// attemptLogOrdinalAlter 0003 对 err_logs 的唯一改动（逐字：类型与可空性都算）。
//
// err_logs 的「当前形状」= 0001 + 这条 ALTER：`errLogColumnDefs` 是 0001 的
// 生成器输入，**不得**把它加进去（那会让已冻结的 0001 漂移、账本 checksum 对
// 不上）。可空是语义的一部分：循环不可见的 err_logs 行（本地拒绝 / 已交付失败）
// 没有序号，强制造值就是编数据——所以这里钉 NULL，不是 NOT NULL。
const attemptLogOrdinalAlter = "ALTER TABLE err_logs ADD COLUMN attempt_ordinal smallint NULL"

// attemptLogUpSQL 取内嵌的 0003 up 文本（缺失即失败：文件被删/改名）。
func attemptLogUpSQL(t *testing.T) string {
	t.Helper()
	migrations, err := loadMigrations(migrationFS)
	require.NoError(t, err)
	for _, m := range migrations {
		if m.Version == attemptLogMigrationVersion {
			require.NotEmpty(t, m.UpSQL, "0003 的 up 文件不得为空")
			return m.UpSQL
		}
	}
	require.FailNow(t, "0003_attempt_logs.up.sql is missing from the embedded migrations")
	return ""
}

// TestAttemptLogMigrationMatchesRuntimeDDL 钉住 0003 的建表文本、索引文本与运行时
// 事实源逐字一致，且分区/序列绑定齐备。
//
// 比较前只抹掉 schema 限定符 `public.`：迁移文件全量限定（与 0001 同风格，防
// search_path 被改后建到别的 schema），而事实源（partitionedCreateDDL /
// attemptLogIndexDDLs）不带限定。这是**唯一**允许的差异——抹掉之后仍是逐字比较。
func TestAttemptLogMigrationMatchesRuntimeDDL(t *testing.T) {
	up := strings.ReplaceAll(attemptLogUpSQL(t), "public.", "")
	table := repository.MigrationAttemptLogDDLs()

	require.Equal(t, "attempt_logs", table.Name)
	require.True(t, table.Partitioned, "attempt_logs 是分区父表")
	require.Equal(t, "attempt_logs_id_seq", table.Sequence, "id 走专用序列（与 ensured 分区表同款）")

	require.Contains(t, up, table.Create, "建表文本必须与运行时事实源逐字一致（列名/类型/默认值/主键/分区键）")
	require.Len(t, table.Indexes, 2, "只允许 2 条索引：裸 (created_at) 已被评审砍掉（见 0003 文件头）")
	for _, idx := range table.Indexes {
		require.Contains(t, up, idx, "索引文本必须与运行时事实源逐字一致")
	}

	// 运行时建表走 IF NOT EXISTS / execDDLTolerateRace 形态，拼不出这两个语句，
	// 而它们决定分区与序列的生命周期 ⇒ 在迁移文本上单独钉。
	require.Contains(t, up, "CREATE SEQUENCE attempt_logs_id_seq")
	require.Contains(t, up, "ALTER SEQUENCE attempt_logs_id_seq OWNED BY attempt_logs.id",
		"序列必须 OWNED BY 表列：分区父表 DROP 时序列要跟着走")
	require.Contains(t, up, ") PARTITION BY RANGE (created_at)",
		"分区键 created_at 是保留清理 O(1) 的前提")

	// 反向守卫：被砍掉的第三条索引不得以任何形式回来。
	require.NotContains(t, strings.Join(table.Indexes, "\n"), "ON attempt_logs (created_at)",
		"裸 (created_at) 索引只增写放大、没有消费者")
}

// TestAttemptLogMigrationAddsErrLogOrdinal 钉住 0003 对 err_logs 的唯一改动：
// 逐字 ALTER 文本（列名 + smallint + NULL）。
//
// 「err_logs 的当前形状」是 0001 ∪ 0003 两处文本的并集，而不是某个 Go 常量——
// 常量若跟着加列，0001 的 checksum 就会漂移。列真的存在且可空由 PG 用例
// （attemptlog_repo_pg_test）钉。
func TestAttemptLogMigrationAddsErrLogOrdinal(t *testing.T) {
	up := strings.ReplaceAll(attemptLogUpSQL(t), "public.", "")
	require.Contains(t, up, attemptLogOrdinalAlter,
		"err_logs.attempt_ordinal 的类型/可空性属于契约：smallint NULL（循环不可见的行没有序号）")
	// 0003 只加这一列：不得顺手给 err_logs 造索引或改别的列（那是另一个迁移）。
	require.NotContains(t, up, "ON err_logs", "0003 不给 err_logs 加索引")
}
