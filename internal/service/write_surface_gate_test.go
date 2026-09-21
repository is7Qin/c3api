// SPDX-License-Identifier: AGPL-3.0-or-later
package service_test

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

// allowedServiceMethods 是 Service 全部导出方法的显式允许集。
// 本门按语法枚举 receiver 为 Service 的导出方法，要求枚举结果与此表完全
// 一致：新增、删除或改名任何导出方法都会使断言失败，修改者必须有意改表。
// 新增的账号配置或身份写入方法若不登记在此表，门必失败，这正是收口要锁住
// 的形状。
//
// 分组（现状与理由）：
//   - 收口（管理面账号配置与身份写入的唯一入口）：CreateAccount 创建；
//     PatchAccount 单账号三态补丁（无条件推进配置代际，身份字段按值推进身份
//     代际）；UpdateAccountsBatch 批量；UpsertAccountExt 凭据扩展围栏写；
//     导入两动词（ImportCodexOAuthAccounts、ImportCodexPATAccounts）经凭据
//     列部分更新与单行事务落库。
//   - 豁免动词（运行时语义，不推进配置或身份代际之外的收口义务）：
//     RecoverAccount 是失效恢复的围栏恢复动词（清失效三字段）。
//   - 软删除 DeleteAccount 与 DeleteAccountsBatch 只写删除标记，不推进代际，
//     不是配置或身份写入。
//   - 其余各项是读、其它实体的写入或纯编排 helper（键、组、用户、模板、规则、
//     定价、兑换、邮件、统计、用量、路由观测），不触账号配置或身份代际。
var allowedServiceMethods = []string{
	"AccountUsageCredential",
	"AccountsGatewayUsage",
	"BalanceWarningEnabled",
	"ChangePassword",
	"CreateAccount",
	"CreateGroup",
	"CreateKey",
	"CreateRule",
	"CreateTemplate",
	"CreateUser",
	"DeactivateCode",
	"DeactivateCodesBatch",
	"DeleteAccount",
	"DeleteAccountsBatch",
	"DeleteGroup",
	"DeleteGroupsBatch",
	"DeleteKey",
	"DeletePriceEntry",
	"DeleteRule",
	"DeleteRulesBatch",
	"DeleteTemplate",
	"DeleteTemplatesBatch",
	"GenerateCodes",
	"GetAccount",
	"GetAccountExt",
	"GetAccountGroups",
	"GetBalanceWarningThreshold",
	"GetCode",
	"GetCodeUses",
	"GetGroup",
	"GetGroupAssignments",
	"GetKey",
	"GetPriceEntry",
	"GetSettings",
	"GetTemplate",
	"GetTemplateExt",
	"GetUser",
	"GetUserGroups",
	"GetUserMe",
	"ImportCodexOAuthAccounts",
	"ImportCodexPATAccounts",
	"ListAccountViews",
	"ListAccounts",
	"ListAdminKeys",
	"ListCodes",
	"ListGroups",
	"ListGroupsForUser",
	"ListKeys",
	"ListMailTemplates",
	"ListMyRedemptions",
	"ListPriceEntries",
	"ListPriceVariants",
	"ListRules",
	"ListTempBalances",
	"ListTemplates",
	"ListUserTempBalances",
	"ListUsers",
	"LoginUser",
	"MailConfig",
	"Overview",
	"PatchAccount",
	"PriceModels",
	"PriceSourceURL",
	"PriceSyncCron",
	"QueryEntityTrend",
	"QueryErrLogs",
	"QueryRoutingFlow",
	"QueryRoutingFrontier",
	"QueryStatsTTFT",
	"QueryStatsTop",
	"QueryStatsTrend",
	"QueryUsages",
	"RecoverAccount",
	"Redeem",
	"RegisterUser",
	"RegisterUserWithCode",
	"ReloadPricingAndNotifyCompiler",
	"ReloadPricingCtx",
	"ReloadSettings",
	"RenderTemplate",
	"ReplacePriceVariants",
	"ResetPassword",
	"ResolvePrices",
	"ResolvedPricesByModel",
	"RotateKey",
	"RoutingPlanExplanation",
	"SendForgotPasswordCode",
	"SendMailChannelTest",
	"SendRegisterCode",
	"ServiceTierPolicy",
	"SetGroupAssignments",
	"SetUserGroups",
	"UpdateAccountsBatch",
	"UpdateBalanceWarningThreshold",
	"UpdateGroup",
	"UpdateGroupsBatch",
	"UpdateKey",
	"UpdateMailTemplate",
	"UpdateRule",
	"UpdateSetting",
	"UpdateTemplate",
	"UpdateTemplatesBatch",
	"UpdateUser",
	"UpsertAccountExt",
	"UpsertPriceEntry",
	"UpsertTemplateExt",
	"UserEmails",
	"UserStats",
	"UserStatsTTFT",
}

// allowedServiceAccountWrites 是 Service 上账号域写方法的显式允许集。
// 收口：CreateAccount、PatchAccount、UpdateAccountsBatch、UpsertAccountExt、
// 导入两动词。豁免：RecoverAccount（运行时失效恢复动词）。软删除两方法只写
// 删除标记。除此之外不得有其它推进配置或身份代际的写方法：配置中间层的三个
// 单字段 CAS 写点（启停、倍率、缓存域）若还存在，必落在此表之外，门必失败。
var allowedServiceAccountWrites = []string{
	"CreateAccount",
	"DeleteAccount",
	"DeleteAccountsBatch",
	"ImportCodexOAuthAccounts",
	"ImportCodexPATAccounts",
	"PatchAccount",
	"RecoverAccount",
	"UpdateAccountsBatch",
	"UpsertAccountExt",
}

// servicePackageDir 返回被测包 internal/service 的源码目录。
func servicePackageDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller must locate this test file")
	return filepath.Dir(file)
}

// parseServiceGoFiles 解析目录下全部非测试 Go 源文件（测试替身不计入写面）。
func parseServiceGoFiles(t *testing.T, dir string) []*ast.File {
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

// serviceReceiverBaseName 取方法 receiver 的基类型名（*T 与 T 都归一为 T）。
func serviceReceiverBaseName(fn *ast.FuncDecl) string {
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

// exportedServiceMethods 枚举指定 receiver 全部导出方法名（排序后返回）。
// 用语法枚举而不用反射：反射只能看到编译后的方法集，表达不了"方法名是不是写
// 动词"这类语义，也无法断言"不存在"。
func exportedServiceMethods(files []*ast.File, receiver string) []string {
	var out []string
	for _, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || serviceReceiverBaseName(fn) != receiver {
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

// isServiceAccountDomain 报告 Service 方法名是否属于账号域：方法名含
// Account、OAuth 或 PAT。账号域之外的写入（键、组、用户、模板等各有自己的
// 归属）不含这三者，不在此门内。模板侧的 UpsertTemplateExt 也不含这三者，
// 自然排除。
func isServiceAccountDomain(name string) bool {
	return strings.Contains(name, "Account") ||
		strings.Contains(name, "OAuth") ||
		strings.Contains(name, "PAT")
}

// isServiceAccountRead 报告账号域方法名是否为读或纯数据组装（Get、List、
// Find、Load 前缀，或 Usage 组装）。写面 = 账号域全集 − 此读集，故任何写
// 动词前缀（Create、Patch、Update、Delete、Recover、Upsert、Import、Set、
// Write、Rotate……）的新方法都自动落入写面，无需逐个登记动词。
func isServiceAccountRead(name string) bool {
	return strings.HasPrefix(name, "Get") ||
		strings.HasPrefix(name, "List") ||
		strings.HasPrefix(name, "Find") ||
		strings.HasPrefix(name, "Load") ||
		strings.Contains(name, "Usage")
}

// TestServiceMethodSurface 钉住 Service 的方法面：枚举结果必须与显式允许集
// 完全一致，新增写方法必须有意改表。
func TestServiceMethodSurface(t *testing.T) {
	files := parseServiceGoFiles(t, servicePackageDir(t))
	require.Equal(t, allowedServiceMethods, exportedServiceMethods(files, "Service"),
		"Service method surface changed: register the new method deliberately or remove it")
}

// TestServiceAccountWriteSurface 钉住 Service 层的账号写面：账号域写方法子集
// 必须与显式表一致。除收口与豁免动词外不得有其它会推进配置或身份代际的写
// 方法：没有活调用者的配置中间层写点若还存在，必落在此表之外，门必失败。
func TestServiceAccountWriteSurface(t *testing.T) {
	files := parseServiceGoFiles(t, servicePackageDir(t))
	var writes []string
	for _, name := range exportedServiceMethods(files, "Service") {
		if isServiceAccountDomain(name) && !isServiceAccountRead(name) {
			writes = append(writes, name)
		}
	}
	sort.Strings(writes)
	require.Equal(t, allowedServiceAccountWrites, writes,
		"Service account writes changed: only the funnel and the exempt runtime verbs may advance account generations")
}
