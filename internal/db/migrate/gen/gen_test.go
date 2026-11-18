// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package gen

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// R2-4：PG 版本策略是纯函数，可直接钉住三种结局。
func TestCheckPGVersionPolicy(t *testing.T) {
	cases := []struct {
		name         string
		recorded     int
		current      int
		force        bool
		wantWarning  bool
		wantError    bool
		errorKeyword string
	}{
		{name: "identical", recorded: 180006, current: 180006},
		{name: "same major newer patch", recorded: 180004, current: 180006, wantWarning: true},
		{name: "same major older patch", recorded: 180010, current: 180002, wantWarning: true},
		{name: "cross major", recorded: 170004, current: 180006, wantError: true, errorKeyword: "major 17"},
		{name: "cross major forced", recorded: 170004, current: 180006, force: true, wantWarning: true},
	}
	for _, tc := range cases {
		warning, err := checkPGVersion(tc.recorded, tc.current, tc.force)
		if tc.wantError {
			require.Error(t, err, tc.name)
			require.Contains(t, err.Error(), tc.errorKeyword, tc.name)
			require.Contains(t, err.Error(), "-force-version", tc.name)
			continue
		}
		require.NoError(t, err, tc.name)
		require.Equal(t, tc.wantWarning, warning != "", tc.name)
	}
}

// R2-4：版本标记行是元数据——解析它、并在内容比对时把它抹平，这样「同 major
// 换小版本」不会伪装成产物漂移。
func TestPGVersionHeaderIsComparable(t *testing.T) {
	const header = "-- generated-by-pg:"

	version, ok := recordedPGVersion("-- 0001_baseline.up.sql\n" + header + " 180006\nSET search_path = public;\n")
	require.True(t, ok)
	require.Equal(t, 180006, version)

	_, ok = recordedPGVersion("-- 0001_baseline.up.sql\nSET search_path = public;\n")
	require.False(t, ok, "a file without the header must be reported (so -check can complain)")

	older := stripPGVersionLine(header + " 180004\nrest")
	newer := stripPGVersionLine(header + " 180006\nrest")
	require.Equal(t, older, newer, "the version line must not count as a content difference")
}

// 生成器必须在产物里带上统计口径（一次 CLI 调用只跑一遍，值不稳定时用例会红）。
func TestRenderUpCountsSections(t *testing.T) {
	var statements []statement
	for _, table := range []string{"a", "b"} {
		statements = append(statements, statement{kind: kindEnt, table: table, sql: "CREATE TABLE public." + table + " (id bigint);"})
	}
	for _, table := range []string{"p1", "p2", "p3", "p4", "p5", "p6"} {
		statements = append(statements, statement{kind: kindPartitioned, table: table, sql: "CREATE TABLE public." + table + " (id bigint) PARTITION BY RANGE (id);"})
	}
	for _, table := range []string{"s1", "s2"} {
		statements = append(statements, statement{kind: kindStatic, table: table, sql: "CREATE TABLE public." + table + " (id bigint);"})
	}
	up, stats, err := renderUp(statements, 2, 180006)
	require.NoError(t, err)
	require.Equal(t, 2, stats.EntTables)
	require.Equal(t, 6, stats.PartitionedTable)
	require.Equal(t, 2, stats.StaticTables)
	require.Contains(t, up, "-- generated-by-pg: 180006")
	require.Equal(t, 2+6+2, stats.Statements)

	// 分区表/静态表数量不对时必须报错（防止事实源漏一张表而产物静默变瘦）。
	_, _, err = renderUp(statements[:len(statements)-1], 2, 180006)
	require.Error(t, err)
}

// R3-A：默认产物目录按**本包源码位置**解析，不是 cwd。cwd 可以是 repo 根、包目录
// 或任意目录，不能成为生成器正确性的隐式条件。
func TestDefaultDirsFollowPackageDir(t *testing.T) {
	root := t.TempDir()
	pkgDir := filepath.Join(root, "internal", "db", "migrate", "gen")
	migrations, testdata, err := resolveDefaultDirs(pkgDir, true)
	require.NoError(t, err)
	require.Equal(t, filepath.Join(root, "internal", "db", "migrate", "migrations"), migrations)
	require.Equal(t, filepath.Join(root, "internal", "db", "migrate", "testdata"), testdata)
	require.True(t, filepath.IsAbs(migrations))
	require.True(t, filepath.IsAbs(testdata))
}

// R3-A：-trimpath 下 runtime.Caller 拿不到源码路径 ⇒ 必须报错并点名 cwd、默认
// 目录与该传的 flag，绝不静默退回 cwd 猜。
func TestDefaultDirsUnavailableFailsLoudly(t *testing.T) {
	wd, err := os.Getwd()
	require.NoError(t, err)
	_, _, err = resolveDefaultDirs("", false)
	require.Error(t, err)
	for _, want := range []string{
		wd,
		filepath.Join("..", "migrations"),
		filepath.Join("..", "testdata"),
		"-migrations",
		"-testdata",
	} {
		require.Contains(t, err.Error(), want,
			"the diagnostic must name cwd, the unresolvable default dirs and the flags to pass")
	}
}

// R3-A：普通构建里 packageDir 定位到本包，默认目录因此存在（且与包目录同级）。
func TestPackageDirResolvesInNormalBuild(t *testing.T) {
	dir, ok := packageDir()
	if !ok {
		t.Skip("built with -trimpath: the CLI then demands explicit -migrations/-testdata (covered above)")
	}
	require.True(t, filepath.IsAbs(dir))
	require.FileExists(t, filepath.Join(dir, "gen.go"), "packageDir must point at the gen package source")
	migrations, testdata, err := defaultDirs()
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "..", "migrations"), migrations)
	require.Equal(t, filepath.Join(dir, "..", "testdata"), testdata)
}

// R3-A 加固：解析到了默认目录但产物不存在（二进制被搬到源码树外 / checkout
// 不完整）⇒ 必须报错点名缺失目录与 flags，绝不回退 cwd。
func TestDefaultDirsMissingNextToSourceFails(t *testing.T) {
	root := t.TempDir()
	pkgDir := filepath.Join(root, "internal", "db", "migrate", "gen")

	_, _, err := defaultDirsFrom(pkgDir, true)
	require.Error(t, err)
	migrationsDir, testdataDir, resolveErr := resolveDefaultDirs(pkgDir, true)
	require.NoError(t, resolveErr)
	for _, want := range []string{
		migrationsDir,
		testdataDir,
		"not found next to the generator source",
		"cwd is not used as a fallback",
		"-migrations",
		"-testdata",
	} {
		require.Contains(t, err.Error(), want)
	}

	// 只缺一个也不行；已存在的那个不能被误报。
	require.NoError(t, os.MkdirAll(migrationsDir, 0o755))
	_, _, err = defaultDirsFrom(pkgDir, true)
	require.Error(t, err)
	require.Contains(t, err.Error(), testdataDir)
	require.NotContains(t, err.Error(), migrationsDir)
}

// 显式目录优先，且完全不触发默认解析（-trimpath 下也能用）。
func TestExplicitDirsBypassDefaultResolution(t *testing.T) {
	cfg, err := Config{
		MigrationsDir: filepath.Join("out", "migrations"),
		TestdataDir:   filepath.Join("out", "testdata"),
	}.withArtifactDirs()
	require.NoError(t, err)
	require.Equal(t, filepath.Join("out", "migrations"), cfg.MigrationsDir)
	require.Equal(t, filepath.Join("out", "testdata"), cfg.TestdataDir)
}
