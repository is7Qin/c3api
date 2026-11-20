// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 凭据分发单通道门禁：proxy 只许经 credential.ChannelOf 分流，不许按类型判定。
//
// 背景：这里原先有 14 处 `sel.CredentialType.IsX()` 手写分发（caller.go 的
// if / else-if / else 是代表）。类型谓词散落在调用点是「同一策略多处各写一遍」的
// 入口——加一个类型时每一处都要记得改，漏一处就静默走默认分支。现在「怎么分发」只有
// 一个答案（ChannelOf），本门禁把「再写回来」变成红。
//
// 判据必须落在 SelectorExpr 上而不是 CallExpr 上：方法值形式
// `f := sel.CredentialType.IsCodex`（不带括号）在 AST 里是 SelectorExpr，按调用判会漏。
//
// 已知残余（诚实标注，不假装堵死）：
//   - reflect.Value.MethodByName("IsCodex") 一类反射可以绕过本门；那属于故意作恶，
//     只能靠 review。
//   - 白名单函数 respImageDetectOn 内部仍按类型分支：它本质是 capability，日后应并入
//     caps 表。本轮不并——那是 proxy 侧的计数策略（哪些类型计 resp 图片），不是凭据
//     语义，硬并会让凭据表长出与鉴权无关的列。
var credentialPredicateNames = map[string]bool{
	"IsCodex":             true,
	"IsBPS":               true,
	"IsResponsesProvider": true,
}

// credentialTypeLiterals 是凭据类型的线上字面量：写死字符串比较等于把类型分支搬回
// 调用点（今天 proxy 非测试代码里 0 处）。
var credentialTypeLiterals = map[string]bool{
	"api_key":           true,
	"responses-special": true,
	"codex-oauth":       true,
	"codex-pat":         true,
	"custom":            true,
	"bps":               true,
}

// credentialTypeConstNames 是凭据类型常量：只许出现在白名单函数的函数体内。
var credentialTypeConstNames = map[string]bool{
	"TypeCodexOAuth":       true,
	"TypeCodexPAT":         true,
	"TypeBPS":              true,
	"TypeResponsesSpecial": true,
}

const (
	credentialTypeConstFile = "usage_extract.go"
	credentialTypeConstFunc = "respImageDetectOn"
	credentialImportPath    = "github.com/is7qin/c3api/internal/credential"
)

// TestProxyHasNoCredentialBranch 四条断言（全部扫非测试源，逐文件）：
//  1. 谓词选择器零命中（含方法值形式）；2. 凭据类型字面量零命中；
//  3. string(...) 转换零命中；4. 类型常量只出现在白名单函数的函数体内。
//
// 另加一条前置断言：禁止点导入 internal/credential——点导入会把 `credential.TypeBPS`
// 洗成裸 Ident，上面 1/4 两条按 SelectorExpr 的判据会失明（与 sdkbridge 边界门禁
// 同款理由）。
func TestProxyHasNoCredentialBranch(t *testing.T) {
	files := proxySourceFiles(t)
	require.Positive(t, len(files), "internal/proxy 非测试源文件为 0：门禁在空转")

	for _, path := range files {
		fset := token.NewFileSet()
		src, err := os.ReadFile(path)
		require.NoError(t, err)
		af, err := parser.ParseFile(fset, path, src, parser.ParseComments)
		require.NoError(t, err, "解析 %s", path)
		require.False(t, dotImportsCredential(af), "%s：点导入 internal/credential 会绕过本门禁（改用 credential.ChannelOf 限定名）", path)
		start, end, windowOK := credentialTypeConstWindow(t, af, filepath.Base(path))

		ast.Inspect(af, func(node ast.Node) bool {
			switch x := node.(type) {
			case *ast.SelectorExpr:
				require.False(t, credentialPredicateNames[x.Sel.Name],
					"%s：直接用了凭据类型的 %s——proxy 的分发只许经 credential.ChannelOf（方法值形式同样禁止）",
					fset.Position(x.Pos()), x.Sel.Name)
				if credentialTypeConstNames[x.Sel.Name] {
					pos := x.Pos()
					require.True(t, windowOK && pos >= start && pos <= end,
						"%s：引用了凭据类型常量 %s——类型常量只许出现在 %s 的 %s（其余分发走 ChannelOf）",
						fset.Position(pos), x.Sel.Name, credentialTypeConstFile, credentialTypeConstFunc)
				}
			case *ast.BasicLit:
				if x.Kind != token.STRING {
					return true
				}
				lit, unqErr := strconv.Unquote(x.Value)
				require.NoError(t, unqErr, "%s：解析字符串字面量", fset.Position(x.Pos()))
				require.False(t, credentialTypeLiterals[lit],
					"%s：凭据类型字面量 %q——按类型字符串分支等同于绕过 ChannelOf", fset.Position(x.Pos()), lit)
			case *ast.CallExpr:
				if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "string" && len(x.Args) == 1 {
					require.False(t, credentialSelectorSuffix(x.Args[0], "CredentialType"),
						"%s：string(...) 把凭据类型转成了字符串——按类型字符串分支等同于绕过 ChannelOf",
						fset.Position(x.Pos()))
				}
			}
			return true
		})
	}
}

// dotImportsCredential 判断文件是否点导入 internal/credential。
func dotImportsCredential(af *ast.File) bool {
	for _, spec := range af.Imports {
		if strings.Trim(spec.Path.Value, `"`) != credentialImportPath || spec.Name == nil {
			continue
		}
		if spec.Name.Name == "." {
			return true
		}
	}
	return false
}

// credentialTypeConstWindow 给出白名单函数函数体的位置窗口。函数改名/消失即 FailNow：
// 静默放行整包比没有门禁更糟。
func credentialTypeConstWindow(t *testing.T, af *ast.File, file string) (token.Pos, token.Pos, bool) {
	t.Helper()
	if file != credentialTypeConstFile {
		return token.NoPos, token.NoPos, false
	}
	for _, decl := range af.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != credentialTypeConstFunc || fn.Body == nil {
			continue
		}
		return fn.Body.Pos(), fn.Body.End(), true
	}
	require.FailNow(t, "凭据类型常量白名单失效",
		"%s 里找不到 %s：改名请同步本门禁，不许退化成「整包放行」", credentialTypeConstFile, credentialTypeConstFunc)
	return token.NoPos, token.NoPos, false
}

// credentialSelectorSuffix 判断表达式是否以 name 结尾：sel.CredentialType（SelectorExpr）
// 或点导入后的 CredentialType（裸 Ident）。
func credentialSelectorSuffix(expr ast.Expr, name string) bool {
	switch x := expr.(type) {
	case *ast.SelectorExpr:
		return x.Sel.Name == name
	case *ast.Ident:
		return x.Name == name
	}
	return false
}

// proxySourceFiles 列出 internal/proxy 全部**非测试** `.go`（排序 ⇒ 稳定）。
func proxySourceFiles(t *testing.T) []string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	require.True(t, ok)
	dir := filepath.Dir(self)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		names = append(names, filepath.Join(dir, name))
	}
	sort.Strings(names)
	return names
}
