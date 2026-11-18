// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"go/ast"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 建表真相只有一处：`c3api -migrate up`。
//
// 为什么源码门和 app 能不能跑起来无关：行为门（internal/repository 的
// TestNewWithoutMigrateCreatesNoSchema）守的是「NewWithoutMigrate 不建表」，
// 但把**调用点**换回 NewWithPG(ctx, drv, true, pool) 时构造函数本身没变——
// 只有看调用点才抓得住。momus 的变异正是这一处，而且整包测试全绿。
//
// 本门的两条契约（都在 cmd/server 全部非测试源的合并视图上求值）：
//
//  1. 不得出现任何 ent 自动迁移调用（`client.Schema.Create(...)`）——那是
//     repository 内部的实现细节，app 进程不该自己驱动建表；
//  2. 每一处仓库构造都必须走具名的 NewWithoutMigrate。`NewWithPG(ctx, drv, false, pool)`
//     语义相同但名字不带契约，一次 flag 翻转（false→true）就能把建表带回启动
//     路径；具名变体让「启动不迁移」在调用点可读，也让这道门有唯一合法答案。
func TestCmdServerOwnsNoSchema(t *testing.T) {
	fset, merged := parsePkgSrc(t)
	aliases := repositoryAliases(t, merged)

	type ctor struct {
		pkg  string // import 的本地名（默认 repository）
		sel  string // 构造器名
		file string
	}
	var ctors []ctor
	var entMigrations []string

	ast.Inspect(merged, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		path, ok := calleePath(call.Fun)
		if !ok {
			return true
		}
		// ent 自动迁移：`client.Schema.Create(...)`（任意接收者）。
		if strings.HasSuffix(path, "Schema.Create") {
			entMigrations = append(entMigrations, path+" @ "+filepath.Base(fset.Position(call.Pos()).Filename))
			return true
		}
		dot := strings.LastIndexByte(path, '.')
		if dot <= 0 {
			return true
		}
		pkg, sel := path[:dot], path[dot+1:]
		if !aliases[pkg] || (sel != "New" && sel != "NewWithPG" && sel != "NewWithoutMigrate") {
			return true
		}
		ctors = append(ctors, ctor{pkg: pkg, sel: sel, file: filepath.Base(fset.Position(call.Pos()).Filename)})
		return true
	})

	require.Empty(t, entMigrations,
		"cmd/server 不得自己驱动 ent 自动迁移：建表真相只有 `c3api -migrate up`，app 进程只读 schema 水位")

	require.NotEmpty(t, ctors,
		"cmd/server 里找不到任何 repository 构造调用：扫描在空转（import 别名或路径变了？）")
	var offenders []string
	files := map[string]bool{}
	for _, c := range ctors {
		files[c.file] = true
		if c.sel != "NewWithoutMigrate" {
			offenders = append(offenders, c.pkg+"."+c.sel+" @ "+c.file)
		}
	}
	sort.Strings(offenders)
	require.Empty(t, offenders,
		"cmd/server 只许用 repository.NewWithoutMigrate 构造仓库：New/NewWithPG 的 migrate 参数是启动期建表的开关，一次翻转就能把 ent 自动迁移带回启动路径")

	// 启动路径必须真的走这个入口——否则「两处都改掉」的变异只剩 CLI 那一处，
	// 上面两条断言仍会绿。
	require.True(t, files["wiring_infra.go"],
		"启动装配（wiring_infra.go）必须用 NewWithoutMigrate 构造仓库；实际构造点：%v", files)
}

// repositoryImportPath 是仓库包的导入路径（解析别名用）。
const repositoryImportPath = "github.com/is7qin/c3api/internal/repository"

// repositoryAliases 返回 repository 包在 cmd/server 里用到的本地名（默认
// "repository"；带别名的 import 也认，避免改个名字就绕过源码门）。
func repositoryAliases(t *testing.T, f *ast.File) map[string]bool {
	t.Helper()
	aliases := map[string]bool{}
	for _, imp := range f.Imports {
		if strings.Trim(imp.Path.Value, `"`) != repositoryImportPath {
			continue
		}
		name := "repository"
		if imp.Name != nil {
			name = imp.Name.Name
		}
		aliases[name] = true
	}
	require.NotEmpty(t, aliases,
		"cmd/server 必须 import %s：启动装配怎么可能不构造仓库？", repositoryImportPath)
	return aliases
}

// calleePath 把调用目标还原成点分路径（`repository.NewWithoutMigrate`、
// `client.Schema.Create`）；对括号表达式剥壳，其它形态返回 false。
func calleePath(e ast.Expr) (string, bool) {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name, true
	case *ast.SelectorExpr:
		base, ok := calleePath(x.X)
		if !ok {
			return "", false
		}
		return base + "." + x.Sel.Name, true
	case *ast.ParenExpr:
		return calleePath(x.X)
	}
	return "", false
}
