// Command cmd 是 0001_baseline 生成器的 CLI：
//
//	go run ./internal/db/migrate/gen/cmd            # 重写 migrations/ 与 testdata/
//	go run ./internal/db/migrate/gen/cmd -check      # 只比对，漂移即非零退出（CI）
//
// DSN 优先级：-dsn > MIGRATE_GEN_DSN > TEST_DATABASE_URL > 本地 compose 的 postgres 库。
// 生成器需要能 CREATE DATABASE（临时空库，跑完 DROP）。
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/is7qin/c3api/internal/db/migrate/gen"
)

// writeLegacy 生成（并落盘）老路径的 schema 快照。只在 M0b 之前可用。
func writeLegacy(ctx context.Context, cfg gen.Config, pgDump, pgTarget, commit string, stdout io.Writer) error {
	if commit == "" {
		commit = gitHead()
	}
	artifacts, err := gen.GenerateLegacy(ctx, gen.LegacyConfig{
		DSN:          cfg.DSN,
		TestdataDir:  cfg.TestdataDir,
		PGDump:       splitCommand(pgDump),
		PGDumpTarget: pgTarget,
		Commit:       commit,
	})
	if err != nil {
		return err
	}
	if err := artifacts.Write(gen.LegacyConfig{DSN: cfg.DSN, TestdataDir: cfg.TestdataDir}); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "gen: legacy fixture %s written (pg=%d, %d lines, %d bytes, commit %s)\n",
		gen.LegacyFixtureFile, artifacts.ServerVersionNum, artifacts.Lines, artifacts.Bytes, commit)
	return nil
}

// splitCommand 把 `docker,exec,-i,db,pg_dump` 这样的逗号串拆成命令前缀。
func splitCommand(spec string) []string {
	var out []string
	for _, part := range strings.Split(spec, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// gitHead 取当前 commit 的短 hash（拿不到就是 "unknown"，不阻塞生成）。
func gitHead() string {
	out, err := exec.Command("git", "rev-parse", "--short=8", "HEAD").Output()
	if err != nil {
		return "unknown"
	}
	return strings.TrimSpace(string(out))
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run 是 CLI 的可测入口：flag 解析、执行、退出码映射都在这里。main 只负责
// os.Exit——「-check 漂移 ⇒ 非零退出」是 CI 的唯一信号，不能只靠人肉跑 CLI 验证。
func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("gen", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		check  = fs.Bool("check", false, "regenerate and diff against committed files (non-zero exit on drift)")
		force  = fs.Bool("force-version", false, "with -check: accept a cross-major PostgreSQL difference instead of failing")
		dsn    = fs.String("dsn", "", "maintenance PostgreSQL DSN (needs CREATEDB); defaults to MIGRATE_GEN_DSN/TEST_DATABASE_URL")
		migs   = fs.String("migrations", "", "migrations output dir (default: <gen package dir>/../migrations, resolved from the source path, not cwd)")
		testda = fs.String("testdata", "", "testdata output dir (default: <gen package dir>/../testdata, resolved from the source path, not cwd)")
		legacy = fs.Bool("legacy", false, "regenerate testdata/legacy/*.sql through the OLD bootstrap path (pre-M0b only)")
		pgDump = fs.String("pg-dump", "pg_dump", "comma-separated pg_dump command prefix (e.g. 'docker,exec,-i,db,pg_dump,-U,postgres')")
		pgTgt  = fs.String("pg-dump-target", "", "pg_dump --dbname template, %s = scratch database name (default: the full DSN)")
		commit = fs.String("commit", "", "commit the legacy fixture was generated from (default: git rev-parse --short=8 HEAD)")
	)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	cfg := gen.Config{DSN: *dsn, MigrationsDir: *migs, TestdataDir: *testda, ForceVersion: *force}
	ctx := context.Background()
	if *legacy {
		if err := writeLegacy(ctx, cfg, *pgDump, *pgTgt, *commit, stdout); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	if *check {
		warnings, err := gen.Check(ctx, cfg)
		for _, warning := range warnings {
			fmt.Fprintln(stderr, "WARN:", warning)
		}
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintln(stdout, "gen: committed migrations match the generator output")
		return 0
	}
	artifacts, err := gen.Run(ctx, cfg)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := artifacts.Write(cfg); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	s := artifacts.Stats
	fmt.Fprintf(stdout, "gen: ent tables=%d partitioned=%d static=%d statements=%d up=%d lines down=%d lines catalog=%d lines\n",
		s.EntTables, s.PartitionedTable, s.StaticTables, s.Statements, s.UpLines, s.DownLines, s.CatalogLines)
	return 0
}
