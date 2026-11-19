// Package migrate 是版本化 schema 迁移的 runner：嵌入 SQL 文件、逐条单事务执行、
// 记录 checksum 与账本，并用 pg_try_advisory_lock 保证多实例串行。
//
// 设计边界（spec 2026-11-18-schema-migrations §2.1）：
//   - 迁移不可变：checksum = 文件 sha256，已应用版本内容变化即报错；改错只能新增迁移。
//   - 逐条单事务：DDL 与账本记账同事务，失败只回滚该条并报版本号。
//   - 锁用两参 pg_try_advisory_lock + 轮询 + 超时（默认 60s），持在专用连接上——
//     池复用即丢锁，与 repository.go 的既有铁律同源。
//   - Down 默认拒绝：必须显式 AllowDestructive；origin='adopted' 的行永久拒绝 down。
//   - 每条迁移在事务里先 `SET LOCAL lock_timeout = '3s'`（§2.10）：拿不到锁就快速
//     失败，绝不排队堵表。
//
// 运行时边界：app 进程**永不**执行迁移。启动只调 Verify（算水位、不写库）与
// Decide（折成 serve/readonly），`up`/`down`/`adopt` 只经命令行显式触发。
package migrate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/is7qin/c3api/internal/db/migrate/catalog"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

//go:embed testdata/*.catalog.txt
var snapshotFS embed.FS

// migrationDir 是 go:embed 的目录前缀（文件命名 NNNN_name.up.sql / .down.sql）。
const migrationDir = "migrations"

// 迁移阶段。文件名以 _contract 后缀结尾的版本属于收缩阶段，其余是 expand 阶段。
const (
	PhaseExpand   = "expand"
	PhaseContract = "contract"
)

// 账本来源：executed = 本 runner 执行；adopted = 收养来的既有库（永久禁止 down）。
const (
	OriginExecuted = "executed"
	OriginAdopted  = "adopted"
)

// 文件名后缀。
const (
	upSuffix   = ".up.sql"
	downSuffix = ".down.sql"
)

// Migration 是一个迁移版本（up/down 成对出现，down 可缺——缺则 down 报错）。
type Migration struct {
	// Version 是文件名的四位数字前缀。
	Version int64
	// Name 是去掉版本号与后缀的名字，例如 `baseline` / `attempt_logs_contract`。
	Name  string
	Phase string
	// UpSQL / DownSQL 是文件内容（DownSQL 为空表示没有 down 文件）。
	UpSQL   string
	DownSQL string
	// Checksum 是 UpSQL 的 sha256（账本里记的就是它）。
	Checksum []byte
}

// Head 返回二进制内嵌迁移的最高版本与它的阶段。
func Head() (version int64, phase string) {
	all, err := loadMigrations(migrationFS)
	if err != nil || len(all) == 0 {
		return 0, PhaseExpand
	}
	last := all[len(all)-1]
	return last.Version, last.Phase
}

// RequiredVersion 是本二进制**真正依赖**的最低 expand 版本（spec §2.13）。
//
// 它是**代码常量**，不是从迁移文件头推断出来的：仓库里没有「在线二进制版本
// 注册表」，任何从文件头/文件名推断「最老在线二进制」的机制都是假精度，只会多
// 出一处需要同步的真相。代码开始依赖新列/新表时，人跟它一起改：
//
//   - `0001_baseline` 是建表基线 ⇒ 1；
//   - `0002_account_ext_identity_group` 把导入幂等键按凭据类型收窄（同一
//     ChatGPT 账号可以同时是 Codex 与 BPS 账号）⇒ 2。没有这条索引，BPS 导入
//     会撞 0001 的两列唯一索引，功能在旧库上直接不可用，所以它是硬要求。
//   - 3 = `0003_attempt_logs`（attempt_logs 表 + err_logs.attempt_ordinal）。
//     写路径真的依赖这两处结构，缺了就是 42P01（表不存在）/ 42703（列不存在）
//     ——那是**启动期就该拦住**的失败，不是运行期 500。注意判据的方向：仓库里
//     多一条 expand 迁移本身并不提高水位（旧代码照样能跑新 schema），这里提高
//     是因为本二进制真的开始依赖它。
//
// 将来某个二进制读了新迁移建的列，它必须把这行改成那一版，否则
// `db_head < RequiredVersion` 的库会被误判成「可服务」，然后在查询里报 42703。
const RequiredVersion int64 = 3

// LegacyFixture 是「老路径建出来的 schema」冻结快照的文件名（testdata/legacy 下）。
// 它不是内嵌资源：只在 PG 用例里由 database/sql 直接执行，不进二进制。
const LegacyFixture = "aba8042e.sql"

// Snapshot 返回内嵌的静态指纹快照（testdata/<tag>.catalog.txt）。tag 是四位版本号：
//   - "0001" 是 0001 单独建出来的老库基线（legacy 收养用）；
//   - "0002" 是 0002 跑到 head 的形状（0001 + 身份组索引拆分，用户那条独立迁移）；
//   - "0003" 是当前 head 的完整 shape（0001 + 0002 + 0003）。
func Snapshot(tag string) (catalog.Manifest, error) {
	body, err := snapshotFS.ReadFile(path.Join("testdata", tag+".catalog.txt"))
	if err != nil {
		return catalog.Manifest{}, fmt.Errorf("migrate: snapshot %q: %w", tag, err)
	}
	return catalog.Manifest{Static: string(body)}, nil
}

// ErrDestructiveDenied 是 Down 未显式允许破坏性操作时的拒绝原因。
var ErrDestructiveDenied = errors.New("migrate: down is destructive and requires explicit AllowDestructive (CLI: -migrate down -allow-destructive)")

// ErrAdoptedBaseline 是 origin='adopted' 的版本被要求 down 时的拒绝原因。
var ErrAdoptedBaseline = errors.New("migrate: adopted baseline versions can never be rolled back")

// Result 是一次 Up 的结果。
type Result struct {
	// From 是执行前的最高已应用版本（0 = 空账本）。
	From int64
	// To 是执行后的最高已应用版本。
	To int64
	// Applied 是按序实际执行的版本。
	Applied []AppliedMigration
}

// AppliedMigration 是一个已应用的版本。
type AppliedMigration struct {
	Version     int64
	Name        string
	Phase       string
	Origin      string
	Checksum    []byte
	AppliedAt   string
	ExecutionMS int
}

// DownOptions 是 Down 的闸门（默认全部关闭）。
type DownOptions struct {
	// AllowDestructive 必须显式置 true——down 会 DROP 掉 up 建的东西（数据随之丢弃）。
	AllowDestructive bool
}

// DownResult 是一次 Down 的结果。
type DownResult struct {
	// Rolled 是按序实际回滚的版本（由新到旧）。
	Rolled []AppliedMigration
	// SQL 是每一步执行的 down 文件内容（供 CLI 打印后 WARN）。
	SQL []string
}

// LedgerStatus 是账本视图（Status 函数返回它）。
type LedgerStatus struct {
	Applied []AppliedMigration
	Pending []Migration
	// Mismatches 是「已应用但文件内容变了」的版本。
	Mismatches []ChecksumMismatch
}

// ChecksumMismatch 描述一个 checksum 不一致的已应用版本。
type ChecksumMismatch struct {
	Version int64
	Name    string
	// Recorded / Current 是 hex 编码的 sha256。
	Recorded string
	Current  string
}

// Verdict 是启动门需要的版本水位（spec §2.5）。
type Verdict struct {
	// BinaryHead / BinaryContract 是二进制内嵌迁移的最高版本 / 最高收缩版本。
	BinaryHead     int64
	BinaryContract int64
	// DBHead / DBContract 是库里已应用的最高版本 / 最高收缩版本。
	DBHead     int64
	DBContract int64
	// Ahead 是「已应用、但本二进制没有」的版本（> BinaryHead）。这是 k8s 回退
	// 场景下的正常状态：expand-only 保证旧二进制能跑新库，所以它是 WARN 而不是
	// 问题。低于 BinaryHead 的未知版本仍是问题（有人删了迁移）。
	Ahead []int64
	// Problems 非空表示不可服务（缺版本、收缩超前、checksum 不一致、未知版本）。
	Problems []string
}

// Compatible 表示按 expand-only 承诺可以服务当前库。
func (v Verdict) Compatible() bool { return len(v.Problems) == 0 }

// 进程级服务模式（spec §2.6）：readOnly 只由 Verify 决定，不由配置开关决定。
const (
	// ModeServe 正常服务（含「库比二进制新、contract 水位相同」的 WARN 态）。
	ModeServe = "serve"
	// ModeReadonly 不可服务：进程存活但业务/写面全拒（NotReady，不是 CrashLoop）。
	ModeReadonly = "readonly"
)

// Decision 是启动门的判定：Mode + 人话原因 + 需要 WARN 的兼容态说明。
type Decision struct {
	Mode string
	// Reason 非空 = 不可服务的原因（逐条拼接，含下一步该做什么）。
	Reason string
	// Warnings 是可以服务但必须被看见的兼容态（库比二进制新）。
	Warnings []string
}

// Ready 表示可以服务。
func (d Decision) Ready() bool { return d.Mode == ModeServe }

// Decide 把 Verify 的水位折成进程级模式（spec §2.5）：
//
//	db_head < RequiredVersion            ⇒ readonly（缺本二进制必需的列/表）
//	contract 水位超前 / checksum 漂移     ⇒ readonly（收缩已发生，本二进制真不兼容）
//	db_head > binary_head，contract 相同 ⇒ serve + WARN（k8s 回退靠这条）
//	相等                                 ⇒ serve
//
// 迁移永不在这里执行：本函数只判「能不能服务」。
func Decide(v Verdict) Decision {
	var reasons []string
	if v.DBHead < RequiredVersion {
		reasons = append(reasons, fmt.Sprintf(
			"database schema head %d is behind this binary's required version %d: run `c3api -migrate up`",
			v.DBHead, RequiredVersion))
	}
	reasons = append(reasons, v.Problems...)
	d := Decision{Mode: ModeServe}
	if len(reasons) > 0 {
		d.Mode = ModeReadonly
		d.Reason = strings.Join(reasons, "; ")
		return d
	}
	if v.DBHead > v.BinaryHead {
		d.Warnings = append(d.Warnings, fmt.Sprintf(
			"database schema head %d is ahead of this binary's head %d (contract watermarks equal: %d); serving on the expand-only guarantee",
			v.DBHead, v.BinaryHead, v.DBContract))
	}
	return d
}

// AdoptOptions 是 baseline 收养的参数（spec §2.8）。
type AdoptOptions struct {
	// Snapshot 是已知发布版本的指纹 tag（"0001" 老库基线、"0002" 0002 head、
	// "0003" 当前 head）：
	// 账本空 + 有 schema 时，当前 catalog 必须与该快照逐对象相等。
	Snapshot string
	// SchemaVersion 是 settings.schema_version 的期望值。非空且库里存了别的值
	// 即判定「账本丢失」并拒绝。
	SchemaVersion string
	// Tolerate 是「已知且可解释的静态差异」白名单：每一项必须是**一条完整的
	// 规范化指纹行**（与 catalog.Manifest.Lines 的字节逐字相等），比较用 `==`。
	// 一行 = 一个完整对象（表 / 列 / 约束 / 索引 / sequence / enum），所以
	// 「同名但 type/validated/def 不同」是另一行，命中不了白名单——不能靠名字
	// 冒充成兼容基线。它**只**放宽「库里多出的行」；基线对象缺失永远算错
	// （库不是这个版本的指纹）。命中项会回显在 AdoptResult.Tolerated，不允许
	// 静默通过。
	Tolerate []string
}

// LegacyTolerances 是「老路径独有、0001 尚未收敛」的已知结构差异。
//
// 目前只有一条：`price_variants_effect_at_least_one_v2` CHECK 由启动期
// `EnsurePriceVariantsEffectCheck`（DO $$ 块）创建，spec §3 明确把「price_variants
// CHECK 收敛进 0001」留到后续批次。收养老库时把它作为**显式**白名单传入。
//
// 这里写的是 catalog 规范化后的整行（pg_get_constraintdef 原文），name/type/
// validated/def 全参与比较：同名约束只要定义差一个字符就是另一行，Adopt 会把它
// 当漂移拒绝。跨 PG major 若 pg_get_constraintdef 的措辞变了，这一条会显式变红，
// 逼人重新审一次白名单——这是有意的（宁可要求人确认，也不静默接受不同定义）。
var LegacyTolerances = []string{
	`constraint|price_variants|name=price_variants_effect_at_least_one_v2|type=c|validated=true|def=CHECK (((mult_bp IS NOT NULL) OR (set_input_per_m IS NOT NULL) OR (set_output_per_m IS NOT NULL) OR (set_cache_read_per_m IS NOT NULL) OR (set_cache_creation_per_m IS NOT NULL) OR (set_price_per_call IS NOT NULL) OR (set_img_in_tok_per_m IS NOT NULL) OR (set_img_out_tok_per_m IS NOT NULL) OR (set_price_per_image IS NOT NULL)))`,
}

// AdoptResult 是收养结果。
type AdoptResult struct {
	// Adopted 是按序记为 applied 的版本（≤ 快照版本）。
	Adopted []AppliedMigration
	// Tolerated 是命中 AdoptOptions.Tolerate 的实际差异行（调用方必须打印）。
	Tolerated []string
}

// UpOptions 是 Up 的参数。
type UpOptions struct {
	// Target 是要升到的最大版本；<= 0 表示 Head。
	Target int64
	// AllowContract 允许执行 contract 阶段。缺省拒绝：contract 是不可逆收缩
	// （DROP TABLE / DROP COLUMN、数据随之丢弃），不能因为「Head 含它」就自动跑。
	AllowContract bool
}

// Up 把库升到 opts.Target（<= 0 表示 Head）。
//
// contract 阶段默认拒跑：任何待应用的 contract 都会让整个 up 在**写入之前**被拒，
// 并一次性列出全部被挡版本（见 UpOptions.AllowContract）。
func Up(ctx context.Context, db *sql.DB, opts UpOptions) (Result, error) {
	return UpFS(ctx, db, nil, opts)
}

// UpFS 是 Up 的「可注入迁移集」版本（fsys == nil ⇒ 内嵌迁移集，与 Up 等价）。
//
// 它存在只为造证据：生产迁移集当前**全是 expand**（0001/0002/0003），而「contract 阶段
// 默认拒跑、且拒绝发生在任何写入之前」这条 CLI 级契约必须有一条真的 contract
// 迁移才能演出来。生产路径恒走 Up，命令行也只在拿不到内嵌集时才用本变体。
func UpFS(ctx context.Context, db *sql.DB, fsys fs.FS, opts UpOptions) (Result, error) {
	if fsys == nil {
		fsys = migrationFS
	}
	return newRunner(db, fsys, LockOptions{}).up(ctx, opts)
}

// Down 回滚**最后一个**已应用版本（必须显式 AllowDestructive）。
//
// 只有一步：没有 steps 参数，也没有「一次回滚多条」的路径——生产不该有这条路
// （spec §2.9 的闸门装在「会不会毁数据」上，而多步回滚把一次误操作放大成多次）。
func Down(ctx context.Context, db *sql.DB, opts DownOptions) (DownResult, error) {
	return newRunner(db, migrationFS, LockOptions{}).down(ctx, opts)
}

// Status 返回账本视图。
func Status(ctx context.Context, db *sql.DB) (LedgerStatus, error) {
	return newRunner(db, migrationFS, LockOptions{}).status(ctx)
}

// Inspect 返回当前库的结构指纹（catalog dumper）。
func Inspect(ctx context.Context, db *sql.DB) (catalog.Manifest, error) {
	return catalog.Dump(ctx, db)
}

// Verify 计算启动门四态所需的水位，不改变任何东西。
func Verify(ctx context.Context, db *sql.DB) (Verdict, error) {
	return newRunner(db, migrationFS, LockOptions{}).verify(ctx)
}

// Adopt 把「账本空 + 已有 schema」的库收养为 baseline 版本。
func Adopt(ctx context.Context, db *sql.DB, opts AdoptOptions) (AdoptResult, error) {
	return newRunner(db, migrationFS, LockOptions{}).adopt(ctx, opts)
}

// loadMigrations 解析 fs 里的迁移文件对，按版本升序返回。
func loadMigrations(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, migrationDir)
	if err != nil {
		return nil, fmt.Errorf("migrate: read %s dir: %w", migrationDir, err)
	}
	byVersion := make(map[int64]*Migration, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		var file string
		switch {
		case strings.HasSuffix(name, upSuffix):
			file = upSuffix
		case strings.HasSuffix(name, downSuffix):
			file = downSuffix
		default:
			return nil, fmt.Errorf("migrate: %s: unexpected file name (want NNNN_name.up.sql or .down.sql)", name)
		}
		stem := strings.TrimSuffix(name, file)
		version, migrationName, ok := parseStem(stem)
		if !ok {
			return nil, fmt.Errorf("migrate: %s: want NNNN_name%s", name, file)
		}
		body, err := fs.ReadFile(fsys, path.Join(migrationDir, name))
		if err != nil {
			return nil, fmt.Errorf("migrate: read %s: %w", name, err)
		}
		m := byVersion[version]
		if m == nil {
			m = &Migration{Version: version, Name: migrationName, Phase: phaseOf(migrationName)}
			byVersion[version] = m
		} else if m.Name != migrationName {
			return nil, fmt.Errorf("migrate: version %d has two names: %q and %q", version, m.Name, migrationName)
		}
		// 规范化后再使用：**执行的内容与 checksum 覆盖的内容必须是同一串字节**
		// （否则「记账的 checksum」与「跑过的 SQL」是两个真相）。
		canonical := canonicalSQL(body)
		if file == upSuffix {
			m.UpSQL = string(canonical)
			sum := sha256.Sum256(canonical)
			m.Checksum = sum[:]
		} else {
			m.DownSQL = string(canonical)
		}
	}
	out := make([]Migration, 0, len(byVersion))
	for _, m := range byVersion {
		out = append(out, *m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// canonicalSQL 把迁移文件字节规范化为「与编辑器/检出设置无关」的形态：
//   - 去 UTF-8 BOM（Windows 编辑器与 PowerShell 重定向常带）；
//   - CRLF / 单独 CR 统一成 LF（core.autocrlf、CRLF 检出会让原始字节变、SQL 语义不变）。
//
// 只做这两件事：任何更强的重写（去尾随空格、改大小写）都会让「文件内容」与
// 「实际执行内容」不再一致，进而让 checksum 不再能证明跑过什么。
func canonicalSQL(body []byte) []byte {
	body = bytes.TrimPrefix(body, []byte{0xEF, 0xBB, 0xBF})
	if !bytes.ContainsRune(body, '\r') {
		return body
	}
	normalized := bytes.ReplaceAll(body, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(normalized, []byte("\r"), []byte("\n"))
}

// parseStem 拆 `NNNN_name`（四位数字版本前缀）。
func parseStem(stem string) (version int64, name string, ok bool) {
	head, tail, found := strings.Cut(stem, "_")
	if !found || len(head) != 4 || tail == "" {
		return 0, "", false
	}
	version, err := strconv.ParseInt(head, 10, 64)
	if err != nil || version <= 0 {
		return 0, "", false
	}
	return version, tail, true
}

func phaseOf(name string) string {
	if strings.HasSuffix(name, "_contract") {
		return PhaseContract
	}
	return PhaseExpand
}

// forbiddenSQLPattern 是**所有 phase** 都禁的构造（spec §2.4 的兜底 lint）：
//   - `DO $$…$$` 让 lint 出现盲区；
//   - `CREATE INDEX CONCURRENTLY` 与「单事务」互斥（实测 cannot run inside a transaction block）。
var forbiddenSQLPattern = regexp.MustCompile(`(?i)\bDO\s*\$|CREATE\s+INDEX\s+CONCURRENTLY`)

// expandForbiddenSQLPattern 是 expand 阶段**额外**禁止的收缩动作。contract 阶段
// 存在的意义就是收缩（删旧表/旧列），所以它只对 PhaseExpand 生效——contract 的
// 破坏性由 AllowDestructive 与 contract 水位闸门把关，不由文本 lint 把关。
// 未知 phase 按 expand 从严（fail-closed）。
var expandForbiddenSQLPattern = regexp.MustCompile(`(?i)\bDROP\s+(?:TABLE|SCHEMA)\b|\bTRUNCATE\b`)

// commentLineRE 去掉行注释，避免注释里的字样被误判（down 文件头就有 ROLLBACK DROPS）。
var commentLineRE = regexp.MustCompile(`(?m)^\s*--.*$`)

// validateSQL 拒绝禁用构造：DO $$ 与 CREATE INDEX CONCURRENTLY 对所有 phase 生效；
// DROP TABLE|SCHEMA 与 TRUNCATE 是收缩动作，只在 expand 阶段被拒（contract 阶段
// 允许，见 expandForbiddenSQLPattern）。
func validateSQL(m Migration) error {
	body := commentLineRE.ReplaceAllString(m.UpSQL, "")
	if match := forbiddenSQLPattern.FindString(body); match != "" {
		return fmt.Errorf("migrate: %04d_%s.up.sql: forbidden construct %q (DO $$ / CREATE INDEX CONCURRENTLY are not allowed in any migration phase)",
			m.Version, m.Name, match)
	}
	if m.Phase != PhaseContract {
		if match := expandForbiddenSQLPattern.FindString(body); match != "" {
			return fmt.Errorf("migrate: %04d_%s.up.sql: forbidden construct %q in an expand migration (DROP TABLE/SCHEMA and TRUNCATE belong in a *_contract migration, guarded by AllowDestructive / the contract watermark)",
				m.Version, m.Name, match)
		}
	}
	return nil
}

// ensureControl 幂等创建账本表——它不属于任何迁移版本（由 runner 负责）。
func ensureControl(ctx context.Context, q execer) error {
	_, err := q.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS public.`+catalog.ControlTable+` (
	version bigint PRIMARY KEY,
	name text NOT NULL,
	checksum bytea NOT NULL,
	phase text NOT NULL,
	origin text NOT NULL,
	applied_at timestamptz NOT NULL,
	execution_ms int NOT NULL
)`)
	if err != nil {
		return fmt.Errorf("migrate: create %s: %w", catalog.ControlTable, err)
	}
	return nil
}

// execer 是 *sql.DB / *sql.Tx 的公共子集。
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func isMissingRelation(err error) bool {
	var sqlState interface{ SQLState() string }
	if errors.As(err, &sqlState) {
		return sqlState.SQLState() == "42P01"
	}
	return strings.Contains(err.Error(), "42P01")
}
