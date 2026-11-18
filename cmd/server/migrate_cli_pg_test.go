// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"context"
	"database/sql"
	"testing"
	"testing/fstest"
	"time"

	// 注册 database/sql 的 pgx 驱动（探针库句柄走 database/sql）。
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/is7qin/c3api/internal/db/migrate"
	"github.com/is7qin/c3api/internal/db/migrate/catalog"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/testsupport/pgtest"
)

// `-migrate` 子命令的证据（自证 T5/T7 + 锁预算）：
//
//   - flag 闸门默认全拒（不连库，纯逻辑）；
//   - 空库跑完 up：35 张业务表 + 账本 + 当日/明日分区齐备，且再跑一次幂等；
//   - 撞 contract 且没有 -allow-contract ⇒ 整体拒绝，一行都没应用；
//   - 另一个会话持 ACCESS EXCLUSIVE 时 up 在 lock_timeout（3s）内失败退出。

// TestMigrateFlagGates 钉住子命令的参数闸门：破坏性动作必须显式、交叉 flag 必须
// 报错（悄悄忽略的 flag 等于"看起来生效了"的假象）。
func TestMigrateFlagGates(t *testing.T) {
	t.Run("serve path keeps every migrate flag off", func(t *testing.T) {
		_, err := (&migrateFlags{allowContract: true}).options()
		require.ErrorContains(t, err, "-allow-contract requires -migrate")
		_, err = (&migrateFlags{target: 3}).options()
		require.ErrorContains(t, err, "-target requires -migrate")
		_, err = (&migrateFlags{snapshot: "0001"}).options()
		require.ErrorContains(t, err, "-snapshot requires -migrate")

		opt, err := (&migrateFlags{}).options()
		require.NoError(t, err)
		require.Empty(t, opt.Command, "不带 -migrate 就是 serve")
	})

	t.Run("up maps allow-contract and target", func(t *testing.T) {
		opt, err := (&migrateFlags{command: "up", allowContract: true, target: 2}).options()
		require.NoError(t, err)
		require.Equal(t, "up", opt.Command)
		require.True(t, opt.AllowContract)
		require.Equal(t, int64(2), opt.Target)
	})

	t.Run("up rejects negative target", func(t *testing.T) {
		_, err := (&migrateFlags{command: "up", target: -1}).options()
		require.ErrorContains(t, err, "-target must be >= 0")
	})

	t.Run("down requires allow-destructive", func(t *testing.T) {
		_, err := (&migrateFlags{command: "down"}).options()
		require.ErrorContains(t, err, "-allow-destructive")

		opt, err := (&migrateFlags{command: "down", allowDestructive: true}).options()
		require.NoError(t, err)
		require.True(t, opt.AllowDestructive)
	})

	t.Run("adopt requires snapshot", func(t *testing.T) {
		_, err := (&migrateFlags{command: "adopt"}).options()
		require.ErrorContains(t, err, "-snapshot")

		opt, err := (&migrateFlags{command: "adopt", snapshot: "0001"}).options()
		require.NoError(t, err)
		require.Equal(t, "0001", opt.Snapshot)
	})

	t.Run("cross flags are rejected", func(t *testing.T) {
		_, err := (&migrateFlags{command: "adopt", allowContract: true, snapshot: "0001"}).options()
		require.ErrorContains(t, err, "-allow-contract is not valid with -migrate adopt")
		_, err = (&migrateFlags{command: "status", target: 1}).options()
		require.ErrorContains(t, err, "-target is not valid with -migrate status")
		_, err = (&migrateFlags{command: "up", snapshot: "0001"}).options()
		require.ErrorContains(t, err, "-snapshot is not valid with -migrate up")
	})

	t.Run("unknown command lists the valid ones", func(t *testing.T) {
		_, err := (&migrateFlags{command: "migrat"}).options()
		require.ErrorContains(t, err, "unknown -migrate")
		require.ErrorContains(t, err, "up|down|status|verify|adopt")
	})
}

// TestMigrateUpBootstrapsEmptyDatabasePG 是「空库跑一次 up 就够」的完整证据：
// 35 张业务表（27 ent + 6 分区父表 + 2 静态表）+ 账本 + 六张父表的当日/明日分区，
// 再跑一次不产生任何变化。
func TestMigrateUpBootstrapsEmptyDatabasePG(t *testing.T) {
	ctx := context.Background()
	db, dsn := openProbeDBWithDSN(t)
	pool := pgtest.OpenPool(t, dsn)

	require.Equal(t, 0, runMigrateUp(ctx, db, pool, nil, migrateOptions{Command: migrateCmdUp}))

	require.Equal(t, 35, countBusinessTables(t, db), "0001 建 35 张业务表（27 ent + 6 分区父表 + 2 静态表）")
	require.Equal(t, 1, countRows(t, db, catalog.ControlTable), "账本恰一行（0001）")
	requirePartitionHorizon(t, db)

	// 幂等：第二次 up 不应用任何版本，也不改变对象数。
	require.Equal(t, 0, runMigrateUp(ctx, db, pool, nil, migrateOptions{Command: migrateCmdUp}))
	require.Equal(t, 35, countBusinessTables(t, db))
	require.Equal(t, 1, countRows(t, db, catalog.ControlTable))

	ledger, err := migrate.Status(ctx, db)
	require.NoError(t, err)
	require.Len(t, ledger.Applied, 1)
	require.Empty(t, ledger.Pending)
	require.Empty(t, ledger.Mismatches)
}

// TestMigrateUpRefusesContractWithoutOptInPG 是 CLI 层的 contract 闸门证据：拒绝
// 发生在**任何写入之前**——探针表没建、账本表都没建。
func TestMigrateUpRefusesContractWithoutOptInPG(t *testing.T) {
	ctx := context.Background()
	db := openProbeDB(t)

	fsys := fstest.MapFS{
		"migrations/0001_probe.up.sql": &fstest.MapFile{Data: []byte(
			"CREATE TABLE public.probe_expand (id bigint PRIMARY KEY);\n")},
		"migrations/0002_cleanup_contract.up.sql": &fstest.MapFile{Data: []byte(
			"DROP TABLE public.probe_expand;\n")},
	}

	_, err := migrateUp(ctx, db, fsys, migrate.UpOptions{})
	require.ErrorContains(t, err, "refusing to apply 1 contract migration")
	require.ErrorContains(t, err, "-allow-contract", "拒绝文案必须指名真实的 flag（运维照着抄就要能跑）")

	require.False(t, relationExists(t, db, "probe_expand"), "被拒的 up 必须一行都没应用")
	require.False(t, relationExists(t, db, catalog.ControlTable), "拒绝发生在建账本之前")
}

// TestMigrateUpFailsFastOnLockTimeoutPG 是锁预算的证据：另一个会话持 ACCESS
// EXCLUSIVE 时，up 不在 DDL 上排队堵表，而是 3s 内失败退出并说明"另一事务持锁、
// 稍后重跑"。
func TestMigrateUpFailsFastOnLockTimeoutPG(t *testing.T) {
	ctx := context.Background()
	db := openProbeDB(t)

	fsys := fstest.MapFS{
		"migrations/0001_probe.up.sql": &fstest.MapFile{Data: []byte(
			"CREATE TABLE public.probe_lock (id bigint PRIMARY KEY);\n")},
		"migrations/0002_probe_next.up.sql": &fstest.MapFile{Data: []byte(
			"ALTER TABLE public.probe_lock ADD COLUMN note varchar NULL;\n")},
	}
	_, err := migrateUp(ctx, db, fsys, migrate.UpOptions{Target: 1})
	require.NoError(t, err)

	// 另一个会话拿住 ACCESS EXCLUSIVE（LOCK TABLE 在返回时锁已在手：不需要 sleep
	// 去"等它拿到"）。
	locker, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = locker.Rollback() }()
	_, err = locker.ExecContext(ctx, "LOCK TABLE public.probe_lock IN ACCESS EXCLUSIVE MODE")
	require.NoError(t, err)

	// 这条用例自带 10s deadline：变异（删掉 SET LOCAL lock_timeout）时它必须在
	// 10s 内以「可读错误」返回，而不是把整包拖到 go test 的 20 分钟超时——那种红
	// 只有一行 action timed out，看不出是哪条迁移在等锁。
	lockCtx, lockCancel := context.WithTimeout(ctx, 10*time.Second)
	defer lockCancel()
	started := time.Now()
	_, err = migrateUp(lockCtx, db, fsys, migrate.UpOptions{})
	elapsed := time.Since(started)

	require.Error(t, err)
	require.Less(t, elapsed, 12*time.Second, "锁等待必须有界：默认 60s 的 advisory 超时不是这条路径的上限")
	require.ErrorContains(t, err, "lock not acquired within lock_timeout=3s")
	require.ErrorContains(t, err, "retry later")
	require.Greater(t, elapsed, time.Second, "必须真的等过 lock_timeout，而不是因为别的原因立刻失败")
}

// TestMigrateUpCLIHonoursAllowContractFlag 是 CLI 层的 contract 接线证据：runMigrateUp
// 必须把 migrateOptions.AllowContract 真的传进 UpOptions。
//
// 为什么要单独一条：库函数层面的 TestMigrateUpRefusesContractWithoutOptInPG
// 证明的是 migrate.Up 的闸门，证不了「flag → 选项」这一段——把
// runMigrateUp 里的 AllowContract 写成常量的话，那条用例依旧全绿。
func TestMigrateUpCLIHonoursAllowContractFlag(t *testing.T) {
	ctx := context.Background()
	db, dsn := openProbeDBWithDSN(t)
	pool := pgtest.OpenPool(t, dsn)
	fsys := fstest.MapFS{
		"migrations/0001_probe.up.sql": &fstest.MapFile{Data: []byte(
			"CREATE TABLE public.probe_expand (id bigint PRIMARY KEY);\n")},
		"migrations/0002_cleanup_contract.up.sql": &fstest.MapFile{Data: []byte(
			"DROP TABLE public.probe_expand;\n")},
	}

	// 不带 -allow-contract（默认）：非 0 退出，且一行都没应用。
	exit := runMigrateUp(ctx, db, pool, fsys, migrateOptions{Command: migrateCmdUp})
	require.NotZero(t, exit, "contract 默认拒跑时 CLI 必须非 0 退出")
	require.False(t, relationExists(t, db, "probe_expand"), "被拒的 up 必须一行都没应用")
	require.False(t, relationExists(t, db, catalog.ControlTable), "拒绝发生在建账本之前")

	// 带 -allow-contract：两版都应用（contract 真的跑了，所以探针表被 DROP）。
	exit = runMigrateUp(ctx, db, pool, fsys, migrateOptions{Command: migrateCmdUp, AllowContract: true})
	require.Zero(t, exit, "-allow-contract 必须把选项传到 UpOptions")
	require.Equal(t, 2, countRows(t, db, catalog.ControlTable), "两个版本都应计入账本")
	require.False(t, relationExists(t, db, "probe_expand"), "contract 迁移必须真的执行")
}

// TestEnsureAllTablePartitionsRequiresMigratedParentsPG 钉住「父表由迁移建」这条
// 结构性约束：空库上补分区必须报错并指向 `c3api -migrate up`，而不是自己把父表
// 建出来（那会形成第二建表真相，让"跑过迁移的库"与"启动随手建的库"不可区分）。
func TestEnsureAllTablePartitionsRequiresMigratedParentsPG(t *testing.T) {
	ctx := context.Background()
	db, dsn := openProbeDBWithDSN(t)
	pool := pgtest.OpenPool(t, dsn)
	repos, err := repository.NewWithoutMigrate(ctx, entsql.OpenDB(dialect.Postgres, db), pool)
	require.NoError(t, err)

	now := time.Now().UTC()
	err = repos.EnsureAllTablePartitions(ctx, now, now.Add(24*time.Hour))
	require.ErrorContains(t, err, "run `c3api -migrate up`")
	require.ErrorContains(t, err, "usage_logs", "报错必须指名第一张缺失的父表")
	require.False(t, relationExists(t, db, "usage_logs"), "启动/补分区路径不得建父表")
}

// countBusinessTables 数 public 下的业务表（排除账本与分区实例）。
func countBusinessTables(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(
		`SELECT count(*) FROM pg_class c JOIN pg_namespace ns ON ns.oid = c.relnamespace
		 WHERE ns.nspname = 'public' AND c.relkind IN ('r', 'p') AND NOT c.relispartition AND c.relname <> $1`,
		catalog.ControlTable).Scan(&n))
	return n
}

func countRows(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM public.`+table).Scan(&n))
	return n
}

// requirePartitionHorizon 断言七组的当日/明日分区齐备（六张父表按日，静态表不过日）。
func requirePartitionHorizon(t *testing.T, db *sql.DB) {
	t.Helper()
	now := time.Now().UTC()
	for _, table := range []string{
		"usage_logs", "err_logs", "usage_stats", "usage_entity_stats", "routing_quality_fact", "routing_flow_fact",
	} {
		for _, day := range []time.Time{now, now.Add(24 * time.Hour)} {
			name := "public." + table + "_" + day.Format("20060102")
			require.True(t, relationExists(t, db, name), "缺分区 %s（EnsureAllTablePartitions 必须在 up 退出前跑过）", name)
		}
	}
	require.True(t, relationExists(t, db, "routing_flow_snapshot_state"), "静态快照表由 0001 建")
}

// relationExists 判断 public 下的关系是否存在（按完整名，含 schema 前缀）。
func relationExists(t *testing.T, db *sql.DB, qualified string) bool {
	t.Helper()
	var ok bool
	require.NoError(t, db.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, qualified).Scan(&ok))
	return ok
}
