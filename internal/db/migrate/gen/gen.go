// Package gen 生成 0001_baseline 迁移（up/down）与基线结构指纹快照。
//
// 单一建表真相原则：本生成器不复制任何列定义——
//   - ent 侧：`migrate.NewSchema(drv).WriteTo(…)` 直接问 ent 要 DDL（钩子
//     repository.MigrateHookExcludesPartitioned 让 ent 跳过分区父表）；
//   - 分区/静态表侧：repository.MigrationBaselineDDLs() 返回由既有事实源
//     （partitionedCreateDDL / *ColumnDefs / *IndexDDLs / statsAggWatermarkDDL /
//     routingFlowSnapshotStateDDL）拼出的 DDL；
//   - 指纹侧：把生成的 up.sql 真的跑在一个空临时库上，再用 catalog dumper 导出
//     规范化文本——不手写、不冻结人工快照。
//
// 为什么需要真实 PG：ent 的 WriteTo 走 atlas 的 ModeInspect，必须 inspect 连接上
// 的库（NewMigrate 还会先 `SHOW server_version_num`）。故生成器连到 Config.DSN
// 指向的维护库，建临时空库 → 生成 → 应用 → 导出 → DROP。
//
// 用法（默认路径按本包源码目录解析，与 cwd 无关）：
//
//	go generate ./internal/db/migrate/gen        # 重写 migrations/ 与 testdata/
//	go run ./internal/db/migrate/gen/cmd -check  # CI：重新生成并比对，漂移即非零退出
//
// 产物目录的默认值是 `<本包目录>/../migrations` 与 `<本包目录>/../testdata`，
// 所以从 repo 根、从包目录、从任意 cwd 运行都指向同一份产物；-migrations /
// -testdata 可显式覆盖。`-trimpath` 构建下拿不到源码路径，默认解析会**报错**
// 要求显式 flag，而不是退回 cwd 猜。
package gen

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/is7qin/c3api/internal/db/migrate/catalog"
	entmigrate "github.com/is7qin/c3api/internal/ent/migrate"
	"github.com/is7qin/c3api/internal/repository"
)

//go:generate go run ./cmd
//go:generate go run ./cmd -snapshot-head

// 产物文件名。
const (
	upFile      = "0001_baseline.up.sql"
	downFile    = "0001_baseline.down.sql"
	catalogFile = "0001.catalog.txt"
)

// Config 是生成器配置。零值字段用默认值（DSN 见 withDSN，产物目录见
// withArtifactDirs）。
type Config struct {
	// DSN 是维护库连接串：生成器会在它上面 CREATE DATABASE 临时空库（需要
	// CREATEDB 权限）。
	DSN string
	// MigrationsDir / TestdataDir 是产物目录；空值按本包源码目录解析为绝对
	// 路径（见 defaultDirs），与 cwd 无关。
	MigrationsDir string
	TestdataDir   string
	// ForceVersion 让 -check 在「跨 PG major」时只告警不报错（-force-version）。
	ForceVersion bool
}

// Artifacts 是三个生成产物 + 供实现报告使用的硬数字。
type Artifacts struct {
	UpSQL      string
	DownSQL    string
	CatalogTxt string
	Stats      Stats
	// ServerVersionNum 是产出这些 DDL 的 PostgreSQL `server_version_num`
	// （写进 up.sql 头注释，-check 据此判定版本漂移）。
	ServerVersionNum int
}

// Stats 是生成统计。
type Stats struct {
	EntTables        int
	PartitionedTable int
	StaticTables     int
	Statements       int
	UpLines          int
	DownLines        int
	CatalogLines     int
}

// Files 返回「路径 → 内容」的产物映射。
func (a Artifacts) Files(cfg Config) (map[string]string, error) {
	cfg, err := cfg.withArtifactDirs()
	if err != nil {
		return nil, err
	}
	return map[string]string{
		filepath.Join(cfg.MigrationsDir, upFile):    a.UpSQL,
		filepath.Join(cfg.MigrationsDir, downFile):  a.DownSQL,
		filepath.Join(cfg.TestdataDir, catalogFile): a.CatalogTxt,
	}, nil
}

// Write 把产物落盘（MkdirAll + 0644）。
func (a Artifacts) Write(cfg Config) error {
	files, err := a.Files(cfg)
	if err != nil {
		return err
	}
	for _, path := range sortedKeys(files) {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("gen: mkdir %s: %w", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(files[path]), 0o644); err != nil {
			return fmt.Errorf("gen: write %s: %w", path, err)
		}
	}
	return nil
}

// withDSN 只填补 DSN——生成产物（Run）不需要产物目录，不该被目录解析拖下水。
func (cfg Config) withDSN() Config {
	if cfg.DSN == "" {
		cfg.DSN = DefaultDSN()
	}
	return cfg
}

// withArtifactDirs 填补产物目录。默认值是**可失败的解析**：拿不到本包源码位置
// 时宁可报错，也不退回 cwd 猜。
func (cfg Config) withArtifactDirs() (Config, error) {
	cfg = cfg.withDSN()
	if cfg.MigrationsDir == "" || cfg.TestdataDir == "" {
		migrationsDir, testdataDir, err := defaultDirs()
		if err != nil {
			return Config{}, err
		}
		if cfg.MigrationsDir == "" {
			cfg.MigrationsDir = migrationsDir
		}
		if cfg.TestdataDir == "" {
			cfg.TestdataDir = testdataDir
		}
	}
	return cfg, nil
}

// defaultDirs 把产物目录解析为**相对本包源码位置**的绝对路径，并确认它们真的
// 存在：产物（migrations/testdata）是源码树的一部分；源码位置解析到了但产物不在
// （二进制被搬到源码树外、checkout 不完整）就是「装错了」，必须报错要求显式 flag
// ——绝不回退 cwd 猜。
func defaultDirs() (migrationsDir, testdataDir string, err error) {
	return defaultDirsFrom(packageDir())
}

// defaultDirsFrom 是 defaultDirs 的内核（测试可注入包目录/定位结果）。
func defaultDirsFrom(pkgDir string, located bool) (migrationsDir, testdataDir string, err error) {
	migrationsDir, testdataDir, err = resolveDefaultDirs(pkgDir, located)
	if err != nil {
		return "", "", err
	}
	return requireDirsExist(migrationsDir, testdataDir)
}

// requireDirsExist 校验解析出的默认目录真实存在（是目录），返回可读的诊断。
func requireDirsExist(migrationsDir, testdataDir string) (string, string, error) {
	var missing []string
	for _, dir := range []string{migrationsDir, testdataDir} {
		if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
			missing = append(missing, dir)
		}
	}
	if len(missing) > 0 {
		return "", "", fmt.Errorf(
			"gen: default artifact dir(s) %s not found next to the generator source (the binary was installed outside the source tree, or the checkout is incomplete); cwd is not used as a fallback: pass -migrations and -testdata explicitly",
			strings.Join(missing, ", "))
	}
	return migrationsDir, testdataDir, nil
}

// packageDir 返回本包源码目录。`-trimpath` 构建下 runtime.Caller 只给出模块内
// 相对路径（或失败），此时 ok=false——调用方必须报错要求显式 flag。
func packageDir() (dir string, ok bool) {
	_, file, _, ok := runtime.Caller(0)
	if !ok || !filepath.IsAbs(file) {
		return "", false
	}
	return filepath.Dir(file), true
}

// resolveDefaultDirs 是默认目录解析的纯函数内核（located 由 packageDir 给出，
// 测试可直接注入「拿不到源码路径」这一支）。
func resolveDefaultDirs(pkgDir string, located bool) (migrationsDir, testdataDir string, err error) {
	if located {
		return filepath.Join(pkgDir, "..", "migrations"), filepath.Join(pkgDir, "..", "testdata"), nil
	}
	wd, wdErr := os.Getwd()
	if wdErr != nil {
		wd = "<unknown>"
	}
	return "", "", fmt.Errorf(
		"gen: cannot resolve the generator source dir (built with -trimpath?); default artifact dirs %s and %s are relative to internal/db/migrate/gen and cwd %s is deliberately not used: pass -migrations and -testdata explicitly",
		filepath.Join("..", "migrations"), filepath.Join("..", "testdata"), wd)
}

// DefaultDSN 从环境变量取维护库 DSN（MIGRATE_GEN_DSN > TEST_DATABASE_URL > 本地
// compose 的 postgres 库）。
func DefaultDSN() string {
	for _, key := range []string{"MIGRATE_GEN_DSN", "TEST_DATABASE_URL"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return v
		}
	}
	return "postgres://postgres:c3api@localhost:15432/postgres"
}

// Run 生成三个产物（不落盘）。
func Run(ctx context.Context, cfg Config) (Artifacts, error) {
	cfg = cfg.withDSN()
	statements, entTables, serverVersion, err := buildStatements(ctx, cfg.DSN)
	if err != nil {
		return Artifacts{}, err
	}
	up, stats, err := renderUp(statements, entTables, serverVersion)
	if err != nil {
		return Artifacts{}, err
	}
	down := renderDown(statements)
	catalogTxt, err := renderCatalog(ctx, cfg.DSN, up)
	if err != nil {
		return Artifacts{}, err
	}
	stats.DownLines = countLines(down)
	stats.CatalogLines = countLines(catalogTxt)
	return Artifacts{UpSQL: up, DownSQL: down, CatalogTxt: catalogTxt, Stats: stats, ServerVersionNum: serverVersion}, nil
}

// pgVersionPrefix 是产物头注释里的 PG 版本标记行前缀。
const pgVersionPrefix = "-- generated-by-pg:"

// pgVersionLine 匹配 up.sql 里的版本标记行（内容比对时将其剔除，版本策略单独判）。
var pgVersionLine = regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(pgVersionPrefix) + ` \d+$`)

// Check 是 -check 模式：重新生成并与已提交文件逐字节比对；漂移返回错误
// （CLI 据此非零退出）。
//
// PG 版本策略：产物头注释记录生成时的 `server_version_num`。
//   - 相同：正常比对；
//   - 同 major 不同小版本：**只告警**（catalog/DDL 输出在同一 major 内稳定）；
//   - 跨 major：报错，除非 Config.ForceVersion（cross-major 的 pg_catalog 输出与
//     ent/atlas inspect 行为可能变，这个差异必须是人决定的）。
//
// 返回的 warnings 供 CLI 打到 stderr（不影响退出码）。
func Check(ctx context.Context, cfg Config) (warnings []string, err error) {
	cfg, err = cfg.withArtifactDirs()
	if err != nil {
		return nil, err
	}
	want, err := Run(ctx, cfg)
	if err != nil {
		return nil, err
	}
	files, err := want.Files(cfg)
	if err != nil {
		return nil, err
	}
	var problems []string
	for _, path := range sortedKeys(files) {
		got, err := os.ReadFile(path)
		switch {
		case os.IsNotExist(err):
			problems = append(problems, path+": missing")
		case err != nil:
			return nil, fmt.Errorf("gen: read %s: %w", path, err)
		case path == filepath.Join(cfg.MigrationsDir, upFile):
			recorded, ok := recordedPGVersion(string(got))
			if !ok {
				problems = append(problems, path+": missing \""+pgVersionPrefix+" NNNNNN\" header")
				continue
			}
			warning, err := checkPGVersion(recorded, want.ServerVersionNum, cfg.ForceVersion)
			if err != nil {
				problems = append(problems, path+": "+err.Error())
				continue
			}
			if warning != "" {
				warnings = append(warnings, warning)
			}
			// 版本标记行是元数据：比对前剔除，版本差异走上面的策略而不是字节比对。
			if stripPGVersionLine(string(got)) != stripPGVersionLine(files[path]) {
				problems = append(problems, path+": differs from generator output")
			}
		case string(got) != files[path]:
			problems = append(problems, path+": differs from generator output")
		}
	}
	head, err := migrationHead(cfg.MigrationsDir)
	if err != nil {
		return nil, err
	}
	if head > 1 {
		tag, headTxt, err := snapshotHead(ctx, cfg, want.UpSQL)
		if err != nil {
			return nil, err
		}
		path := filepath.Join(cfg.TestdataDir, tag+".catalog.txt")
		got, err := os.ReadFile(path)
		switch {
		case os.IsNotExist(err):
			problems = append(problems, path+": missing")
		case err != nil:
			return nil, fmt.Errorf("gen: read %s: %w", path, err)
		case string(got) != headTxt:
			problems = append(problems, path+": differs from generator output")
		}
	}
	if len(problems) > 0 {
		return warnings, fmt.Errorf("gen: committed migrations are stale — rerun `go generate ./internal/db/migrate/gen`:\n  %s",
			strings.Join(problems, "\n  "))
	}
	return warnings, nil
}

// recordedPGVersion 取产物里的 `-- generated-by-pg: NNNNNN`。
func recordedPGVersion(upSQL string) (int, bool) {
	m := pgVersionLine.FindString(upSQL)
	if m == "" {
		return 0, false
	}
	version, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(m, pgVersionPrefix)))
	if err != nil {
		return 0, false
	}
	return version, true
}

func stripPGVersionLine(upSQL string) string {
	return pgVersionLine.ReplaceAllString(upSQL, pgVersionPrefix+" <stripped>")
}

// pgMajor 取 server_version_num 的 major（PG10+ 六位数字，前两位即 major）。
func pgMajor(serverVersionNum int) int { return serverVersionNum / 10000 }

// checkPGVersion 是纯函数（可单测）：完全相同 → 无告警无错；同 major → 告警；
// 跨 major → 报错（force 时降级为告警）。
func checkPGVersion(recorded, current int, force bool) (warning string, err error) {
	if recorded == current {
		return "", nil
	}
	if pgMajor(recorded) == pgMajor(current) {
		return fmt.Sprintf("gen: PostgreSQL patch level differs from the committed artifacts (generated on %d, running %d); same major %d, staying quiet",
			recorded, current, pgMajor(current)), nil
	}
	if force {
		return fmt.Sprintf("gen: PostgreSQL major differs from the committed artifacts (generated on major %d, running major %d); forced by -force-version",
			pgMajor(recorded), pgMajor(current)), nil
	}
	return "", fmt.Errorf("generated on PostgreSQL major %d but running major %d; pg_catalog output and ent/atlas inspection can differ across majors — rerun the generator on the pinned major, or pass -force-version to accept it",
		pgMajor(recorded), pgMajor(current))
}

// kind 是语句分组（决定 up.sql 的分段注释与统计）。
type kind string

const (
	kindEnt         kind = "ent"
	kindPartitioned kind = "partitioned"
	kindStatic      kind = "static"
)

// statement 是一条待写入 up.sql 的 DDL。
type statement struct {
	kind kind
	// table 非空表示它是一条 CREATE TABLE（down 按逆序 DROP 这些表）。
	table string
	sql   string
}

// buildStatements 组装全部 DDL 语句：ent 侧在前，分区父表与静态表在后。
func buildStatements(ctx context.Context, dsn string) ([]statement, int, int, error) {
	scratch, err := newScratchDB(ctx, dsn)
	if err != nil {
		return nil, 0, 0, err
	}
	defer scratch.close()

	serverVersion, err := serverVersionNum(ctx, scratch.pool)
	if err != nil {
		return nil, 0, 0, err
	}
	ddl, err := entDDL(ctx, scratch.pool)
	if err != nil {
		return nil, 0, 0, err
	}
	entStatements, entTables, err := splitEntStatements(ddl)
	if err != nil {
		return nil, 0, 0, err
	}
	baseline, err := baselineStatements()
	if err != nil {
		return nil, 0, 0, err
	}
	return append(entStatements, baseline...), len(entTables), serverVersion, nil
}

// serverVersionNum 读服务端 `server_version_num`（PG10+ 六位，前两位是 major）。
// 生成产物依赖 pg_catalog 输出与 ent/atlas inspect 行为，跨 major 可能变，故写进
// 产物头注释让 -check 能判定。
func serverVersionNum(ctx context.Context, db *sql.DB) (int, error) {
	var version int
	if err := db.QueryRowContext(ctx, `SHOW server_version_num`).Scan(&version); err != nil {
		return 0, fmt.Errorf("gen: read server_version_num: %w", err)
	}
	return version, nil
}

// entDDL 让 ent 生成全部非分区表的 DDL（写进 buffer：不执行、只在空库上 diff）。
func entDDL(ctx context.Context, db *sql.DB) (string, error) {
	var sb strings.Builder
	drv := entsql.OpenDB(dialect.Postgres, db)
	if err := entmigrate.NewSchema(drv).WriteTo(ctx, &sb, repository.MigrateHookExcludesPartitioned()); err != nil {
		return "", fmt.Errorf("gen: ent schema WriteTo: %w", err)
	}
	return sb.String(), nil
}

// splitEntStatements 把 ent 的输出切成语句并加 public. 限定，同时收集表名。
func splitEntStatements(ddl string) ([]statement, []string, error) {
	var (
		out    []statement
		tables []string
	)
	for _, line := range strings.Split(strings.TrimSpace(ddl), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasSuffix(line, ";") {
			return nil, nil, fmt.Errorf("gen: ent DDL line does not end with ';' (output format changed?): %s", line)
		}
		stmt := qualifyEnt(line)
		entry := statement{kind: kindEnt, sql: stmt}
		if name := createdTable(stmt); name != "" {
			entry.table = name
			tables = append(tables, name)
		}
		out = append(out, entry)
	}
	if len(tables) == 0 {
		return nil, nil, fmt.Errorf("gen: ent produced no CREATE TABLE statements")
	}
	return out, tables, nil
}

// baselineStatements 把 repository 的 8 张表展开成按序语句：序列 → 建表 →
// OWNED BY → 索引（与 ensureTablePartitioned 的步骤顺序一致）。
func baselineStatements() ([]statement, error) {
	tables := repository.MigrationBaselineDDLs()
	if len(tables) == 0 {
		return nil, fmt.Errorf("gen: repository baseline DDL is empty")
	}
	var out []statement
	for _, table := range tables {
		k := kindStatic
		if table.Partitioned {
			k = kindPartitioned
		}
		if table.Sequence != "" {
			// 分区父表的 DEFAULT nextval(...) 需要序列先存在（CREATE TABLE 的
			// 默认值在解析时就要能解析该序列）。
			out = append(out, statement{kind: k, sql: "CREATE SEQUENCE public." + table.Sequence + ";"})
		}
		out = append(out, statement{kind: k, table: table.Name, sql: qualifyPlain(table.Create)})
		if table.Sequence != "" {
			// OWNED BY 让 DROP TABLE 级联回收序列（serial 同款生命周期）。
			out = append(out, statement{
				kind: k,
				sql:  fmt.Sprintf("ALTER SEQUENCE public.%s OWNED BY public.%s.id;", table.Sequence, table.Name),
			})
		}
		for _, index := range table.Indexes {
			out = append(out, statement{kind: k, sql: qualifyPlain(index)})
		}
	}
	return out, nil
}

// 语句形状：ent 侧带双引号，本仓既有 DDL 常量不带；两种都要能加限定。
var (
	reEntCreateTable = regexp.MustCompile(`^(CREATE TABLE )("?[a-z0-9_]+"?)`)
	reEntCreateIndex = regexp.MustCompile(`^(CREATE (?:UNIQUE )?INDEX "?[a-z0-9_]+"? ON )("?[a-z0-9_]+"?)`)
	reReferences     = regexp.MustCompile(`REFERENCES ("?[a-z0-9_]+"?) \(`)
	// 注意可选的 `public.` 前缀：qualifyEnt/qualifyPlain 之后表名已经带 schema，
	// 少了这个分支会把表名解析成 "public"（down 就会去 DROP public.public）。
	reCreatedTable = regexp.MustCompile(`^CREATE TABLE (?:IF NOT EXISTS )?(?:public\.)?"?([a-z0-9_]+)"?`)
	rePlainTable   = regexp.MustCompile(`^CREATE TABLE ([a-z0-9_]+) \(`)
	rePlainIndexOn = regexp.MustCompile(`^(CREATE (?:UNIQUE )?INDEX [a-z0-9_]+ ON )([a-z0-9_]+)`)
	rePlainNextval = regexp.MustCompile(`nextval\('([a-z0-9_]+)'::regclass\)`)
)

// qualifyEnt 给 ent 语句加 public. 限定（ent 输出不带 schema，靠 search_path）。
func qualifyEnt(stmt string) string {
	out := stmt
	if m := reEntCreateTable.FindStringSubmatch(out); m != nil {
		out = m[1] + "public." + m[2] + strings.TrimPrefix(out, m[0])
	}
	if m := reEntCreateIndex.FindStringSubmatch(out); m != nil {
		out = m[1] + "public." + m[2] + strings.TrimPrefix(out, m[0])
	}
	return reReferences.ReplaceAllString(out, "REFERENCES public.$1 (")
}

// qualifyPlain 给本仓既有 DDL 常量（不带引号）加 public. 限定，并把
// `DEFAULT nextval('x_id_seq'::regclass)` 里的序列改成 public. 限定。
func qualifyPlain(stmt string) string {
	out := stmt
	if m := rePlainTable.FindStringSubmatch(out); m != nil {
		// m[0] 含建表名后的 " ("，替换时必须把它补回去。
		out = "CREATE TABLE public." + m[1] + " (" + strings.TrimPrefix(out, m[0])
	}
	if m := rePlainIndexOn.FindStringSubmatch(out); m != nil {
		out = m[1] + "public." + m[2] + strings.TrimPrefix(out, m[0])
	}
	out = rePlainNextval.ReplaceAllString(out, "nextval('public.$1'::regclass)")
	return strings.TrimSuffix(strings.TrimSpace(out), ";") + ";"
}

// createdTable 返回 CREATE TABLE 语句建的表名（非建表语句返回空）。
func createdTable(stmt string) string {
	if m := reCreatedTable.FindStringSubmatch(stmt); m != nil {
		return m[1]
	}
	return ""
}

// renderUp 组装 up.sql：头注释 + 只保留两条 SET（spec §2.2 导出策略）+ 语句分段。
func renderUp(statements []statement, entTables int, serverVersion int) (string, Stats, error) {
	var (
		sb    strings.Builder
		stats = Stats{EntTables: entTables, Statements: len(statements)}
		last  kind
	)
	fmt.Fprintf(&sb, "-- %s — GENERATED FILE, DO NOT EDIT.\n", upFile)
	fmt.Fprintf(&sb, "%s %d\n", pgVersionPrefix, serverVersion)
	sb.WriteString("-- 生成器：internal/db/migrate/gen（go generate ./internal/db/migrate/gen）。\n")
	sb.WriteString("-- 内容：ent schema + 6 张分区父表 + 2 张代码内静态表；**不含任何分区实例**。\n")
	sb.WriteString("-- 手改会被 -check 判红；改结构请改事实源后重跑生成器。\n")
	sb.WriteString("SET search_path = public;\n")
	sb.WriteString("SET standard_conforming_strings = on;\n")
	for _, st := range statements {
		if st.kind != last {
			switch st.kind {
			case kindEnt:
				fmt.Fprintf(&sb, "\n-- ==== ent schema：%d 张表（含索引/FK/CHECK）====\n", entTables)
			case kindPartitioned:
				sb.WriteString("\n-- ==== 分区父表：6 张（不含任何按日分区；分区由 runtime 补齐）====\n")
			case kindStatic:
				sb.WriteString("\n-- ==== 代码内静态表：2 张（唯一建表点，runtime 不再建）====\n")
			}
			last = st.kind
		}
		if st.table != "" {
			switch st.kind {
			case kindPartitioned:
				stats.PartitionedTable++
			case kindStatic:
				stats.StaticTables++
			}
		}
		sb.WriteString(st.sql + "\n")
	}
	if stats.PartitionedTable != 6 || stats.StaticTables != 2 {
		return "", Stats{}, fmt.Errorf("gen: baseline must cover 6 partitioned + 2 static tables, counted %d + %d",
			stats.PartitionedTable, stats.StaticTables)
	}
	up := sb.String()
	stats.UpLines = countLines(up)
	return up, stats, nil
}

// renderDown 按建表逆序生成 DROP（建表顺序即依赖顺序，故逆序 drop 不需要 CASCADE）。
func renderDown(statements []statement) string {
	tables := createdTables(statements)
	var sb strings.Builder
	fmt.Fprintf(&sb, "-- %s — GENERATED FILE, DO NOT EDIT.\n", downFile)
	fmt.Fprintf(&sb, "-- ROLLBACK DROPS: 全部业务表（%d 张）及其序列/索引/数据；仅供空库与测试库使用。\n", len(tables))
	sb.WriteString("-- 逆序 DROP：建表顺序即外键依赖顺序，故逆序不需要 CASCADE。\n")
	sb.WriteString("SET search_path = public;\n")
	for i := len(tables) - 1; i >= 0; i-- {
		fmt.Fprintf(&sb, "DROP TABLE IF EXISTS %s;\n", tables[i])
	}
	return sb.String()
}

// migrationUpName is the only accepted migration filename shape. The leading
// ^ is required: Go regexp treats a leading * as a quantifier, so "^*" would
// accept arbitrary prefixes.
var migrationUpName = regexp.MustCompile(`^\d{4}_.+\.up\.sql$`)

type migrationFile struct {
	version int64
	path    string
}

// migrationFiles lists NNNN_*.up.sql in version order. Bad stems are errors,
// not skips: a typo must not silently change the head snapshot.
func migrationFiles(dir string) ([]migrationFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("gen: read migrations dir %s: %w", dir, err)
	}
	var files []migrationFile
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".up.sql") {
			continue
		}
		if !migrationUpName.MatchString(entry.Name()) {
			return nil, fmt.Errorf("gen: unexpected migration file %q (want NNNN_name.up.sql)", entry.Name())
		}
		version, err := strconv.ParseInt(entry.Name()[:4], 10, 64)
		if err != nil || version <= 0 {
			return nil, fmt.Errorf("gen: invalid migration version in %q", entry.Name())
		}
		files = append(files, migrationFile{version: version, path: filepath.Join(dir, entry.Name())})
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("gen: no *.up.sql in %s", dir)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].version < files[j].version })
	for i := 1; i < len(files); i++ {
		if files[i].version == files[i-1].version {
			return nil, fmt.Errorf("gen: duplicate migration version %04d (%s and %s)", files[i].version, files[i-1].path, files[i].path)
		}
	}
	return files, nil
}

// migrationHead returns the highest version in dir.
func migrationHead(dir string) (int64, error) {
	files, err := migrationFiles(dir)
	if err != nil {
		return 0, err
	}
	return files[len(files)-1].version, nil
}

// snapshotHead builds the catalog for the directory's head version: the freshly
// generated 0001 SQL plus every later up migration, applied to a scratch DB.
func snapshotHead(ctx context.Context, cfg Config, upSQL0001 string) (tag, catalogTxt string, err error) {
	files, err := migrationFiles(cfg.MigrationsDir)
	if err != nil {
		return "", "", err
	}
	head := files[len(files)-1].version
	tag = fmt.Sprintf("%04d", head)
	if head <= 1 {
		// A 0001-only tree still has a baseline snapshot; render it so
		// -snapshot-head can rewrite 0001.catalog.txt.
		catalogTxt, err = renderCatalog(ctx, cfg.DSN, upSQL0001)
		if err != nil {
			return "", "", err
		}
		return tag, catalogTxt, nil
	}
	var script strings.Builder
	script.WriteString(upSQL0001)
	for _, file := range files {
		if file.version <= 1 {
			continue
		}
		body, err := os.ReadFile(file.path)
		if err != nil {
			return "", "", fmt.Errorf("gen: read %s: %w", file.path, err)
		}
		if bytes.ContainsRune(body, '\r') || bytes.HasPrefix(body, []byte{0xEF, 0xBB, 0xBF}) {
			return "", "", fmt.Errorf("gen: %s must be LF-only without BOM", file.path)
		}
		script.WriteString("\n")
		script.Write(body)
	}
	catalogTxt, err = renderCatalog(ctx, cfg.DSN, script.String())
	if err != nil {
		return "", "", err
	}
	return tag, catalogTxt, nil
}

// SnapshotHead regenerates 0001 in memory, then the head snapshot. CLI use.
func SnapshotHead(ctx context.Context, cfg Config) (tag, catalogTxt string, err error) {
	cfg, err = cfg.withArtifactDirs()
	if err != nil {
		return "", "", err
	}
	artifacts, err := Run(ctx, cfg)
	if err != nil {
		return "", "", err
	}
	return snapshotHead(ctx, cfg, artifacts.UpSQL)
}

// WriteHeadSnapshot writes testdata/<tag>.catalog.txt (overwrites).
func WriteHeadSnapshot(cfg Config, tag, catalogTxt string) (string, error) {
	cfg, err := cfg.withArtifactDirs()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(cfg.TestdataDir, 0o755); err != nil {
		return "", fmt.Errorf("gen: mkdir %s: %w", cfg.TestdataDir, err)
	}
	path := filepath.Join(cfg.TestdataDir, tag+".catalog.txt")
	if err := os.WriteFile(path, []byte(catalogTxt), 0o644); err != nil {
		return "", fmt.Errorf("gen: write %s: %w", path, err)
	}
	return path, nil
}

// unregisteredPartitionParent returns the first partitioned parent absent from
// catalog.PartitionParents, so a future migration cannot silently repeat the
// adopt-oracle gap.
func unregisteredPartitionParent(static string) string {
	known := make(map[string]bool, len(catalog.PartitionParents))
	for _, parent := range catalog.PartitionParents {
		known[parent] = true
	}
	for _, line := range strings.Split(static, "\n") {
		if !strings.HasPrefix(line, "table|") || !strings.Contains(line, "|relkind=p|") {
			continue
		}
		name, _, _ := strings.Cut(strings.TrimPrefix(line, "table|"), "|")
		if !known[name] {
			return name
		}
	}
	return ""
}

// renderCatalog 在空临时库上跑一遍 up.sql，再用 catalog dumper 导出静态指纹。
func renderCatalog(ctx context.Context, dsn, upSQL string) (string, error) {
	scratch, err := newScratchDB(ctx, dsn)
	if err != nil {
		return "", err
	}
	defer scratch.close()
	if _, err := scratch.pool.ExecContext(ctx, upSQL); err != nil {
		return "", fmt.Errorf("gen: apply generated baseline on scratch database: %w", err)
	}
	m, err := catalog.Dump(ctx, scratch.pool)
	if err != nil {
		return "", err
	}
	if m.Static == "" {
		return "", fmt.Errorf("gen: catalog dumper returned an empty manifest")
	}
	if parent := unregisteredPartitionParent(m.Static); parent != "" {
		return "", fmt.Errorf("gen: partitioned parent %q is not registered in catalog.PartitionParents", parent)
	}
	if len(m.Violations) > 0 {
		return "", fmt.Errorf("gen: baseline must not create views/triggers/policies/functions, found: %s",
			strings.Join(m.Violations, "; "))
	}
	if len(m.Dynamic.Violations) > 0 {
		return "", fmt.Errorf("gen: baseline must not create partition instances: %s", strings.Join(m.Dynamic.Violations, "; "))
	}
	if len(m.Dynamic.Partitions) > 0 {
		return "", fmt.Errorf("gen: baseline created %d partition instances (must be 0)", len(m.Dynamic.Partitions))
	}
	return m.Static, nil
}

// createdTables 按建表顺序返回 public.<table>。
func createdTables(statements []statement) []string {
	var out []string
	for _, st := range statements {
		if st.table != "" {
			out = append(out, "public."+st.table)
		}
	}
	return out
}

func countLines(s string) int {
	return len(strings.Split(strings.TrimSuffix(s, "\n"), "\n"))
}

func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// scratchDB 是一次性的空库（TEMPLATE template0），用完 WITH (FORCE) 删掉。
type scratchDB struct {
	admin *sql.DB
	pool  *sql.DB
	dsn   string
}

func newScratchDB(ctx context.Context, dsn string) (*scratchDB, error) {
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("gen: %q is not a full PostgreSQL DSN", dsn)
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("gen: open maintenance database: %w", err)
	}
	suffix := make([]byte, 6)
	if _, err := rand.Read(suffix); err != nil {
		_ = admin.Close()
		return nil, fmt.Errorf("gen: random suffix: %w", err)
	}
	name := "c3api_migrate_gen_" + hex.EncodeToString(suffix)
	if _, err := admin.ExecContext(ctx, `CREATE DATABASE `+name+` TEMPLATE template0`); err != nil {
		_ = admin.Close()
		return nil, fmt.Errorf("gen: create scratch database %s (needs CREATEDB): %w", name, err)
	}
	u.Path = "/" + name
	conn := u.String()
	pool, err := sql.Open("pgx", conn)
	if err != nil {
		_, _ = admin.ExecContext(ctx, `DROP DATABASE IF EXISTS `+name+` WITH (FORCE)`)
		_ = admin.Close()
		return nil, fmt.Errorf("gen: open scratch database: %w", err)
	}
	return &scratchDB{admin: admin, pool: pool, dsn: conn}, nil
}

// databaseName 从 DSN 取库名（`/` 之后的部分）。
func (s *scratchDB) databaseName() string {
	if idx := strings.LastIndex(s.dsn, "/"); idx >= 0 {
		name := s.dsn[idx+1:]
		if q := strings.IndexByte(name, '?'); q >= 0 {
			name = name[:q]
		}
		return name
	}
	return s.dsn
}

func (s *scratchDB) close() {
	if s == nil {
		return
	}
	_ = s.pool.Close()
	// WITH (FORCE) 断掉残余连接（PG13+），失败也不影响主流程：名字带随机后缀。
	_, _ = s.admin.Exec(`DROP DATABASE IF EXISTS ` + s.databaseName() + ` WITH (FORCE)`)
	_ = s.admin.Close()
}
