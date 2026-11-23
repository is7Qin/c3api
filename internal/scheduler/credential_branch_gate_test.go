// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件是 internal/scheduler 的**凭据分支门禁**：调度层只许按
// credential.ChannelOf(t) 的执行通道分支，不许回到「按类型谓词 / 按类型字面量 /
// 按类型常量」的分支。四条断言（**零白名单**：命中即红，没有豁免名单）：
//
//	① 谓词选择器零命中：IsCodex / IsBPS / IsResponsesProvider 作为 SelectorExpr.Sel。
//	   这一条同时抓方法值形式（`f := tpl.IsBPS`，不带括号也是选择器）。
//	② 凭据类型字面量零命中："api_key" / "responses-special" / "codex-oauth" /
//	   "codex-pat" / "custom" / "bps"（含常量折叠后的拼接，见 constStringExpr）。
//	③ 类型常量零命中：credential.TypeCodexOAuth / TypeCodexPAT / TypeBPS /
//	   TypeResponsesSpecial / TypeAPIKey / TypeCustom（按字段名匹配，别名导入同样抓）。
//	④ string(...CredentialType) 转换零命中（把类型洗成裸字符串正是绕过 ①②③ 的写法）。
//	前置断言：**禁止点导入** internal/credential——点导入会把常量洗成裸 Ident，
//	①②③ 的判据全部失明，所以它本身就是违规。
//
// **已知边界（写在明处，别过度声称）**：
//   - 反射 `MethodByName("IsCodex")` 可绕过（字符串里的方法名不是选择器）；
//   - 跨包替身（在别的包定义一个同义 helper）不在本门禁扫描面内——本门禁只钉
//     internal/scheduler；credential 包侧的「不许新增按类型判定的入口」由
//     internal/credential 的 TestCredentialPkgOwnsNoTypePredicateHelper 守；
//   - **能力判定盲区**：`credential.ChannelOf(t) == credential.ChannelX` 是本门禁
//     **允许**的通道分支形状，所以「某处用通道表达式去回答一个能力问题」（例如
//     "能不能续接"）在形状上合规、本门禁看不见。这一类靠行为用例守：
//     续接两条轴见 continuation_policy_test.go（TestSkipContinuationFor* /
//     TestPlanAllCandidatesNativeContinuationReadsCapabilityNotChannel），它们用
//     "同类型、不同能力答案"的替身把通道实现逼红。
//   - 两种绕过（反射 / 跨包替身）都要人主动作恶，且 ①②③ 仍会命中大部分写法。
//
// 判据只有一处（credentialBranchViolations）：门禁本体与自测喂**同一个函数**，
// 自测不得复制一份判定逻辑（否则测的是副本，不是门禁）。

// bannedPredicateSelectors 是 ① 的名单：类型谓词的方法名。
var bannedPredicateSelectors = map[string]bool{
	"IsCodex":             true,
	"IsBPS":               true,
	"IsResponsesProvider": true,
}

// bannedCredentialLiterals 是 ② 的名单：credential.Type 的底层字面量。
var bannedCredentialLiterals = map[string]bool{
	"api_key":           true,
	"responses-special": true,
	"codex-oauth":       true,
	"codex-pat":         true,
	"custom":            true,
	"bps":               true,
}

// bannedCredentialConstants 是 ③ 的名单：凭据类型常量名（按字段名匹配，故
// `c.TypeBPS`、别名导入的 `cred.TypeBPS` 一律命中）。
var bannedCredentialConstants = map[string]bool{
	"TypeCodexOAuth":       true,
	"TypeCodexPAT":         true,
	"TypeBPS":              true,
	"TypeResponsesSpecial": true,
	"TypeAPIKey":           true,
	"TypeCustom":           true,
}

// credentialImportPath 是点导入前置断言的目标包。
const credentialImportPath = "github.com/is7qin/c3api/internal/credential"

// credentialBranchViolations 是本门禁的**唯一判据**：返回违规描述（空 = 合规），
// 每条形如 `<dir>/<file>:<line>: <原因>`。输出按文件与行号排序，便于 diff。
func credentialBranchViolations(srcs []gateFile) []string {
	var out []string
	for _, src := range srcs {
		site := func(pos token.Pos) string {
			p := src.fset.Position(pos)
			return src.dir + "/" + src.name + ":" + strconv.Itoa(p.Line)
		}
		// 前置：点导入 internal/credential（把常量洗成裸 Ident，判据失明）。
		for _, imp := range src.file.Imports {
			if imp.Name == nil || imp.Name.Name != "." {
				continue
			}
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				continue
			}
			if path == credentialImportPath || strings.HasSuffix(path, "/internal/credential") {
				out = append(out, site(imp.Pos())+": 禁止点导入 internal/credential（常量会被洗成裸 Ident，①②③ 全部失明）")
			}
		}
		ast.Inspect(src.file, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.SelectorExpr:
				if bannedPredicateSelectors[node.Sel.Name] {
					out = append(out, site(node.Pos())+": 凭据类型谓词 "+node.Sel.Name+"（含方法值形式）必须改成 credential.ChannelOf(...) 的通道比较")
				}
				if bannedCredentialConstants[node.Sel.Name] {
					out = append(out, site(node.Pos())+": 凭据类型常量 "+node.Sel.Name+" 必须从 planKey/事实里读，不得在调度层按类型字面判定")
				}
			case *ast.CallExpr:
				// ④ string(x.CredentialType)：把类型洗成裸字符串。
				if id, ok := node.Fun.(*ast.Ident); ok && id.Name == "string" && len(node.Args) == 1 {
					if sel, ok := node.Args[0].(*ast.SelectorExpr); ok && sel.Sel.Name == "CredentialType" {
						out = append(out, site(node.Pos())+": string(...CredentialType) 转换必须禁止（绕过类型常量检查的洗白写法）")
					}
				}
			case *ast.BasicLit, *ast.BinaryExpr:
				// ② 字面量，含 `"api_" + "key"` 这类常量折叠后的拼接。
				if lit, ok := constStringExpr(n); ok && bannedCredentialLiterals[lit] {
					out = append(out, site(n.Pos())+": 凭据类型字面量 "+strconv.Quote(lit)+" 必须从 credential 常量读")
				}
			}
			return true
		})
	}
	sort.Strings(out)
	return out
}

// constStringExpr 求字符串常量表达式的值：字面量与 `+` 拼接（可嵌套）。非
// 常量表达式返回 ok=false。用途：把 `"api_" + "key"` 这类绕过字面量检查的写法
// 折回真值再比对。
func constStringExpr(n ast.Node) (string, bool) {
	switch node := n.(type) {
	case *ast.BasicLit:
		if node.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(node.Value)
		if err != nil {
			return "", false
		}
		return s, true
	case *ast.BinaryExpr:
		if node.Op != token.ADD {
			return "", false
		}
		left, lok := constStringExpr(node.X)
		right, rok := constStringExpr(node.Y)
		if !lok || !rok {
			return "", false
		}
		return left + right, true
	}
	return "", false
}

// TestCredentialBranchGate 门禁本体：扫 internal/scheduler 的**全部非测试 .go**，
// 四条断言 + 点导入前置断言都必须零命中。
func TestCredentialBranchGate(t *testing.T) {
	root := gateRoot(t)
	srcs := gateSources(t, root, []string{"internal/scheduler"})
	require.NotEmpty(t, srcs, "扫描面不得为空（空转的门禁比没有门禁更危险）")
	require.Empty(t, credentialBranchViolations(srcs),
		"internal/scheduler 只许按 ChannelOf 的通道分支：凭据类型谓词/字面量/常量/string 转换一律禁止")
}

// parseGateSource 把一段合成源码解析成 gateFile：自测喂给**生产判据**。
func parseGateSource(t *testing.T, name, body string) gateFile {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, name, body, parser.SkipObjectResolution)
	require.NoError(t, err, "合成源码必须可解析：%s", name)
	return gateFile{dir: "synthetic", name: name, fset: fset, file: f}
}

// TestCredentialBranchGateSelfTest 自测：合成源码 → credentialBranchViolations
// （**同一个生产判据**，不复制判定逻辑）。覆盖四种违规、点导入、常量折叠拼接，
// 以及一条合规对照——对照的作用是证明判据不是"恒报违规"。
func TestCredentialBranchGateSelfTest(t *testing.T) {
	const header = "package scheduler\n\nimport (\n\t\"github.com/is7qin/c3api/internal/credential\"\n\t\"github.com/is7qin/c3api/internal/domain\"\n)\n\nvar _ = credential.ChannelOf\nvar _ = domain.FormatOpenAIResponses\n\n"

	cases := []struct {
		name string
		body string
		want string // 期望违规里出现的片段；"" = 期望零违规
	}{
		{
			name: "compliant_channel_branch",
			body: header + "func f(tpl *domain.Template) bool {\n" +
				"\treturn credential.ChannelOf(tpl.CredentialType) == credential.ChannelCodex\n}\n",
		},
		{
			name: "predicate_call",
			body: header + "func f(tpl *domain.Template) bool { return tpl.CredentialType.IsBPS() }\n",
			want: "IsBPS",
		},
		{
			name: "predicate_method_value",
			body: header + "func f(tpl *domain.Template) func() bool { return tpl.CredentialType.IsCodex }\n",
			want: "IsCodex",
		},
		{
			name: "type_literal",
			body: header + "func f() string { return \"codex-pat\" }\n",
			want: "codex-pat",
		},
		{
			name: "type_literal_folded",
			body: header + "func f() string { return \"api_\" + \"key\" }\n",
			want: "api_key",
		},
		{
			name: "type_constant",
			body: header + "func f() credential.Type { return credential.TypeResponsesSpecial }\n",
			want: "TypeResponsesSpecial",
		},
		{
			name: "string_conversion",
			body: header + "func f(acc *domain.Account) string { return string(acc.CredentialType) }\n",
			want: "string(...CredentialType)",
		},
		{
			name: "dot_import",
			body: "package scheduler\n\nimport . \"github.com/is7qin/c3api/internal/credential\"\n\nfunc f() Type { return TypeBPS }\n",
			want: "禁止点导入",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := credentialBranchViolations([]gateFile{parseGateSource(t, tc.name+".go", tc.body)})
			if tc.want == "" {
				require.Empty(t, got, "合规写法不得被报违规：%v", got)
				return
			}
			require.NotEmpty(t, got, "这个写法必须被拒（判据失明）：%s", tc.body)
			require.Contains(t, strings.Join(got, "\n"), tc.want, "违规必须点名原因")
		})
	}
}

// TestCredentialBranchGateRejectsProductionInjection 变异证明的**常驻**版本：把
// 一处真实生产代码（正例 shape）与一处注入的违规放进同一次扫描，断言违规被点名。
//
// 它不替代实仓变异（报告里另有一次真改文件 ⇒ 红 ⇒ 还原），作用是让"注入即红"这条
// 性质留在套件里，而不是只存在于一次性的手工记录。
func TestCredentialBranchGateRejectsProductionInjection(t *testing.T) {
	root := gateRoot(t)
	real := gateSources(t, root, []string{"internal/scheduler"})
	require.Empty(t, credentialBranchViolations(real), "真实源码必须本来就合规，否则下面的注入证明不了任何东西")

	injected := parseGateSource(t, "injected.go", `package scheduler

import "github.com/is7qin/c3api/internal/domain"

func warnDirty(tpl *domain.Template) bool {
	if tpl.CredentialType.IsBPS() {
		return true
	}
	return false
}
`)
	got := credentialBranchViolations(append(real, injected))
	require.Len(t, got, 1, "注入恰好一处违规：%v", got)
	require.Contains(t, got[0], "synthetic/injected.go:6", "违规必须点名文件与行号")
	require.Contains(t, got[0], "IsBPS")
}
