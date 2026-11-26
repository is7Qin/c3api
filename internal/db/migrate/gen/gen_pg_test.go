// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package gen

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"

	"entgo.io/ent/dialect/sql/schema"
	"github.com/stretchr/testify/require"

	entmigrate "github.com/is7qin/c3api/internal/ent/migrate"
	"github.com/is7qin/c3api/internal/testsupport/pgtest"
)

// TestCommittedArtifactsMatchGenerator 是 -check 模式的测试入口：重新生成并与已
// 提交的 migrations/ 与 testdata/ 逐字节比对。漂移 ⇒ 红（CI 与本地同一条断言）。
//
// PG 版本只放行 spec 里那一条「同 major 不同 patch」的告警：CI 的
// postgres:18-alpine 是浮动 tag，自己就会滚补丁版（产物头 180006 vs runner
// 180007），gen.Check 按设计只回 WARN、rc 仍 0——把这条 WARN 当失败等于让 CI
// 单方面变红。跨 major 仍是 error，照样红。
func TestCommittedArtifactsMatchGenerator(t *testing.T) {
	dsn := pgtest.DSN(t)
	warnings, err := Check(context.Background(), Config{DSN: dsn})
	require.NoError(t, err)
	allowed, unexpected := patchOnlyWarnings(warnings)
	require.Empty(t, unexpected, "only a same-major patch-level difference may pass (allowed: %v)", allowed)
}

// patchOnlyWarnings 只放行「同 major 不同 patch」这一种已文档化的告警；
// 任何别的告警都必须让测试失败（否则真漂移会被这条豁免吞掉）。
//
// 判据是文本 + 语义双重保险：必须「含 patch level differs」且「不含 major
// differs」。语义上 patch 差异是 gen.Check 唯一以 WARN 返回的情况（跨 major 走
// error；-force-version 后的那条写的是 major differs），文本上再钉一次：改措辞
// 或新增告警都会落进 unexpected，而不是被静默放过。
func patchOnlyWarnings(ws []string) (allowed, unexpected []string) {
	for _, w := range ws {
		if strings.Contains(w, "patch level differs") && !strings.Contains(w, "major differs") {
			allowed = append(allowed, w)
			continue
		}
		unexpected = append(unexpected, w)
	}
	return allowed, unexpected
}

// patchOnlyWarnings 是纯函数，所以这条用例不连 PG、也不看本机补丁版：跑在
// 180006 或 180007 的机器上结论相同。
func TestPatchOnlyWarnings(t *testing.T) {
	const (
		patch = "gen: PostgreSQL patch level differs from the committed artifacts (generated on 180006, running 180007); same major 18, staying quiet"
		major = "gen: PostgreSQL major differs from the committed artifacts (generated on major 17, running major 18); forced by -force-version"
	)
	cases := []struct {
		name           string
		warnings       []string
		wantAllowed    []string
		wantUnexpected []string
	}{
		{name: "no warnings"},
		{name: "patch only", warnings: []string{patch}, wantAllowed: []string{patch}},
		{name: "cross major", warnings: []string{major}, wantUnexpected: []string{major}},
		{name: "unrelated warning", warnings: []string{"stale"}, wantUnexpected: []string{"stale"}},
		{name: "empty warning", warnings: []string{""}, wantUnexpected: []string{""}},
		{name: "mixed", warnings: []string{patch, major, "stale"}, wantAllowed: []string{patch}, wantUnexpected: []string{major, "stale"}},
		// 两种措辞都出现 ⇒ 不放行：宁可红，也不让豁免吞掉真漂移。
		{name: "both phrasings", warnings: []string{patch + " / major differs"}, wantUnexpected: []string{patch + " / major differs"}},
	}
	for _, tc := range cases {
		allowed, unexpected := patchOnlyWarnings(tc.warnings)
		require.Equal(t, tc.wantAllowed, allowed, tc.name)
		require.Equal(t, tc.wantUnexpected, unexpected, tc.name)
	}
}

// TestGenerateIsDeterministic 连跑两次生成器：产物必须逐字节相同（排序稳定、
// 无 oid/随机量渗入）。
//
// 同时钉住 hook 拆分的两条纪律（spec §4.2；评审 momus C3 / oracle I1）：
//   - hook 幂等：ent 的 Tables 是进程级全局对象，两次 Run 必须输出逐字节相同；
//   - hook 不污染全局表：Run 前后 ent 全局 Tables 的 Columns 逐表逐列不变
//     （实现只做浅拷贝 + 重建 Columns slice，原地删列会在这里红）；
//   - 列排除只属于 baseline 生成：request_patch 在 ent schema 里（runtime
//     automigrate 建全列），但**不得**进冻结的 0001 生成物。
func TestGenerateIsDeterministic(t *testing.T) {
	dsn := pgtest.DSN(t)
	ctx := context.Background()

	before := entColumnSnapshot()
	first, err := Run(ctx, Config{DSN: dsn})
	require.NoError(t, err)
	second, err := Run(ctx, Config{DSN: dsn})
	require.NoError(t, err)
	require.Equal(t, first.UpSQL, second.UpSQL)
	require.Equal(t, first.DownSQL, second.DownSQL)
	require.Equal(t, first.CatalogTxt, second.CatalogTxt)
	require.Equal(t, first.Stats, second.Stats)

	require.Equal(t, before, entColumnSnapshot(),
		"MigrateHookForBaseline 不得改写 ent 全局 Tables 的 Columns（只允许克隆）")
	templateExts := entTableByName(t, "template_exts")
	require.Contains(t, tableColumnNames(templateExts), "request_patch",
		"ent schema 必须保留 request_patch：runtime automigrate 建全列（列排除只属 baseline 生成）")
	require.NotContains(t, first.UpSQL, "request_patch",
		"post-baseline 列不得进冻结的 0001 生成物（应由 0004 迁移 ADD COLUMN）")
}

// entTableByName 从 ent 的全局 Tables 里按表名取表（只读）。
func entTableByName(t *testing.T, name string) *schema.Table {
	t.Helper()
	for _, table := range entmigrate.Tables {
		if table.Name == name {
			return table
		}
	}
	require.FailNow(t, "ent 全局 Tables 里没有表 "+name)
	return nil
}

// tableColumnNames 返回表的列名（按 ent 定义顺序）。
func tableColumnNames(table *schema.Table) []string {
	names := make([]string, 0, len(table.Columns))
	for _, column := range table.Columns {
		names = append(names, column.Name)
	}
	return names
}

// entColumnSnapshot 拍一张「表名 → 列名」的快照，用于断言 hook 未污染全局表。
func entColumnSnapshot() map[string][]string {
	out := make(map[string][]string, len(entmigrate.Tables))
	for _, table := range entmigrate.Tables {
		out[table.Name] = tableColumnNames(table)
	}
	return out
}

// TestGeneratedSQLHonoursExportPolicy 覆盖 spec §2.2 的导出策略：只留两条 SET、
// 全对象 public. 限定、无 COMMENT、无分区实例、down 头一行写清会丢什么。
func TestGeneratedSQLHonoursExportPolicy(t *testing.T) {
	dsn := pgtest.DSN(t)
	artifacts, err := Run(context.Background(), Config{DSN: dsn})
	require.NoError(t, err)

	up := artifacts.UpSQL
	for _, want := range []string{"SET search_path = public;", "SET standard_conforming_strings = on;"} {
		require.Contains(t, up, want)
	}
	require.NotContains(t, up, "COMMENT ON")
	require.NotContains(t, up, "DO $$")
	require.NotContains(t, up, "CONCURRENTLY")
	require.NotContains(t, up, "CREATE EXTENSION")
	// 统计出来的 SET 行只有两条（statement_timeout / row_security 等噪声必须被剔掉）。
	require.Equal(t, 2, countPrefix(up, "SET "))

	// R2-4：产物头注释记录生成用的 PG server_version_num，-check 据此判定漂移。
	recorded, ok := recordedPGVersion(up)
	require.True(t, ok, "up.sql must carry a -- generated-by-pg header")
	require.Equal(t, artifacts.ServerVersionNum, recorded)

	// 每条建表/建索引语句都限定 public.（不依赖 search_path）。
	for _, line := range strings.Split(up, "\n") {
		switch {
		case strings.HasPrefix(line, "CREATE TABLE "):
			require.True(t, strings.HasPrefix(line, "CREATE TABLE public."), line)
		case strings.HasPrefix(line, "CREATE SEQUENCE "):
			require.True(t, strings.HasPrefix(line, "CREATE SEQUENCE public."), line)
		case strings.HasPrefix(line, "CREATE INDEX "), strings.HasPrefix(line, "CREATE UNIQUE INDEX "):
			require.Contains(t, line, " ON public.", line)
		}
	}
	require.NotContains(t, up, "_2025", "no partition instances may be generated")

	down := strings.Split(artifacts.DownSQL, "\n")
	require.Contains(t, down[1], "ROLLBACK DROPS:", "the down file must open with what it drops")
	// down 的每条 DROP 都必须指向真实表名（曾经出现过把限定名解析成 public.public
	// 的 bug：DROP 静默 no-op，表全留着）。
	drops := 0
	known := make(map[string]struct{})
	for _, line := range strings.Split(up, "\n") {
		if name := createdTable(line); name != "" {
			known[name] = struct{}{}
		}
	}
	for _, line := range strings.Split(artifacts.DownSQL, "\n") {
		if !strings.HasPrefix(line, "DROP TABLE IF EXISTS public.") {
			continue
		}
		drops++
		name := strings.TrimSuffix(strings.TrimPrefix(line, "DROP TABLE IF EXISTS public."), ";")
		require.Contains(t, known, name, "down drops an unknown table: %s", line)
	}
	require.Equal(t, artifacts.Stats.PartitionedTable+artifacts.Stats.StaticTables+artifacts.Stats.EntTables, drops)
}

func countPrefix(text, prefix string) int {
	n := 0
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, prefix) {
			n++
		}
	}
	return n
}

// R2-4：-check 的 PG 版本策略端到端。同 major 只告警；跨 major 报错，除非
// ForceVersion（= -force-version）。
func TestCheckPGVersionPolicyEndToEnd(t *testing.T) {
	dsn := pgtest.DSN(t)
	dir := t.TempDir()
	cfg := Config{
		DSN:           dsn,
		MigrationsDir: dir + string(os.PathSeparator) + "migrations",
		TestdataDir:   dir + string(os.PathSeparator) + "testdata",
	}
	artifacts, err := Run(context.Background(), cfg)
	require.NoError(t, err)
	require.NoError(t, artifacts.Write(cfg))
	path := cfg.MigrationsDir + string(os.PathSeparator) + upFile
	committed, err := os.ReadFile(path)
	require.NoError(t, err)
	recorded, ok := recordedPGVersion(string(committed))
	require.True(t, ok)

	rewrite := func(version int) {
		body := pgVersionLine.ReplaceAllString(string(committed), pgVersionPrefix+" "+strconv.Itoa(version))
		require.NoError(t, os.WriteFile(path, []byte(body), 0o644))
	}

	// 同 major（换一个小版本号）：只告警，退出码仍是 0。
	// 本机若是 x.1 就换 x.2——重写成同一个版本号等于没改，那条告警根本不会出现。
	sameMajor := recorded - (recorded % 10000) + 1
	if sameMajor == recorded {
		sameMajor++
	}
	rewrite(sameMajor)
	warnings, err := Check(context.Background(), cfg)
	require.NoError(t, err, "same-major differences must not fail -check")
	require.Len(t, warnings, 1)

	// 跨 major：报错并提示 -force-version；force 后降级为告警。
	rewrite((pgMajor(recorded) - 1) * 10000)
	_, err = Check(context.Background(), cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), "-force-version")

	forced := cfg
	forced.ForceVersion = true
	warnings, err = Check(context.Background(), forced)
	require.NoError(t, err)
	require.Len(t, warnings, 1)
	require.Contains(t, warnings[0], "forced by -force-version")
}

// TestCheckDetectsDrift 证明 -check 不是空话：改一位就该报 stale。
func TestCheckDetectsDrift(t *testing.T) {
	dsn := pgtest.DSN(t)
	dir := t.TempDir()
	migrations := dir + string(os.PathSeparator) + "migrations"
	testdata := dir + string(os.PathSeparator) + "testdata"
	cfg := Config{DSN: dsn, MigrationsDir: migrations, TestdataDir: testdata}

	artifacts, err := Run(context.Background(), cfg)
	require.NoError(t, err)
	require.NoError(t, artifacts.Write(cfg))
	_, err = Check(context.Background(), cfg)
	require.NoError(t, err, "a freshly written tree is in sync")

	path := migrations + string(os.PathSeparator) + upFile
	body, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, append([]byte("-- drift\n"), body...), 0o644))
	_, err = Check(context.Background(), cfg)
	require.Error(t, err)
	require.Contains(t, err.Error(), upFile)
	require.Contains(t, err.Error(), "stale")
}
