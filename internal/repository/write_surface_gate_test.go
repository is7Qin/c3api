// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// allowedAccountRepoMethods 是 AccountRepo 全部导出方法的显式允许集。
// 本门按语法枚举 receiver 为 AccountRepo 的导出方法，要求枚举结果与此表完全
// 一致：新增、删除或改名任何导出方法都会使断言失败，修改者必须有意改表。
// 新增的配置或身份写入方法若不登记在此表，门必失败，这正是收口要锁住的形状。
//
// 分组（现状与理由）：
//   - 收口（管理面配置与身份写入的唯一入口）：CreateAccount 创建；
//     UpdateAccountsBatch 批量（配置代际无条件推进，身份代际按值推进）；
//     SetAccountGroups 分组归属边缘写（创建与导入事务经此归属）。
//   - 运行时动词（豁免：不推进身份代际）：FailAccountCAS 与 RecoverAccountCAS
//     是失效与恢复的围栏动词；SetAccountFailed 是旧简化路径，只写运行时拥有的
//     三个字段，不推进任何代际。
//   - 软删除 DeleteAccount 与 DeleteAccountsBatch 只写删除标记，不推进代际，
//     不是配置或身份写入。
//   - 其余四项是读。
var allowedAccountRepoMethods = []string{
	"CreateAccount",
	"DeleteAccount",
	"DeleteAccountsBatch",
	"FailAccountCAS",
	"GetAccount",
	"GetAccountGroups",
	"GetAccountWithTemplate",
	"ListAccounts",
	"RecoverAccountCAS",
	"SetAccountFailed",
	"SetAccountGroups",
	"UpdateAccountsBatch",
}

// repositoryAccountSurface 是 Repository 门面上账号域方法的显式允许集。
// 账号域按方法名含 Account、OAuth 或 PAT 判定（其它实体的写入不在此门内，
// 它们各有自己的归属）。此表读写全列，写子集与读子集另有两张分表钉住。
var repositoryAccountSurface = []string{
	"AdminUpsertAccountExtCAS",
	"AdminWriteOAuthRotationCAS",
	"AdminWritePATKeyCAS",
	"CreateAccount",
	"DeleteAccount",
	"DeleteAccountsBatch",
	"FailAccountCAS",
	"FindAccountExtByCodexKey",
	"GetAccount",
	"GetAccountExt",
	"GetAccountGroups",
	"GetAccountWithTemplate",
	"ListAccounts",
	"LoadGroupAccounts",
	"RecoverAccountCAS",
	"SetAccountGroups",
	"TryInsertAccountExt",
	"UpdateAccountsBatch",
	"UpsertAccountExt",
	"WriteOAuthRotation",
}

// allowedRepositoryAccountWrites 是门面上账号域写方法的显式允许集。
// 收口：CreateAccount、UpdateAccountsBatch、三个围栏凭据动词
// （AdminUpsertAccountExtCAS、AdminWriteOAuthRotationCAS、
// AdminWritePATKeyCAS）、事务内导入动词（UpsertAccountExt、
// TryInsertAccountExt）、分组归属（SetAccountGroups）、软删除（DeleteAccount、
// DeleteAccountsBatch）。豁免：WriteOAuthRotation 是运行时凭据刷新，只写扩展
// 表三列，不触账号行；FailAccountCAS 与 RecoverAccountCAS 是运行时失效动词。
var allowedRepositoryAccountWrites = []string{
	"AdminUpsertAccountExtCAS",
	"AdminWriteOAuthRotationCAS",
	"AdminWritePATKeyCAS",
	"CreateAccount",
	"DeleteAccount",
	"DeleteAccountsBatch",
	"FailAccountCAS",
	"RecoverAccountCAS",
	"SetAccountGroups",
	"TryInsertAccountExt",
	"UpdateAccountsBatch",
	"UpsertAccountExt",
	"WriteOAuthRotation",
}

// allowedRepositoryAccountReads 是门面上账号域读方法的显式允许集。
var allowedRepositoryAccountReads = []string{
	"FindAccountExtByCodexKey",
	"GetAccount",
	"GetAccountExt",
	"GetAccountGroups",
	"GetAccountWithTemplate",
	"ListAccounts",
	"LoadGroupAccounts",
}

// downstreamStoreInterfaces 是投给调度器与适配层的持久化接口的显式允许集。
// 收窄的含义：这些接口只含读与运行时动词，管理面写动词（见
// managementWriteVerbs）一个都不含。装配点传入的是完整仓库实现，但消费方只能
// 经此面调用，故新增写方法无法被下游拿到而不改表。
var downstreamStoreInterfaces = []struct {
	file    string
	iface   string
	methods []string
}{
	{"../scheduler/rule_persist.go", "rulePersistStore", []string{"FailAccountCAS", "GetAccount", "GetAccountGroups"}},
	{"../scheduler/rule_persist.go", "rulePersistTemplateStore", []string{"GetAccountWithTemplate"}},
	{"../scheduler/scheduler.go", "Loader", []string{"LoadGroupAccounts", "LoadGroupsAccounts"}},
	{"../sdkbridge/failure.go", "FailureStore", []string{"SetAccountFailed"}},
	{"../sdkbridge/failure.go", "casStore", []string{"FailAccountCAS", "GetAccount"}},
	{"../sdkbridge/failure.go", "casStoreTemplate", []string{"GetAccountWithTemplate"}},
	{"../sdkbridge/failure.go", "groupGetter", []string{"GetAccountGroups"}},
	{"../sdkbridge/codex.go", "RotationStore", []string{"WriteOAuthRotation"}},
}

// managementWriteVerbs 是管理面写动词：下游持久化接口里一个都不许出现。
var managementWriteVerbs = []string{
	"AdminUpsertAccountExtCAS",
	"AdminWriteOAuthRotationCAS",
	"AdminWritePATKeyCAS",
	"CreateAccount",
	"DeleteAccount",
	"DeleteAccountsBatch",
	"SetAccountGroups",
	"TryInsertAccountExt",
	"UpdateAccountsBatch",
	"UpsertAccountExt",
}

// repoPackageDir 返回本包源码目录（被测包 internal/repository 的目录）。
func repoPackageDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller must locate this test file")
	return filepath.Dir(file)
}

// parseGoFiles 解析目录下全部非测试 Go 源文件（测试替身不计入写面）。
func parseGoFiles(t *testing.T, dir string) []*ast.File {
	t.Helper()
	entries, err := filepath.Glob(filepath.Join(dir, "*.go"))
	require.NoError(t, err)
	require.NotEmpty(t, entries, "package directory must contain Go sources: %s", dir)
	fset := token.NewFileSet()
	var out []*ast.File
	for _, path := range entries {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err, "parse %s", path)
		out = append(out, f)
	}
	require.NotEmpty(t, out, "package must contain non-test Go sources")
	return out
}

// parseSingleGoFile 解析单个 Go 源文件（下游接口文件按名显式定位，改名即失败）。
func parseSingleGoFile(t *testing.T, path string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	require.NoError(t, err, "parse %s", path)
	return f
}

// receiverBaseName 取方法 receiver 的基类型名（*T 与 T 都归一为 T）。
func receiverBaseName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return ""
	}
	switch typ := fn.Recv.List[0].Type.(type) {
	case *ast.StarExpr:
		if id, ok := typ.X.(*ast.Ident); ok {
			return id.Name
		}
	case *ast.Ident:
		return typ.Name
	}
	return ""
}

// exportedMethodsOnReceiver 枚举指定 receiver 全部导出方法名（排序后返回）。
// 用语法枚举而不用反射：反射只能看到编译后的方法集，表达不了"方法名是不是写
// 动词"这类语义，也无法断言"不存在"。
func exportedMethodsOnReceiver(files []*ast.File, receiver string) []string {
	var out []string
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || receiverBaseName(fn) != receiver {
				continue
			}
			if fn.Name.IsExported() {
				out = append(out, fn.Name.Name)
			}
		}
	}
	sort.Strings(out)
	return out
}

// interfaceMethods 取具名接口的全部方法名（排序后返回）。接口内出现内嵌接口
// 即失败：内嵌会静默扩大写面，必须显式展开。
func interfaceMethods(t *testing.T, file *ast.File, name string) []string {
	t.Helper()
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != name {
				continue
			}
			iface, ok := ts.Type.(*ast.InterfaceType)
			require.True(t, ok, "%s must be an interface", name)
			var out []string
			for _, m := range iface.Methods.List {
				_, ok := m.Type.(*ast.FuncType)
				require.True(t, ok, "interface %s must list methods explicitly, no embedding", name)
				require.NotEmpty(t, m.Names, "interface %s must name every method", name)
				for _, id := range m.Names {
					out = append(out, id.Name)
				}
			}
			sort.Strings(out)
			return out
		}
	}
	t.Fatalf("interface %s not found", name)
	return nil
}

// funcDeclExists 报告包内是否存在精确同名的函数或方法声明（大小写敏感的整
// 名匹配，AdminWritePATKeyCAS 这类长名不会把 WritePATKey 误判为存在）。
func funcDeclExists(files []*ast.File, name string) bool {
	for _, f := range files {
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Name.Name == name {
				return true
			}
		}
	}
	return false
}

// isAccountDomain 报告门面方法名是否属于账号域。
func isAccountDomain(name string) bool {
	return strings.Contains(name, "Account") ||
		strings.Contains(name, "OAuth") ||
		strings.Contains(name, "PAT")
}

// isReadMethod 报告方法名是否为读动词（Get、List、Find、Load 前缀）。
func isReadMethod(name string) bool {
	return strings.HasPrefix(name, "Get") ||
		strings.HasPrefix(name, "List") ||
		strings.HasPrefix(name, "Find") ||
		strings.HasPrefix(name, "Load")
}

// TestAccountRepoMethodSurface 钉住 AccountRepo 的方法面：枚举结果必须与显式
// 允许集完全一致，新增写方法必须有意改表。
func TestAccountRepoMethodSurface(t *testing.T) {
	files := parseGoFiles(t, repoPackageDir(t))
	require.Equal(t, allowedAccountRepoMethods, exportedMethodsOnReceiver(files, "AccountRepo"),
		"AccountRepo method surface changed: register the new method deliberately or remove it")
}

// TestRepositoryAccountWriteSurface 钉住 Repository 门面的账号域方法面：
// 账号域子集必须与显式表一致，且读写划分必须与两张分表一致。除收口与豁免集
// 外不得有其它配置或身份写入方法。
func TestRepositoryAccountWriteSurface(t *testing.T) {
	files := parseGoFiles(t, repoPackageDir(t))
	var surface []string
	for _, name := range exportedMethodsOnReceiver(files, "Repository") {
		if isAccountDomain(name) {
			surface = append(surface, name)
		}
	}
	sort.Strings(surface)
	require.Equal(t, repositoryAccountSurface, surface,
		"Repository account surface changed: register the new method deliberately or remove it")
	var writes, reads []string
	for _, name := range surface {
		if isReadMethod(name) {
			reads = append(reads, name)
		} else {
			writes = append(writes, name)
		}
	}
	sort.Strings(writes)
	sort.Strings(reads)
	require.Equal(t, allowedRepositoryAccountWrites, writes,
		"Repository account writes changed: only the funnel and the exempt runtime verbs may write account or identity state")
	require.Equal(t, allowedRepositoryAccountReads, reads,
		"Repository account reads changed: register the new read deliberately or remove it")
}

// TestWritePATKeyRemoved 断言无围栏的旧凭据写点已不存在，而围栏三动词仍在。
func TestWritePATKeyRemoved(t *testing.T) {
	files := parseGoFiles(t, repoPackageDir(t))
	require.False(t, funcDeclExists(files, "WritePATKey"),
		"unfenced WritePATKey must stay removed; credential writes go through the fenced CAS verbs")
	for _, name := range []string{
		"AdminUpsertAccountExtCAS",
		"AdminWriteOAuthRotationCAS",
		"AdminWritePATKeyCAS",
		"WriteOAuthRotation",
	} {
		require.True(t, funcDeclExists(files, name), "%s must exist", name)
	}
}

// TestDownstreamStoreInterfacesNarrowed 断言投给调度器与适配层的持久化接口已
// 收窄：每个接口的方法集必须与显式表一致，且不得含有任何管理面写动词。
func TestDownstreamStoreInterfacesNarrowed(t *testing.T) {
	base := repoPackageDir(t)
	for _, tc := range downstreamStoreInterfaces {
		tc := tc
		t.Run(tc.iface, func(t *testing.T) {
			got := interfaceMethods(t, parseSingleGoFile(t, filepath.Join(base, tc.file)), tc.iface)
			require.Equal(t, tc.methods, got,
				"store interface %s changed: keep it narrow, register deliberately", tc.iface)
			for _, verb := range managementWriteVerbs {
				require.NotContains(t, got, verb,
					"store interface %s must not expose management write %s", tc.iface, verb)
			}
		})
	}
}
