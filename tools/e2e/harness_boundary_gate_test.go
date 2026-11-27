// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 资源/进程边界 gate（spec §4.2/§4.4；不带 e2e build tag => 普通 test job 跑）。
//
// 机制：go/parser AST 扫描 tools/e2e/*_test.go（场景与自测文件），禁止：
//   - 自建二进制/exec 被测进程（exec.Command(srvBin/upBin)、cmd/server、tools/fakeupstream）；
//   - CREATE/DROP DATABASE（建删库唯一入口是 harness）；
//   - "-migrate"（迁移唯一入口是 harness 的 run 级 lazy migrate）；
//   - docker 调用 / 固定容器名（deploy-redis-1 / deploy-db-1）；
//   - 固定回环端口字面量（只允许 127.0.0.1:0 与 httptest）与固定 e2e 库名；
//   - 计算型固定端口（"127.0.0.1:" + "18094" / fmt.Sprintf("127.0.0.1:%d", p)）与
//     场景自建监听（net.Listen*、*.ListenAndServe*；in-process 上游只能用 httptest）；
//   - 子进程 env 设置（cmd.Env = ... / os.Setenv；env deny-list 是 harness 独占能力）；
//   - t.Parallel（e2e 必须串行）。
//
// 豁免 = **显式文件白名单**（不接受 harness_* 前缀模式）。P1 迁移完成后全目录生效：
// 不再存在 legacy 文件名 allowlist（billing/routing/routing_multi 与其它场景同权扫描）。
package e2e

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// boundaryViolation 违规项（file:line:reason）。
type boundaryViolation struct {
	File   string
	Line   int
	Reason string
}

func (v boundaryViolation) String() string {
	return fmt.Sprintf("%s:%d: %s", v.File, v.Line, v.Reason)
}

// boundaryExemptFiles 显式白名单（spec §4.4；不接受 harness_* 前缀豁免）：
// harness 自身实现与自测可以 exec/DB/docker/env 操作；场景文件不能。
var boundaryExemptFiles = map[string]bool{
	"harness_test.go":               true,
	"harness_process_test.go":       true,
	"harness_services_test.go":      true,
	"harness_registry_test.go":      true,
	"harness_gate_test.go":          true,
	"harness_boundary_gate_test.go": true,
	// 断言 gate 本身也是 harness 文件（只读 git/BASE 源码，不持有外部资源）；它读取
	// E2E_BASELINE_* 生成器开关，属于 gate 实现而非场景。
	"assertion_surface_gate_test.go": true,
}

var (
	boundaryDBDDLRe     = regexp.MustCompile(`(?i)\b(create|drop)\s+database\b`)
	boundaryLoopbackRe  = regexp.MustCompile(`(?i)\b(127\.0\.0\.1|localhost):(\d+)\b`)
	boundaryDockerRe    = regexp.MustCompile(`(?i)\bdocker\b`)
	boundaryContainerRe = regexp.MustCompile(`(?i)\bdeploy-(redis|db)-1\b`)
	// boundaryLoopbackConstructRe 计算型固定端口启发式（P0 review momus Minor-1 /
	// spec §13）：字面量固定端口已被 boundaryLoopbackRe 拦下；这里抓「回环主机 + 冒号」
	// 之后没有明确 :0 端口的构造（拼接 "127.0.0.1:" + "18094"、fmt.Sprintf(...%d...)）。
	boundaryLoopbackConstructRe = regexp.MustCompile(`(?i)(127\.0\.0\.1|localhost):`)
)

// boundaryFixedDBNames 固定库名（每 run 唯一库名由 harness 生成；场景不得出现字面量）。
var boundaryFixedDBNames = []string{
	"c3api_e2e", "c3api_routing_e2e", "c3api_supplier_e2e", "c3api_request_patch_e2e",
}

// literalValue 取字符串字面量的内容（解释型与 raw 字符串都支持）。
func literalValue(lit *ast.BasicLit) string {
	if value, err := strconv.Unquote(lit.Value); err == nil {
		return value
	}
	return lit.Value
}

// scanBoundaryLiteral 检查单个字符串字面量的违规面。
func scanBoundaryLiteral(value string) string {
	if strings.Contains(value, "-migrate") {
		return "禁止内联 -migrate（迁移唯一入口是 harness 的 run 级 lazy migrate）"
	}
	if boundaryDBDDLRe.MatchString(value) {
		return "禁止 CREATE/DROP DATABASE（建删库唯一入口是 harness）"
	}
	if boundaryDockerRe.MatchString(value) {
		return "禁止 docker 调用/字面量（专用栈只能经 harness e2eServices）"
	}
	if boundaryContainerRe.MatchString(value) {
		return "禁止固定容器名 deploy-redis-1/deploy-db-1（专用栈用唯一 project）"
	}
	for _, dbName := range boundaryFixedDBNames {
		if strings.Contains(value, dbName) {
			return "禁止固定 e2e 库名 " + dbName + "（每 run 唯一库名由 harness 生成）"
		}
	}
	if m := boundaryLoopbackRe.FindStringSubmatch(value); m != nil {
		if m[2] != "0" {
			return "禁止固定端口字面量（动态端口由 harness 分配；只允许 127.0.0.1:0）"
		}
	}
	// 计算型固定端口启发式：回环主机字面量后紧跟非数字（拼接/格式化占位）或非 :0 端口。
	if loc := boundaryLoopbackConstructRe.FindStringIndex(value); loc != nil {
		rest := value[loc[1]:]
		portLen := 0
		for portLen < len(rest) && rest[portLen] >= '0' && rest[portLen] <= '9' {
			portLen++
		}
		if rest[:portLen] != "0" {
			return "禁止计算型固定端口（回环主机字面量后拼接/格式化端口；动态端口由 harness 分配，只允许 127.0.0.1:0）"
		}
	}
	// 场景不得直读 C3API_*/E2E_* env（base 资源与测试旋钮由 harness 独占）。
	if strings.HasPrefix(value, "C3API_") || strings.HasPrefix(value, "E2E_") {
		return "禁止场景直读 C3API_*/E2E_* env（由 harness 独占）"
	}
	return ""
}

// scanBoundaryViolations 对「文件内容集合」做 AST 边界扫描（纯函数，变异测试复用）。
func scanBoundaryViolations(files map[string][]byte) []boundaryViolation {
	var violations []boundaryViolation
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if boundaryExemptFiles[name] {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, files[name], parser.ParseComments)
		if err != nil {
			violations = append(violations, boundaryViolation{File: name, Line: 1, Reason: "语法错误：" + err.Error()})
			continue
		}
		add := func(pos token.Pos, reason string) {
			violations = append(violations, boundaryViolation{File: name, Line: fset.Position(pos).Line, Reason: reason})
		}
		for _, imp := range file.Imports {
			if literalValue(imp.Path) == "os/exec" {
				add(imp.Pos(), "禁止场景 exec 外部进程（子进程生命周期归 harness；import os/exec）")
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch n := node.(type) {
			case *ast.CallExpr:
				if sel, ok := n.Fun.(*ast.SelectorExpr); ok {
					if qual, ok := sel.X.(*ast.Ident); ok {
						if qual.Name == "t" && sel.Sel.Name == "Parallel" {
							add(n.Pos(), "禁止 t.Parallel（e2e 串行）")
						}
						if qual.Name == "os" && sel.Sel.Name == "Setenv" {
							add(n.Pos(), "禁止场景设置进程 env（env deny-list 是 harness 独占能力）")
						}
						// 场景禁止自建监听（P0 review momus Minor-1 / spec §13）：监听端口/
						// 进程生命周期归 harness；进程内上游只能用 httptest（其自身 :0 监听
						// 属于允许项，故不拦 httptest.NewServer 等调用）。
						switch {
						case qual.Name == "net" && strings.HasPrefix(sel.Sel.Name, "Listen"):
							add(n.Pos(), "禁止场景自建监听（net.Listen* 归 harness；进程内上游用 httptest）")
						case sel.Sel.Name == "ListenAndServe" || sel.Sel.Name == "ListenAndServeTLS":
							add(n.Pos(), "禁止场景自建监听（*.ListenAndServe* 归 harness/httptest）")
						case sel.Sel.Name == "Listen" || sel.Sel.Name == "ListenPacket":
							add(n.Pos(), "禁止场景自建监听（*.Listen/ListenPacket 归 harness/httptest）")
						}
					}
					if sel.Sel.Name == "Command" || sel.Sel.Name == "CommandContext" {
						start := 1
						if sel.Sel.Name == "CommandContext" {
							start = 2
						}
						if len(n.Args) > start {
							if ident, ok := n.Args[start].(*ast.Ident); ok && (ident.Name == "srvBin" || ident.Name == "upBin") {
								add(n.Pos(), "禁止场景自建被测进程（exec srvBin/upBin 归 harness）")
							}
							if lit, ok := n.Args[start].(*ast.BasicLit); ok {
								value := literalValue(lit)
								if strings.Contains(value, "cmd/server") || strings.Contains(value, "tools/fakeupstream") {
									add(n.Pos(), "禁止场景 go build/exec 被测二进制（构建归 harness）")
								}
							}
						}
					}
				}
			case *ast.BasicLit:
				if n.Kind != token.STRING {
					return true
				}
				if reason := scanBoundaryLiteral(literalValue(n)); reason != "" {
					add(n.Pos(), reason)
				}
			case *ast.AssignStmt:
				// 子进程 env 设置：cmd.Env = ...
				for _, lhs := range n.Lhs {
					if sel, ok := lhs.(*ast.SelectorExpr); ok && sel.Sel.Name == "Env" {
						add(n.Pos(), "禁止场景设置子进程 env（cmd.Env = ... 归 harness）")
					}
				}
			}
			return true
		})
	}
	return violations
}

// formatBoundaryViolations 违规列表 -> 可读文本（gate 失败信息）。
func formatBoundaryViolations(violations []boundaryViolation) string {
	if len(violations) == 0 {
		return ""
	}
	lines := make([]string, 0, len(violations))
	for _, v := range violations {
		lines = append(lines, v.String())
	}
	return strings.Join(lines, "\n")
}

// TestHarnessBoundaryGateNoLegacyExemption 全目录生效自测（P1）：boundary gate 不再按
// 文件名放行任何场景文件——即使文件名叫 billing_e2e_test.go / routing_e2e_test.go /
// routing_multi_e2e_test.go（P0 legacy allowlist），固定端口字面量也照常红。
func TestHarnessBoundaryGateNoLegacyExemption(t *testing.T) {
	files := map[string][]byte{
		"billing_e2e_test.go":       []byte("package e2e\n\nimport \"testing\"\n\nconst legacyFixedAddr = \"127.0.0.1:18090\"\n\nfunc TestLegacy(t *testing.T) { _ = legacyFixedAddr }\n"),
		"routing_e2e_test.go":       []byte("package e2e\n\nimport \"testing\"\n\nconst rtFixedAddr = \"127.0.0.1:18093\"\n\nfunc TestRT(t *testing.T) { _ = rtFixedAddr }\n"),
		"routing_multi_e2e_test.go": []byte("package e2e\n\nimport \"testing\"\n\nconst rmFixedAddr = \"127.0.0.1:18094\"\n\nfunc TestRM(t *testing.T) { _ = rmFixedAddr }\n"),
	}
	violations := scanBoundaryViolations(files)
	require.Len(t, violations, 3, "legacy 文件名不再豁免（全目录生效）：%s", formatBoundaryViolations(violations))
	for _, v := range violations {
		require.Contains(t, v.Reason, "禁止固定端口字面量", "违规原因：%s", v.String())
	}
}

// TestHarnessBoundaryGateComputedPort 计算型固定端口 / 自建监听启发式自测（spec §13 /
// P0 review momus Minor-1）：拼接常量、Sprintf 格式化端口、net.Listen、*.ListenAndServe
// 必须红；httptest、127.0.0.1:0、net.Dial、显式白名单文件必须绿。
func TestHarnessBoundaryGateComputedPort(t *testing.T) {
	header := "package e2e\n\nimport \"testing\"\n\n"
	mk := func(body string) map[string][]byte {
		return map[string][]byte{"scenario_test.go": []byte(header + body)}
	}
	red := []struct{ name, body, want string }{
		{"concat_const", "const hostPart = \"127.0.0.1:\"\nconst fixedAddr = hostPart + \"18094\"\n\nfunc TestEvilConcat(t *testing.T) {\n\t_ = fixedAddr\n}\n", "计算型固定端口"},
		{"concat_inline", "func TestEvilConcatInline(t *testing.T) {\n\t_ = \"127.0.0.1:\" + \"18094\"\n}\n", "计算型固定端口"},
		{"sprintf", "func TestEvilSprintf(t *testing.T) {\n\t_ = fmt.Sprintf(\"127.0.0.1:%d\", 18094)\n}\n", "计算型固定端口"},
		{"localhost_concat", "func TestEvilLocalhost(t *testing.T) {\n\t_ = \"localhost:\" + \"18094\"\n}\n", "计算型固定端口"},
		{"net_listen", "func TestEvilListen(t *testing.T) {\n\t_, _ = net.Listen(\"tcp\", \"127.0.0.1:0\")\n}\n", "禁止场景自建监听"},
		{"http_listen_and_serve", "func TestEvilHTTPListen(t *testing.T) {\n\t_ = http.ListenAndServe(\"127.0.0.1:0\", nil)\n}\n", "禁止场景自建监听"},
	}
	for _, tc := range red {
		violations := scanBoundaryViolations(mk(tc.body))
		require.NotEmpty(t, violations, "计算型/自建监听变异 %s 必须红", tc.name)
		require.Contains(t, formatBoundaryViolations(violations), tc.want,
			"变异 %s 的违规原因：%s", tc.name, formatBoundaryViolations(violations))
	}
	green := []struct{ name, body string }{
		{"httptest", "func TestOKHTTPTest(t *testing.T) {\n\tsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))\n\tdefer srv.Close()\n}\n"},
		{"loopback_zero", "func TestOKLoopbackZero(t *testing.T) {\n\t_ = \"127.0.0.1:0\"\n}\n"},
		{"net_dial", "func TestOKDial(t *testing.T) {\n\t_, _ = net.DialTimeout(\"tcp\", \"127.0.0.1:0\", 0)\n}\n"},
	}
	for _, tc := range green {
		violations := scanBoundaryViolations(mk(tc.body))
		require.Empty(t, violations, "对照 %s 不得误报：%s", tc.name, formatBoundaryViolations(violations))
	}
	// 显式白名单文件（harness 实现）豁免监听/拼接字面量。
	exempt := map[string][]byte{"harness_test.go": []byte(header +
		"func TestHarnessExempt(t *testing.T) {\n\t_, _ = net.Listen(\"tcp\", \"127.0.0.1:\"+\"18094\")\n}\n")}
	require.Empty(t, scanBoundaryViolations(exempt), "显式白名单文件豁免")
}

// TestHarnessBoundaryGate 资源/进程边界 gate 主入口。
func TestHarnessBoundaryGate(t *testing.T) {
	repoRoot := gateRepoRoot(t)
	dir := filepath.Join(repoRoot, "tools", "e2e")
	files := readLocalTestFiles(t, dir)
	require.NotEmpty(t, files, "tools/e2e 下必须有 *_test.go（扫描集为空 = gate 失效）")
	violations := scanBoundaryViolations(files)
	require.Empty(t, violations, "资源/进程边界 gate 违规（场景不得拥有外部资源/进程生命周期）：%s",
		formatBoundaryViolations(violations))
}
