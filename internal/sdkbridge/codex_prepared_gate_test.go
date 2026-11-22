// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件钉住 Codex 适配层的**唯一入口形状**：请求先 `Prepare` 拿句柄，再调
// `*Prepared`。未 Prepared 的 `Responses/StreamBody/Dial` 已删除——它们把
// "凭据 → 条目 → client/auth" 的两次查表藏在一次调用里，绕过 Prepared 边界
// （同一版本的 auth 与 owner）在类型层是**不可表达**的。
//
// 门禁为什么不是"*Codex 的导出方法集合 == 白名单"（对抗评审 C-2 否掉的那个形状）：
// *Codex 的导出面本来就宽（DirectEgress / GenerateImage* / Search / Prepare /
// RetireThread / GetUsageSnapshot / QuotaDeadline / FatalPrepared / *Call …），
// 而且其中多数合法地**不**走 Prepared。把整个导出集合冻成白名单，得到的是脆弱的
// API 快照：每加一个无关方法都要改门禁，改了也不代表挡住了绕过。这里只锁两件
// 与安全意图等价的事：
//
//  1. 三个**精确禁名**不存在，三个 Prepared 名存在（名字即边界）；
//  2. 新增方法不得重新长出一条绕过面：`(*Codex)` 上参数含 `*AccountCredential`
//     且体内直接调 SDK client 的 `Responses/StreamBody/Dial` 的方法，必须显式登记。

// unpreparedEntryNames 是已删除的入口方法名。它们不是"不推荐"，是不存在。
var unpreparedEntryNames = []string{"Responses", "StreamBody", "Dial"}

// preparedEntryNames 是它们**唯一**的合法替代（同一语义，改用句柄）。
var preparedEntryNames = []string{"ResponsesPrepared", "StreamBodyPrepared", "DialPrepared"}

// prepareBypassAllowlist 是 AST 规则的免登记表：键 = `(*Codex)` 方法名，值 = 为什么
// 这个方法必须自己调 SDK 的未 Prepared 形态。**当前应为空**：请求准备是 `Prepare`
// 的事，热路径上没有任何理由再讲一遍。
//
// 上限 2（评审给的形状）：免登记一旦成表就会变成"绕过面的官方住址"。真需要第 3 条
// 时，该讨论的是把这条规则本身改对，而不是继续加行。
var prepareBypassAllowlist = map[string]string{}

// TestCodexUnpreparedEntryPointsAreRemoved 名字门禁：禁名不存在、Prepared 名存在。
//
// 用 reflect 读**真实方法集**而不是 AST 文本：方法集包含提升（嵌入类型带进来的
// 方法）与所有文件里的声明，文本扫描两样都会漏。这也顺带钉住"没被同名嵌入方法
// 顶替"——那种形态下 `a.Responses(...)` 依然可编译，禁名却看起来"删干净了"。
func TestCodexUnpreparedEntryPointsAreRemoved(t *testing.T) {
	codexType := reflect.TypeOf((*Codex)(nil))

	for _, name := range unpreparedEntryNames {
		_, ok := codexType.MethodByName(name)
		require.False(t, ok, "(*Codex).%s 必须不存在：它会自己 Prepare，从而绕过"+
			"\"同一句柄的 auth/owner\"边界。要用就调 %sPrepared", name, name)
	}
	for _, name := range preparedEntryNames {
		_, ok := codexType.MethodByName(name)
		require.True(t, ok, "(*Codex).%s 必须存在：它是 %s 的唯一合法入口",
			name, strings.TrimSuffix(name, "Prepared"))
	}
}

// TestCodexCredentialMethodsDoNotBypassPrepare AST 规则：防"换个名字重新长回来"。
//
// 判据：`(*Codex)` 上的方法，参数含 `*AccountCredential`（凭据在手 = 有能力自己
// 查条目），且体内的**直接**选择器调用命中 `Responses/StreamBody/Dial`。命中即
// 视为一条未 Prepared 入口，必须登记进 prepareBypassAllowlist（双向相等：登记了
// 却不再命中同样报错——免登记表只能是"当前存在的例外"，不能变成历史豁免）。
//
// 不误判 SDK 面：`Search`/`GenerateImage*`/usage 面调的是 client 的别的方法名；
// `stream_body_once.go` 调第三方 client 的 StreamBody，但它是普通函数、参数不是
// 凭据，不在扫描面。
//
// 扫描面是该包**全部非测试 .go**（不是只有 codex.go）：新建一个文件放绕过方法
// 同样要被挡住。
func TestCodexCredentialMethodsDoNotBypassPrepare(t *testing.T) {
	require.LessOrEqual(t, len(prepareBypassAllowlist), 2,
		"免登记表最多 2 条：再长就不是例外，是通道")

	root := moduleRoot(t)
	forbidden := map[string]bool{}
	for _, name := range unpreparedEntryNames {
		forbidden[name] = true
	}

	hits := map[string]bool{}
	for _, path := range goFiles(t, filepath.Join(root, selfPkgDir)) {
		rel := relSlash(root, path)
		if strings.HasSuffix(rel, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		af, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		require.NoError(t, err, "解析 %s 失败", rel)
		for _, decl := range af.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || !isCodexMethod(fd) || !takesAccountCredential(fd) {
				continue
			}
			if callsForbiddenForm(fd, forbidden) {
				hits[fd.Name.Name] = true
			}
		}
	}

	for method := range hits {
		if _, ok := prepareBypassAllowlist[method]; !ok {
			t.Errorf("(*Codex).%s 参数含 *AccountCredential 且直接调 SDK 的未 Prepared 形态："+
				"这是一条绕过 Prepare 的入口。改走 *Prepared，或登记进 prepareBypassAllowlist 并写明理由",
				method)
		}
	}
	for method, why := range prepareBypassAllowlist {
		if !hits[method] {
			t.Errorf("免登记陈旧：(*Codex).%s 已不再直接调未 Prepared 形态（%s）。请删掉这一行",
				method, why)
		}
	}
}

// isCodexMethod 报告 fd 是否声明在 `*Codex`（或 Codex）上。
func isCodexMethod(fd *ast.FuncDecl) bool {
	if fd.Recv == nil || len(fd.Recv.List) != 1 {
		return false
	}
	t := fd.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	id, ok := t.(*ast.Ident)
	return ok && id.Name == "Codex"
}

// takesAccountCredential 报告方法签名里是否有 `*AccountCredential` 参数。
//
// 只认类型名不认包限定符：`domain.AccountCredential`、别名导入、点导入、乃至本包
// 同名类型都算命中——规则要挡的是"手里有凭据就能绕"，不是某个 import 路径的拼写。
func takesAccountCredential(fd *ast.FuncDecl) bool {
	if fd.Type.Params == nil {
		return false
	}
	for _, field := range fd.Type.Params.List {
		if isCredentialType(field.Type) {
			return true
		}
	}
	return false
}

// isCredentialType 解一层指针/切片后看类型名是否为 AccountCredential。
func isCredentialType(expr ast.Expr) bool {
	switch e := expr.(type) {
	case *ast.StarExpr:
		return isCredentialType(e.X)
	case *ast.SelectorExpr:
		return e.Sel.Name == "AccountCredential"
	case *ast.Ident:
		return e.Name == "AccountCredential"
	}
	return false
}

// callsForbiddenForm 报告方法体内是否直接调用了 forbidden 里的方法名。
//
// 跳过接收者自身的调用（`a.Responses(...)`）：那不是"直接调 SDK"，而且真出现时
// 名字门禁先红。其余任何接收者（`client`、`e.client`、`sdk`…）都算命中。
func callsForbiddenForm(fd *ast.FuncDecl, forbidden map[string]bool) bool {
	recv := ""
	if names := fd.Recv.List[0].Names; len(names) > 0 {
		recv = names[0].Name
	}
	hit := false
	ast.Inspect(fd.Body, func(n ast.Node) bool {
		if hit {
			return false
		}
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !forbidden[sel.Sel.Name] {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == recv {
			return true
		}
		hit = true
		return false
	})
	return hit
}
