// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// G1 + V0：数据面 → 契约（internal/api）的消费面门禁（spec 2026-11-23 P3b §2.5）。
//
// 单钉「scheduler.Selection 引用 == 0」是最危险的绿：扫描面一旦静默缩小（改目录、
// 改后缀、把文件挪进子包），零引用不证明任何事。所以本门禁同时钉三个锚点：
//
//	① 扫描文件数（精确相等，只许同提交调整）；
//	② 契约形参位点（api.Selection 出现次数）——V0 的下限 >= 84 防「契约定义了没人用」；
//	③ Release 调用点（V0 下限 >= 33）——防句柄定义了没人调。
//
// 判据与自测同住一个文件：只有一个 checkDatapathContract，自测矩阵与实仓扫描都调它，
// 不另写一套「看起来等价」的规则。

const (
	// 实仓基线（P3b 提交 2，da139cc9 + 契约改写）。数字必须与扫描面同源：
	// 迁移后改动调用面 ⇒ 同提交更新这里，diff 里看得见。
	//
	// 为什么是精确相等而不是下限：spec 写「文件数 >=55、契约位点 ==84」，实测
	// 非测试源 **54** 个文件、**86** 个契约位点（brief 的 55/84 各差一个）。
	// 精确相等是更紧的钉子：扫描面缩小、文件增删、符号改名都会红。
	// P3（request_patch HTTP 数据面）新增非测试源 request_patch.go（helper +
	// 限频 WARN + 脱敏）；同时新增 6 个 api.Selection 位点（patchOf / formatPatchFor /
	// headerDecorator / bodyPatch / upstreamKeyOf / warnBodyPatchFailure）。
	// P4（request_patch WS 数据面）未新增源文件，request_patch.go 追加 2 个
	// api.Selection 位点（wsHeaderDecorator / wsClientFrameHook）⇒ 92 → 94。
	// P5（probe 数据面）把 request_patch 的头/体执行核心抽到 internal/domain
	// （RequestPatchHeaderDecorator / ApplyRequestPatch 等，proxy/probe 共用），
	// request_patch.go 的 patchOf / formatPatchFor 两个纯投影 helper 随之删除、
	// 换成 1 个 upstreamOf（Selection → *domain.TemplateUpstream）⇒ 94 → 93。
	// 文件数不变（probe 的新 helper 在 internal/probe，不属本包扫描面）。
	datapathScanFiles     = 55
	datapathContractSites = 93
	datapathReleaseSites  = 38
	datapathHandleCalls   = 50
	datapathContractFloor = 84 // V0 下限
	datapathReleaseFloor  = 33 // V0 下限
)

// datapathScan 是一次消费面扫描的结果。零值不是合法结果：调用方必须同时断言锚点。
type datapathScan struct {
	Files          int
	SchedulerSel   int // `scheduler.Selection` 选择器（必须 0）
	ContractSites  int // `api.Selection` 选择器（形参/返回/复合字面量）
	PlanSites      int // `api.AttemptPlan` 引用（计划窄消费面）
	HandleCalls    int // `...HandleOrDetached().<M>()` 全部句柄调用
	ReleaseSites   int // 其中 Release 的调用点（V0 计数对象）
	RawHandleCalls int // `X.Handle.<M>()`：绕过取值口的直接字段调用（必须 0）
	Violations     []string
}

// checkDatapathContract 是唯一判据：单遍 AST，按选择器名计数。
func checkDatapathContract(fset *token.FileSet, paths []string, files []*ast.File) datapathScan {
	out := datapathScan{Files: len(files)}
	for i, file := range files {
		path := paths[i]
		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			// `x.Handle.<M>()`：绕过取值口的直接字段调用。
			if inner, ok := sel.X.(*ast.SelectorExpr); ok && inner.Sel.Name == "Handle" && isHandleMethod(sel.Sel.Name) {
				out.RawHandleCalls++
				out.Violations = append(out.Violations,
					fset.Position(sel.Pos()).String()+"：`."+inner.Sel.Name+"."+sel.Sel.Name+
						"()` 绕过 HandleOrDetached：nil 句柄直接 panic，且契约消费点不再集中")
			}
			// `HandleOrDetached().<M>()`：X 是 CallExpr，Fun 是 `.HandleOrDetached`。
			if call, ok := sel.X.(*ast.CallExpr); ok && isHandleMethod(sel.Sel.Name) {
				if fn, ok := call.Fun.(*ast.SelectorExpr); ok && fn.Sel.Name == "HandleOrDetached" {
					out.HandleCalls++
					if sel.Sel.Name == "Release" {
						out.ReleaseSites++
					}
				}
			}
			pkg, isPkg := sel.X.(*ast.Ident)
			if isPkg {
				if pkg.Name == "scheduler" && sel.Sel.Name == "Selection" {
					out.SchedulerSel++
					out.Violations = append(out.Violations,
						fset.Position(sel.Pos()).String()+"：数据面引用 `scheduler.Selection`（G1 要求 0）")
				}
				if pkg.Name == "api" && sel.Sel.Name == "Selection" {
					out.ContractSites++
				}
				if pkg.Name == "api" && sel.Sel.Name == "AttemptPlan" {
					out.PlanSites++
				}
			}
			_ = path
			return true
		})
	}
	sort.Strings(out.Violations)
	return out
}

func isHandleMethod(name string) bool {
	switch name {
	case "Release", "Thread", "StepIdentity", "Credential":
		return true
	}
	return false
}

// TestDatapathContractRepo 是实仓判据：0 引用 + 四个锚点。
func TestDatapathContractRepo(t *testing.T) {
	paths := proxySourceFiles(t)
	require.NotEmpty(t, paths, "internal/proxy 非测试源为 0：门禁在空转")

	fset := token.NewFileSet()
	files := make([]*ast.File, 0, len(paths))
	for _, path := range paths {
		af, err := parser.ParseFile(fset, path, nil, 0)
		require.NoError(t, err, "解析 %s", path)
		files = append(files, af)
	}
	scan := checkDatapathContract(fset, paths, files)
	require.Empty(t, scan.Violations, "契约消费面违例（共 %d 条）：\n%s",
		len(scan.Violations), strings.Join(scan.Violations, "\n"))

	// ① 扫描面锚点：文件数静默缩小时，「0 引用」是最危险的绿。
	require.Equal(t, datapathScanFiles, scan.Files,
		"扫描文件数漂移：确认 internal/proxy 没有把源文件挪进子包/改后缀（P3b 基线 %d）", datapathScanFiles)
	// ② 契约位点（V0 孤儿断言的反面）：契约必须被真实消费，不是定义完没人用。
	require.GreaterOrEqual(t, scan.ContractSites, datapathContractFloor,
		"V0：api.Selection 形参位点 < %d ⇒ 契约定义完没人用（数据面还没改写）", datapathContractFloor)
	require.Equal(t, datapathContractSites, scan.ContractSites,
		"契约位点数漂移：修改函数签名必须同提交更新锚点（P3b 基线 %d）", datapathContractSites)
	// ③ 句柄消费点（V0）：Release 必须被数据面真实调用。
	require.GreaterOrEqual(t, scan.ReleaseSites, datapathReleaseFloor,
		"V0：Release 调用点 < %d ⇒ 句柄定义了没人调", datapathReleaseFloor)
	require.Equal(t, datapathReleaseSites, scan.ReleaseSites,
		"Release 调用点数漂移：修改收尾路径必须同提交更新锚点（P3b 基线 %d）", datapathReleaseSites)
	// ④ 取值口唯一：所有句柄调用都经 HandleOrDetached（nil 安全 + 契约消费点集中）。
	require.Zero(t, scan.RawHandleCalls, "不得直接调 `X.Handle.<M>()`：绕过 HandleOrDetached 会在 nil 句柄上 panic")
	require.Zero(t, scan.SchedulerSel, "G1：数据面非测试代码零 `scheduler.Selection` 引用")

	require.GreaterOrEqual(t, scan.PlanSites, 1,
		"api.AttemptPlan 必须被数据面真实消费（否则窄接口只是装饰）")
	require.Equal(t, datapathHandleCalls, scan.HandleCalls,
		"句柄调用总数漂移：Release/Thread/StepIdentity/Credential 任一增删都要同提交更新锚点")
	t.Logf("扫描面：文件 %d，契约位点 %d，计划位点 %d，句柄调用 %d（Release %d），scheduler.Selection %d",
		scan.Files, scan.ContractSites, scan.PlanSites, scan.HandleCalls, scan.ReleaseSites, scan.SchedulerSel)
}

// TestDatapathContractSelfTests 自测矩阵：每条都喂唯一判据，证明门禁不是装饰。
func TestDatapathContractSelfTests(t *testing.T) {
	cases := []struct {
		name         string
		src          string
		wantViolate  int
		wantSched    int
		wantContract int
		wantRelease  int
	}{
		{
			name: "契约形参通过",
			src: `package p
import "github.com/is7qin/c3api/internal/api"
func f(sel *api.Selection) { sel.HandleOrDetached().Release() }`,
			wantContract: 1,
			wantRelease:  1,
		},
		{
			name: "scheduler.Selection 红（G1）",
			src: `package p
import "github.com/is7qin/c3api/internal/scheduler"
func f(sel *scheduler.Selection) { _ = sel }`,
			wantViolate: 1,
			wantSched:   1,
		},
		{
			name: "直接字段调用红（绕过取值口）",
			src: `package p
import "github.com/is7qin/c3api/internal/api"
func f(sel *api.Selection) { sel.Handle.Release() }`,
			wantViolate:  1,
			wantContract: 1,
		},
		{
			name: "三步链不误报",
			src: `package p
import "github.com/is7qin/c3api/internal/api"
func f(sel *api.Selection) {
	sel.HandleOrDetached().Release()
	sel.HandleOrDetached().Thread()
	sel.HandleOrDetached().StepIdentity()
	sel.HandleOrDetached().Credential()
}`,
			wantContract: 1,
			wantRelease:  1,
		},
		{
			name: "纯投影不是句柄调用",
			src: `package p
import "github.com/is7qin/c3api/internal/api"
func f(sel *api.Selection) string { return sel.LogMappedModel("m") }`,
			wantContract: 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fset := token.NewFileSet()
			af, err := parser.ParseFile(fset, "synthetic.go", tc.src, 0)
			require.NoError(t, err)
			scan := checkDatapathContract(fset, []string{"synthetic.go"}, []*ast.File{af})
			require.Len(t, scan.Violations, tc.wantViolate, "违规集合不符：%v", scan.Violations)
			require.Equal(t, tc.wantSched, scan.SchedulerSel)
			require.Equal(t, tc.wantContract, scan.ContractSites)
			require.Equal(t, tc.wantRelease, scan.ReleaseSites)
		})
	}
}
