// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// 跨租户拒绝路径的**站点**门禁与行为表（spec 2026-11-21-wave2 §1）。
//
// # 为什么覆盖对象是「站点」而不是「方法名」
//
// owner 谓词在本包里有四种写法，只有第一种含 helper 名：
//
//	entHelper    `accountOwnerPred(...)` / `proxyOwnerPred(...)`
//	entDirect    `account.SupplierUserID(scope.OwnerUID)`（不经 helper，直接进 ent 条件）
//	rawSQL       原生 SQL 里的 `supplier_user_id = $n`（JOIN / EXISTS / FOR UPDATE 三种载体）
//	helperFanout 方法自身含谓词、语义是给**调用方**加 owner（`accountExtQuery`、
//	             `checkAccountExist`、`accountOwnerUpdate`）；调用方体内没有任何谓词文本
//
// 按方法名扫描只能回答「谁调了那两个 helper」，不能回答「每一处实际 SQL owner 谓词是否
// 都有拒绝路径覆盖」——直写谓词、原生 SQL、helper 扇出都会被漏掉。故登记单位是
// (file, method, 谓词形态) 三元组：一个方法可以有多条站点（`ListAccounts` 的账号面与
// 代理展开面、`DeleteProxy` 的代理面与解绑面）。
//
// # 两条门禁都是**形状**门禁，行为表管结论
//
//	门禁① （正向 witness）每个站点声明的判别片段必须仍在该 FuncDecl 体内。
//	门禁② （反向完备）扫描面上「消费 ctx 作用域 + 至少一处 owner 谓词」的方法集合
//	                必须 ⊆ 站点清单；未登记 ⇒ 红，点名方法并提示补三态行为行。
//
// 门禁管「谓词还在不在、方法有没有登记」，不假装管语义；语义由行为表管：越域真的被拒、
// 不泄漏存在性（错误 / 空集 / 无键 / 不落地，按方法**现有契约**单值钉死）。
//
// # 判据边界（每条都排除了一类真实存在的方法，不是"漏覆盖"）
//
//   - **排除 helper 定义**：`accountOwnerPred` / `proxyOwnerPred` 只接收显式 scope 入参、
//     不消费 ctx；它们是谓词的**拼装点**，正确性由各站点的 witness 与行为行间接守住。
//     把定义当站点会让「一处谓词写错」表现为「定义行变红」，定位反而失真。
//   - **排除不消费 ctx 作用域的 owner 谓词所在的整个方法**：`user_repo.UpdateUser` 的
//     `account.SupplierUserID(p.ID)`（禁用用户连带停名下账号）以**显式 uid** 为判据，没有
//     「注入作用域 ⇒ 越域被拒」这条路径，由 `TestPGAccountLifecycleLockOrder` 覆盖。
//     注意这条边界只作用于**方法级判据**：`DeleteProxy` 解绑面的 `*row.OwnerUID` 虽然也是
//     显式 uid，但它决定了跨租户**副作用**的范围，仍作为第二个站点单独登记（门禁②按方法
//     集合比较，方法内新增谓词在方法级看不见，只有 witness 看得见）。
//   - **排除只写归属、不设 fence 的创建面**：`CreateProxy` 从作用域**落** owner_uid，没有
//     foreign 语义（forge 别人的 owner 在类型上不可表达），由 `TestPGProxyScopeIsolation`
//     钉住。
//   - **rawSQL 判据限定在 accounts 表**：供应商账务表（`supplier_balances` /
//     `supplier_settlements` / `supplier_frozen_chunks`）里的 `supplier_user_id = $1`
//     是按**入参 uid** 查自己的账，不设租户 fence；判别片段因此要求同一条 SQL 里出现
//     `accounts` 表——`usage_logs` 上的 owner 过滤正是这么写的（EXISTS 子句引用 accounts）。
//
// # 跑法
//
//	（形状门禁不需要 PG；行为表按 pgtest 约定在无 DSN 时 skip）
//	go test ./internal/repository/ -run 'TestScopeFence' -count=1 -v

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// --- 站点模型 ---

// siteKind 是 owner 谓词的四种载体（见文件头）。
type siteKind string

const (
	kindEntHelper    siteKind = "entHelper"
	kindEntDirect    siteKind = "entDirect"
	kindRawSQL       siteKind = "rawSQL"
	kindHelperFanout siteKind = "helperFanout"
)

// wantKind 是拒绝路径的**唯一**结论形态，按方法现有契约取值——不允许"空集或 ErrNotFound"
// 这类二选一：那会让"单行读取被错改成空对象"和"列表被错改成错误"两种实现都通过。
type wantKind string

const (
	// wantVisible 管理面/本人作用域：调用成功，且目标事实被观测到（读到行、装配了代理、
	// 探测到"无缺失"……由各行注释写明 present 的确切含义）。
	wantVisible wantKind = "visible"
	// wantNotFound 单行读取 / 单条写入 / 加锁读：越域 ⇒ ErrNotFound（0 行，与不存在不可区分）。
	wantNotFound wantKind = "notFound"
	// wantEmptyList 列表：越域 ⇒ 空结果且总数为 0，**非错误**。
	wantEmptyList wantKind = "emptyList"
	// wantExcluded 集合读取：越域目标被过滤出结果，**非错误**（同页其它租户的行照常返回）。
	wantExcluded wantKind = "excluded"
	// wantZeroCount 聚合：越域 ⇒ 输出不含该键（不是 0 值条目）。
	wantZeroCount wantKind = "zeroCount"
	// wantMissingID 存在性探测：越域 ⇒ 点名报出第一个越域 id（既非错误也非空集）。
	wantMissingID wantKind = "missingID"
	// wantUnassembled 关联展开：账号行可见，但越域代理**不装配**（Proxy 保持 nil）。
	wantUnassembled wantKind = "unassembled"
	// wantUnchanged 写路径：越域 ⇒ 返回错误**且**回读逐字段不变（不写他人行）。
	wantUnchanged wantKind = "unchanged"
)

// ownerSite 是一个谓词站点：源码位置 × 判别形态 × 结论契约。
type ownerSite struct {
	id      string   // "account_repo.GetAccount/ent"：文件.方法/形态
	file    string   // internal/repository 下的文件名
	method  string   // 承载谓词的 FuncDecl（helper 站点即 helper 自身）
	kind    siteKind // 四种载体之一
	witness string   // 方法体内必须出现的判别片段（门禁①）
	entry   string   // 行为行的打点入口：helper 站点填**调用方**（门禁②只看 method，行为看 entry）
	want    wantKind // 该站点 foreign 行的唯一期望（行为表逐行核对相等）
}

// ownerSites 是站点清单：数据的唯一来源，同时喂门禁①②与行为表。
//
// 覆盖范围 = 扫描面上「消费 ctx 作用域 + 至少一处 owner 谓词」的全部方法（门禁②反向
// 断言其 ⊆ 本清单）；helper 站点额外声明它的调用方入口，因为越域拒绝发生在调用边界。
var ownerSites = []ownerSite{
	// --- account_repo.go：ent helper 形态 ---
	{
		id: "account_repo.GetAccount/ent", file: "account_repo.go", method: "GetAccount",
		kind: kindEntHelper, witness: "accountOwnerPred(", entry: "GetAccount", want: wantNotFound,
	},
	{
		id: "account_repo.GetAccountWithTemplate/ent", file: "account_repo.go", method: "GetAccountWithTemplate",
		kind: kindEntHelper, witness: "accountOwnerPred(", entry: "GetAccountWithTemplate", want: wantNotFound,
	},
	{
		id: "account_repo.ListAccounts/ent", file: "account_repo.go", method: "ListAccounts",
		kind: kindEntHelper, witness: "accountOwnerPred(", entry: "ListAccounts", want: wantEmptyList,
	},
	{
		// 多站点方法的第二面：账号行可见，但绑定到他人代理时不装配（I1 的方向二）。
		id: "account_repo.expandProxies/proxy", file: "account_repo.go", method: "expandProxies",
		kind: kindHelperFanout, witness: "proxyOwnerPred(", entry: "ListAccounts", want: wantUnassembled,
	},
	{
		id: "account_repo.DeleteAccount/ent", file: "account_repo.go", method: "DeleteAccount",
		kind: kindEntHelper, witness: "accountOwnerPred(", entry: "DeleteAccount", want: wantUnchanged,
	},
	{
		id: "account_repo.GetAccountGroups/ent", file: "account_repo.go", method: "GetAccountGroups",
		kind: kindEntHelper, witness: "accountOwnerPred(", entry: "GetAccountGroups", want: wantEmptyList,
	},
	{
		// entDirect：作用域分支直接写 `account.SupplierUserID(sc.OwnerUID)`，没有 helper 文本。
		id: "account_repo.SetAccountGroups/ent-direct", file: "account_repo.go", method: "SetAccountGroups",
		kind: kindEntDirect, witness: "account.SupplierUserID(", entry: "SetAccountGroups", want: wantUnchanged,
	},
	{
		id: "account_repo.FindMissingOwnedAccountID/ent", file: "account_repo.go", method: "FindMissingOwnedAccountID",
		kind: kindEntHelper, witness: "accountOwnerPred(", entry: "FindMissingOwnedAccountID", want: wantMissingID,
	},
	{
		id: "account_repo.FailAccountCAS/ent", file: "account_repo.go", method: "FailAccountCAS",
		kind: kindEntHelper, witness: "accountOwnerPred(", entry: "FailAccountCAS", want: wantUnchanged,
	},
	{
		id: "account_repo.RecoverAccountCAS/ent", file: "account_repo.go", method: "RecoverAccountCAS",
		kind: kindEntHelper, witness: "accountOwnerPred(", entry: "RecoverAccountCAS", want: wantUnchanged,
	},

	// --- batch.go / codex_write.go：批量面（存在性检查 + 加锁读） ---
	{
		// helperFanout：自身无业务语义，只给调用方的存在性检查加 owner；调用方
		// `DeleteAccountsBatch` 体内没有任何谓词文本。
		id: "batch.checkAccountExist/ent", file: "batch.go", method: "checkAccountExist",
		kind: kindHelperFanout, witness: "accountOwnerPred(", entry: "DeleteAccountsBatch", want: wantUnchanged,
	},
	{
		// rawSQL：`WHERE id IN (...) AND supplier_user_id = $n ... FOR UPDATE`。
		id: "codex_write.lockAccountsForUpdate/sql", file: "codex_write.go", method: "lockAccountsForUpdate",
		kind: kindRawSQL, witness: "supplier_user_id = $", entry: "UpdateAccountsBatch", want: wantUnchanged,
	},

	// --- proxy_repo.go：proxy helper 形态 ---
	{
		id: "proxy_repo.ListProxies/proxy", file: "proxy_repo.go", method: "ListProxies",
		kind: kindEntHelper, witness: "proxyOwnerPred(", entry: "ListProxies", want: wantExcluded,
	},
	{
		id: "proxy_repo.GetProxy/proxy", file: "proxy_repo.go", method: "GetProxy",
		kind: kindEntHelper, witness: "proxyOwnerPred(", entry: "GetProxy", want: wantNotFound,
	},
	{
		id: "proxy_repo.UpdateProxy/proxy", file: "proxy_repo.go", method: "UpdateProxy",
		kind: kindEntHelper, witness: "proxyOwnerPred(", entry: "UpdateProxy", want: wantUnchanged,
	},
	{
		id: "proxy_repo.DeleteProxy/proxy", file: "proxy_repo.go", method: "DeleteProxy",
		kind: kindEntHelper, witness: "proxyOwnerPred(", entry: "DeleteProxy", want: wantUnchanged,
	},

	{
		// 同一方法的第二面：解绑连坐范围随**代理**归属（`*row.OwnerUID`），不是 ctx 作用域。
		// 门禁②按方法集合比较，方法内新增第二个谓词在方法级看不见——witness 才看得见，
		// 故这一面单独登记。
		id: "proxy_repo.DeleteProxy/unbind", file: "proxy_repo.go", method: "DeleteProxy",
		kind: kindEntDirect, witness: "account.SupplierUserID(", entry: "DeleteProxy", want: wantUnchanged,
	},
	{
		id: "proxy_repo.WriteProxyProbe/proxy", file: "proxy_repo.go", method: "WriteProxyProbe",
		kind: kindEntHelper, witness: "proxyOwnerPred(", entry: "WriteProxyProbe", want: wantUnchanged,
	},

	// --- ext_codex_repo.go：ext 查询基座 / 直接谓词 / 原生 SQL ---
	{
		// helperFanout：owner 谓词经 EXISTS 子查询挂在 ext 查询基座上，调用方
		// `FindAccountExtByCodexKey` 自己看不到任何谓词文本。
		id: "ext_codex_repo.accountExtQuery/ent", file: "ext_codex_repo.go", method: "accountExtQuery",
		kind: kindHelperFanout, witness: "accountOwnerPred(", entry: "FindAccountExtByCodexKey", want: wantNotFound,
	},
	{
		// helperFanout（entDirect 载体）：把 owner 谓词 AND 进调用方的 accounts UPDATE。
		id: "ext_codex_repo.accountOwnerUpdate/ent", file: "ext_codex_repo.go", method: "accountOwnerUpdate",
		kind: kindHelperFanout, witness: "account.SupplierUserID(", entry: "AdminWriteOAuthRotationCAS", want: wantUnchanged,
	},
	{
		id: "ext_codex_repo.lockOwnedAccountForUpdate/sql", file: "ext_codex_repo.go", method: "lockOwnedAccountForUpdate",
		kind: kindRawSQL, witness: "supplier_user_id = $", entry: "UpsertAccountExt", want: wantNotFound,
	},
	{
		// entDirect：`HasAccountWith(account.SupplierUserID(s.OwnerUID), ...)`。
		id: "ext_codex_repo.GetOwnedAccountExt/ent", file: "ext_codex_repo.go", method: "GetOwnedAccountExt",
		kind: kindEntDirect, witness: "account.SupplierUserID(", entry: "GetOwnedAccountExt", want: wantNotFound,
	},
	{
		// rawSQL：一条 JOIN 同时读凭据列与父账号 K，owner 谓词在同一语句的 WHERE 里。
		id: "ext_codex_repo.GetOwnedAccountCredential/sql", file: "ext_codex_repo.go", method: "GetOwnedAccountCredential",
		kind: kindRawSQL, witness: "supplier_user_id = $", entry: "GetOwnedAccountCredential", want: wantNotFound,
	},

	// --- usage_repo.go：聚合 SQL 的 EXISTS owner 过滤 ---
	{
		id: "usage_repo.ScanUsageAgg/sql", file: "usage_repo.go", method: "ScanUsageAgg",
		kind: kindRawSQL, witness: "supplier_user_id = $", entry: "ScanUsageAgg", want: wantZeroCount,
	},
}

// --- 行为表模型 ---

// fenceScope 是一行的**作用域来源**：admin 无作用域；owner/foreign 都是「A 的 ctx」
// （foreign 用同一枚 ctx 去打 B 的行——两个值同 ctx 而不同目标，正是"越域"的定义）；
// stranger 是一个不拥有任何行的供应商作用域，用于「空列表 + 总数 0」这种需要"有作用域
// 但作用域内无行"的契约行。
type fenceScope int

const (
	asAdmin fenceScope = iota
	asOwner
	asStranger
	asForeign
)

func (s fenceScope) String() string {
	switch s {
	case asAdmin:
		return "admin"
	case asOwner:
		return "owner"
	case asStranger:
		return "stranger"
	case asForeign:
		return "foreign"
	}
	return "unknown"
}

// fenceOutcome 是一次调用的**观测形状**：各字段按站点类型取子集，判据（requireWant）
// 只读这里——期望值因此是"钉死的单值"，改期望即改判据。
type fenceOutcome struct {
	err       error
	present   bool  // 目标行是否被观测到（读到 / 在结果里 / 装配了 / 无缺失，见各行注释）
	count     int   // 结果条数（列表 / 分组）
	total     int64 // 列表总数（分页面）
	attached  bool  // 关联代理是否装配（expandProxies 面）
	missingID int64 // 存在性探测点名的越域 id（0 = 未点名）
	targetID  int64 // 本行探测的目标 id（wantMissingID 与 missingID 对照）
	unchanged bool  // 写路径：回读是否与写前逐字段相同
}

// fenceRow 是一条行为行：{站点, 作用域, 调用, 期望}。
type fenceRow struct {
	label   string // "admin" / "owner" / "foreign" / "owner-unbind"：与站点拼成完整用例名
	site    string // ownerSite.id
	scope   fenceScope
	want    wantKind
	wantErr error // 可选：越域错误的**哨兵**必须精确（nil = 只判形状）
	run     func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome
}

// requireWant 是唯一的结论判据。
func requireWant(t *testing.T, want wantKind, got fenceOutcome) {
	t.Helper()
	switch want {
	case wantVisible:
		require.NoError(t, got.err)
		require.True(t, got.present, "管理/本人作用域必须观测到目标事实")
	case wantNotFound:
		require.ErrorIs(t, got.err, repository.ErrNotFound, "越域必须与「不存在」同码")
		require.False(t, got.present, "越域不得观测到目标行（不泄漏存在性）")
	case wantEmptyList:
		require.NoError(t, got.err, "列表/分组读取越域不是错误")
		require.Zero(t, got.count, "越域列表必须为空")
		require.Zero(t, got.total, "越域列表总数必须为 0")
	case wantExcluded:
		require.NoError(t, got.err)
		require.False(t, got.present, "越域目标必须被过滤出集合")
	case wantZeroCount:
		require.NoError(t, got.err)
		require.False(t, got.present, "聚合输出不得含越域键")
	case wantMissingID:
		require.NoError(t, got.err)
		require.Equal(t, got.targetID, got.missingID, "存在性探测必须点名报出第一个越域 id")
	case wantUnassembled:
		require.NoError(t, got.err)
		require.True(t, got.present, "账号行必须可见（越域只影响代理装配）")
		require.False(t, got.attached, "越域代理不得装配回显")
	case wantUnchanged:
		require.Error(t, got.err, "越域写必须报错")
		require.True(t, got.unchanged, "越域写不得落地（回读必须逐字段不变）")
	default:
		t.Fatalf("未定义的 wantKind：%q", want)
	}
}

// --- 门禁基础设施 ---

// fenceSource 是一个已解析的扫描面文件（真实源码或注入的假源码）。
type fenceSource struct {
	name string // 文件名，与 ownerSite.file 对齐
	fset *token.FileSet
	file *ast.File
}

// fenceRepoRoot 定位模块根：本文件在 <root>/internal/repository/ 下。
func fenceRepoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	require.True(t, ok, "runtime.Caller 必须可用")
	dir := filepath.Dir(file)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, parent, dir, "向上未找到 go.mod：模块根定位失败")
		dir = parent
	}
}

// fenceSources 解析 internal/repository 下全部**非测试** go 文件。
func fenceSources(t *testing.T) []fenceSource {
	t.Helper()
	dir := filepath.Join(fenceRepoRoot(t), "internal", "repository")
	ents, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []fenceSource
	for _, e := range ents {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, fenceParseSource(t, name, filepath.Join(dir, name)))
	}
	require.NotEmpty(t, out, "扫描面不得为空")
	return out
}

func fenceParseSource(t *testing.T, name, path string) fenceSource {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
	require.NoErrorf(t, err, "解析失败：%s", name)
	return fenceSource{name: name, fset: fset, file: file}
}

// fenceParseInjected 把一段**注入的源码**送进真正的扫描链（解析 → AST 判据）。负例因此
// 测的是「判据 × 扫描器」本身，不是另一套手写逻辑。
func fenceParseInjected(t *testing.T, name, src string) fenceSource {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	require.NoErrorf(t, err, "注入源码必须可解析：%s", name)
	return fenceSource{name: name, fset: fset, file: file}
}

// fenceFuncDecl 在扫描面上按 (文件, 方法) 取顶层函数声明。
func fenceFuncDecl(sources []fenceSource, file, method string) (fenceSource, *ast.FuncDecl) {
	for _, src := range sources {
		if src.name != file {
			continue
		}
		for _, decl := range src.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if ok && fn.Body != nil && fn.Name.Name == method {
				return src, fn
			}
		}
	}
	return fenceSource{}, nil
}

// fenceBodyText 渲染函数体源码。用 AST 渲染而不是原始字节：注释不在 AST 里，故注释中
// 出现的判别片段（例如 `usage_repo.go` 里解释 EXISTS 的说明文字）不会被当成 witness；
// 字符串字面量原样保留，rawSQL 的 `supplier_user_id = $` 因此仍可判。
func fenceBodyText(t *testing.T, src fenceSource, fn *ast.FuncDecl) string {
	t.Helper()
	var b strings.Builder
	require.NoError(t, printer.Fprint(&b, src.fset, fn.Body))
	return b.String()
}

// fenceFragments 是三种**行内**判别片段（witness 取值只能来自这里或 rawSQL 判据）。
var fenceFragments = map[siteKind][]string{
	kindEntHelper:    {"accountOwnerPred(", "proxyOwnerPred("},
	kindEntDirect:    {"account.SupplierUserID("},
	kindHelperFanout: {"accountOwnerPred(", "proxyOwnerPred(", "account.SupplierUserID("},
}

// rawSQL 判据：同一条语句里既有 owner 列比较又有 accounts 表（见文件头的边界说明）。
const (
	fenceRawSQLLiteral = "supplier_user_id = $"
	fenceRawSQLTable   = "accounts"
)

// fenceWitnessViolations 是门禁①的判据本体（空 = 合规）。
func fenceWitnessViolations(t *testing.T, sources []fenceSource, sites []ownerSite) []string {
	t.Helper()
	var out []string
	for _, s := range sites {
		src, fn := fenceFuncDecl(sources, s.file, s.method)
		if fn == nil {
			out = append(out, fmt.Sprintf("%s：%s 里找不到 %s 的函数体——站点登记与源码脱节", s.id, s.file, s.method))
			continue
		}
		if !strings.Contains(fenceBodyText(t, src, fn), s.witness) {
			out = append(out, fmt.Sprintf("%s：%s.%s 体内不再出现 %q——owner 谓词被删或被改写", s.id, s.file, s.method, s.witness))
		}
	}
	return out
}

// fenceHit 是扫描面上的一处站点：方法 + 命中的载体形态。
type fenceHit struct {
	file   string
	method string
	kinds  []siteKind
}

func (h fenceHit) String() string {
	names := make([]string, 0, len(h.kinds))
	for _, k := range h.kinds {
		names = append(names, string(k))
	}
	return fmt.Sprintf("%s.%s（%s）", h.file, h.method, strings.Join(names, "+"))
}

// fenceHelperDefs 是谓词拼装点本身：不是业务站点（文件头边界①）。
var fenceHelperDefs = map[string]bool{"accountOwnerPred": true, "proxyOwnerPred": true}

// fenceObservedSites 是门禁②的扫描器：收集「消费 ctx 作用域 + 至少一处 owner 谓词」的
// 方法。两个条件都要求，缺一不可——
//   - 只消费作用域（`CreateProxy`）不设 fence，没有 foreign 语义；
//   - 只有 owner 谓词但不消费 ctx（`UpdateUser` 的显式 uid / 解绑面读 `*row.OwnerUID`）
//     不是"注入作用域"路径，越域不是它要回答的问题。
func fenceObservedSites(t *testing.T, sources []fenceSource) []fenceHit {
	t.Helper()
	var out []fenceHit
	for _, src := range sources {
		for _, decl := range src.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fenceHelperDefs[fn.Name.Name] {
				continue
			}
			body := fenceBodyText(t, src, fn)
			if !strings.Contains(body, "domain.AccountScopeFrom(") {
				continue
			}
			var kinds []siteKind
			for _, kind := range []siteKind{kindEntHelper, kindEntDirect} {
				for _, frag := range fenceFragments[kind] {
					if strings.Contains(body, frag) {
						kinds = append(kinds, kind)
						break
					}
				}
			}
			if strings.Contains(body, fenceRawSQLLiteral) && strings.Contains(body, fenceRawSQLTable) {
				kinds = append(kinds, kindRawSQL)
			}
			if len(kinds) == 0 {
				continue
			}
			out = append(out, fenceHit{file: src.name, method: fn.Name.Name, kinds: kinds})
		}
	}
	return out
}

// fenceUndeclaredViolations 是门禁②的判据本体（空 = 合规）：观察集合 ⊆ 声明集合。
// 允许**多余声明**（声明了但扫描不到该形态，例如 helper 站点的形态只是调用方视角）——
// 那由门禁①的 witness 负责，门禁②只管"有没有漏登记"。
func fenceUndeclaredViolations(hits []fenceHit, sites []ownerSite) []string {
	declared := make(map[string]bool, len(sites))
	for _, s := range sites {
		declared[s.file+"."+s.method] = true
	}
	var out []string
	for _, h := range hits {
		if declared[h.file+"."+h.method] {
			continue
		}
		out = append(out, fmt.Sprintf("未登记的 owner 站点：%s——给这个方法补 ownerSite 行（形态 + witness）与 admin/owner/foreign 三态行为行", h.String()))
	}
	return out
}

// --- 清单自身的形状断言（表腐化在这里就该红，而不是等到门禁①②给出误导信息） ---

func fenceSiteTableViolations(sites []ownerSite) []string {
	var out []string
	ids := make(map[string]bool, len(sites))
	for _, s := range sites {
		if ids[s.id] {
			out = append(out, fmt.Sprintf("站点 id 重复：%s", s.id))
		}
		ids[s.id] = true
		if s.file == "" || s.method == "" || s.witness == "" || s.entry == "" {
			out = append(out, fmt.Sprintf("站点 %s 有空字段（file/method/witness/entry 都必填）", s.id))
		}
		if !strings.HasSuffix(s.file, ".go") {
			out = append(out, fmt.Sprintf("站点 %s 的 file 必须是 .go 文件名：%q", s.id, s.file))
		}
		switch s.kind {
		case kindEntHelper, kindEntDirect, kindHelperFanout:
			if !slices.Contains(fenceFragments[s.kind], s.witness) {
				out = append(out, fmt.Sprintf("站点 %s 的 witness %q 与形态 %s 不匹配（该形态只认 %v）",
					s.id, s.witness, s.kind, fenceFragments[s.kind]))
			}
		case kindRawSQL:
			if !strings.Contains(s.witness, fenceRawSQLLiteral) {
				out = append(out, fmt.Sprintf("站点 %s 的 rawSQL witness %q 必须含 %q", s.id, s.witness, fenceRawSQLLiteral))
			}
		default:
			out = append(out, fmt.Sprintf("站点 %s 的 kind 未定义：%q", s.id, s.kind))
		}
		switch s.want {
		case wantVisible, wantNotFound, wantEmptyList, wantExcluded, wantZeroCount,
			wantMissingID, wantUnassembled, wantUnchanged:
		default:
			out = append(out, fmt.Sprintf("站点 %s 的 want 未定义：%q", s.id, s.want))
		}
	}
	return out
}

// --- 行为表夹具 ---

// fenceFixture 是一套跨租户夹具：
//
//	供应商 A（owner）  ← 本文件的"我方"，owner/foreign 行都用 A 的 ctx
//	供应商 B（other）  ← "他方"：它的账号 / 代理 / ext 行就是越域目标
//	供应商 C（stranger）← 不拥有任何行：用于「有作用域但作用域内无行」的契约行
//	平台              ← supplier=0 的账号、owner_uid IS NULL 的代理
//
// 账号三件套构成「账号归属 × 代理归属」的 2×2 缺失角，正好是 `expandProxies` 越域
// 不装配的最小夹具：mine（A 的账号 + A 的代理）、mineOnTheirProxy（A 的账号 + B 的代理，
// 即管理员把 A 的账号错绑到 B 的私池）、theirs（B 的账号 + B 的代理）。
type fenceFixture struct {
	repos *repository.Repository
	tpl   *domain.Template

	group, otherGroup *domain.Group

	owner, other, stranger *domain.User

	mine, mineOnTheirProxy, theirs *domain.Account

	mineProxy, theirProxy, platformProxy int64

	usageFrom, usageTo time.Time

	seq int
}

func newFenceFixture(t *testing.T) *fenceFixture {
	t.Helper()
	f := &fenceFixture{repos: newPGReposShared(t)}

	f.tpl = seedPGTemplate(t, f.repos)
	f.group = seedPGGroup(t, f.repos, "fence-g1")
	f.otherGroup = seedPGGroup(t, f.repos, "fence-g2")

	f.owner = seedSupplierUser(t, f.repos, "fence-owner@example.com")
	f.other = seedSupplierUser(t, f.repos, "fence-other@example.com")
	f.stranger = seedSupplierUser(t, f.repos, "fence-stranger@example.com")

	f.mineProxy = seedPGProxyOwned(t, f.repos, f.owner.ID, "fence-proxy-mine")
	f.theirProxy = seedPGProxyOwned(t, f.repos, f.other.ID, "fence-proxy-theirs")
	f.platformProxy = seedPGProxyNamed(t, f.repos, "fence-proxy-platform")

	f.mine = f.account(t, f.owner.ID, &f.mineProxy, "mine")
	f.mineOnTheirProxy = f.account(t, f.owner.ID, &f.theirProxy, "mine-on-theirs")
	f.theirs = f.account(t, f.other.ID, &f.theirProxy, "theirs")

	seedCodexKeyExtPG(t, f.repos, f.mine.ID, "fence-mine@example.com", "fence-codex-mine", "fence-pat-mine")
	seedCodexKeyExtPG(t, f.repos, f.theirs.ID, "fence-theirs@example.com", "fence-codex-theirs", "fence-pat-theirs")

	// usage 聚合面的越域夹具：两条日志（各租户一条）落在同一时间窗内。
	f.usageFrom = time.Now().UTC().Truncate(time.Second).Add(-time.Minute)
	f.usageTo = f.usageFrom.Add(2 * time.Hour)
	require.NoError(t, f.repos.Usages.InsertBatch(context.Background(), []*domain.UsageLog{
		{RequestID: "fence-mine-1", AccountID: f.mine.ID, Model: "m", Format: domain.FormatOpenAIChat,
			ErrorType: domain.ErrNone, TotalTokens: 10, Cost: 100, RawCost: 200, CreatedAt: f.usageFrom},
		{RequestID: "fence-theirs-1", AccountID: f.theirs.ID, Model: "m", Format: domain.FormatOpenAIChat,
			ErrorType: domain.ErrNone, TotalTokens: 99, Cost: 9999, RawCost: 9999, CreatedAt: f.usageFrom},
	}))
	return f
}

// ctxOf 把作用域来源翻译成 ctx：foreign 与 owner 同 ctx（A 的），差别在**目标**是别人的行。
func (f *fenceFixture) ctxOf(s fenceScope) context.Context {
	switch s {
	case asAdmin:
		return context.Background()
	case asStranger:
		return scopeCtx(f.stranger.ID)
	case asOwner, asForeign:
		return scopeCtx(f.owner.ID)
	}
	panic("未定义的作用域来源：" + s.String())
}

// account 建一个归属指定供应商（0 = 平台自有）的账号；名字带自增后缀避免唯一冲突。
func (f *fenceFixture) account(t *testing.T, ownerUID int64, proxyID *int64, label string) *domain.Account {
	t.Helper()
	f.seq++
	name := fmt.Sprintf("fence-%s-%d", label, f.seq)
	a, err := f.repos.Accounts.CreateAccount(context.Background(), &domain.Account{
		Name: name, TemplateID: f.tpl.ID, UpstreamKey: "sk-" + name,
		MaxConcurrency: 8, Enabled: true, SupplierUserID: ownerUID, ProxyID: proxyID,
	})
	require.NoError(t, err)
	return a
}

// accountWithExt 建账号并写 pat ext 行（凭据面站点用）。
func (f *fenceFixture) accountWithExt(t *testing.T, ownerUID int64, label string) *domain.Account {
	t.Helper()
	a := f.account(t, ownerUID, nil, label)
	seedPATExtPG(t, f.repos, a.ID, "pat-"+a.Name)
	return a
}

// proxy 建代理：uid > 0 = 该供应商的私池（归属由作用域钉死），0 = 平台代理。
func (f *fenceFixture) proxy(t *testing.T, ownerUID int64, label string) int64 {
	t.Helper()
	f.seq++
	name := fmt.Sprintf("fence-%s-%d", label, f.seq)
	if ownerUID == 0 {
		return seedPGProxyNamed(t, f.repos, name)
	}
	return seedPGProxyOwned(t, f.repos, ownerUID, name)
}

// fenceAccountState 是「回读不变」的判据面：只取写路径可能触碰的列。整对象比较会把与写面
// 无关的列混进判据；逐列取又能覆盖本表所有写站点（存活 / 启用 / 失效 / 两个代际 / 绑定）。
type fenceAccountState struct {
	deleted   bool
	enabled   bool
	failed    bool
	lifecycle int64
	identity  int64
	proxyID   int64
}

func (f *fenceFixture) accountState(t *testing.T, id int64) fenceAccountState {
	t.Helper()
	a, err := f.repos.Accounts.GetAccount(context.Background(), id)
	if errors.Is(err, repository.ErrNotFound) {
		return fenceAccountState{deleted: true}
	}
	require.NoError(t, err)
	require.NotNil(t, a)
	st := fenceAccountState{
		deleted: a.DeletedAt != nil, enabled: a.Enabled, failed: a.FailedAt != nil,
		lifecycle: a.LifecycleRevision, identity: a.IdentityRevision,
	}
	if a.ProxyID != nil {
		st.proxyID = *a.ProxyID
	}
	return st
}

// fenceProxyState 同上，判据面是代理条目可能被改的列。
type fenceProxyState struct {
	deleted  bool
	name     string
	remark   string
	hasProbe bool
	probeOK  bool
	probeErr string
	probeIP  string
}

func (f *fenceFixture) proxyState(t *testing.T, id int64) fenceProxyState {
	t.Helper()
	p, err := f.repos.Proxies.GetProxy(context.Background(), id)
	if errors.Is(err, repository.ErrNotFound) {
		return fenceProxyState{deleted: true}
	}
	require.NoError(t, err)
	return fenceProxyState{
		name: p.Name, remark: p.Remark, hasProbe: p.LastProbeAt != nil,
		probeOK: p.LastProbeOK, probeErr: p.LastProbeError, probeIP: p.LastProbeIP,
	}
}

// accountGroups 读分组（排序后比较：m2m 的返回顺序不是契约）。
func (f *fenceFixture) accountGroups(t *testing.T, id int64) []int64 {
	t.Helper()
	ids, err := f.repos.Accounts.GetAccountGroups(context.Background(), id)
	require.NoError(t, err)
	slices.Sort(ids)
	return ids
}

// setGroupsAdmin 用管理面写入分组（夹具前置状态，不是被测行为）。
func (f *fenceFixture) setGroupsAdmin(t *testing.T, id int64, groupIDs ...int64) {
	t.Helper()
	require.NoError(t, f.repos.Accounts.SetAccountGroups(context.Background(), id, groupIDs))
}

// fenceAccountIDs 抽出列表里的账号 id 集合。
func fenceAccountIDs(list []*domain.Account) []int64 {
	out := make([]int64, 0, len(list))
	for _, a := range list {
		out = append(out, a.ID)
	}
	return out
}

// fenceAccountOf 在列表里找指定 id 的账号（nil = 不可见）。
func fenceAccountOf(list []*domain.Account, id int64) *domain.Account {
	for _, a := range list {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// fenceProxyIDs 抽出代理列表的 id 集合。
func fenceProxyIDs(list []domain.Proxy) []int64 {
	out := make([]int64, 0, len(list))
	for _, p := range list {
		out = append(out, p.ID)
	}
	return out
}

// --- 行为表 ---

// fenceRows 是行为表：行 = {站点, 作用域, 调用, 期望}。
//
// 每行都**自带前置状态**（需要新账号/新代理就在行内建），因此行之间没有顺序耦合：
// 一条变异只会让被它打中的站点变红，不会污染邻行。
func fenceRows() []fenceRow {
	now := time.Now().UTC()
	disabled := false
	return []fenceRow{
		// ---------- GetAccount：单行读取 ⇒ 越域 = ErrNotFound ----------
		{
			label: "admin", site: "account_repo.GetAccount/ent", scope: asAdmin, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				got, err := f.repos.Accounts.GetAccount(ctx, f.theirs.ID)
				return fenceOutcome{err: err, present: err == nil && got != nil && got.ID == f.theirs.ID}
			},
		},
		{
			label: "owner", site: "account_repo.GetAccount/ent", scope: asOwner, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				got, err := f.repos.Accounts.GetAccount(ctx, f.mine.ID)
				return fenceOutcome{err: err, present: err == nil && got != nil && got.ID == f.mine.ID}
			},
		},
		{
			label: "foreign", site: "account_repo.GetAccount/ent", scope: asForeign, want: wantNotFound,
			wantErr: repository.ErrNotFound,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				got, err := f.repos.Accounts.GetAccount(ctx, f.theirs.ID)
				return fenceOutcome{err: err, present: got != nil}
			},
		},

		// ---------- GetAccountWithTemplate：同上（带 edge 的单读） ----------
		{
			label: "admin", site: "account_repo.GetAccountWithTemplate/ent", scope: asAdmin, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				got, err := f.repos.Accounts.GetAccountWithTemplate(ctx, f.theirs.ID)
				return fenceOutcome{err: err, present: err == nil && got != nil && got.ID == f.theirs.ID}
			},
		},
		{
			label: "owner", site: "account_repo.GetAccountWithTemplate/ent", scope: asOwner, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				got, err := f.repos.Accounts.GetAccountWithTemplate(ctx, f.mine.ID)
				return fenceOutcome{err: err, present: err == nil && got != nil && got.ID == f.mine.ID}
			},
		},
		{
			label: "foreign", site: "account_repo.GetAccountWithTemplate/ent", scope: asForeign, want: wantNotFound,
			wantErr: repository.ErrNotFound,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				got, err := f.repos.Accounts.GetAccountWithTemplate(ctx, f.theirs.ID)
				return fenceOutcome{err: err, present: got != nil}
			},
		},

		// ---------- ListAccounts：列表 ⇒ 越域 = 空集 + 总数 0（不是错误） ----------
		{
			label: "admin", site: "account_repo.ListAccounts/ent", scope: asAdmin, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				list, total, err := f.repos.Accounts.ListAccounts(ctx, repository.ListQuery{})
				require.NoError(t, err)
				require.Contains(t, fenceAccountIDs(list), f.theirs.ID, "管理面必须看得到他方账号")
				return fenceOutcome{err: err, present: slices.Contains(fenceAccountIDs(list), f.theirs.ID), count: len(list), total: total}
			},
		},
		{
			label: "owner", site: "account_repo.ListAccounts/ent", scope: asOwner, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				list, _, err := f.repos.Accounts.ListAccounts(ctx, repository.ListQuery{})
				ids := fenceAccountIDs(list)
				require.NotContains(t, ids, f.theirs.ID, "本人作用域不得出现他方账号")
				return fenceOutcome{err: err, present: slices.Contains(ids, f.mine.ID), count: len(list)}
			},
		},
		{
			// 契约行：作用域内没有任何行 ⇒ 空列表 + 总数 0，且不报错。
			label: "stranger", site: "account_repo.ListAccounts/ent", scope: asStranger, want: wantEmptyList,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				list, total, err := f.repos.Accounts.ListAccounts(ctx, repository.ListQuery{})
				return fenceOutcome{err: err, count: len(list), total: total}
			},
		},
		{
			// 方向一（账号越域）：A 的 ctx 列全量，B 的行必须被过滤掉而 A 的行照常返回。
			label: "foreign-row", site: "account_repo.ListAccounts/ent", scope: asForeign, want: wantExcluded,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				list, _, err := f.repos.Accounts.ListAccounts(ctx, repository.ListQuery{})
				ids := fenceAccountIDs(list)
				require.Contains(t, ids, f.mine.ID, "本人行必须仍在结果里（证明是行级过滤，不是空表）")
				return fenceOutcome{err: err, present: slices.Contains(ids, f.theirs.ID), count: len(list)}
			},
		},

		// ---------- expandProxies：多站点方法的第二面（方向二：关联代理越域不装配） ----------
		{
			label: "admin", site: "account_repo.expandProxies/proxy", scope: asAdmin, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				list, _, err := f.repos.Accounts.ListAccounts(ctx, repository.ListQuery{})
				acc := fenceAccountOf(list, f.mineOnTheirProxy.ID)
				installed := acc != nil && acc.Proxy != nil && acc.Proxy.ID == f.theirProxy
				require.True(t, installed, "管理面必须展开他方私池代理（可见性只由作用域决定）")
				return fenceOutcome{err: err, present: installed, attached: installed}
			},
		},
		{
			label: "owner", site: "account_repo.expandProxies/proxy", scope: asOwner, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				list, _, err := f.repos.Accounts.ListAccounts(ctx, repository.ListQuery{})
				acc := fenceAccountOf(list, f.mine.ID)
				installed := acc != nil && acc.Proxy != nil && acc.Proxy.ID == f.mineProxy
				require.True(t, installed, "本人代理必须展开")
				return fenceOutcome{err: err, present: installed, attached: installed}
			},
		},
		{
			// 账号行可见（绑定事实不改写），但代理不装配：Proxy 保持 nil。
			label: "foreign", site: "account_repo.expandProxies/proxy", scope: asForeign, want: wantUnassembled,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				list, _, err := f.repos.Accounts.ListAccounts(ctx, repository.ListQuery{})
				acc := fenceAccountOf(list, f.mineOnTheirProxy.ID)
				require.NotNil(t, acc, "账号行本身可见（越域只影响代理装配）")
				require.NotNil(t, acc.ProxyID, "绑定列不得被读路径改写")
				return fenceOutcome{err: err, present: acc != nil, attached: acc != nil && acc.Proxy != nil}
			},
		},

		// ---------- DeleteAccount：写 ⇒ 越域报错且行不落地 ----------
		{
			label: "admin", site: "account_repo.DeleteAccount/ent", scope: asAdmin, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				acc := f.account(t, 0, nil, "del-admin")
				err := f.repos.Accounts.DeleteAccount(ctx, acc.ID)
				return fenceOutcome{err: err, present: err == nil && f.accountState(t, acc.ID).deleted}
			},
		},
		{
			label: "owner", site: "account_repo.DeleteAccount/ent", scope: asOwner, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				acc := f.account(t, f.owner.ID, nil, "del-owner")
				err := f.repos.Accounts.DeleteAccount(ctx, acc.ID)
				return fenceOutcome{err: err, present: err == nil && f.accountState(t, acc.ID).deleted}
			},
		},
		{
			label: "foreign", site: "account_repo.DeleteAccount/ent", scope: asForeign, want: wantUnchanged,
			wantErr: repository.ErrNotFound,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				acc := f.account(t, f.other.ID, nil, "del-foreign")
				before := f.accountState(t, acc.ID)
				err := f.repos.Accounts.DeleteAccount(ctx, acc.ID)
				after := f.accountState(t, acc.ID)
				return fenceOutcome{err: err, unchanged: before == after}
			},
		},

		// ---------- GetAccountGroups：不存在/越域 ⇒ 空集而非错误 ----------
		{
			label: "admin", site: "account_repo.GetAccountGroups/ent", scope: asAdmin, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				f.setGroupsAdmin(t, f.theirs.ID, f.group.ID)
				ids, err := f.repos.Accounts.GetAccountGroups(ctx, f.theirs.ID)
				return fenceOutcome{err: err, present: slices.Contains(ids, f.group.ID), count: len(ids)}
			},
		},
		{
			label: "owner", site: "account_repo.GetAccountGroups/ent", scope: asOwner, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				f.setGroupsAdmin(t, f.mine.ID, f.group.ID)
				ids, err := f.repos.Accounts.GetAccountGroups(ctx, f.mine.ID)
				return fenceOutcome{err: err, present: slices.Contains(ids, f.group.ID), count: len(ids)}
			},
		},
		{
			label: "foreign", site: "account_repo.GetAccountGroups/ent", scope: asForeign, want: wantEmptyList,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				f.setGroupsAdmin(t, f.theirs.ID, f.group.ID)
				ids, err := f.repos.Accounts.GetAccountGroups(ctx, f.theirs.ID)
				return fenceOutcome{err: err, count: len(ids)}
			},
		},

		// ---------- SetAccountGroups：entDirect 分支 ⇒ 越域报错且分组不变 ----------
		{
			label: "admin", site: "account_repo.SetAccountGroups/ent-direct", scope: asAdmin, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				err := f.repos.Accounts.SetAccountGroups(ctx, f.theirs.ID, []int64{f.otherGroup.ID})
				return fenceOutcome{err: err, present: err == nil && slices.Equal(f.accountGroups(t, f.theirs.ID), []int64{f.otherGroup.ID})}
			},
		},
		{
			label: "owner", site: "account_repo.SetAccountGroups/ent-direct", scope: asOwner, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				err := f.repos.Accounts.SetAccountGroups(ctx, f.mine.ID, []int64{f.group.ID})
				return fenceOutcome{err: err, present: err == nil && slices.Equal(f.accountGroups(t, f.mine.ID), []int64{f.group.ID})}
			},
		},
		{
			label: "foreign", site: "account_repo.SetAccountGroups/ent-direct", scope: asForeign, want: wantUnchanged,
			wantErr: repository.ErrNotFound,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				f.setGroupsAdmin(t, f.theirs.ID, f.group.ID)
				before := f.accountGroups(t, f.theirs.ID)
				err := f.repos.Accounts.SetAccountGroups(ctx, f.theirs.ID, []int64{f.otherGroup.ID})
				after := f.accountGroups(t, f.theirs.ID)
				return fenceOutcome{err: err, unchanged: slices.Equal(before, after)}
			},
		},

		// ---------- FindMissingOwnedAccountID：存在性探测 ⇒ 点名报出越域 id ----------
		{
			label: "admin", site: "account_repo.FindMissingOwnedAccountID/ent", scope: asAdmin, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				_, missing, err := f.repos.Accounts.FindMissingOwnedAccountID(ctx, []int64{f.theirs.ID})
				return fenceOutcome{err: err, present: err == nil && !missing}
			},
		},
		{
			label: "owner", site: "account_repo.FindMissingOwnedAccountID/ent", scope: asOwner, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				_, missing, err := f.repos.Accounts.FindMissingOwnedAccountID(ctx, []int64{f.mine.ID})
				return fenceOutcome{err: err, present: err == nil && !missing}
			},
		},
		{
			label: "foreign", site: "account_repo.FindMissingOwnedAccountID/ent", scope: asForeign, want: wantMissingID,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				id, missing, err := f.repos.Accounts.FindMissingOwnedAccountID(ctx, []int64{f.mine.ID, f.theirs.ID})
				require.True(t, missing, "混入他方 id 必须报缺失")
				return fenceOutcome{err: err, missingID: id, targetID: f.theirs.ID}
			},
		},

		// ---------- FailAccountCAS：越域 ⇒ 陈旧判决（不写他人行、不推他人代际） ----------
		{
			label: "admin", site: "account_repo.FailAccountCAS/ent", scope: asAdmin, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				acc := f.account(t, 0, nil, "fail-admin")
				st := f.accountState(t, acc.ID)
				err := f.repos.Accounts.FailAccountCAS(ctx, acc.ID, st.identity, "fence", now, "boom")
				return fenceOutcome{err: err, present: err == nil && f.accountState(t, acc.ID).failed}
			},
		},
		{
			label: "owner", site: "account_repo.FailAccountCAS/ent", scope: asOwner, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				acc := f.account(t, f.owner.ID, nil, "fail-owner")
				st := f.accountState(t, acc.ID)
				err := f.repos.Accounts.FailAccountCAS(ctx, acc.ID, st.identity, "fence", now, "boom")
				return fenceOutcome{err: err, present: err == nil && f.accountState(t, acc.ID).failed}
			},
		},
		{
			label: "foreign", site: "account_repo.FailAccountCAS/ent", scope: asForeign, want: wantUnchanged,
			wantErr: repository.ErrStaleIdentityRevision,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				acc := f.account(t, f.other.ID, nil, "fail-foreign")
				before := f.accountState(t, acc.ID)
				err := f.repos.Accounts.FailAccountCAS(ctx, acc.ID, before.identity, "fence", now, "boom")
				after := f.accountState(t, acc.ID)
				return fenceOutcome{err: err, unchanged: before == after}
			},
		},

		// ---------- RecoverAccountCAS：同上（C 陈旧哨兵不同） ----------
		{
			label: "admin", site: "account_repo.RecoverAccountCAS/ent", scope: asAdmin, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				acc := f.account(t, 0, nil, "recover-admin")
				st := f.accountState(t, acc.ID)
				require.NoError(t, f.repos.Accounts.FailAccountCAS(ctx, acc.ID, st.identity, "fence", now, "boom"))
				st = f.accountState(t, acc.ID)
				err := f.repos.Accounts.RecoverAccountCAS(ctx, acc.ID, st.lifecycle)
				return fenceOutcome{err: err, present: err == nil && !f.accountState(t, acc.ID).failed}
			},
		},
		{
			label: "owner", site: "account_repo.RecoverAccountCAS/ent", scope: asOwner, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				acc := f.account(t, f.owner.ID, nil, "recover-owner")
				st := f.accountState(t, acc.ID)
				require.NoError(t, f.repos.Accounts.FailAccountCAS(ctx, acc.ID, st.identity, "fence", now, "boom"))
				st = f.accountState(t, acc.ID)
				err := f.repos.Accounts.RecoverAccountCAS(ctx, acc.ID, st.lifecycle)
				return fenceOutcome{err: err, present: err == nil && !f.accountState(t, acc.ID).failed}
			},
		},
		{
			label: "foreign", site: "account_repo.RecoverAccountCAS/ent", scope: asForeign, want: wantUnchanged,
			wantErr: repository.ErrStaleRevision,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				acc := f.account(t, f.other.ID, nil, "recover-foreign")
				st := f.accountState(t, acc.ID)
				require.NoError(t, f.repos.Accounts.FailAccountCAS(context.Background(), acc.ID, st.identity, "fence", now, "boom"))
				before := f.accountState(t, acc.ID)
				err := f.repos.Accounts.RecoverAccountCAS(ctx, acc.ID, before.lifecycle)
				after := f.accountState(t, acc.ID)
				return fenceOutcome{err: err, unchanged: before == after}
			},
		},

		// ---------- checkAccountExist（helperFanout，入口 DeleteAccountsBatch） ----------
		{
			label: "admin", site: "batch.checkAccountExist/ent", scope: asAdmin, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				acc := f.account(t, 0, nil, "batchdel-admin")
				err := f.repos.Accounts.DeleteAccountsBatch(ctx, []int64{acc.ID})
				return fenceOutcome{err: err, present: err == nil && f.accountState(t, acc.ID).deleted}
			},
		},
		{
			label: "owner", site: "batch.checkAccountExist/ent", scope: asOwner, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				acc := f.account(t, f.owner.ID, nil, "batchdel-owner")
				err := f.repos.Accounts.DeleteAccountsBatch(ctx, []int64{acc.ID})
				return fenceOutcome{err: err, present: err == nil && f.accountState(t, acc.ID).deleted}
			},
		},
		{
			label: "foreign", site: "batch.checkAccountExist/ent", scope: asForeign, want: wantUnchanged,
			wantErr: repository.ErrNotFound,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				own := f.account(t, f.owner.ID, nil, "batchdel-own")
				foreign := f.account(t, f.other.ID, nil, "batchdel-foreign")
				beforeOwn, beforeForeign := f.accountState(t, own.ID), f.accountState(t, foreign.ID)
				err := f.repos.Accounts.DeleteAccountsBatch(ctx, []int64{own.ID, foreign.ID})
				return fenceOutcome{
					err:       err,
					unchanged: beforeOwn == f.accountState(t, own.ID) && beforeForeign == f.accountState(t, foreign.ID),
				}
			},
		},

		// ---------- lockAccountsForUpdate（rawSQL，入口 UpdateAccountsBatch） ----------
		{
			label: "admin", site: "codex_write.lockAccountsForUpdate/sql", scope: asAdmin, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				acc := f.account(t, 0, nil, "batchupd-admin")
				patch := repository.AccountPatch{Enabled: &disabled}
				_, err := f.repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc.ID}, patch)
				return fenceOutcome{err: err, present: err == nil && !f.accountState(t, acc.ID).enabled}
			},
		},
		{
			label: "owner", site: "codex_write.lockAccountsForUpdate/sql", scope: asOwner, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				acc := f.account(t, f.owner.ID, nil, "batchupd-owner")
				patch := repository.AccountPatch{Enabled: &disabled}
				_, err := f.repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc.ID}, patch)
				return fenceOutcome{err: err, present: err == nil && !f.accountState(t, acc.ID).enabled}
			},
		},
		{
			label: "foreign", site: "codex_write.lockAccountsForUpdate/sql", scope: asForeign, want: wantUnchanged,
			wantErr: repository.ErrNotFound,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				acc := f.account(t, f.other.ID, nil, "batchupd-foreign")
				before := f.accountState(t, acc.ID)
				patch := repository.AccountPatch{Enabled: &disabled}
				_, err := f.repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc.ID}, patch)
				return fenceOutcome{err: err, unchanged: before == f.accountState(t, acc.ID)}
			},
		},

		// ---------- ListProxies：集合读取 ⇒ 越域目标被过滤 ----------
		{
			label: "admin", site: "proxy_repo.ListProxies/proxy", scope: asAdmin, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				list, err := f.repos.Proxies.ListProxies(ctx)
				ids := fenceProxyIDs(list)
				require.Contains(t, ids, f.platformProxy, "管理面必须看得到平台代理")
				return fenceOutcome{err: err, present: slices.Contains(ids, f.theirProxy), count: len(list)}
			},
		},
		{
			label: "owner", site: "proxy_repo.ListProxies/proxy", scope: asOwner, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				list, err := f.repos.Proxies.ListProxies(ctx)
				ids := fenceProxyIDs(list)
				require.NotContains(t, ids, f.theirProxy, "本人列表不得出现他方私池代理")
				require.NotContains(t, ids, f.platformProxy, "平台代理对供应商不可见")
				return fenceOutcome{err: err, present: slices.Contains(ids, f.mineProxy), count: len(list)}
			},
		},
		{
			label: "foreign", site: "proxy_repo.ListProxies/proxy", scope: asForeign, want: wantExcluded,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				list, err := f.repos.Proxies.ListProxies(ctx)
				ids := fenceProxyIDs(list)
				require.Contains(t, ids, f.mineProxy, "本人代理必须仍在结果里")
				return fenceOutcome{err: err, present: slices.Contains(ids, f.theirProxy), count: len(list)}
			},
		},

		// ---------- GetProxy：单条读取 ⇒ ErrNotFound ----------
		{
			label: "admin", site: "proxy_repo.GetProxy/proxy", scope: asAdmin, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				got, err := f.repos.Proxies.GetProxy(ctx, f.theirProxy)
				return fenceOutcome{err: err, present: err == nil && got.ID == f.theirProxy}
			},
		},
		{
			label: "owner", site: "proxy_repo.GetProxy/proxy", scope: asOwner, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				got, err := f.repos.Proxies.GetProxy(ctx, f.mineProxy)
				return fenceOutcome{err: err, present: err == nil && got.ID == f.mineProxy}
			},
		},
		{
			label: "foreign", site: "proxy_repo.GetProxy/proxy", scope: asForeign, want: wantNotFound,
			wantErr: repository.ErrNotFound,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				got, err := f.repos.Proxies.GetProxy(ctx, f.theirProxy)
				return fenceOutcome{err: err, present: got.ID != 0}
			},
		},

		// ---------- UpdateProxy：全量更新 ⇒ 越域报错且条目不变 ----------
		{
			label: "admin", site: "proxy_repo.UpdateProxy/proxy", scope: asAdmin, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				cur, err := f.repos.Proxies.GetProxy(ctx, f.theirProxy)
				require.NoError(t, err)
				cur.Remark = "admin-touched"
				err = f.repos.Proxies.UpdateProxy(ctx, updateOf(cur, repository.ProxyCredential{}))
				return fenceOutcome{err: err, present: err == nil && f.proxyState(t, f.theirProxy).remark == "admin-touched"}
			},
		},
		{
			label: "owner", site: "proxy_repo.UpdateProxy/proxy", scope: asOwner, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				cur, err := f.repos.Proxies.GetProxy(ctx, f.mineProxy)
				require.NoError(t, err)
				cur.Remark = "owner-touched"
				err = f.repos.Proxies.UpdateProxy(ctx, updateOf(cur, repository.ProxyCredential{}))
				return fenceOutcome{err: err, present: err == nil && f.proxyState(t, f.mineProxy).remark == "owner-touched"}
			},
		},
		{
			label: "foreign", site: "proxy_repo.UpdateProxy/proxy", scope: asForeign, want: wantUnchanged,
			wantErr: repository.ErrNotFound,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				before := f.proxyState(t, f.theirProxy)
				cur, err := f.repos.Proxies.GetProxy(context.Background(), f.theirProxy)
				require.NoError(t, err)
				cur.Remark = "foreign-attempt"
				err = f.repos.Proxies.UpdateProxy(ctx, updateOf(cur, repository.ProxyCredential{}))
				return fenceOutcome{err: err, unchanged: before == f.proxyState(t, f.theirProxy)}
			},
		},

		// ---------- DeleteProxy：代理面 + 解绑面（同一方法的两个方向） ----------
		{
			label: "admin", site: "proxy_repo.DeleteProxy/proxy", scope: asAdmin, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				id := f.proxy(t, 0, "delprox-admin")
				err := f.repos.Proxies.DeleteProxy(ctx, id)
				return fenceOutcome{err: err, present: err == nil && f.proxyState(t, id).deleted}
			},
		},
		{
			label: "owner", site: "proxy_repo.DeleteProxy/proxy", scope: asOwner, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				id := f.proxy(t, f.owner.ID, "delprox-owner")
				err := f.repos.Proxies.DeleteProxy(ctx, id)
				return fenceOutcome{err: err, present: err == nil && f.proxyState(t, id).deleted}
			},
		},
		{
			label: "foreign", site: "proxy_repo.DeleteProxy/proxy", scope: asForeign, want: wantUnchanged,
			wantErr: repository.ErrNotFound,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				id := f.proxy(t, f.other.ID, "delprox-foreign")
				before := f.proxyState(t, id)
				err := f.repos.Proxies.DeleteProxy(ctx, id)
				return fenceOutcome{err: err, unchanged: before == f.proxyState(t, id)}
			},
		},
		// ---------- DeleteProxy/unbind：解绑连坐范围随归属 ----------
		{
			// 平台代理（owner IS NULL）⇒ 既有行为：解绑全部引用（含他方账号）。
			label: "admin", site: "proxy_repo.DeleteProxy/unbind", scope: asAdmin, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				pid := f.proxy(t, 0, "unbind-admin")
				mine := f.account(t, f.owner.ID, &pid, "unbind-admin-mine")
				theirs := f.account(t, f.other.ID, &pid, "unbind-admin-theirs")
				err := f.repos.Proxies.DeleteProxy(ctx, pid)
				a, b := f.accountState(t, mine.ID).proxyID, f.accountState(t, theirs.ID).proxyID
				return fenceOutcome{err: err, present: err == nil && a == 0 && b == 0}
			},
		},
		{
			// 私池代理 ⇒ 只解绑**其主**的账号：他方账号的绑定不得被连带抖掉（跨租户副作用）。
			label: "owner", site: "proxy_repo.DeleteProxy/unbind", scope: asOwner, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				pid := f.proxy(t, f.owner.ID, "unbind-owner")
				own := f.account(t, f.owner.ID, &pid, "unbind-own")
				foreignBound := f.account(t, f.other.ID, &pid, "unbind-foreign")
				err := f.repos.Proxies.DeleteProxy(ctx, pid)
				ownAfter := f.accountState(t, own.ID).proxyID
				foreignAfter := f.accountState(t, foreignBound.ID).proxyID
				require.Zero(t, ownAfter, "自己的账号必须被解绑")
				require.Equal(t, pid, foreignAfter, "他方账号的绑定不得被连带解绑")
				return fenceOutcome{err: err, present: err == nil && ownAfter == 0 && foreignAfter == pid}
			},
		},
		{
			// 越域删代理：代理不删、账号不解绑（两个副作用一起拒）。
			label: "foreign", site: "proxy_repo.DeleteProxy/unbind", scope: asForeign, want: wantUnchanged,
			wantErr: repository.ErrNotFound,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				pid := f.proxy(t, f.other.ID, "unbind-foreign")
				bound := f.account(t, f.other.ID, &pid, "unbind-foreign-acc")
				before := f.proxyState(t, pid)
				err := f.repos.Proxies.DeleteProxy(ctx, pid)
				return fenceOutcome{
					err:       err,
					unchanged: before == f.proxyState(t, pid) && f.accountState(t, bound.ID).proxyID == pid,
				}
			},
		},

		// ---------- WriteProxyProbe：探测四列的写面 ⇒ 越域报错且列不变 ----------
		{
			label: "admin", site: "proxy_repo.WriteProxyProbe/proxy", scope: asAdmin, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				err := f.repos.Proxies.WriteProxyProbe(ctx, f.theirProxy, now, true, "", "203.0.113.7")
				st := f.proxyState(t, f.theirProxy)
				return fenceOutcome{err: err, present: err == nil && st.probeOK && st.probeIP == "203.0.113.7"}
			},
		},
		{
			label: "owner", site: "proxy_repo.WriteProxyProbe/proxy", scope: asOwner, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				err := f.repos.Proxies.WriteProxyProbe(ctx, f.mineProxy, now, false, "dial tcp: timeout", "")
				st := f.proxyState(t, f.mineProxy)
				return fenceOutcome{err: err, present: err == nil && !st.probeOK && st.probeErr != ""}
			},
		},
		{
			label: "foreign", site: "proxy_repo.WriteProxyProbe/proxy", scope: asForeign, want: wantUnchanged,
			wantErr: repository.ErrNotFound,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				before := f.proxyState(t, f.theirProxy)
				err := f.repos.Proxies.WriteProxyProbe(ctx, f.theirProxy, now, false, "foreign-attempt", "")
				return fenceOutcome{err: err, unchanged: before == f.proxyState(t, f.theirProxy)}
			},
		},

		// ---------- accountExtQuery（helperFanout，入口 FindAccountExtByCodexKey） ----------
		{
			label: "admin", site: "ext_codex_repo.accountExtQuery/ent", scope: asAdmin, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				ext, err := f.repos.AccountExts.FindAccountExtByCodexKey(ctx, "fence-theirs@example.com", "fence-codex-theirs", credential.TypeCodexPAT)
				return fenceOutcome{err: err, present: err == nil && ext != nil && ext.AccountID == f.theirs.ID}
			},
		},
		{
			label: "owner", site: "ext_codex_repo.accountExtQuery/ent", scope: asOwner, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				ext, err := f.repos.AccountExts.FindAccountExtByCodexKey(ctx, "fence-mine@example.com", "fence-codex-mine", credential.TypeCodexPAT)
				return fenceOutcome{err: err, present: err == nil && ext != nil && ext.AccountID == f.mine.ID}
			},
		},
		{
			label: "foreign", site: "ext_codex_repo.accountExtQuery/ent", scope: asForeign, want: wantNotFound,
			wantErr: repository.ErrNotFound,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				ext, err := f.repos.AccountExts.FindAccountExtByCodexKey(ctx, "fence-theirs@example.com", "fence-codex-theirs", credential.TypeCodexPAT)
				return fenceOutcome{err: err, present: ext != nil}
			},
		},

		// ---------- accountOwnerUpdate（helperFanout，入口 AdminWriteOAuthRotationCAS） ----------
		{
			label: "admin", site: "ext_codex_repo.accountOwnerUpdate/ent", scope: asAdmin, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				acc := f.accountWithExt(t, 0, "rot-admin")
				st := f.accountState(t, acc.ID)
				err := f.repos.AccountExts.AdminWriteOAuthRotationCAS(ctx, acc.ID, st.lifecycle, "at", "rt", nil)
				return fenceOutcome{err: err, present: err == nil && f.accountState(t, acc.ID).lifecycle == st.lifecycle+1}
			},
		},
		{
			label: "owner", site: "ext_codex_repo.accountOwnerUpdate/ent", scope: asOwner, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				acc := f.accountWithExt(t, f.owner.ID, "rot-owner")
				st := f.accountState(t, acc.ID)
				err := f.repos.AccountExts.AdminWriteOAuthRotationCAS(ctx, acc.ID, st.lifecycle, "at", "rt", nil)
				return fenceOutcome{err: err, present: err == nil && f.accountState(t, acc.ID).lifecycle == st.lifecycle+1}
			},
		},
		{
			label: "foreign", site: "ext_codex_repo.accountOwnerUpdate/ent", scope: asForeign, want: wantUnchanged,
			wantErr: repository.ErrStaleRevision,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				acc := f.accountWithExt(t, f.other.ID, "rot-foreign")
				before := f.accountState(t, acc.ID)
				err := f.repos.AccountExts.AdminWriteOAuthRotationCAS(ctx, acc.ID, before.lifecycle, "at2", "rt2", nil)
				return fenceOutcome{err: err, unchanged: before == f.accountState(t, acc.ID)}
			},
		},

		// ---------- lockOwnedAccountForUpdate（rawSQL，入口 UpsertAccountExt） ----------
		{
			label: "admin", site: "ext_codex_repo.lockOwnedAccountForUpdate/sql", scope: asAdmin, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				acc := f.account(t, 0, nil, "lock-admin")
				_, err := f.repos.AccountExts.UpsertAccountExt(ctx, fencePATExt(acc.ID, "k-admin"))
				return fenceOutcome{err: err, present: err == nil}
			},
		},
		{
			label: "owner", site: "ext_codex_repo.lockOwnedAccountForUpdate/sql", scope: asOwner, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				acc := f.account(t, f.owner.ID, nil, "lock-owner")
				_, err := f.repos.AccountExts.UpsertAccountExt(ctx, fencePATExt(acc.ID, "k-owner"))
				return fenceOutcome{err: err, present: err == nil}
			},
		},
		{
			label: "foreign", site: "ext_codex_repo.lockOwnedAccountForUpdate/sql", scope: asForeign, want: wantNotFound,
			wantErr: repository.ErrNotFound,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				acc := f.account(t, f.other.ID, nil, "lock-foreign")
				_, err := f.repos.AccountExts.UpsertAccountExt(ctx, fencePATExt(acc.ID, "k-foreign"))
				// 加锁读 0 行 ⇒ ErrNotFound，且事务未创建.ext 行（不落地就是"不变"）。
				return fenceOutcome{err: err, present: !errors.Is(err, repository.ErrNotFound)}
			},
		},

		// ---------- GetOwnedAccountExt：entDirect ⇒ ErrNotFound ----------
		{
			label: "admin", site: "ext_codex_repo.GetOwnedAccountExt/ent", scope: asAdmin, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				ext, err := f.repos.AccountExts.GetOwnedAccountExt(ctx, f.theirs.ID)
				return fenceOutcome{err: err, present: err == nil && ext != nil && ext.AccountID == f.theirs.ID}
			},
		},
		{
			label: "owner", site: "ext_codex_repo.GetOwnedAccountExt/ent", scope: asOwner, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				ext, err := f.repos.AccountExts.GetOwnedAccountExt(ctx, f.mine.ID)
				return fenceOutcome{err: err, present: err == nil && ext != nil && ext.AccountID == f.mine.ID}
			},
		},
		{
			label: "foreign", site: "ext_codex_repo.GetOwnedAccountExt/ent", scope: asForeign, want: wantNotFound,
			wantErr: repository.ErrNotFound,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				ext, err := f.repos.AccountExts.GetOwnedAccountExt(ctx, f.theirs.ID)
				return fenceOutcome{err: err, present: ext != nil}
			},
		},

		// ---------- GetOwnedAccountCredential：rawSQL JOIN ⇒ ErrNotFound ----------
		{
			label: "admin", site: "ext_codex_repo.GetOwnedAccountCredential/sql", scope: asAdmin, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				cred, err := f.repos.AccountExts.GetOwnedAccountCredential(ctx, f.theirs.ID)
				return fenceOutcome{err: err, present: err == nil && cred != nil}
			},
		},
		{
			label: "owner", site: "ext_codex_repo.GetOwnedAccountCredential/sql", scope: asOwner, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				cred, err := f.repos.AccountExts.GetOwnedAccountCredential(ctx, f.mine.ID)
				return fenceOutcome{err: err, present: err == nil && cred != nil}
			},
		},
		{
			label: "foreign", site: "ext_codex_repo.GetOwnedAccountCredential/sql", scope: asForeign, want: wantNotFound,
			wantErr: repository.ErrNotFound,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				cred, err := f.repos.AccountExts.GetOwnedAccountCredential(ctx, f.theirs.ID)
				return fenceOutcome{err: err, present: cred != nil}
			},
		},

		// ---------- ScanUsageAgg：聚合 SQL ⇒ 越域键不出现 ----------
		{
			label: "admin", site: "usage_repo.ScanUsageAgg/sql", scope: asAdmin, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				aggs, err := f.repos.Usages.ScanUsageAgg(ctx, []int64{f.mine.ID, f.theirs.ID}, f.usageFrom, f.usageTo)
				return fenceOutcome{err: err, present: aggs[f.theirs.ID] != nil, count: len(aggs)}
			},
		},
		{
			label: "owner", site: "usage_repo.ScanUsageAgg/sql", scope: asOwner, want: wantVisible,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				aggs, err := f.repos.Usages.ScanUsageAgg(ctx, []int64{f.mine.ID}, f.usageFrom, f.usageTo)
				return fenceOutcome{err: err, present: aggs[f.mine.ID] != nil, count: len(aggs)}
			},
		},
		{
			// 混批：本属行保留、他方行被 SQL 过滤（证明是行级过滤而不是整批失败）。
			label: "foreign", site: "usage_repo.ScanUsageAgg/sql", scope: asForeign, want: wantZeroCount,
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				aggs, err := f.repos.Usages.ScanUsageAgg(ctx, []int64{f.mine.ID, f.theirs.ID}, f.usageFrom, f.usageTo)
				require.NotNil(t, aggs[f.mine.ID], "本属行必须保留（否则这条用例证明不了行级过滤）")
				return fenceOutcome{err: err, present: aggs[f.theirs.ID] != nil, count: len(aggs)}
			},
		},
	}
}

// fencePATExt 构造一条 pat ext 行（UpsertAccountExt 的入参）。
func fencePATExt(accountID int64, pat string) *domain.AccountExt {
	return &domain.AccountExt{
		AccountID: accountID, CredentialType: credential.TypeCodexPAT,
		CodexIdentity: &domain.CodexIdentity{InstallationID: "11111111-2222-3333-4444-555555555555"},
		CodexPATKey:   &pat,
	}
}

// --- 门禁测试 ---

// TestScopeFenceSiteTableIsWellFormed 清单自身的形状：id 唯一、字段齐备、witness 与形态
// 匹配。表腐化在这里就该红，否则门禁①②给出的会是一条误导性的失败信息。
func TestScopeFenceSiteTableIsWellFormed(t *testing.T) {
	require.Empty(t, fenceSiteTableViolations(ownerSites), "站点清单自相矛盾")

	// 四种形态都必须至少有一个站点：否则"形态×2×3"的覆盖会出现静默空洞。
	seen := make(map[siteKind]int, len(ownerSites))
	for _, s := range ownerSites {
		seen[s.kind]++
	}
	for _, kind := range []siteKind{kindEntHelper, kindEntDirect, kindRawSQL, kindHelperFanout} {
		require.NotZero(t, seen[kind], "形态 %s 没有任何站点", kind)
	}
}

// TestScopeFenceWitnessGate 门禁①（正向）：每个站点声明的判别片段必须仍在该 FuncDecl 体内。
//
// 它守的是"谓词还在不在"：删掉/改写 owner 条件 ⇒ 红，而不是等到行为表（可能因夹具原因）
// 恰好还能通过。
func TestScopeFenceWitnessGate(t *testing.T) {
	sources := fenceSources(t)
	require.Empty(t, fenceWitnessViolations(t, sources, ownerSites),
		"owner 谓词的 witness 缺失：有站点的谓词被删或被改写")

	t.Run("对照：witness 在场不报", func(t *testing.T) {
		injected := fenceParseInjected(t, "account_repo.go", `package repository

import "context"

func GetAccount(ctx context.Context, id int64) error {
	_ = accountOwnerPred(domain.AccountScopeFrom(ctx))
	return nil
}
`)
		sites := []ownerSite{{
			id: "account_repo.GetAccount/ent", file: "account_repo.go", method: "GetAccount",
			kind: kindEntHelper, witness: "accountOwnerPred(", entry: "GetAccount", want: wantNotFound,
		}}
		require.Empty(t, fenceWitnessViolations(t, []fenceSource{injected}, sites),
			"谓词在场的函数体不得被判违规（判据写反时这里会红）")
	})

	t.Run("谓词被删", func(t *testing.T) {
		injected := fenceParseInjected(t, "account_repo.go", `package repository

import "context"

func GetAccount(ctx context.Context, id int64) error {
	return nil
}
`)
		got := fenceWitnessViolations(t, []fenceSource{injected}, ownerSites[:1])
		require.Len(t, got, 1, "只该报被注入的那个站点：%v", got)
		require.Contains(t, got[0], "account_repo.GetAccount/ent")
		require.Contains(t, got[0], "不再出现")
	})

	t.Run("注释里的片段不算 witness", func(t *testing.T) {
		// 渲染走 AST（注释不在 AST 里）：这正是不用原始字节切片的原因。
		injected := fenceParseInjected(t, "account_repo.go", `package repository

import "context"

func GetAccount(ctx context.Context, id int64) error {
	// accountOwnerPred( 曾经在这里
	return nil
}
`)
		got := fenceWitnessViolations(t, []fenceSource{injected}, ownerSites[:1])
		require.Len(t, got, 1)
		require.Contains(t, got[0], "account_repo.GetAccount/ent")
	})

	t.Run("方法整条消失", func(t *testing.T) {
		injected := fenceParseInjected(t, "account_repo.go", "package repository\n")
		got := fenceWitnessViolations(t, []fenceSource{injected}, ownerSites[:1])
		require.Len(t, got, 1)
		require.Contains(t, got[0], "找不到")
	})
}

// TestScopeFenceDeclaredSupersetGate 门禁②（反向完备）：扫描面上「消费 ctx 作用域 + 至少
// 一处 owner 谓词」的方法集合必须 ⊆ 站点清单。
//
// 它守的是"新方法有没有登记"：给已有方法加第二个谓词、或新增一个直写 SupplierUserID /
// 原生 SQL fence 的方法 ⇒ 红，并点名该方法。
func TestScopeFenceDeclaredSupersetGate(t *testing.T) {
	hits := fenceObservedSites(t, fenceSources(t))
	require.NotEmpty(t, hits, "扫描面必须至少命中一处站点（否则判据恒真）")
	names := make([]string, 0, len(hits))
	for _, h := range hits {
		names = append(names, h.String())
	}
	t.Logf("扫描面命中 %d 处 owner 站点：%s", len(hits), strings.Join(names, "、"))
	require.Empty(t, fenceUndeclaredViolations(hits, ownerSites),
		"有 owner 站点未登记：%v", hits)

	t.Run("对照：已登记的方法不报", func(t *testing.T) {
		injected := fenceParseInjected(t, "account_repo.go", `package repository

import "context"

func GetAccount(ctx context.Context, id int64) error {
	_ = accountOwnerPred(domain.AccountScopeFrom(ctx))
	return nil
}
`)
		got := fenceUndeclaredViolations(fenceObservedSites(t, []fenceSource{injected}), ownerSites)
		require.Empty(t, got, "已登记方法不得被判违规：%v", got)
	})

	t.Run("直写谓词的新方法", func(t *testing.T) {
		injected := fenceParseInjected(t, "account_repo.go", `package repository

import "context"

func (r *AccountRepo) Sneaky(ctx context.Context, id int64) error {
	scope := domain.AccountScopeFrom(ctx)
	return r.client.Account.Update().Where(account.SupplierUserID(scope.OwnerUID)).Exec(ctx)
}
`)
		got := fenceUndeclaredViolations(fenceObservedSites(t, []fenceSource{injected}), ownerSites)
		require.Len(t, got, 1, "只该报注入的新方法：%v", got)
		require.Contains(t, got[0], "Sneaky")
		require.Contains(t, got[0], "entDirect")
		require.Contains(t, got[0], "三态行为行")
	})

	t.Run("原生 SQL fence 的新方法", func(t *testing.T) {
		injected := fenceParseInjected(t, "usage_repo.go", `package repository

import "context"

func (r *UsageRepo) SneakySQL(ctx context.Context, id int64) error {
	q := "UPDATE accounts SET enabled = false WHERE id = $1"
	if s := domain.AccountScopeFrom(ctx); s.Set {
		q += " AND supplier_user_id = $2"
	}
	_, err := r.pool.Exec(ctx, q, id)
	return err
}
`)
		got := fenceUndeclaredViolations(fenceObservedSites(t, []fenceSource{injected}), ownerSites)
		require.Len(t, got, 1, "只该报注入的新方法：%v", got)
		require.Contains(t, got[0], "SneakySQL")
		require.Contains(t, got[0], "rawSQL")
	})

	t.Run("只消费作用域不设 fence 不算站点", func(t *testing.T) {
		// CreateProxy 的形状：从作用域**落** owner，没有 foreign 语义。
		injected := fenceParseInjected(t, "proxy_repo.go", `package repository

import "context"

func (r *ProxyRepo) CreateOnly(ctx context.Context, name string) error {
	b := r.client.Proxy.Create().SetName(name)
	if scope := domain.AccountScopeFrom(ctx); scope.Set {
		b = b.SetOwnerUID(scope.OwnerUID)
	}
	_, err := b.Save(ctx)
	return err
}
`)
		require.Empty(t, fenceUndeclaredViolations(fenceObservedSites(t, []fenceSource{injected}), ownerSites))
	})

	t.Run("不消费作用域的显式 uid 谓词不算站点", func(t *testing.T) {
		// UpdateUser 的形状：uid 来自显式入参，没有「注入作用域 ⇒ 越域被拒」这条路径。
		injected := fenceParseInjected(t, "user_repo.go", `package repository

import "context"

func (r *UserRepo) DisableAccounts(ctx context.Context, uid int64) error {
	_, err := r.client.Account.Update().Where(account.SupplierUserID(uid)).SetEnabled(false).Save(ctx)
	return err
}
`)
		require.Empty(t, fenceUndeclaredViolations(fenceObservedSites(t, []fenceSource{injected}), ownerSites))
	})

	t.Run("供应商账务表的显式 uid SQL 不算站点", func(t *testing.T) {
		// supplier_balances 的 `supplier_user_id = $1` 是"查自己的账"，不是租户 fence。
		injected := fenceParseInjected(t, "supplier_admin_repo.go", `package repository

import "context"

func (r *SupplierAdminRepo) Balance(ctx context.Context, uid int64) error {
	_, err := r.pool.Exec(ctx, `+"`"+`UPDATE supplier_balances SET available = 0 WHERE supplier_user_id = $1`+"`"+`, uid)
	return err
}
`)
		require.Empty(t, fenceUndeclaredViolations(fenceObservedSites(t, []fenceSource{injected}), ownerSites))
	})
}

// TestScopeFenceBehaviour 行为表：每个站点 ≥3 行（admin 通 / owner 通 / foreign 拒），
// foreign 行的期望必须等于站点声明（wantKind 单值化），且越域**真的**被拒。
func TestScopeFenceBehaviour(t *testing.T) {
	f := newFenceFixture(t)
	rows := fenceRows()

	declared := make(map[string]bool, len(ownerSites))
	for _, s := range ownerSites {
		declared[s.id] = true
	}
	seenRow := make(map[string]bool, len(rows))
	for _, r := range rows {
		require.True(t, declared[r.site], "行为行 %s/%s 指向未声明的站点——删站点必须同时删行", r.site, r.label)
		key := r.site + "/" + r.label
		require.False(t, seenRow[key], "行为行重复：%s", key)
		seenRow[key] = true
	}

	for _, site := range ownerSites {
		t.Run(site.id, func(t *testing.T) {
			var mine []fenceRow
			scopes := make(map[fenceScope]bool)
			for _, r := range rows {
				if r.site != site.id {
					continue
				}
				mine = append(mine, r)
				scopes[r.scope] = true
			}
			require.NotEmpty(t, mine, "站点 %s 没有任何行为行", site.id)
			for _, scope := range []fenceScope{asAdmin, asOwner, asForeign} {
				require.True(t, scopes[scope], "站点 %s 缺少 %s 行", site.id, scope)
			}

			for _, r := range mine {
				t.Run(r.label, func(t *testing.T) {
					got := r.run(t, f, f.ctxOf(r.scope))
					if r.wantErr != nil {
						require.ErrorIs(t, got.err, r.wantErr, "%s/%s 的错误哨兵必须精确", site.id, r.label)
					}
					requireWant(t, r.want, got)
				})
			}

			// 契约行：**恰好一条**拒绝行（非 admin/owner 作用域）承载站点声明的单值结论。
			// 0 条 ⇒ 期望被改成了别的东西（M6 的形状）；≥2 条 ⇒ 出现"两种结论都算过"的
			// 二选一，正是本规格要禁的东西。多站点方法的附加方向（如 ListAccounts 的
			// foreign-row）各自钉死自己的形状，但不冒充站点的契约。
			var contract []fenceRow
			for _, r := range mine {
				if r.scope != asAdmin && r.scope != asOwner && r.want == site.want {
					contract = append(contract, r)
				}
			}
			require.Len(t, contract, 1,
				"站点 %s 声明的契约 %q 必须恰好被一条拒绝行承载（当前 %d 条）", site.id, site.want, len(contract))
		})
	}
}
