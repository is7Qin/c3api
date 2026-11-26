// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package gen

// request_patch_test.go 是 custom request_patch 的「0004 迁移 ↔ PostBaselineColumns」
// 互锁里**不连 PG** 的两向（spec 2026-11-26-custom-request-patch §4.2 / §9）：
//
//	(a) 0004 up/down 文本逐字钉死：迁移一经发布即不可变（账本 checksum 漂移 ⇒
//	    启动门 NotReady），改错只能新增迁移；这条锚让「改了一个字节」在评审期就红，
//	    而不是等某个已应用 0004 的库升级时才发现；
//	(b) 0004 up 的 ADD COLUMN 集 ↔ repository.PostBaselineColumns() 双向相等：
//	    多 = 0001 生成器会裁掉一列却没人补（运行期 42703）；少 = 列进了冻结的 0001
//	    （已发布库 checksum 漂移）。多/少都必须红，不能只断言「清单 ⊆ 迁移」。
//
// 文本锚的代价是「改注释也要改这里」——这是刻意的：迁移文件不是文档。

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/repository"
)

const (
	customRequestPatchUpFile   = "0004_custom_request_patch.up.sql"
	customRequestPatchDownFile = "0004_custom_request_patch.down.sql"
)

// customRequestPatchUpSQL / customRequestPatchDownSQL 是 0004 两个迁移文件的逐字
// 内容（含注释与空行）。只允许按「新增迁移」的方式演进，禁止就地改。
const customRequestPatchUpSQL = `-- 0004_custom_request_patch.up.sql — 手写迁移（**非生成物**）。
--
-- 为什么手写、为什么不重生成 0001：template_exts.request_patch 是 request_patch
-- 功能（custom 模板出站请求修饰器，spec 2026-11-26-custom-request-patch §4.2）的
-- 持久化列。0001_baseline 一旦发布即不可变（checksum 漂移 ⇒ 启动门 NotReady），
-- 所以 ent schema 新增的列**不重生成 0001**，而是登记到
-- repository.PostBaselineColumns()（baseline 生成器据此裁列），物理列由本迁移
-- ADD COLUMN 建立。
--
-- 为什么是 expand：只加可空列，旧二进制不读新列（回退无需 down）；回滚走 CLI 的
-- -allow-destructive（DROP COLUMN 是破坏性动作）。
--
-- 本列上层承载的 JSON 形状与校验属控制面（domain/service），迁移只管物理列。
SET search_path = public;

ALTER TABLE public.template_exts ADD COLUMN request_patch jsonb NULL;
`

const customRequestPatchDownSQL = `-- 0004_custom_request_patch.down.sql — 回滚 0004。
-- ROLLBACK DROPS: template_exts.request_patch 的全部 custom 请求修饰器配置。
-- 只允许在「旧版本 pod 已全部退出」后执行（见 CHANGELOG 的 down 前置条件）。
SET search_path = public;

ALTER TABLE public.template_exts DROP COLUMN IF EXISTS request_patch;
`

// readMigration 读真实迁移目录里的文件（默认目录由本包源码位置解析，与 cwd 无关）。
func readMigration(t *testing.T, name string) string {
	t.Helper()
	migrationsDir, _, err := defaultDirs()
	require.NoError(t, err)
	body, err := os.ReadFile(filepath.Join(migrationsDir, name))
	require.NoError(t, err)
	return string(body)
}

// TestCustomRequestPatchMigrationTextIsPinned 钉住 0004 up/down 的逐字文本。
func TestCustomRequestPatchMigrationTextIsPinned(t *testing.T) {
	up := readMigration(t, customRequestPatchUpFile)
	down := readMigration(t, customRequestPatchDownFile)
	require.Equal(t, customRequestPatchUpSQL, up,
		"0004 up 文本是冻结的：列/注释的任何改动都必须新增迁移，不能就地漂移")
	require.Equal(t, customRequestPatchDownSQL, down,
		"0004 down 文本是冻结的")

	// LF-only / 无 BOM：snapshotHead 只在「0004 之后再来一条迁移」时才读本文件校验，
	// 这里提前钉死——Windows 编辑器把 0004 存成 CRLF 时账本 checksum 会被规范化
	// 掩盖（canonicalSQL 吃掉换行差异），但文件字节仍不符合迁移不变量的要求。
	for name, body := range map[string]string{customRequestPatchUpFile: up, customRequestPatchDownFile: down} {
		require.NotContains(t, body, "\r", "%s 必须是 LF-only", name)
		require.False(t, strings.HasPrefix(body, "\ufeff"), "%s 不得带 UTF-8 BOM", name)
	}
}

// addColumnRE 匹配 0004 up 里真正的 ADD COLUMN 语句（public. 限定 + 行首）。
// 注释先剥掉：注释里出现 ADD COLUMN 字样不得算进互锁清单。
var addColumnRE = regexp.MustCompile(`(?m)^ALTER TABLE public\.([a-z0-9_]+) ADD COLUMN ([a-z0-9_]+) `)

// sqlCommentLineRE 剥行注释（与 migrate.validateSQL 的 commentLineRE 同形；本包
// 不能 import migrate 的内部正则，故就地声明——差异由本文件的两向断言兜底）。
var sqlCommentLineRE = regexp.MustCompile(`(?m)^\s*--.*$`)

// TestCustomRequestPatchMigrationMatchesPostBaselineColumns 是反向互锁：0004 up 的
// ADD COLUMN 集与 repository.PostBaselineColumns() **双向**相等。
func TestCustomRequestPatchMigrationMatchesPostBaselineColumns(t *testing.T) {
	up := readMigration(t, customRequestPatchUpFile)
	body := sqlCommentLineRE.ReplaceAllString(up, "")

	fromMigration := make(map[string]struct{})
	for _, match := range addColumnRE.FindAllStringSubmatch(body, -1) {
		key := match[1] + "." + match[2]
		require.NotContains(t, fromMigration, key, "0004 对同一列重复 ADD COLUMN（%s）", key)
		fromMigration[key] = struct{}{}
	}

	declared := make(map[string]struct{})
	for _, column := range repository.PostBaselineColumns() {
		key := column.Table + "." + column.Column
		require.NotContains(t, declared, key, "PostBaselineColumns 里 %s 重复声明", key)
		declared[key] = struct{}{}
	}
	require.NotEmpty(t, declared, "空的 PostBaselineColumns 会让这条互锁变成空话")

	// 逐项点名（require.Equal 的 map diff 对「哪一列多了/少了」不够可归因）。
	for key := range fromMigration {
		require.Contains(t, declared, key,
			"0004 加了 %s，但 repository.PostBaselineColumns 没声明 ⇒ gen 会把它写进冻结的 0001", key)
	}
	for key := range declared {
		require.Contains(t, fromMigration, key,
			"repository.PostBaselineColumns 声明了 %s，但 0004 没有 ADD COLUMN ⇒ 运行期 42703", key)
	}
	require.Equal(t, declared, fromMigration,
		"0004 up 的 ADD COLUMN 集必须与 repository.PostBaselineColumns() 双向相等")
}
