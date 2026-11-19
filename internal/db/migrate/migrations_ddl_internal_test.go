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

	// 「不多不少」也在本文件的职责里：上面全是**包含**式断言（Contains），只能发现
	// "事实源要的东西迁移里没有"；下面这组反向计数发现"迁移里多出来东西"——多一条
	// CREATE TABLE / 多一条索引 / 悄悄加个 DROP，历史锚都会全绿。
	require.Equal(t, 1, strings.Count(up, "CREATE TABLE attempt_logs ("),
		"0003 只能建一次 attempt_logs（重复建表 = 迁移在第二个库里就崩）")
	require.Equal(t, 2, strings.Count(up, "CREATE INDEX attemptlog_"),
		"0003 只能有 2 条 attempt_logs 索引（第三条已被评审砍掉）")
	require.NotContains(t, up, "ON attempt_logs (created_at)",
		"裸 (created_at) 索引不得以任何形式回来")
	require.NotContains(t, up, "DROP",
		"up 迁移只允许加对象：出现 DROP 意味着一次向前迁移在删数据")
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

// TestErrLogAttemptOrdinalDDLMatchesMigration 自举路径（EnsureErrLogPartitioned）
// 补的那条 ALTER 必须与 0003 的迁移语句同源——否则「跑过迁移的库」与「从零自举
// 的库」在 err_logs 上会有两种形状，而写入面（Set attempt_ordinal）只认后者。
//
// 唯一允许的差异是自举侧的 IF NOT EXISTS 幂等修饰（bootstrap 会被重复调用）。
func TestErrLogAttemptOrdinalDDLMatchesMigration(t *testing.T) {
	up := strings.ReplaceAll(attemptLogUpSQL(t), "public.", "")
	bootstrap := repository.ErrLogAttemptOrdinalAlter()
	require.Contains(t, bootstrap, "IF NOT EXISTS", "自举语句必须幂等（bootstrap 会被重复调用）")
	require.Contains(t, up, strings.Replace(bootstrap, " IF NOT EXISTS", "", 1),
		"自举 ALTER 与 0003 的迁移语句必须同源（除 IF NOT EXISTS 修饰）")
	require.Equal(t, attemptLogOrdinalAlter, strings.Replace(bootstrap, " IF NOT EXISTS", "", 1),
		"两条路径的列名/类型/可空性必须逐字一致")
}

// attemptLogDownSQL 取内嵌的 0003 down 文本（缺失即失败：down.sql 被删了脚本
// 会「看起来」回滚成功，实际什么都没做）。
func attemptLogDownSQL(t *testing.T) string {
	t.Helper()
	migrations, err := loadMigrations(migrationFS)
	require.NoError(t, err)
	for _, m := range migrations {
		if m.Version == attemptLogMigrationVersion {
			require.NotEmpty(t, m.DownSQL, "0003 的 down 文件不得为空/缺失")
			return m.DownSQL
		}
	}
	require.FailNow(t, "0003_attempt_logs.down.sql is missing from the embedded migrations")
	return ""
}

// sqlStatements 把迁移文本切成**语句序列**（跳过注释与空行，按分号收句）。
// 用它冻结 down.sql 比按行比对稳：加一行注释不会红，多加/少加/改一条语句必红。
func sqlStatements(t *testing.T, sql string) []string {
	t.Helper()
	var out []string
	var cur []string
	for line := range strings.SplitSeq(sql, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "--") {
			continue
		}
		cur = append(cur, trimmed)
		if strings.HasSuffix(trimmed, ";") {
			out = append(out, strings.Join(cur, " "))
			cur = nil
		}
	}
	require.Empty(t, cur, "迁移文本有未以分号结束的语句")
	return out
}

// TestAttemptLogDownSQLDropsWhatUpCreates 0003 的回滚面：down.sql 的语句集合与
// 顺序逐条冻结（建了什么都得拆干净，且**只**拆这些）。
//
// 为什么要占位符+顺序两条一起断言：漏一条 ⇒ 回滚后残留对象（再 up 会撞名）；
// 多一条 ⇒ 回滚在别人的表上动手。评审的变异是「删掉 down.sql 仍全绿」——本用例
// 先随包装载文本（缺失即 FailNow），再比语句序列。
func TestAttemptLogDownSQLDropsWhatUpCreates(t *testing.T) {
	down := strings.ReplaceAll(attemptLogDownSQL(t), "public.", "")
	require.Equal(t, []string{
		"SET search_path = public;",
		"DROP TABLE IF EXISTS attempt_logs;",
		"DROP SEQUENCE IF EXISTS attempt_logs_id_seq;",
		"ALTER TABLE err_logs DROP COLUMN IF EXISTS attempt_ordinal;",
	}, sqlStatements(t, down),
		"down.sql 只允许拆 0003 建的两个对象 + 那一列，且顺序固定（先表、后序列、再列）")

	// 顺序语义：表先走（序列靠 OWNED BY 级联），列最后。
	joined := strings.Join(sqlStatements(t, down), "\n")
	require.Less(t, strings.Index(joined, "DROP TABLE IF EXISTS attempt_logs"),
		strings.Index(joined, "DROP SEQUENCE IF EXISTS attempt_logs_id_seq"),
		"表必须在序列之前 DROP（序列随表级联，倒过来会留下孤儿序列）")
	require.Less(t, strings.Index(joined, "DROP SEQUENCE IF EXISTS attempt_logs_id_seq"),
		strings.Index(joined, "ALTER TABLE err_logs DROP COLUMN IF EXISTS attempt_ordinal"),
		"列最后拆（err_logs 是 0001 的对象，必须最后动）")
}
