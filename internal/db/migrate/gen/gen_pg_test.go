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

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/testsupport/pgtest"
)

// TestCommittedArtifactsMatchGenerator 是 -check 模式的测试入口：重新生成并与已
// 提交的 migrations/ 与 testdata/ 逐字节比对。漂移 ⇒ 红（CI 与本地同一条断言）。
func TestCommittedArtifactsMatchGenerator(t *testing.T) {
	dsn := pgtest.DSN(t)
	warnings, err := Check(context.Background(), Config{DSN: dsn})
	require.NoError(t, err)
	require.Empty(t, warnings, "same PG patch level as the committed artifacts ⇒ no warning expected")
}

// TestGenerateIsDeterministic 连跑两次生成器：产物必须逐字节相同（排序稳定、
// 无 oid/随机量渗入）。
func TestGenerateIsDeterministic(t *testing.T) {
	dsn := pgtest.DSN(t)
	ctx := context.Background()
	first, err := Run(ctx, Config{DSN: dsn})
	require.NoError(t, err)
	second, err := Run(ctx, Config{DSN: dsn})
	require.NoError(t, err)
	require.Equal(t, first.UpSQL, second.UpSQL)
	require.Equal(t, first.DownSQL, second.DownSQL)
	require.Equal(t, first.CatalogTxt, second.CatalogTxt)
	require.Equal(t, first.Stats, second.Stats)
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
	rewrite(recorded - (recorded % 10000) + 1)
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
