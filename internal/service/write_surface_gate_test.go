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
//   - 代理条目写面（spec-proxy-pool M1b）：CreateProxy / UpdateProxy / DeleteProxy /
//     ProbeProxy / ListProxies / GetProxy。它们写的是 proxy 条目本身，不触账号配置
//     或身份代际——代理 CRUD 的失效走**全量 reload**（KindTemplates →
//     Sched.InvalidateAll，与模板变更同一条 seam），不推进 C/K；账号侧的绑定变更
//     仍只能经收口三动词（CreateAccount/PatchAccount/UpdateAccountsBatch）落到
//     account.proxy_id。
//   - 代理批量导入（spec-proxy-import）：ImportProxies 逐行走**同一个** createProxyOnce
//     （归一 + 私网判据 + 落库），唯一区别是失效收在循环之后（N 行一次重载）。它没有
//     自己的写语义，因此也不推进任何代际。
//   - 支付编排（spec 支付网关）：CreateOrder/CompleteOrder/SyncOrder/CloseOrder/
//     ListOrders/GetOrder/PaymentMethods 只写订单与兑换码，且入账一律经兑换 applier
//     （applyInTx）——不推进账号配置或身份代际，故不进 allowedServiceAccountWrites。
//     接口层（B2）另有三个薄入口：OrderPayURL（详情重签，纯本地计算不落库）、
//     RecordNotifyFailure（§4-4 的事务外失败审计，只改订单 fail_* 列）、
//     ResolveOrder（写 resolved_at 的人工处置标记）——同样只碰订单行。
//   - 其余各项是读、其它实体的写入或纯编排 helper（键、组、用户、模板、规则、
//     定价、模型目录读（RouteModels/GroupMultiplierOverrides/PriceDisplayForModels）、
//     兑换、邮件、统计、用量、路由观测），不触账号配置或身份代际。
var allowedServiceMethods = []string{
	"AccountUsageCredential",
	"AccountUsageFrozen",
	"AccountsGatewayUsage",
	"AdminApproveSettlement",
	"AdminClaimSettlement",
	"AdminConfirmFailedSettlement",
	"AdminListSupplierBalances",
	"AdminListSupplierSettlements",
	"AdminPaidSettlement",
	"AdminPatchSupplierBalance",
	"AdminRejectSettlement",
	"AdminRequestSettlement",
	"ApplySupplierSettlement",
	"BalanceWarningEnabled",
	"ChangePassword",
	"CloseOrder",
	// 设备码登录（发起/完成）：完成时**复用**导入核心（单行 upsert），不新增落库
	// 路径/不新增代际推进点——与导入动词同属账号写面，故在此表登记。
	"CompleteCodexOAuthDeviceLogin",
	"CompleteOrder",
	"CreateAccount",
	"CreateGroup",
	"CreateKey",
	"CreateManagementKey",
	"CreateOrder",
	"CreatePaymentAccount",
	"CreateProxy",
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
	"DeleteManagementKey",
	"DeletePaymentChannel",
	"DeletePriceEntry",
	"DeleteProxy",
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
	"GetOrder",
	"GetPaymentAccount",
	"GetPaymentAccountConfig",
	"GetPriceEntry",
	"GetProxy",
	"GetSettings",
	"GetTemplate",
	"GetTemplateExt",
	"GetUser",
	"GetUserGroups",
	"GetUserMe",
	"GroupMultiplierOverrides",
	"ImportCodexOAuthAccounts",
	"ImportCodexPATAccounts",
	"ImportProxies",
	"ListAccountViews",
	"ListAccounts",
	"ListAdminKeys",
	"ListCodes",
	"ListGroups",
	"ListGroupsForUser",
	"ListKeys",
	"ListMailTemplates",
	"ListManagementKeys",
	"ListMyRedemptions",
	"ListOrders",
	"ListPaymentAccounts",
	"ListPriceEntries",
	"ListPriceVariants",
	"ListProxies",
	"ListRules",
	"ListTempBalances",
	"ListTemplates",
	"ListUserBalanceLogs",
	"ListUserTempBalances",
	"ListUsers",
	"LoginUser",
	"MailConfig",
	"OrderPayURL",
	"Overview",
	"PatchAccount",
	"PaymentMethods",
	"PaymentProvider",
	"PortalBaseURL",
	"PriceDisplayForModels",
	"PriceModels",
	"PriceSourceURL",
	"PriceSyncCron",
	"ProbeAccount",
	"ProbePaymentAccount",
	"ProbeProxy",
	"QueryEntityTrend",
	"QueryErrLogs",
	"QueryRoutingFlow",
	"QueryRoutingFrontier",
	"QueryRoutingPlan",
	"QueryStatsTTFT",
	"QueryStatsTop",
	"QueryStatsTrend",
	"QueryUsages",
	"RecordNotifyFailure",
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
	"ResolveChannel",
	"ResolveOrder",
	"ResolvePrices",
	"ResolvedPricesByModel",
	"RotateKey",
	"RouteModels",
	"RoutingPlanExplanation",
	"SendForgotPasswordCode",
	"SendMailChannelTest",
	"SendRegisterCode",
	"ServiceTierPolicy",
	"SetGroupAssignments",
	"SetUserGroups",
	"SoftDeletePaymentAccount",
	"StartCodexOAuthDeviceLogin",
	"StatsCapabilities",
	"SupplierChunks",
	"SupplierEarnings",
	"SupplierOverview",
	"SupplierSettlements",
	"SyncOrder",
	"UpdateAccountsBatch",
	"UpdateBalanceWarningThreshold",
	"UpdateGroup",
	"UpdateGroupsBatch",
	"UpdateKey",
	"UpdateMailTemplate",
	"UpdateManagementKey",
	"UpdatePaymentAccount",
	"UpdateProxy",
	"UpdateRule",
	"UpdateSetting",
	"UpdateTemplate",
	"UpdateTemplatesBatch",
	"UpdateUser",
	"UpsertAccountExt",
	"UpsertPaymentChannel",
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
	// 设备码登录（完成时落库）：凭据来源不同、落库点相同（importCodexAccounts）。
	// 登记在此 = 显式承认它是账号写面入口，而不是绕开收口的新写路径。
	"CompleteCodexOAuthDeviceLogin",
	"CreateAccount",
	"CreatePaymentAccount",
	"DeleteAccount",
	"DeleteAccountsBatch",
	"ImportCodexOAuthAccounts",
	"ImportCodexPATAccounts",
	"PatchAccount",
	"RecoverAccount",
	"SoftDeletePaymentAccount",
	"StartCodexOAuthDeviceLogin",
	"UpdateAccountsBatch",
	"UpdatePaymentAccount",
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
// Find、Load 前缀，Usage 组装，或 Probe——探测是只读诊断观测：不落库、不计费、
// 不改账号状态，故不属于写面）。
func isServiceAccountRead(name string) bool {
	return strings.HasPrefix(name, "Get") ||
		strings.HasPrefix(name, "List") ||
		strings.HasPrefix(name, "Find") ||
		strings.HasPrefix(name, "Load") ||
		strings.HasPrefix(name, "Probe") ||
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
