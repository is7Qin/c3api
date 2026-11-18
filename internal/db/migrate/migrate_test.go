// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package migrate

import (
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/db/migrate/catalog"
)

// 本文件是纯逻辑用例（不碰 PG）：文件名解析/阶段判定/禁用构造/内嵌迁移自检。

// R3-B：Tolerate 的粒度是**整行字面量**（`==`）。同名约束、不同 def 必须落到
// diff，不能被「名字相同」或「前缀相同」顺手放过。
func TestStaticDiffToleranceIsExactWholeLine(t *testing.T) {
	const (
		shared    = `table|price_variants|column=id|type=bigint|nullable=false`
		tolerated = `constraint|price_variants|name=price_variants_effect_at_least_one_v2|type=c|validated=true|def=CHECK ((a IS NOT NULL))`
		wrongDef  = `constraint|price_variants|name=price_variants_effect_at_least_one_v2|type=c|validated=true|def=CHECK (((a IS NOT NULL) OR (b IS NOT NULL)))`
	)
	want := catalog.Manifest{Static: shared + "\n"}
	got := catalog.Manifest{Static: strings.Join([]string{shared, tolerated, wrongDef}, "\n") + "\n"}

	toleratedLines, diff := splitStaticDiff(want, got, []string{tolerated})
	require.Equal(t, []string{tolerated}, toleratedLines, "only the exact line is tolerated")
	require.Equal(t, []string{"unexpected: " + wrongDef}, diff,
		"same name + different def must be a diff, not a tolerance")

	// 前缀相同不算命中：把白名单截短一截，必须一条都不生效。
	_, diff = splitStaticDiff(want, got, []string{tolerated[:len(tolerated)-10]})
	require.Len(t, diff, 2, "a prefix of a line must not tolerate anything")

	// 缺失行永不被白名单覆盖（白名单只放宽「多出的行」）。
	_, diff = splitStaticDiff(got, want, []string{tolerated})
	require.Contains(t, diff, "missing: "+tolerated, "tolerance never covers a missing baseline object")
}

func TestParseStemAndPhase(t *testing.T) {
	cases := []struct {
		stem    string
		version int64
		name    string
		ok      bool
	}{
		{"0001_baseline", 1, "baseline", true},
		{"0012_attempt_logs", 12, "attempt_logs", true},
		{"0013_drop_legacy_contract", 13, "drop_legacy_contract", true},
		{"1_baseline", 0, "", false},
		{"0000_zero", 0, "", false},
		{"0002_", 0, "", false},
		{"abcd_name", 0, "", false},
		{"0003", 0, "", false},
	}
	for _, tc := range cases {
		version, name, ok := parseStem(tc.stem)
		require.Equal(t, tc.ok, ok, tc.stem)
		if tc.ok {
			require.Equal(t, tc.version, version, tc.stem)
			require.Equal(t, tc.name, name, tc.stem)
		}
	}
	require.Equal(t, PhaseContract, phaseOf("drop_legacy_contract"))
	require.Equal(t, PhaseExpand, phaseOf("baseline"))
}

// TestEmbeddedMigrationsAreWellFormed 锚定内嵌迁移本身：每个版本都有 up，且
// checksum 就是 up 文件的 sha256（账本记录的就是这个值）。
func TestEmbeddedMigrationsAreWellFormed(t *testing.T) {
	migrations, err := loadMigrations(migrationFS)
	require.NoError(t, err)
	require.NotEmpty(t, migrations)
	for _, m := range migrations {
		require.NotEmpty(t, m.Name, "version %d", m.Version)
		require.NotEmpty(t, m.UpSQL, "%04d_%s up is empty", m.Version, m.Name)
		require.NotEmpty(t, m.DownSQL, "%04d_%s has no down file", m.Version, m.Name)
		require.Len(t, m.Checksum, sha256.Size)
		require.Equal(t, m.Phase, phaseOf(m.Name))
	}
	version, phase := Head()
	require.Equal(t, migrations[len(migrations)-1].Version, version)
	require.Equal(t, migrations[len(migrations)-1].Phase, phase)
}

// TestValidateSQLRejectsForbiddenConstructs 是 spec §2.1 末 / §2.4 的机械兜底，
// 且按 phase 分层：
//   - `DO $$` 与 `CREATE INDEX CONCURRENTLY` 对**所有** phase 都禁；
//   - `DROP TABLE|SCHEMA` / `TRUNCATE` 只在 expand 禁——contract 阶段的存在意义
//     就是收缩，第一个真正的 contract 迁移不能被自己的文本 lint 拒掉。
func TestValidateSQLRejectsForbiddenConstructs(t *testing.T) {
	cases := []struct {
		name  string
		phase string
		sql   string
		ok    bool
	}{
		{"expand create", PhaseExpand, "CREATE TABLE public.x (id bigint PRIMARY KEY);\n", true},
		{"expand add column", PhaseExpand, "ALTER TABLE public.x ADD COLUMN y text NULL;\n", true},
		{"expand drop table", PhaseExpand, "DROP TABLE public.x;\n", false},
		{"expand drop schema", PhaseExpand, "DROP SCHEMA public CASCADE;\n", false},
		{"expand truncate", PhaseExpand, "TRUNCATE public.x;\n", false},
		{"expand do block", PhaseExpand, "DO $$ BEGIN NULL; END $$;\n", false},
		{"expand concurrently", PhaseExpand, "CREATE INDEX CONCURRENTLY x_idx ON public.x (id);\n", false},
		{"contract drop table", PhaseContract, "DROP TABLE public.x;\n", true},
		{"contract drop column", PhaseContract, "ALTER TABLE public.x DROP COLUMN y;\n", true},
		{"contract truncate", PhaseContract, "TRUNCATE public.x;\n", true},
		{"contract do block", PhaseContract, "DO $$ BEGIN NULL; END $$;\n", false},
		{"contract concurrently", PhaseContract, "CREATE INDEX CONCURRENTLY x_idx ON public.x (id);\n", false},
		// 注释里的字样不算：down 文件头就写着 ROLLBACK DROPS，不能被误判。
		{"comment only", PhaseExpand, "-- DROP TABLE public.x; TRUNCATE y; DO $$\nCREATE TABLE public.z (id bigint);\n", true},
	}
	for _, tc := range cases {
		name := "probe"
		if tc.phase == PhaseContract {
			name = "probe_contract"
		}
		require.Equal(t, tc.phase, phaseOf(name), tc.name)
		err := validateSQL(Migration{Version: 7, Name: name, Phase: phaseOf(name), UpSQL: tc.sql})
		if tc.ok {
			require.NoError(t, err, tc.name)
			continue
		}
		require.Error(t, err, tc.name)
		require.Contains(t, err.Error(), "forbidden construct", tc.name)
		require.Contains(t, err.Error(), "0007_probe", tc.name)
	}

	// 空 Phase 不能当逃生门：不认识的 phase 按 expand 从严（fail-closed）。
	require.Error(t, validateSQL(Migration{Version: 7, Name: "probe", UpSQL: "DROP TABLE public.x;\n"}))
}

// R2-1：checksum 覆盖的是**规范化后**的字节：同一段 SQL 的 LF / CRLF / BOM+CRLF
// 三个文件必须得到同一个 checksum，且执行的内容就是这份规范化字节。
func TestCanonicalChecksumIgnoresLineEndingsAndBOM(t *testing.T) {
	const lf = "CREATE TABLE public.probe (id bigint PRIMARY KEY);\n-- 注释当然也参与规范化\nALTER TABLE public.probe ADD COLUMN note text NULL;\n"
	crlf := strings.ReplaceAll(lf, "\n", "\r\n")
	bom := "\xef\xbb\xbf" + crlf

	version := func(n int, body string) (int64, []byte, string) {
		name := fmt.Sprintf("migrations/%04d_probe.up.sql", n)
		migrations, err := loadMigrations(fstest.MapFS{name: &fstest.MapFile{Data: []byte(body)}})
		require.NoError(t, err)
		require.Len(t, migrations, 1)
		return migrations[0].Version, migrations[0].Checksum, migrations[0].UpSQL
	}

	_, lfSum, lfSQL := version(1, lf)
	_, crlfSum, crlfSQL := version(2, crlf)
	_, bomSum, bomSQL := version(3, bom)

	require.Equal(t, lfSum, crlfSum, "CRLF must hash the same as LF")
	require.Equal(t, lfSum, bomSum, "a UTF-8 BOM must not change the checksum")
	require.Equal(t, lf, lfSQL)
	require.Equal(t, lf, crlfSQL, "the executed bytes are the canonical ones")
	require.Equal(t, lf, bomSQL)
	require.NotContains(t, crlfSQL, "\r")
	require.False(t, strings.HasPrefix(bomSQL, "\xef\xbb\xbf"))

	// 真正的语义改动仍然要改变 checksum（规范化不能把内容差异抹平）。
	_, changed, _ := version(4, strings.ReplaceAll(lf, "bigint", "integer"))
	require.NotEqual(t, lfSum, changed)
}

// TestCanonicalSQLKeepsLoneCR 单独 CR（老 Mac 换行）也归一到 LF。
func TestCanonicalSQLKeepsLoneCR(t *testing.T) {
	require.Equal(t, "a\nb\n", string(canonicalSQL([]byte("a\rb\r"))))
	require.Equal(t, "a\nb\n", string(canonicalSQL([]byte("a\r\nb\r\n"))))
	require.Equal(t, "a\nb\n", string(canonicalSQL([]byte("a\nb\n"))), "LF input is returned untouched")
	require.Equal(t, "", string(canonicalSQL(nil)))
}

// TestSnapshotIsCommitted 确认内嵌指纹快照存在且非空（否则 T1 的「== 快照」变成
// 「两个空字符串相等」的空话）。
func TestSnapshotIsCommitted(t *testing.T) {
	snapshot, err := Snapshot("0001")
	require.NoError(t, err)
	require.NotEmpty(t, snapshot.Static)
	require.Contains(t, snapshot.Static, "table|usage_logs|")
	require.Contains(t, snapshot.Static, "partitioned|usage_logs|strategy=r|key=created_at")
	require.True(t, strings.HasSuffix(snapshot.Static, "\n"))
	_, err = Snapshot("9999")
	require.Error(t, err)
}
