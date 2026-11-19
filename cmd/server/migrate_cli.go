// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// `-migrate` 子命令（spec §2.12 的 flag 分支）。三条硬约束：
//
//  1. **不进 HTTP 服务**：子命令跑完即退出，成功 0 / 失败非 0——k8s 的迁移 Job 与
//     单机的 `compose run --rm` 都靠退出码决定"要不要继续发布"。
//  2. **默认全拒**：`down` 必须 `-allow-destructive`、contract 必须
//     `-allow-contract`、`adopt` 必须 `-snapshot <tag>`。破坏性动作没有"顺手就
//     执行"的形态。
//  3. **app 进程永不迁移**：这条只由「up 只在命令行」保证，见 main 的分支。
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"

	"github.com/is7qin/c3api/internal/config"
	"github.com/is7qin/c3api/internal/db/migrate"
	"github.com/is7qin/c3api/internal/repository"
)

// 子命令名（唯一清单：校验与 usage 提示都从这里派生）。
const (
	migrateCmdUp     = "up"
	migrateCmdDown   = "down"
	migrateCmdStatus = "status"
	migrateCmdVerify = "verify"
	migrateCmdAdopt  = "adopt"
)

// migrateCmdNames 是 usage/错误文案里的一行清单。
const migrateCmdNames = migrateCmdUp + "|" + migrateCmdDown + "|" + migrateCmdStatus + "|" + migrateCmdVerify + "|" + migrateCmdAdopt

// migrateTimeout 是一次子命令的硬上限。Helm Job 侧还有 activeDeadlineSeconds
// （600s）兜底；本地裸跑时这个上限保证"挂住的 DDL"不会永远占着终端。
const migrateTimeout = 10 * time.Minute

// migrateFlags 是 `-migrate` 与其子命令参数的绑定（默认全空 = 现在的 serve 路径）。
type migrateFlags struct {
	command          string
	target           int64
	allowContract    bool
	allowDestructive bool
	snapshot         string
}

// registerMigrateFlags 注册全部迁移 flag。默认路径行为不变：一个都不传就是
// `serve`，与引入这些 flag 之前逐字节一致。
func registerMigrateFlags(fs *flag.FlagSet) *migrateFlags {
	m := &migrateFlags{}
	fs.StringVar(&m.command, "migrate", "",
		"run a schema migration subcommand and exit ("+migrateCmdNames+"); unset = serve")
	fs.Int64Var(&m.target, "target", 0, "with -migrate up: stop at this version (0 = this binary's head)")
	fs.BoolVar(&m.allowContract, "allow-contract", false,
		"with -migrate up: also apply *_contract migrations (destructive, irreversible)")
	fs.BoolVar(&m.allowDestructive, "allow-destructive", false,
		"with -migrate down: confirm that dropping what the migration created also drops the data")
	fs.StringVar(&m.snapshot, "snapshot", "",
		"with -migrate adopt: fingerprint tag of the last released schema (e.g. 0002; 0001 = legacy baseline)")
	return m
}

// migrateOptions 是校验通过的子命令入参。
type migrateOptions struct {
	Command          string
	Target           int64
	AllowContract    bool
	AllowDestructive bool
	Snapshot         string
}

// options 把 flag 折成子命令入参，并按**子命令**逐一校验闸门。
//
// 交叉 flag（例如 `-snapshot` 配 `-migrate down`）一律报错而不是忽略：能悄悄忽略
// 的 flag 等于给了一种"看起来生效了"的假象，而迁移的每一步都不可逆。
func (m *migrateFlags) options() (migrateOptions, error) {
	command := strings.TrimSpace(m.command)
	opt := migrateOptions{Command: command}
	exclusive := func(owner string, set bool, flagName string) error {
		if !set {
			return nil
		}
		if owner == "" {
			return fmt.Errorf("%s requires -migrate (%s)", flagName, migrateCmdNames)
		}
		return fmt.Errorf("%s is not valid with -migrate %s", flagName, owner)
	}
	if command == "" {
		for _, check := range []struct {
			set  bool
			name string
		}{
			{m.target != 0, "-target"},
			{m.allowContract, "-allow-contract"},
			{m.allowDestructive, "-allow-destructive"},
			{m.snapshot != "", "-snapshot"},
		} {
			if err := exclusive("", check.set, check.name); err != nil {
				return migrateOptions{}, err
			}
		}
		return opt, nil
	}

	switch command {
	case migrateCmdUp:
		if err := exclusive(migrateCmdUp, m.allowDestructive, "-allow-destructive"); err != nil {
			return migrateOptions{}, err
		}
		if err := exclusive(migrateCmdUp, m.snapshot != "", "-snapshot"); err != nil {
			return migrateOptions{}, err
		}
		if m.target < 0 {
			return migrateOptions{}, fmt.Errorf("-target must be >= 0 (got %d; 0 = this binary's head)", m.target)
		}
		opt.Target, opt.AllowContract = m.target, m.allowContract
	case migrateCmdDown:
		if err := exclusive(migrateCmdDown, m.target != 0, "-target"); err != nil {
			return migrateOptions{}, err
		}
		if err := exclusive(migrateCmdDown, m.allowContract, "-allow-contract"); err != nil {
			return migrateOptions{}, err
		}
		if err := exclusive(migrateCmdDown, m.snapshot != "", "-snapshot"); err != nil {
			return migrateOptions{}, err
		}
		if !m.allowDestructive {
			return migrateOptions{}, errors.New("`-migrate down` drops what the migration created, data included; pass -allow-destructive to confirm (the migration's `-- ROLLBACK DROPS:` line says what goes)")
		}
		opt.AllowDestructive = true
	case migrateCmdAdopt:
		if err := exclusive(migrateCmdAdopt, m.target != 0, "-target"); err != nil {
			return migrateOptions{}, err
		}
		if err := exclusive(migrateCmdAdopt, m.allowContract, "-allow-contract"); err != nil {
			return migrateOptions{}, err
		}
		if err := exclusive(migrateCmdAdopt, m.allowDestructive, "-allow-destructive"); err != nil {
			return migrateOptions{}, err
		}
		if strings.TrimSpace(m.snapshot) == "" {
			return migrateOptions{}, errors.New("`-migrate adopt` requires -snapshot <tag>: the fingerprint of the last released schema (e.g. -snapshot 0002; 0001 = legacy baseline). Adoption is never automatic")
		}
		opt.Snapshot = strings.TrimSpace(m.snapshot)
	case migrateCmdStatus, migrateCmdVerify:
		for _, check := range []struct {
			set  bool
			name string
		}{
			{m.target != 0, "-target"},
			{m.allowContract, "-allow-contract"},
			{m.allowDestructive, "-allow-destructive"},
			{m.snapshot != "", "-snapshot"},
		} {
			if err := exclusive(command, check.set, check.name); err != nil {
				return migrateOptions{}, err
			}
		}
	default:
		return migrateOptions{}, fmt.Errorf("unknown -migrate %q: want %s", command, migrateCmdNames)
	}
	return opt, nil
}

// runMigrate 执行子命令并返回进程退出码（0 = 成功）。
//
// 只开 DB：不连 Redis、不装配仓库图、不起监听端口。配置仍由 config.Load 校验
// （与 serve 同一份文件），所以 `-migrate up` 的失败面就只有"配置错"与"迁移错"。
func runMigrate(cfg *config.Config, opt migrateOptions) int {
	ctx, cancel := context.WithTimeout(context.Background(), migrateTimeout)
	defer cancel()

	pool, err := repository.OpenPG(ctx, cfg.DB.DSN, int32(cfg.DB.MaxConns))
	if err != nil {
		return migrateFail(opt.Command, "connect database", err)
	}
	defer pool.Close()
	db := stdlib.OpenDBFromPool(pool)
	defer func() { _ = db.Close() }()

	switch opt.Command {
	case migrateCmdUp:
		return runMigrateUp(ctx, db, pool, nil, opt)
	case migrateCmdDown:
		return runMigrateDown(ctx, db, opt)
	case migrateCmdStatus:
		return runMigrateStatus(ctx, db)
	case migrateCmdVerify:
		return runMigrateVerify(ctx, db)
	case migrateCmdAdopt:
		return runMigrateAdopt(ctx, db, opt)
	default:
		return migrateFail(opt.Command, "dispatch", fmt.Errorf("unknown command %q", opt.Command))
	}
}

// migrateUp 是 up 子命令的唯一执行点（fsys == nil ⇒ 内嵌迁移集，即生产路径）。
//
// 为什么留一个 fsys 参数：CLI 级的「contract 默认拒跑、且拒绝发生在任何写入之前」
// 需要一条真的 contract 迁移才能演出来，而生产迁移集当前全是 expand。命令行恒传
// nil；注入点只服务于证据。
func migrateUp(ctx context.Context, db *sql.DB, fsys fs.FS, opts migrate.UpOptions) (migrate.Result, error) {
	return migrate.UpFS(ctx, db, fsys, opts)
}

// runMigrateUp 是 up 子命令的完整执行体：应用迁移 → 打印 → 补齐分区。
//
// fsys 是**唯一**的注入点（nil = 内嵌迁移集，生产恒为 nil）：真实迁移集当前全是
// expand，而「-allow-contract 真的接到了 UpOptions」这条 CLI 级契约需要一条真的
// contract 迁移才能演出来（见 migrate_cli_pg_test.go）。注入集不是本仓 baseline，
// 因此不补分区——分区是 0001 的语义，不是任意迁移集的。
func runMigrateUp(ctx context.Context, db *sql.DB, pool *pgxpool.Pool, fsys fs.FS, opt migrateOptions) int {
	res, err := migrateUp(ctx, db, fsys, migrate.UpOptions{Target: opt.Target, AllowContract: opt.AllowContract})
	if err != nil {
		return migrateFail(opt.Command, "apply migrations", err)
	}
	for _, applied := range res.Applied {
		fmt.Printf("applied %04d_%s (%s, %dms)\n", applied.Version, applied.Name, applied.Phase, applied.ExecutionMS)
	}
	if len(res.Applied) == 0 {
		fmt.Printf("schema already at %04d: nothing to apply\n", res.To)
	}
	if fsys != nil {
		fmt.Println("injected migration set: partition top-up skipped (it belongs to the embedded baseline)")
		return 0
	}
	// 分区补齐（spec §2.7）：up 成功后退出前，与启动路径调**同一个**入口。
	// 先建分区再退出，任何写路径（新 pod）才不需要自己补 DDL。
	repos, err := repository.NewWithoutMigrate(ctx, entsql.OpenDB(dialect.Postgres, db), pool)
	if err != nil {
		return migrateFail(opt.Command, "open repository", err)
	}
	now := time.Now().UTC()
	if err := repos.EnsureAllTablePartitions(ctx, now, now.Add(partitionHorizon)); err != nil {
		return migrateFail(opt.Command, "ensure table partitions", err)
	}
	fmt.Printf("partitions ensured for [%s, %s) (usage_logs, err_logs, attempt_logs, usage_stats, usage_entity_stats, routing_quality_fact, routing_flow_fact, routing_flow_snapshot_state)\n",
		now.Format(time.RFC3339), now.Add(partitionHorizon).Format(time.RFC3339))
	return 0
}

func runMigrateDown(ctx context.Context, db *sql.DB, opt migrateOptions) int {
	res, err := migrate.Down(ctx, db, migrate.DownOptions{AllowDestructive: opt.AllowDestructive})
	if err != nil {
		return migrateFail(opt.Command, "roll back", err)
	}
	if len(res.Rolled) == 0 {
		fmt.Println("ledger is empty: nothing to roll back")
		return 0
	}
	for _, rolled := range res.Rolled {
		fmt.Printf("rolled back %04d_%s (%s)\n", rolled.Version, rolled.Name, rolled.Phase)
	}
	// 数据已经被 DROP 掉，这里只能补一句"刚刚丢了什么"——不能事后撤销。
	for _, stmt := range res.SQL {
		fmt.Printf("WARN: the following rollback SQL has already run (data it dropped is gone):\n%s\n", stmt)
	}
	return 0
}

func runMigrateStatus(ctx context.Context, db *sql.DB) int {
	st, err := migrate.Status(ctx, db)
	if err != nil {
		return migrateFail(migrateCmdStatus, "read ledger", err)
	}
	fmt.Printf("%-4s %-32s %-9s %-9s %s\n", "ver", "name", "phase", "origin", "applied_at")
	for _, row := range st.Applied {
		fmt.Printf("%04d %-32s %-9s %-9s %s\n", row.Version, row.Name, row.Phase, row.Origin, row.AppliedAt)
	}
	for _, m := range st.Pending {
		fmt.Printf("%04d %-32s %-9s %-9s (pending)\n", m.Version, m.Name, m.Phase, "-")
	}
	if len(st.Mismatches) > 0 {
		for _, mm := range st.Mismatches {
			fmt.Fprintf(os.Stderr, "migrate: %04d_%s checksum mismatch: recorded=%s current=%s (migrations are immutable; add a new version instead)\n",
				mm.Version, mm.Name, mm.Recorded, mm.Current)
		}
		return 1
	}
	return 0
}

func runMigrateVerify(ctx context.Context, db *sql.DB) int {
	verdict, err := migrate.Verify(ctx, db)
	if err != nil {
		return migrateFail(migrateCmdVerify, "verify schema", err)
	}
	decision := migrate.Decide(verdict)
	fmt.Printf("db_head=%04d db_contract=%04d binary_head=%04d binary_contract=%04d required=%04d mode=%s\n",
		verdict.DBHead, verdict.DBContract, verdict.BinaryHead, verdict.BinaryContract, migrate.RequiredVersion, decision.Mode)
	if len(verdict.Ahead) > 0 {
		fmt.Printf("WARN: database is ahead of this binary (expand-only keeps it compatible): versions %v\n", verdict.Ahead)
	}
	for _, warning := range decision.Warnings {
		fmt.Printf("WARN: %s\n", warning)
	}
	if !decision.Ready() {
		fmt.Fprintf(os.Stderr, "migrate: not serviceable: %s\n", decision.Reason)
		return 1
	}
	fmt.Println("schema is serviceable by this binary")
	return 0
}

func runMigrateAdopt(ctx context.Context, db *sql.DB, opt migrateOptions) int {
	res, err := migrate.Adopt(ctx, db, migrate.AdoptOptions{
		Snapshot: opt.Snapshot,
		// LegacyTolerances = 「老路径独有、0001 尚未收敛」的已知结构差异（当前只有
		// price_variants 的 effect CHECK）。收养必须把它显式交出来，并且把命中的行
		// 打印出来——否则"收养成功"就成了一次静默的放宽。
		Tolerate: migrate.LegacyTolerances,
	})
	if err != nil {
		return migrateFail(opt.Command, "adopt baseline", err)
	}
	for _, line := range res.Tolerated {
		fmt.Printf("tolerated known legacy delta: %s\n", line)
	}
	for _, adopted := range res.Adopted {
		fmt.Printf("adopted %04d_%s (%s, origin=%s)\n", adopted.Version, adopted.Name, adopted.Phase, adopted.Origin)
	}
	fmt.Printf("adopted baseline %s: %d version(s) recorded as origin=adopted (they can never be rolled back)\n",
		opt.Snapshot, len(res.Adopted))
	return 0
}

// migrateFail 打印可读诊断并返回非 0 退出码（子命令失败的唯一出口）。
func migrateFail(command, step string, err error) int {
	fmt.Fprintf(os.Stderr, "migrate %s: %s: %v\n", command, step, err)
	return 1
}
