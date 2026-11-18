package gen

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/is7qin/c3api/internal/repository"
)

// LegacyFixtureFile 是「老路径建出来的 schema」的冻结快照（T2 的输入）。
const LegacyFixtureFile = "aba8042e.sql"

// LegacyConfig 是 -legacy 模式的参数。
type LegacyConfig struct {
	// DSN 是维护库连接串（需要 CREATEDB）。
	DSN string
	// TestdataDir 是输出目录（产物落在 <TestdataDir>/legacy/<LegacyFixtureFile>）。
	TestdataDir string
	// PGDump 是 pg_dump 命令前缀，例如 {"pg_dump"} 或
	// {"docker","exec","-i","deploy-db-1","pg_dump","-U","postgres"}（本地没有
	// pg_dump 时的唯一可行方式：容器里的 PG 自带同 major 的 pg_dump）。
	PGDump []string
	// PGDumpTarget 是 `--dbname` 的取值模板：`%s` 会被替换成临时库名。
	// 空 = 直接用临时库的完整 DSN（本机 pg_dump）；在容器里跑 pg_dump 时用
	// 纯库名（`%s`，走容器内 socket + trust），因为宿主的 localhost:<port> 在
	// 容器内不可达。
	PGDumpTarget string
	// Commit 是被冻结库对应的 commit（写进头注释）。
	Commit string
}

// LegacyArtifacts 是 -legacy 的产物。
type LegacyArtifacts struct {
	SQL              string
	ServerVersionNum int
	Lines            int
	Bytes            int
}

// GenerateLegacy 用**当前仍存在的老路径**建库并 dump 出 schema：
//
//	repository.New(drv, true)                      // ent 自动迁移（生产同款）
//	EnsureUsageLogPartitioned / EnsureErrLogPartitioned /
//	EnsureUsageStatsPartitioned / EnsureUsageEntityStatsPartitioned /
//	EnsureRoutingPartitions                        // 分区 bootstrap（wiring_infra.go:132-146）
//	EnsurePriceVariantsEffectCheck                  // price_variants 效果字段约束（DO $$）
//	EnsureCodexSearchSeed                           // codex-search 种子行（数据，不影响结构）
//
// 这就是 cmd/server/wiring_infra.go:119-152 的那一串。它只在 M0b 拆掉老路径之前
// 可生成——之后这个函数会编译失败（老方法不存在），**这正是它的过期标记**。
func GenerateLegacy(ctx context.Context, cfg LegacyConfig) (LegacyArtifacts, error) {
	if len(cfg.PGDump) == 0 {
		cfg.PGDump = []string{"pg_dump"}
	}
	if cfg.DSN == "" {
		cfg.DSN = DefaultDSN()
	}
	if cfg.TestdataDir == "" {
		_, testdataDir, err := defaultDirs()
		if err != nil {
			return LegacyArtifacts{}, err
		}
		cfg.TestdataDir = testdataDir
	}
	scratch, err := newScratchDB(ctx, cfg.DSN)
	if err != nil {
		return LegacyArtifacts{}, err
	}
	defer scratch.close()
	serverVersion, err := serverVersionNum(ctx, scratch.pool)
	if err != nil {
		return LegacyArtifacts{}, err
	}
	if err := bootstrapOldPath(ctx, scratch.pool); err != nil {
		return LegacyArtifacts{}, err
	}
	target := scratch.dsn
	if cfg.PGDumpTarget != "" {
		target = strings.ReplaceAll(cfg.PGDumpTarget, "%s", scratch.databaseName())
	}
	dump, err := pgDumpSchema(ctx, cfg.PGDump, target)
	if err != nil {
		return LegacyArtifacts{}, err
	}
	body := legacyHeader(cfg.Commit, serverVersion) + normalizeDump(dump)
	return LegacyArtifacts{
		SQL:              body,
		ServerVersionNum: serverVersion,
		Lines:            len(strings.Split(strings.TrimSuffix(body, "\n"), "\n")),
		Bytes:            len(body),
	}, nil
}

// Write 落盘到 <TestdataDir>/legacy/<LegacyFixtureFile>。
func (a LegacyArtifacts) Write(cfg LegacyConfig) error {
	if cfg.TestdataDir == "" {
		_, testdataDir, err := defaultDirs()
		if err != nil {
			return err
		}
		cfg.TestdataDir = testdataDir
	}
	dir := filepath.Join(cfg.TestdataDir, "legacy")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("gen: mkdir %s: %w", dir, err)
	}
	path := filepath.Join(dir, LegacyFixtureFile)
	if err := os.WriteFile(path, []byte(a.SQL), 0o644); err != nil {
		return fmt.Errorf("gen: write %s: %w", path, err)
	}
	return nil
}

// bootstrapOldPath 跑生产启动期的那串 DB 引导（顺序与 wiring_infra.go 一致）。
func bootstrapOldPath(ctx context.Context, db *sql.DB) error {
	repos, err := repository.New(entsql.OpenDB(dialect.Postgres, db), true)
	if err != nil {
		return fmt.Errorf("gen: legacy ent migrate: %w", err)
	}
	now := time.Now()
	for _, step := range []struct {
		name string
		run  func() error
	}{
		{"usagelog partition bootstrap", func() error { return repos.EnsureUsageLogPartitioned(ctx, now) }},
		{"err_logs partition bootstrap", func() error { return repos.EnsureErrLogPartitioned(ctx, now) }},
		{"usage_stats partition bootstrap", func() error { return repos.EnsureUsageStatsPartitioned(ctx, now) }},
		{"usage_entity_stats partition bootstrap", func() error { return repos.EnsureUsageEntityStatsPartitioned(ctx, now) }},
		{"routing partition bootstrap", func() error { return repos.Partitions.EnsureRoutingPartitions(ctx, now) }},
		{"price_variants effect check bootstrap", func() error { return repos.EnsurePriceVariantsEffectCheck(ctx) }},
		{"codex-search price seed bootstrap", func() error { return repos.EnsureCodexSearchSeed(ctx) }},
	} {
		if err := step.run(); err != nil {
			return fmt.Errorf("gen: legacy %s: %w", step.name, err)
		}
	}
	return nil
}

// pgDumpSchema 跑 `pg_dump --schema-only --no-owner --no-privileges --dbname=<target>`。
func pgDumpSchema(ctx context.Context, prefix []string, target string) (string, error) {
	args := append(append([]string{}, prefix[1:]...),
		"--schema-only", "--no-owner", "--no-privileges", "--dbname="+target)
	cmd := exec.CommandContext(ctx, prefix[0], args...)
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gen: %s failed: %w: %s", strings.Join(prefix, " "), err, strings.TrimSpace(stderr.String()))
	}
	return string(out), nil
}

// normalizeDump 去掉 psql 专有的反斜杠元命令：pg_dump ≥17.6 会输出
// `\restrict <随机串>` / `\unrestrict <随机串>`（每次不同，且不是 SQL），
// database/sql 无法执行，也会破坏产物的确定性。
func normalizeDump(dump string) string {
	var kept []string
	for _, line := range strings.Split(strings.ReplaceAll(dump, "\r\n", "\n"), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), `\`) {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimRight(strings.Join(kept, "\n"), "\n") + "\n"
}

// legacyHeader 是冻结产物的头注释（谁在什么 commit、哪个 PG 上生成的，以及
// 「M0b 之后不可再生」的说明）。
func legacyHeader(commit string, serverVersion int) string {
	if commit == "" {
		commit = "unknown"
	}
	return fmt.Sprintf(`-- LEGACY FIXTURE — schema-only dump of a database built by the OLD bootstrap path.
-- produced-by: go run ./internal/db/migrate/gen/cmd -legacy
-- base-commit: %s（spec 命名的存量基线；schema 由**仍在仓库里的**老 bootstrap 路径产出）
%s %d
-- 内容 = ent 自动迁移 + 五个分区 bootstrap + price_variants CHECK + codex-search seed。
-- 用途：T2「老路径建出的 schema == 0001 baseline」的唯一依据。
-- 只能在 M0b 拆掉老路径之前生成；之后再想重跑会编译失败（老方法已不存在）。
-- DO NOT EDIT.
`, commit, pgVersionPrefix, serverVersion)
}
