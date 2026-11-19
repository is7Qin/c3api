// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

// schema 通道门禁（oracle 双裁决必修）：**生产 schema 只有一个通道 = `-migrate up`**。
//
// 背景：`repository.EnsureErrLogPartitioned` 是「从零自举」路径，它建 err_logs 父表
// 之后还会补一条 `ALTER TABLE err_logs ADD COLUMN IF NOT EXISTS attempt_ordinal`
// （`errLogAttemptOrdinalDDL`，与 0003 同源）。那是**迁移账本之外的第二个 schema
// 通道**——它只服务测试夹具与 legacy 生成器（`internal/db/migrate/gen/legacy.go`
// 用它冻结老形状），生产启动路径若调它，就等于绕开版本门给已发布库加列。
//
// 之前这条约束只写在注释里（"生产路径不得调用"），注释不会红。本门把它变成断言：
// 扫 cmd/server 的**全部非测试源**，任何一处提到 `EnsureErrLogPartitioned` 即失败。

import (
	"go/ast"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// bootstrapOnlySchemaFuncs 是「只能出现在测试夹具/legacy 生成器里」的 bootstrap
// 入口。新增这类入口时把它加进本表——门禁是清单驱动的，不是名字前缀匹配。
var bootstrapOnlySchemaFuncs = []string{
	"EnsureErrLogPartitioned", // 建 err_logs 父表 + 补 0003 的 attempt_ordinal 列
}

// TestCmdServerOwnsNoBootstrapSchema 生产装配不得触达 bootstrap 建表/加列入口。
//
// 与 TestCmdServerOwnsNoSchema 的分工：那道门管「不驱动 ent 自动迁移」，这道门管
// 「不驱动手工 bootstrap DDL」——两条都通向同一个后果（schema 有第二个写者），
// 但绕过方式完全不同（一个是 Schema.Create，一个是 Ensure*Partitioned）。
func TestCmdServerOwnsNoBootstrapSchema(t *testing.T) {
	for _, name := range pkgFileNames(t) {
		_, file := parseSrcFile(t, name)
		for _, target := range bootstrapOnlySchemaFuncs {
			// Ident 命名覆盖全部写法：`repository.EnsureErrLogPartitioned` 的 Sel
			// 也是 Ident，点导入与别名同样命中。
			found := false
			ast.Inspect(file, func(node ast.Node) bool {
				if ident, ok := node.(*ast.Ident); ok && ident.Name == target {
					found = true
				}
				return true
			})
			require.False(t, found,
				"%s 引用了 %s：bootstrap 建表/加列只能服务测试夹具与 legacy 生成器；"+
					"生产 schema 只有一个通道 = `c3api -migrate up`", filepath.Base(name), target)
		}
	}
}
