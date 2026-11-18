// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package migrate

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/db/migrate/catalog"
	"github.com/is7qin/c3api/internal/testsupport/pgtest"
)

// PG 用例（T1/T4/T7/T8）：全部真跑本地 PG（pgtest.CloneEmpty 给一个空库），
// PG 测试 DSN 未设置时 skip（pgtest 统一约定）。

// newEmptyDB 在私有空库上开一个 *sql.DB（runner 需要真正的连接池语义：
// 专用连接持锁 + 多条并发连接）。
func newEmptyDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := pgtest.CloneEmpty(t)
	db, err := sql.Open("pgx", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	require.NoError(t, db.PingContext(context.Background()))
	return db
}

// probeFS 造一个最小的两版本迁移集：0001 建表，0002 由调用方决定（expand 或收缩）。
func probeFS(second string) fstest.MapFS {
	const first = `CREATE TABLE public.probe_items (
	id bigint NOT NULL,
	note varchar NOT NULL DEFAULT '',
	PRIMARY KEY (id)
);`
	return fstest.MapFS{
		"migrations/0001_probe.up.sql":      &fstest.MapFile{Data: []byte(first)},
		"migrations/0002_probe_next.up.sql": &fstest.MapFile{Data: []byte(second)},
	}
}

// oneUp 造一个只有一条 up 的迁移集（R2-1 的换行/BOM 用例用）。
func oneUp(name, body string) fstest.MapFS {
	return fstest.MapFS{"migrations/" + name: &fstest.MapFile{Data: []byte(body)}}
}

// contractFS 造「0001 expand + 0002/0003 contract」的收缩迁移集。
func contractFS() fstest.MapFS {
	return fstest.MapFS{
		"migrations/0001_probe.up.sql": &fstest.MapFile{Data: []byte(
			"CREATE TABLE public.probe_expand (id bigint PRIMARY KEY);\n" +
				"CREATE TABLE public.probe_pair (id bigint PRIMARY KEY);\n")},
		"migrations/0002_cleanup_contract.up.sql": &fstest.MapFile{Data: []byte(
			"DROP TABLE public.probe_expand;\n")},
		"migrations/0003_drop_pair_contract.up.sql": &fstest.MapFile{Data: []byte(
			"DROP TABLE public.probe_pair;\n")},
	}
}

func openCount(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM public.`+table).Scan(&n))
	return n
}

// relationExists 判断表是否还在（refused down 之后结构不能被动过）。
func relationExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()
	var ok bool
	require.NoError(t, db.QueryRow(`SELECT to_regclass('public.`+table+`') IS NOT NULL`).Scan(&ok))
	return ok
}

// appliedVersions 返回账本里的版本号（升序）。断言「哪一条被记账/被忘记」比数
// 行数精确：整批事务、顺序反了这类变异会把行数保持不变而版本集合变错。
func appliedVersions(t *testing.T, db *sql.DB) []int64 {
	t.Helper()
	rows, err := db.Query(`SELECT version FROM public.` + catalog.ControlTable + ` ORDER BY version`)
	require.NoError(t, err)
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var version int64
		require.NoError(t, rows.Scan(&version))
		out = append(out, version)
	}
	require.NoError(t, rows.Err())
	return out
}

// T1：空库 Up 后静态 manifest == 已提交的 0001 快照；动态规则通过。
func TestUpMatchesBaselineSnapshot(t *testing.T) {
	ctx := context.Background()
	db := newEmptyDB(t)

	res, err := Up(ctx, db, UpOptions{})
	require.NoError(t, err)
	require.Equal(t, int64(0), res.From)
	require.Equal(t, int64(1), res.To)
	require.Len(t, res.Applied, 1)
	require.Equal(t, OriginExecuted, res.Applied[0].Origin)
	require.GreaterOrEqual(t, res.Applied[0].ExecutionMS, 0)

	started := time.Now()
	got, err := Inspect(ctx, db)
	require.NoError(t, err)
	dumpTook := time.Since(started)
	t.Logf("catalog dump of the full 0001 schema: %s (%d static objects)", dumpTook.Round(time.Millisecond), len(got.Lines()))
	require.Less(t, dumpTook, 5*time.Second, "catalog dump must stay under 5s (schema-qualified pg_catalog queries)")

	want, err := Snapshot("0001")
	require.NoError(t, err)
	// 两个方向都断言：缺失 = I2 红；多出 = 有人改了 0001 却没重跑生成器。
	require.Empty(t, catalog.Missing(want, got), "I2: migrated catalog must be a superset of the 0001 snapshot")
	require.Empty(t, catalog.Missing(got, want), "catalog must not grow beyond the committed 0001 snapshot (rerun go generate)")
	require.Equal(t, want.Static, got.Static)

	require.Empty(t, got.Dynamic.Violations, "dynamic partition rules must hold")
	require.Empty(t, got.Dynamic.Partitions, "0001 must not create partition instances")
	require.Empty(t, got.Violations, "0001 must not create views/triggers/policies/functions")
	require.Empty(t, catalog.UnexpectedExtensions(got, catalog.BaseExtensions))

	ledger, err := Status(ctx, db)
	require.NoError(t, err)
	require.Len(t, ledger.Applied, 1)
	require.Empty(t, ledger.Pending)
	require.Empty(t, ledger.Mismatches)

	verdict, err := Verify(ctx, db)
	require.NoError(t, err)
	require.Equal(t, int64(1), verdict.DBHead)
	require.Equal(t, int64(0), verdict.DBContract)
	require.Equal(t, int64(1), verdict.BinaryHead)
	require.True(t, verdict.Compatible(), strings.Join(verdict.Problems, "; "))
}

// T4：I3（expand-only）。同一条测试迁移写成 ADD COLUMN 必须通过，写成 DROP COLUMN
// 必须让 I3 红——否则「新 ⊇ 旧」是空话。
func TestExpandOnlyInvariant(t *testing.T) {
	ctx := context.Background()

	run := func(t *testing.T, second string) (catalog.Manifest, catalog.Manifest) {
		t.Helper()
		db := newEmptyDB(t)
		r := newRunner(db, probeFS(second), LockOptions{})
		_, err := r.up(ctx, UpOptions{Target: 1})
		require.NoError(t, err)
		before, err := catalog.Dump(ctx, db)
		require.NoError(t, err)
		_, err = r.up(ctx, UpOptions{})
		require.NoError(t, err)
		after, err := catalog.Dump(ctx, db)
		require.NoError(t, err)
		require.NotEqual(t, before.Static, after.Static, "the second migration must change the catalog")
		return before, after
	}

	t.Run("expand passes", func(t *testing.T) {
		before, after := run(t, "ALTER TABLE public.probe_items ADD COLUMN experiment varchar NULL;\n")
		require.Empty(t, catalog.ExpandOnly(before, after))
	})

	t.Run("drop column fails I3", func(t *testing.T) {
		before, after := run(t, "ALTER TABLE public.probe_items DROP COLUMN note;\n")
		violations := catalog.ExpandOnly(before, after)
		require.NotEmpty(t, violations)
		require.Contains(t, strings.Join(violations, "\n"), "name=note")
	})

	t.Run("new unique index on existing table fails I3", func(t *testing.T) {
		before, after := run(t, "CREATE UNIQUE INDEX probe_items_note ON public.probe_items (note);\n")
		violations := catalog.ExpandOnly(before, after)
		require.NotEmpty(t, violations)
		require.Contains(t, strings.Join(violations, "\n"), "tightening")
	})
}

// 生成的 down 必须真能拆：up → down → up 之后 catalog 逐字段相等（0001 是建表类
// 迁移，spec §2.9 的「列集语义相等」在这里可以升格为逐字节相等）。
func TestBaselineRoundTrip(t *testing.T) {
	ctx := context.Background()
	db := newEmptyDB(t)

	_, err := Up(ctx, db, UpOptions{})
	require.NoError(t, err)
	before, err := Inspect(ctx, db)
	require.NoError(t, err)

	down, err := Down(ctx, db, DownOptions{AllowDestructive: true})
	require.NoError(t, err)
	require.Len(t, down.Rolled, 1)
	require.Contains(t, down.SQL[0], "ROLLBACK DROPS:")
	var leftovers int
	require.NoError(t, db.QueryRowContext(ctx,
		`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = 'public' AND c.relkind IN ('r', 'p') AND c.relname <> $1`,
		catalog.ControlTable).Scan(&leftovers))
	require.Zero(t, leftovers, "down must drop every table it created")

	_, err = Up(ctx, db, UpOptions{})
	require.NoError(t, err)
	after, err := Inspect(ctx, db)
	require.NoError(t, err)
	require.Equal(t, before.Static, after.Static, "round trip must converge to the same catalog")
}

// execScript 执行多语句 SQL 脚本（pgx 的无参 Exec 走 simple 协议，可一次多语句）。
func execScript(t *testing.T, db *sql.DB, script string) {
	t.Helper()
	_, err := db.ExecContext(context.Background(), script)
	require.NoError(t, err)
}

// T2：老路径（ent 自动迁移 + 五个 Ensure* + price_variants CHECK + codex-search seed）
// 建出来的库，与 0001 baseline 收敛到同一份静态指纹。
//
// **不 import internal/repository**：夹具是静态 SQL，直接 database/sql 执行——M0b 之后
// repository 会 import 本包，测试里 import 它会成环。
func TestLegacyBootstrapConvergesToBaseline(t *testing.T) {
	ctx := context.Background()
	fixture, err := os.ReadFile(filepath.Join("testdata", "legacy", LegacyFixture))
	require.NoError(t, err, "the legacy fixture must be committed (regenerate with gen -legacy before M0b)")
	require.NotContains(t, string(fixture), "\n\\restrict", "psql meta-commands must be stripped")

	db := newEmptyDB(t)
	execScript(t, db, string(fixture))

	legacy, err := Inspect(ctx, db)
	require.NoError(t, err)
	require.NotEmpty(t, legacy.Static)
	require.Empty(t, legacy.Dynamic.Violations, "the legacy fixture carries day partitions; they must be valid instances")
	require.NotEmpty(t, legacy.Dynamic.Partitions, "the fixture must include day partitions (else the exclusion is untested)")

	adopted, err := Adopt(ctx, db, AdoptOptions{Snapshot: "0001", Tolerate: LegacyTolerances})
	require.NoError(t, err, "the legacy catalog must match the 0001 snapshot outside the documented allowlist")
	require.Len(t, adopted.Adopted, 1)
	require.Equal(t, OriginAdopted, adopted.Adopted[0].Origin)
	// 白名单必须是**活的**：老路径确实多造了 price_variants 的那条 CHECK，否则这条
	// 白名单就成了掩盖未来漂移的遮羞布。
	require.Len(t, adopted.Tolerated, 1, "exactly one known legacy delta (price_variants CHECK)")
	require.Contains(t, adopted.Tolerated[0], "price_variants_effect_at_least_one_v2")

	migrated, err := Inspect(ctx, db)
	require.NoError(t, err)
	require.Equal(t, legacy.Static, migrated.Static, "A == B: up (here a no-op after adopt) must not drift the schema")

	golden, err := Snapshot("0001")
	require.NoError(t, err)
	for _, manifest := range []catalog.Manifest{legacy, migrated} {
		tolerated, diff := splitStaticDiff(golden, manifest, LegacyTolerances)
		require.Empty(t, diff, "outside the allowlist the legacy schema must equal the 0001 snapshot")
		require.Equal(t, adopted.Tolerated, tolerated, "the tolerated set must be exactly what Adopt reported")
	}
}

// R3-B：白名单按**整行**精确比较。把同名 CHECK 的定义改掉（名字一个字符都不变），
// Adopt 必须报 diff——前缀/正则式匹配会在这里把「错误定义」当成兼容基线放行。
func TestAdoptRejectsSameNameDifferentConstraintDef(t *testing.T) {
	ctx := context.Background()
	fixture, err := os.ReadFile(filepath.Join("testdata", "legacy", LegacyFixture))
	require.NoError(t, err)
	db := newEmptyDB(t)
	execScript(t, db, string(fixture))

	// 同名、不同 def：语义从「至少一个价格字段非空」缩成「只看 mult_bp」。
	execScript(t, db, `
ALTER TABLE public.price_variants DROP CONSTRAINT price_variants_effect_at_least_one_v2;
ALTER TABLE public.price_variants ADD CONSTRAINT price_variants_effect_at_least_one_v2 CHECK (mult_bp IS NOT NULL);
`)
	_, err = Adopt(ctx, db, AdoptOptions{Snapshot: "0001", Tolerate: LegacyTolerances})
	require.Error(t, err, "a same-name constraint with a different definition must not be tolerated")
	require.Contains(t, err.Error(), "does not match snapshot 0001")
	require.Contains(t, err.Error(),
		"unexpected: constraint|price_variants|name=price_variants_effect_at_least_one_v2|type=c|validated=true|def=CHECK (")
	require.Contains(t, err.Error(), "mult_bp IS NOT NULL")
	require.NotContains(t, err.Error(), "missing:", "the rejection must come from the extra (wrong) line, not from a lost baseline object")
}

// R2-1（PG 侧）：CRLF 检出的迁移照常执行，且换行差异不制造假 mismatch。
func TestCRLFMigrationExecutesAndDoesNotMismatch(t *testing.T) {
	ctx := context.Background()
	db := newEmptyDB(t)
	const lf = "CREATE TABLE public.probe_crlf (\n\tid bigint NOT NULL,\n\tnote text NULL,\n\tPRIMARY KEY (id)\n);\n"
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")

	crlfRunner := newRunner(db, oneUp("0001_probe.up.sql", crlf), LockOptions{})
	_, err := crlfRunner.up(ctx, UpOptions{})
	require.NoError(t, err)
	require.True(t, relationExists(t, db, "probe_crlf"), "a CRLF file must execute like its LF twin")
	require.Equal(t, 1, openCount(t, db, catalog.ControlTable))

	// 换行被 git/core.autocrlf 改写后重新检出（LF）⇒ 不算漂移：同一个 checksum。
	lfRunner := newRunner(db, oneUp("0001_probe.up.sql", lf), LockOptions{})
	_, err = lfRunner.up(ctx, UpOptions{})
	require.NoError(t, err, "CRLF vs LF must not be reported as a checksum mismatch")
	status, err := lfRunner.status(ctx)
	require.NoError(t, err)
	require.Empty(t, status.Mismatches)
	require.Empty(t, status.Pending)
}

// T7：改一字节 ⇒ checksum mismatch（up 拒绝、status 报 mismatch）。
func TestChecksumMismatchIsFatal(t *testing.T) {
	ctx := context.Background()
	db := newEmptyDB(t)
	const name = "migrations/0001_probe.up.sql"

	applied := newRunner(db, fstest.MapFS{
		name: &fstest.MapFile{Data: []byte("CREATE TABLE public.probe (id bigint PRIMARY KEY);\n")},
	}, LockOptions{})
	_, err := applied.up(ctx, UpOptions{})
	require.NoError(t, err)

	tamperedFS := fstest.MapFS{
		name: &fstest.MapFile{Data: []byte("CREATE TABLE public.probe (id bigint PRIMARY KEY, extra text NULL);\n")},
	}
	tampered := newRunner(db, tamperedFS, LockOptions{})
	_, err = tampered.up(ctx, UpOptions{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "checksum mismatch")
	require.Contains(t, err.Error(), "0001_probe")

	status, err := tampered.status(ctx)
	require.NoError(t, err)
	require.Len(t, status.Mismatches, 1)
	require.Equal(t, int64(1), status.Mismatches[0].Version)
	require.NotEqual(t, status.Mismatches[0].Recorded, status.Mismatches[0].Current)

	// 表没被重建：账本与结构都没动。
	require.Equal(t, 1, openCount(t, db, catalog.ControlTable))
}

// T7 的严格版：对着真实的 0001 文件改**一个字节**，up 必须拒绝执行。
func TestChecksumMismatchOnRealBaselineOneByte(t *testing.T) {
	ctx := context.Background()
	db := newEmptyDB(t)
	_, err := Up(ctx, db, UpOptions{})
	require.NoError(t, err)

	embedded, err := loadMigrations(migrationFS)
	require.NoError(t, err)
	tampered := make(fstest.MapFS, len(embedded))
	for _, m := range embedded {
		up := m.UpSQL
		if m.Version == 1 {
			up = strings.Replace(up, "SET search_path = public;", "SET search_path = public; ", 1)
			require.Len(t, up, len(m.UpSQL)+1, "tampering must add exactly one byte")
		}
		tampered[fmt.Sprintf("migrations/%04d_%s.up.sql", m.Version, m.Name)] = &fstest.MapFile{Data: []byte(up)}
	}
	_, err = newRunner(db, tampered, LockOptions{}).up(ctx, UpOptions{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "checksum mismatch")
	require.Contains(t, err.Error(), "immutable")
	require.Equal(t, 1, openCount(t, db, catalog.ControlTable))
}

// T7：Down 默认拒绝（必须显式 AllowDestructive），且拒绝信息里列出会丢什么。
func TestDownRequiresExplicitAllowDestructive(t *testing.T) {
	ctx := context.Background()
	db := newEmptyDB(t)
	fsys := fstest.MapFS{
		"migrations/0001_probe.up.sql": &fstest.MapFile{Data: []byte("CREATE TABLE public.probe (id bigint PRIMARY KEY);\n")},
		"migrations/0001_probe.down.sql": &fstest.MapFile{Data: []byte(
			"-- ROLLBACK DROPS: probe 全部探针数据\nDROP TABLE IF EXISTS public.probe;\n")},
	}
	r := newRunner(db, fsys, LockOptions{})
	_, err := r.up(ctx, UpOptions{})
	require.NoError(t, err)

	_, err = r.down(ctx, DownOptions{})
	require.ErrorIs(t, err, ErrDestructiveDenied)
	require.Contains(t, err.Error(), "probe 全部探针数据", "the refusal must name what would be dropped")
	require.True(t, relationExists(t, db, "probe"), "a refused down must not touch the schema")

	res, err := r.down(ctx, DownOptions{AllowDestructive: true})
	require.NoError(t, err)
	require.Len(t, res.Rolled, 1)
	require.Len(t, res.SQL, 1, "the CLI needs the exact SQL to print before it runs")
	require.False(t, relationExists(t, db, "probe"), "the table must be gone after a destructive down")
	require.Equal(t, 0, openCount(t, db, catalog.ControlTable))
}

// T7：origin='adopted' 的基线永久禁止 down；顺带跑通 Adopt 的三重校验。
func TestAdoptedBaselineCannotBeRolledBack(t *testing.T) {
	ctx := context.Background()
	db := newEmptyDB(t)
	_, err := Up(ctx, db, UpOptions{})
	require.NoError(t, err)

	// 抹掉账本 ⇒ 回到「有 schema、无账本」的存量库形态。
	_, err = db.ExecContext(ctx, `DROP TABLE public.`+catalog.ControlTable)
	require.NoError(t, err)

	adopted, err := Adopt(ctx, db, AdoptOptions{Snapshot: "0001"})
	require.NoError(t, err)
	require.Len(t, adopted.Adopted, 1)
	require.Equal(t, OriginAdopted, adopted.Adopted[0].Origin)

	status, err := Status(ctx, db)
	require.NoError(t, err)
	require.Len(t, status.Applied, 1)
	require.Equal(t, OriginAdopted, status.Applied[0].Origin)

	_, err = Down(ctx, db, DownOptions{AllowDestructive: true})
	require.ErrorIs(t, err, ErrAdoptedBaseline)

	// 账本本身还在（拒绝发生在任何 DDL 之前）。
	require.Equal(t, 1, openCount(t, db, catalog.ControlTable))
}

// spec §2.8：账本空 + 已有 schema 时 up 必须拒绝并把运维指向 adopt（不自动收养）。
func TestUpRefusesExistingSchemaWithoutLedger(t *testing.T) {
	ctx := context.Background()
	db := newEmptyDB(t)
	_, err := db.ExecContext(ctx, `CREATE TABLE public.legacy_payments (id bigint PRIMARY KEY)`)
	require.NoError(t, err)

	_, err = Up(ctx, db, UpOptions{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "run -migrate adopt")
	require.Contains(t, err.Error(), "1 business relations")
	require.True(t, relationExists(t, db, "legacy_payments"), "a refused up must not touch the schema")
}

// T8：两条连接验证 try-lock 语义（A 真 / B 假且错误带 classid/objid/等待时长）。
func TestAdvisoryLockIsTryLock(t *testing.T) {
	ctx := context.Background()
	db := newEmptyDB(t)

	a, err := acquireLock(ctx, db, LockOptions{Timeout: 5 * time.Second})
	require.NoError(t, err)

	started := time.Now()
	_, err = acquireLock(ctx, db, LockOptions{Timeout: 150 * time.Millisecond, Poll: 10 * time.Millisecond})
	waited := time.Since(started)

	var busy *LockBusyError
	require.ErrorAs(t, err, &busy)
	require.Equal(t, lockClassID, busy.ClassID)
	require.Equal(t, lockObjID, busy.ObjectID)
	require.GreaterOrEqual(t, busy.Waited, 100*time.Millisecond)
	require.GreaterOrEqual(t, waited, 100*time.Millisecond)
	for _, want := range []string{"migration lock busy", "classid=", "objid=", "waited"} {
		require.Contains(t, err.Error(), want)
	}

	// A 释放后 B 能拿到——锁是会话级的，不是全局毒化。
	a.release(ctx)
	a = nil
	b, err := acquireLock(ctx, db, LockOptions{Timeout: 5 * time.Second})
	require.NoError(t, err)
	b.release(ctx)
}

// T8：并发 Up 的副作用只发生一次（探针表恰好 1 行、账本恰好 1 行）。
func TestConcurrentUpAppliesOnce(t *testing.T) {
	ctx := context.Background()
	db := newEmptyDB(t)
	fsys := fstest.MapFS{
		"migrations/0001_probe.up.sql": &fstest.MapFile{Data: []byte(
			"CREATE TABLE public.probe (id bigint PRIMARY KEY);\nINSERT INTO public.probe VALUES (1);\n")},
	}
	const racers = 2
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := newRunner(db, fsys, LockOptions{Timeout: 30 * time.Second, Poll: 5 * time.Millisecond})
			_, err := r.up(ctx, UpOptions{})
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		}()
	}
	wg.Wait()
	for _, err := range errs {
		require.NoError(t, err)
	}
	require.Equal(t, 1, openCount(t, db, "probe"), "the migration body must run exactly once")
	require.Equal(t, 1, openCount(t, db, catalog.ControlTable))
}

// Down 的签名即闸门：ctx / db / opts，没有 steps。用**编译期**断言而不是反射：
// 多一个 steps 参数就编译不过（比运行时断言更强），也不需要运行测试。
var _ func(context.Context, *sql.DB, DownOptions) (DownResult, error) = Down

// R2：Down 只有一步——签名里没有 steps，且行为上一次只回滚最新版本。
func TestDownIsSingleStep(t *testing.T) {
	ctx := context.Background()
	db := newEmptyDB(t)

	fsys := fstest.MapFS{
		"migrations/0001_probe.up.sql": &fstest.MapFile{Data: []byte("CREATE TABLE public.probe_one (id bigint PRIMARY KEY);\n")},
		"migrations/0001_probe.down.sql": &fstest.MapFile{Data: []byte(
			"-- ROLLBACK DROPS: probe_one\nDROP TABLE IF EXISTS public.probe_one;\n")},
		"migrations/0002_probe.up.sql": &fstest.MapFile{Data: []byte("CREATE TABLE public.probe_two (id bigint PRIMARY KEY);\n")},
		"migrations/0002_probe.down.sql": &fstest.MapFile{Data: []byte(
			"-- ROLLBACK DROPS: probe_two\nDROP TABLE IF EXISTS public.probe_two;\n")},
	}
	r := newRunner(db, fsys, LockOptions{})
	_, err := r.up(ctx, UpOptions{})
	require.NoError(t, err)

	res, err := r.down(ctx, DownOptions{AllowDestructive: true})
	require.NoError(t, err)
	require.Len(t, res.Rolled, 1)
	require.Equal(t, int64(2), res.Rolled[0].Version, "the newest version is the one rolled back")
	require.False(t, relationExists(t, db, "probe_two"))
	require.True(t, relationExists(t, db, "probe_one"), "exactly one step: 0001 must still be applied")
	require.Equal(t, 1, openCount(t, db, catalog.ControlTable))

	// 再来一次回滚掉 0001；账本空了之后的 down 是 no-op，不是错误。
	_, err = r.down(ctx, DownOptions{AllowDestructive: true})
	require.NoError(t, err)
	require.False(t, relationExists(t, db, "probe_one"))
	_, err = r.down(ctx, DownOptions{AllowDestructive: true})
	require.NoError(t, err, "an empty ledger is a no-op, not an error")
	require.Equal(t, 0, openCount(t, db, catalog.ControlTable))
}

// R3-Tg：多版本 down 一次只回滚最新版本，顺序必须是 2 → 1。0002 的表 FK 依赖
// 0001：顺序反了会先 DROP 0001 而被依赖拒绝（红），不是「恰好能跑」。
func TestDownRollsBackNewestFirst(t *testing.T) {
	ctx := context.Background()
	db := newEmptyDB(t)
	fsys := fstest.MapFS{
		"migrations/0001_probe.up.sql": &fstest.MapFile{Data: []byte(
			"CREATE TABLE public.probe_parent (id bigint PRIMARY KEY);\n")},
		"migrations/0001_probe.down.sql": &fstest.MapFile{Data: []byte(
			"-- ROLLBACK DROPS: probe_parent\nDROP TABLE IF EXISTS public.probe_parent;\n")},
		"migrations/0002_probe_child.up.sql": &fstest.MapFile{Data: []byte(
			"CREATE TABLE public.probe_child (\n\tid bigint PRIMARY KEY,\n\tparent_id bigint NOT NULL REFERENCES public.probe_parent (id)\n);\n")},
		"migrations/0002_probe_child.down.sql": &fstest.MapFile{Data: []byte(
			"-- ROLLBACK DROPS: probe_child\nDROP TABLE IF EXISTS public.probe_child;\n")},
	}
	r := newRunner(db, fsys, LockOptions{})
	_, err := r.up(ctx, UpOptions{})
	require.NoError(t, err)
	require.Equal(t, []int64{1, 2}, appliedVersions(t, db))

	first, err := r.down(ctx, DownOptions{AllowDestructive: true})
	require.NoError(t, err)
	require.Len(t, first.Rolled, 1)
	require.Equal(t, int64(2), first.Rolled[0].Version, "the newest version goes first")
	require.False(t, relationExists(t, db, "probe_child"))
	require.True(t, relationExists(t, db, "probe_parent"))
	require.Equal(t, []int64{1}, appliedVersions(t, db))

	second, err := r.down(ctx, DownOptions{AllowDestructive: true})
	require.NoError(t, err)
	require.Len(t, second.Rolled, 1)
	require.Equal(t, int64(1), second.Rolled[0].Version)
	require.False(t, relationExists(t, db, "probe_parent"))
	require.Empty(t, appliedVersions(t, db))
}

// R3-Ta：每条迁移一个事务（spec §2.1）。第二条迁移的 DDL 中途失败时：第一条的
// schema 与账本行**都在**；第二条既无 schema 残留、也无账本行——DDL 与记账同生共死。
// 把 apply 改成「整批一个事务」时，第一条会被连坐回滚，本用例必红。
func TestEachMigrationHasItsOwnTransaction(t *testing.T) {
	ctx := context.Background()
	db := newEmptyDB(t)
	fsys := fstest.MapFS{
		"migrations/0001_probe.up.sql": &fstest.MapFile{Data: []byte(
			"CREATE TABLE public.probe_first (id bigint PRIMARY KEY);\n")},
		"migrations/0002_probe_next.up.sql": &fstest.MapFile{Data: []byte(
			"CREATE TABLE public.probe_second (id bigint PRIMARY KEY);\nSELECT 1 / 0;\n")},
	}
	_, err := newRunner(db, fsys, LockOptions{}).up(ctx, UpOptions{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "0002_probe_next")

	require.True(t, relationExists(t, db, "probe_first"), "0001 must stay applied (per-migration transactions, not one batch)")
	require.False(t, relationExists(t, db, "probe_second"), "the failed 0002 must leave no schema behind")
	require.Equal(t, []int64{1}, appliedVersions(t, db), "the ledger must record exactly 0001")
}

// R3-Tb：DROP COLUMN 之后的库必须和「drop 之前的快照」逐行相等：dumper 不能把
// pg_attribute 的 dead row 当成 ghost 列（type=- / type=??? 之类畸形行），否则
// Adopt 会假拒一个健康库。
func TestDroppedColumnInvisibleToCatalog(t *testing.T) {
	ctx := context.Background()
	db := newEmptyDB(t)
	_, err := Up(ctx, db, UpOptions{})
	require.NoError(t, err)

	before, err := Inspect(ctx, db)
	require.NoError(t, err)
	golden, err := Snapshot("0001")
	require.NoError(t, err)
	require.Equal(t, golden.Static, before.Static, "sanity: a freshly migrated DB equals the snapshot")

	// 写入一行再删列：数据与 pg_attribute 的 dead row 都真实存在。
	execScript(t, db, `
INSERT INTO public.settings (key, type, value, updated_at) VALUES ('probe_dropped', 'string', 'x', now());
ALTER TABLE public.settings ADD COLUMN ghost text;
UPDATE public.settings SET ghost = 'gone' WHERE key = 'probe_dropped';
ALTER TABLE public.settings DROP COLUMN ghost;
`)

	after, err := Inspect(ctx, db)
	require.NoError(t, err)
	require.NotContains(t, after.Static, "ghost", "a dropped column must not appear in the fingerprint")
	for _, line := range after.Lines() {
		require.NotContains(t, line, "type=-", "no malformed type for a dead attribute: %s", line)
		require.NotContains(t, line, "type=???", "no malformed type for a dead attribute: %s", line)
		require.NotContains(t, line, "pg.dropped", "no ghost attribute name: %s", line)
	}
	tolerated, diff := splitStaticDiff(before, after, nil)
	require.Empty(t, tolerated)
	require.Empty(t, diff, "DROP COLUMN must be invisible to the static fingerprint")

	// Adopt 视角：账本丢了之后，这个「曾经删过列」的库必须能收养成功。
	_, err = db.ExecContext(ctx, `DROP TABLE public.`+catalog.ControlTable)
	require.NoError(t, err)
	adopted, err := Adopt(ctx, db, AdoptOptions{Snapshot: "0001"})
	require.NoError(t, err, "a DB that once dropped a column must still be adoptable")
	require.Len(t, adopted.Adopted, 1)
	require.Empty(t, adopted.Tolerated)
}

// R3-Td：只有分区父表（relkind='p'）的库也算「已有 schema」。它没有账本，不能被
// 当成空库继续跑 0001；必须拒绝并指向 adopt（而不是掉进 0001 的 CREATE TABLE 撞名）。
func TestUpRefusesPartitionParentWithoutLedger(t *testing.T) {
	ctx := context.Background()
	db := newEmptyDB(t)
	execScript(t, db, `CREATE TABLE public.probe_parent (
	id bigint NOT NULL,
	created_at timestamptz NOT NULL
) PARTITION BY RANGE (created_at);`)

	_, err := Up(ctx, db, UpOptions{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "-migrate adopt")
	require.Contains(t, err.Error(), "1 business relations", "a partition parent must be counted as an existing business relation")
	require.True(t, relationExists(t, db, "probe_parent"), "the refusal must happen before 0001 touches the schema")
	require.False(t, relationExists(t, db, catalog.ControlTable), "a refused up must not write the ledger table")
}

// R3-D：contract 阶段默认拒跑。判断在**应用任何迁移之前**完成，一次性列出待应用
// 区间内的全部 contract 版本；不能先跑 expand 再在 contract 处失败（那会把库留在
// 半升级状态）。显式 AllowContract 才整批执行。
func TestUpRefusesContractWithoutOptIn(t *testing.T) {
	ctx := context.Background()
	db := newEmptyDB(t)
	r := newRunner(db, contractFS(), LockOptions{})

	_, err := r.up(ctx, UpOptions{})
	require.Error(t, err)

	// 拒绝发生在任何写入之前：0001 的 expand DDL 没跑，连账本表都没建。
	require.False(t, relationExists(t, db, "probe_expand"), "no expand DDL may run before the contract gate")
	require.False(t, relationExists(t, db, "probe_pair"))
	require.False(t, relationExists(t, db, catalog.ControlTable), "a refused up must not write the ledger table")

	require.Contains(t, err.Error(), "2 contract migration(s)", "all blocked versions must be listed at once")
	require.Contains(t, err.Error(), "0002_cleanup_contract")
	require.Contains(t, err.Error(), "0003_drop_pair_contract")
	require.Contains(t, err.Error(), "irreversibly")
	require.Contains(t, err.Error(), "-allow-contract")

	// 显式承认后整批执行（含两条 contract）。
	res, err := r.up(ctx, UpOptions{AllowContract: true})
	require.NoError(t, err)
	require.Len(t, res.Applied, 3)
	require.Equal(t, int64(3), res.To)
	require.Equal(t, []int64{1, 2, 3}, appliedVersions(t, db))
	require.False(t, relationExists(t, db, "probe_expand"))
	require.False(t, relationExists(t, db, "probe_pair"))
}

// R3-D：闸门只覆盖**本次请求的区间**。Target=1 时 0002/0003 不在本次运行里，
// 不得拦住 0001 的 expand。
func TestUpContractGateCoversOnlyRequestedRange(t *testing.T) {
	ctx := context.Background()
	db := newEmptyDB(t)
	r := newRunner(db, contractFS(), LockOptions{})

	res, err := r.up(ctx, UpOptions{Target: 1})
	require.NoError(t, err, "contracts beyond Target are not part of this run")
	require.Len(t, res.Applied, 1)
	require.Equal(t, []int64{1}, appliedVersions(t, db))
	require.True(t, relationExists(t, db, "probe_expand"))
}

// R3-D：已经 applied 的 contract 不再拦（blocked 只算「待应用区间」里的版本）。
func TestUpContractGateSkipsAlreadyApplied(t *testing.T) {
	ctx := context.Background()
	db := newEmptyDB(t)
	r := newRunner(db, contractFS(), LockOptions{})

	_, err := r.up(ctx, UpOptions{Target: 2, AllowContract: true})
	require.NoError(t, err)
	require.Equal(t, []int64{1, 2}, appliedVersions(t, db))

	_, err = r.up(ctx, UpOptions{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "0003_drop_pair_contract")
	require.NotContains(t, err.Error(), "0002_cleanup_contract", "an applied contract must not be reported as blocked")
	require.Equal(t, []int64{1, 2}, appliedVersions(t, db), "a refused up must not change the ledger")
}

// R3-Te-a：DB 里多出快照没有的表 ⇒ Adopt 必须报 diff（不是只查缺失）。
func TestAdoptRejectsUnexpectedTable(t *testing.T) {
	ctx := context.Background()
	db := newEmptyDB(t)
	_, err := Up(ctx, db, UpOptions{})
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DROP TABLE public.`+catalog.ControlTable)
	require.NoError(t, err)
	execScript(t, db, `CREATE TABLE public.rogue_table (id bigint PRIMARY KEY);`)

	_, err = Adopt(ctx, db, AdoptOptions{Snapshot: "0001"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not match snapshot 0001")
	require.Contains(t, err.Error(), "unexpected: table|rogue_table|")
}

// R3-Te-b：快照里有、库里没有 ⇒ 仍是 diff，白名单也不能覆盖（缺失方向）。
func TestAdoptRejectsMissingBaselineTable(t *testing.T) {
	ctx := context.Background()
	db := newEmptyDB(t)
	_, err := Up(ctx, db, UpOptions{})
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DROP TABLE public.`+catalog.ControlTable)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DROP TABLE public.stats_agg_watermark`)
	require.NoError(t, err)

	_, err = Adopt(ctx, db, AdoptOptions{Snapshot: "0001", Tolerate: LegacyTolerances})
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not match snapshot 0001")
	require.Contains(t, err.Error(), "missing: table|stats_agg_watermark|")
}

// R3-Tf：账本空但 settings.schema_version 有值 ⇒ 判定「账本丢失」并拒绝，不能把
// 一个有版本记录的库静默收养成基线。
func TestAdoptRejectsLedgerLostSchemaVersion(t *testing.T) {
	ctx := context.Background()
	db := newEmptyDB(t)
	_, err := Up(ctx, db, UpOptions{})
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `DROP TABLE public.`+catalog.ControlTable)
	require.NoError(t, err)
	execScript(t, db, `INSERT INTO public.settings (key, type, value, updated_at)
VALUES ('schema_version', 'string', '1', now());`)

	_, err = Adopt(ctx, db, AdoptOptions{Snapshot: "0001"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "ledger lost")
	require.Contains(t, err.Error(), `schema_version="1"`)
}
