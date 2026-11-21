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
// # 门禁按 import path 还原包名，不认书写名
//
// 两条门禁都先把 `File.Imports` 解析成「本地名 → 规范包名」表（`internal/domain` →
// domain，`internal/ent/account` → account，`internal/ent/proxy` → proxy），再按
// **选择器根指向哪个包**识别调用。别名（`accountpkg "…/ent/account"`）按
// ImportSpec.Name 记账；点导入没有本地名，落进 dotted 集合，于是裸标识符
// `SupplierUserID(...)` / `AccountScopeFrom(...)` 同样还原到对应包。只认文本 token 的
// 门禁在这里会静默失明：把 domain 导入成 `dom`，扫描面命中 23 → 13 而门禁仍 PASS
// （裁决 I1 实测）——`⊆` 是单向的，掉东西永远不红。
//
// # 行为表的两条结论义务
//
//  1. **整行未被改动**。拒绝行不是「投影列没变」，而是用**平台管理面**（无作用域 ctx）
//     把目标整行读回（GetAccount / GetProxy / GetAccountExt），与操作前快照做整结构
//     `require.Equal`——名称 / upstream_key / supplier_user_id / updated_at 这些投影外
//     的列同样在判据里。只比投影列时，拒绝路径上的一次无作用域写可以让 73 行全绿
//     （裁决 C2 实测）。
//  2. **存在性不可区分**。单行站点各有一条 `nonexistent` 对照行（A 的 ctx + 不存在的
//     id / 不存在的键），它的可观测结论必须与 `foreign` 行逐项相同（错误哨兵集合、
//     空结果形态、是否点名目标 id）。越域已存在行回 ErrNotFound、越域不存在行回别的
//     错误，是另一条泄漏面（裁决 C3）。
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
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
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
	// wantUnchanged 写路径：越域 ⇒ 返回错误**且**目标整行与操作前快照整结构相同
	// （不写他人行的任何一列，含 updated_at）。
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

	// singleRow 标记「单行站点」：目标是一个 id 或一条唯一键。这类站点必须有一条
	// compare:"foreign" 的 nonexistent 对照行——越域与不存在只能给出同一可观测结论。
	// 批量/列表/聚合站点为 false（它们的结论形态是集合，存在性泄漏不适用）。
	singleRow bool
	// leaksExistence 是 singleRow 的**显式豁免**：确实按设计区分「越域」与「不存在」
	// 时必须置位并写明理由，同时该站点不得有对照行。与 singleRow 二选一，不许都空。
	leaksExistence bool
	leakReason     string
}

// ownerSites 是站点清单：数据的唯一来源，同时喂门禁①②与行为表。
//
// 覆盖范围 = 扫描面上「消费 ctx 作用域 + 至少一处 owner 谓词」的全部方法（门禁②反向
// 断言其 ⊆ 本清单）；helper 站点额外声明它的调用方入口，因为越域拒绝发生在调用边界。
var ownerSites = []ownerSite{
	// --- account_repo.go：ent helper 形态 ---
	{
		id: "account_repo.GetAccount/ent", file: "account_repo.go", method: "GetAccount",
		kind: kindEntHelper, witness: "accountOwnerPred(", entry: "GetAccount", want: wantNotFound, singleRow: true,
	},
	{
		id: "account_repo.GetAccountWithTemplate/ent", file: "account_repo.go", method: "GetAccountWithTemplate",
		kind: kindEntHelper, witness: "accountOwnerPred(", entry: "GetAccountWithTemplate", want: wantNotFound, singleRow: true,
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
		kind: kindEntHelper, witness: "accountOwnerPred(", entry: "DeleteAccount", want: wantUnchanged, singleRow: true,
	},
	{
		id: "account_repo.GetAccountGroups/ent", file: "account_repo.go", method: "GetAccountGroups",
		kind: kindEntHelper, witness: "accountOwnerPred(", entry: "GetAccountGroups", want: wantEmptyList, singleRow: true,
	},
	{
		// entDirect：作用域分支直接写 `account.SupplierUserID(sc.OwnerUID)`，没有 helper 文本。
		id: "account_repo.SetAccountGroups/ent-direct", file: "account_repo.go", method: "SetAccountGroups",
		kind: kindEntDirect, witness: "account.SupplierUserID(", entry: "SetAccountGroups", want: wantUnchanged, singleRow: true,
	},
	{
		id: "account_repo.FindMissingOwnedAccountID/ent", file: "account_repo.go", method: "FindMissingOwnedAccountID",
		kind: kindEntHelper, witness: "accountOwnerPred(", entry: "FindMissingOwnedAccountID", want: wantMissingID, singleRow: true,
	},
	{
		id: "account_repo.FailAccountCAS/ent", file: "account_repo.go", method: "FailAccountCAS",
		kind: kindEntHelper, witness: "accountOwnerPred(", entry: "FailAccountCAS", want: wantUnchanged, singleRow: true,
	},
	{
		id: "account_repo.RecoverAccountCAS/ent", file: "account_repo.go", method: "RecoverAccountCAS",
		kind: kindEntHelper, witness: "accountOwnerPred(", entry: "RecoverAccountCAS", want: wantUnchanged, singleRow: true,
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
		kind: kindEntHelper, witness: "proxyOwnerPred(", entry: "GetProxy", want: wantNotFound, singleRow: true,
	},
	{
		id: "proxy_repo.UpdateProxy/proxy", file: "proxy_repo.go", method: "UpdateProxy",
		kind: kindEntHelper, witness: "proxyOwnerPred(", entry: "UpdateProxy", want: wantUnchanged, singleRow: true,
	},
	{
		id: "proxy_repo.DeleteProxy/proxy", file: "proxy_repo.go", method: "DeleteProxy",
		kind: kindEntHelper, witness: "proxyOwnerPred(", entry: "DeleteProxy", want: wantUnchanged, singleRow: true,
	},

	{
		// 同一方法的第二面：解绑连坐范围随**代理**归属（`*row.OwnerUID`），不是 ctx 作用域。
		// 门禁②按方法集合比较，方法内新增第二个谓词在方法级看不见——witness 才看得见，
		// 故这一面单独登记。
		id: "proxy_repo.DeleteProxy/unbind", file: "proxy_repo.go", method: "DeleteProxy",
		kind: kindEntDirect, witness: "account.SupplierUserID(", entry: "DeleteProxy", want: wantUnchanged, singleRow: true,
	},
	{
		id: "proxy_repo.WriteProxyProbe/proxy", file: "proxy_repo.go", method: "WriteProxyProbe",
		kind: kindEntHelper, witness: "proxyOwnerPred(", entry: "WriteProxyProbe", want: wantUnchanged, singleRow: true,
	},

	// --- ext_codex_repo.go：ext 查询基座 / 直接谓词 / 原生 SQL ---
	{
		// helperFanout：owner 谓词经 EXISTS 子查询挂在 ext 查询基座上，调用方
		// `FindAccountExtByCodexKey` 自己看不到任何谓词文本。
		id: "ext_codex_repo.accountExtQuery/ent", file: "ext_codex_repo.go", method: "accountExtQuery",
		kind: kindHelperFanout, witness: "accountOwnerPred(", entry: "FindAccountExtByCodexKey", want: wantNotFound, singleRow: true,
	},
	{
		// helperFanout（entDirect 载体）：把 owner 谓词 AND 进调用方的 accounts UPDATE。
		id: "ext_codex_repo.accountOwnerUpdate/ent", file: "ext_codex_repo.go", method: "accountOwnerUpdate",
		kind: kindHelperFanout, witness: "account.SupplierUserID(", entry: "AdminWriteOAuthRotationCAS", want: wantUnchanged, singleRow: true,
	},
	{
		id: "ext_codex_repo.lockOwnedAccountForUpdate/sql", file: "ext_codex_repo.go", method: "lockOwnedAccountForUpdate",
		kind: kindRawSQL, witness: "supplier_user_id = $", entry: "UpsertAccountExt", want: wantNotFound, singleRow: true,
	},
	{
		// entDirect：`HasAccountWith(account.SupplierUserID(s.OwnerUID), ...)`。
		id: "ext_codex_repo.GetOwnedAccountExt/ent", file: "ext_codex_repo.go", method: "GetOwnedAccountExt",
		kind: kindEntDirect, witness: "account.SupplierUserID(", entry: "GetOwnedAccountExt", want: wantNotFound, singleRow: true,
	},
	{
		// rawSQL：一条 JOIN 同时读凭据列与父账号 K，owner 谓词在同一语句的 WHERE 里。
		id: "ext_codex_repo.GetOwnedAccountCredential/sql", file: "ext_codex_repo.go", method: "GetOwnedAccountCredential",
		kind: kindRawSQL, witness: "supplier_user_id = $", entry: "GetOwnedAccountCredential", want: wantNotFound, singleRow: true,
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
	// unchanged 是「目标整行没被改」的比较结果：nil = 本行没有可比对的目标行；
	// 非 nil = 操作前后整结构（含 updated_at）比较的结果。写行必须填，
	// 读行填了也一律核对——读面同样不得带副作用。
	unchanged *bool
}

// fenceUnchanged 包一个比较结果，区分「没比过」与「比过且相同」。
func fenceUnchanged(v bool) *bool { return &v }

// fenceRow 是一条行为行：{站点, 作用域, 调用, 期望}。
type fenceRow struct {
	label   string // "admin" / "owner" / "foreign" / "nonexistent"：与站点拼成完整用例名
	site    string // ownerSite.id
	scope   fenceScope
	want    wantKind
	wantErr error // 可选：越域错误的**哨兵**必须精确（nil = 只判形状）
	// compare 非空 = 本行是**对照行**：必须与同站点、该 label 的行给出同一可观测结论
	// （哨兵集合 / 空结果形态 / 是否点名目标 id）。对照行不冒充站点契约。
	compare string
	run     func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome
}

// requireWant 是唯一的结论判据。
func requireWant(t *testing.T, want wantKind, got fenceOutcome) {
	t.Helper()
	fenceRequireUntouched(t, got)
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
		require.NotNil(t, got.unchanged, "写站点必须提供整行快照比较")
	default:
		t.Fatalf("未定义的 wantKind：%q", want)
	}
}

// fenceRequireUntouched 核对待核行的「目标整行没被改」结论。nil = 本行没有目标行，
// 不适用；非 nil 则必须是 true——拒绝路径上任何一列被写都要红。
func fenceRequireUntouched(t *testing.T, got fenceOutcome) {
	t.Helper()
	if got.unchanged == nil {
		return
	}
	require.True(t, *got.unchanged, "拒绝路径不得改动目标整行（含投影外的列与 updated_at）")
}

// fenceSentinel 是一个可比较的错误类别。跨行对照只比**哨兵归属**，不比错误文本
// （文本里带目标 id，天然不同）。
type fenceSentinel struct {
	name string
	err  error
}

var fenceSentinels = []fenceSentinel{
	{"ErrNotFound", repository.ErrNotFound},
	{"ErrInvalidInput", repository.ErrInvalidInput},
	{"ErrConflict", repository.ErrConflict},
	{"ErrProviderNotWired", repository.ErrProviderNotWired},
	{"ErrStaleRevision", repository.ErrStaleRevision},
	{"ErrStaleIdentityRevision", repository.ErrStaleIdentityRevision},
}

// fenceObservable 是**跨行可比**的观测摘要：foreign 与 nonexistent 必须逐项相同。
type fenceObservable struct {
	errIsNil     bool
	errSentinels []string // 命中的哨兵名（按 fenceSentinels 序，稳定可比）
	present      bool
	count        int
	total        int64
	attached     bool
	namedTarget  bool // missingID 是否等于本行的 targetID（不比较具体 id 值）
}

func fenceObservableOf(o fenceOutcome) fenceObservable {
	marks := make([]string, 0, 2)
	for _, s := range fenceSentinels {
		if errors.Is(o.err, s.err) {
			marks = append(marks, s.name)
		}
	}
	return fenceObservable{
		errIsNil: o.err == nil, errSentinels: marks,
		present: o.present, count: o.count, total: o.total, attached: o.attached,
		namedTarget: o.missingID != 0 && o.missingID == o.targetID,
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

// fenceFragments 是三种**行内**判别片段（witness 取值只能来自这里或 rawSQL 判据）。
var fenceFragments = map[siteKind][]string{
	kindEntHelper:    {"accountOwnerPred(", "proxyOwnerPred("},
	kindEntDirect:    {"account.SupplierUserID(", "proxy.OwnerUID("},
	kindHelperFanout: {"accountOwnerPred(", "proxyOwnerPred(", "account.SupplierUserID(", "proxy.OwnerUID("},
}

// fenceWitnessSymbols 把 witness 片段翻译成**语法身份**（规范包名 + 符号名）。
// 门禁①因此不依赖书写名：`accountpkg.SupplierUserID(...)` 与点导入后的裸
// `SupplierUserID(...)` 都还原到 {account, SupplierUserID}。
var fenceWitnessSymbols = map[string]fenceSymbol{
	"accountOwnerPred(":       {sel: "accountOwnerPred"},
	"proxyOwnerPred(":         {sel: "proxyOwnerPred"},
	"account.SupplierUserID(": {pkg: "account", sel: "SupplierUserID"},
	"proxy.OwnerUID(":         {pkg: "proxy", sel: "OwnerUID"},
}

// rawSQL 判据：同一条语句里既有 owner 列比较又有 accounts 表（见文件头的边界说明）。
const (
	fenceRawSQLLiteral = "supplier_user_id = $"
	fenceRawSQLTable   = "accounts"
)

// fenceCanonicalPkg 把 import path 归一到判据认的三个包名；其余包返回 ""。
// 只按路径后缀判定：模块路径改名（换 host / 换模块名）不影响判据。
func fenceCanonicalPkg(importPath string) string {
	switch {
	case strings.HasSuffix(importPath, "/internal/domain"):
		return "domain"
	case strings.HasSuffix(importPath, "/internal/ent/account"):
		return "account"
	case strings.HasSuffix(importPath, "/internal/ent/proxy"):
		return "proxy"
	}
	return ""
}

// fenceImportTable 是「文件内本地名 → import path」表，包身份由 fenceCanonicalPkg
// 在**判定时**从路径得出。**别名与点导入都要还原**：
//   - 别名（`accountpkg "…/ent/account"`）：ImportSpec.Name 就是本地名，直接记账；
//   - 无别名：本地名 = import path 最后一段；
//   - 点导入（`. "…/ent/account"`）：没有本地名，记进 dotted，裸标识符才算这个包的符号。
//
// 只认书写名的判据在这里会静默失明：把 domain 导入成 `dom`，扫描面命中 23 → 13
// 而门禁仍然 PASS（裁决 I1 实测）——掉站点永远不会让 ⊆ 变红。
type fenceImportTable struct {
	local  map[string]string // 本地名 → import path
	dotted map[string]bool   // import path → 是否点导入
}

func fenceImportsOf(t *testing.T, src fenceSource) fenceImportTable {
	t.Helper()
	tbl := fenceImportTable{local: make(map[string]string, len(src.file.Imports)), dotted: make(map[string]bool)}
	for _, spec := range src.file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		require.NoErrorf(t, err, "import path 必须是字符串字面量：%s", spec.Path.Value)
		switch {
		case spec.Name == nil:
			tbl.local[importPath[strings.LastIndex(importPath, "/")+1:]] = importPath
		case spec.Name.Name == ".":
			tbl.dotted[importPath] = true
		case spec.Name.Name == "_":
			// 空白导入不引入本地名，不可能产生调用点。
		default:
			tbl.local[spec.Name.Name] = importPath
		}
	}
	return tbl
}

// localPackage：本地名 → 判据认的包身份（不认识的包返回 ""）。
func (t fenceImportTable) localPackage(local string) string {
	return fenceCanonicalPkg(t.local[local])
}

// dottedPackage：该包是否被点导入（点导入后没有本地名，只能从 import path 回答）。
func (t fenceImportTable) dottedPackage(pkg string) bool {
	for importPath := range t.dotted {
		if fenceCanonicalPkg(importPath) == pkg {
			return true
		}
	}
	return false
}

// fenceOwnerSymbols 是判据关心的符号，按规范包名分组：
//   - domain.AccountScopeFrom：消费作用域的唯一入口；
//   - account.SupplierUserID / proxy.OwnerUID：直接进 ent 条件的 owner 谓词。
var fenceOwnerSymbols = map[string][]string{
	"domain":  {"AccountScopeFrom"},
	"account": {"SupplierUserID"},
	"proxy":   {"OwnerUID"},
}

// fenceSymbol 是一处谓词调用的语法身份：规范包名（"" = 同包标识符）+ 符号名。
type fenceSymbol struct {
	pkg string
	sel string
}

// fenceBodyFacts 是函数体的语法事实——门禁①与门禁②共用的唯一判据来源。
// 注释天然不在 AST 遍历里，字符串字面量单独收集（rawSQL 的 owner 列比较只在
// SQL 文本里可判）。
type fenceBodyFacts struct {
	calls       map[fenceSymbol]bool
	rawLiterals string
}

func fenceFacts(t *testing.T, src fenceSource, fn *ast.FuncDecl) fenceBodyFacts {
	t.Helper()
	imports := fenceImportsOf(t, src)
	facts := fenceBodyFacts{calls: make(map[fenceSymbol]bool)}
	var literals strings.Builder
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			facts.noteCall(imports, node.Fun)
		case *ast.BasicLit:
			if node.Kind != token.STRING {
				return true
			}
			if v, err := strconv.Unquote(node.Value); err == nil {
				literals.WriteString(v)
				literals.WriteByte('\n')
			}
		}
		return true
	})
	facts.rawLiterals = literals.String()
	return facts
}

func (f *fenceBodyFacts) noteCall(imports fenceImportTable, fun ast.Expr) {
	switch fn := fun.(type) {
	case *ast.Ident:
		// 同包函数（谓词拼装点）没有包限定符。
		if fn.Name == "accountOwnerPred" || fn.Name == "proxyOwnerPred" {
			f.calls[fenceSymbol{sel: fn.Name}] = true
			return
		}
		// 点导入：裸标识符就是那个包的导出符号。
		for pkg, syms := range fenceOwnerSymbols {
			if slices.Contains(syms, fn.Name) && imports.dottedPackage(pkg) {
				f.calls[fenceSymbol{pkg: pkg, sel: fn.Name}] = true
			}
		}
	case *ast.SelectorExpr:
		root, ok := fn.X.(*ast.Ident)
		if !ok {
			return
		}
		// 选择器根 = 指向 ent/account / ent/proxy / internal/domain 的本地名。
		pkg := imports.localPackage(root.Name)
		if pkg != "" && slices.Contains(fenceOwnerSymbols[pkg], fn.Sel.Name) {
			f.calls[fenceSymbol{pkg: pkg, sel: fn.Sel.Name}] = true
		}
	}
}

// consumesScope：作用域真的被消费（含点导入后的裸 AccountScopeFrom）。
func (f fenceBodyFacts) consumesScope() bool {
	return f.calls[fenceSymbol{pkg: "domain", sel: "AccountScopeFrom"}]
}

// kinds 是方法体命中的载体形态（门禁②的观察面）。
func (f fenceBodyFacts) kinds() []siteKind {
	var out []siteKind
	if f.calls[fenceSymbol{sel: "accountOwnerPred"}] || f.calls[fenceSymbol{sel: "proxyOwnerPred"}] {
		out = append(out, kindEntHelper)
	}
	if f.calls[fenceSymbol{pkg: "account", sel: "SupplierUserID"}] || f.calls[fenceSymbol{pkg: "proxy", sel: "OwnerUID"}] {
		out = append(out, kindEntDirect)
	}
	if f.hasRawSQLLiteral(fenceRawSQLLiteral) {
		out = append(out, kindRawSQL)
	}
	return out
}

// hasRawSQLLiteral 要求字面量里同时出现 owner 列比较与 accounts 表：供应商账务表里的
// 同名列（`supplier_balances` 等）是"查自己的账"，不是租户 fence（文件头边界④）。
func (f fenceBodyFacts) hasRawSQLLiteral(literal string) bool {
	return strings.Contains(f.rawLiterals, literal) && strings.Contains(f.rawLiterals, fenceRawSQLTable)
}

// hasWitness：站点声明的判别片段是否仍在该函数体里（门禁①）。
func (f fenceBodyFacts) hasWitness(s ownerSite) bool {
	if s.kind == kindRawSQL {
		return f.hasRawSQLLiteral(s.witness)
	}
	sym, ok := fenceWitnessSymbols[s.witness]
	return ok && f.calls[sym]
}

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
		if !fenceFacts(t, src, fn).hasWitness(s) {
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
//
// 识别按 import path 还原后的**包身份**，不按文本 token：别名与点导入都不会让站点
// 从扫描面上消失（裁决 I1）。
func fenceObservedSites(t *testing.T, sources []fenceSource) []fenceHit {
	t.Helper()
	var out []fenceHit
	for _, src := range sources {
		for _, decl := range src.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fenceHelperDefs[fn.Name.Name] {
				continue
			}
			facts := fenceFacts(t, src, fn)
			if !facts.consumesScope() {
				continue
			}
			kinds := facts.kinds()
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
		out = append(out, fmt.Sprintf("未登记的 owner 站点：%s——给这个方法补 ownerSite 行（形态 + witness）与 admin/owner/foreign 三态行为行；单行站点还要补 nonexistent 对照行", h.String()))
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
			if _, ok := fenceWitnessSymbols[s.witness]; !ok {
				out = append(out, fmt.Sprintf("站点 %s 的 witness %q 没有语法身份映射——门禁①无法在别名/点导入下还原它",
					s.id, s.witness))
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
		// 存在性契约二选一（只对单行站点）：靠对照行，或靠 leaksExistence + 理由。
		// 两边都空 ⇒ "越域与不存在不可区分"这条语义没有任何载体。
		switch {
		case s.leaksExistence && s.leakReason == "":
			out = append(out, fmt.Sprintf("站点 %s 声明 leaksExistence 但没写 leakReason", s.id))
		case s.leaksExistence && s.singleRow:
			out = append(out, fmt.Sprintf("站点 %s 既声明 leaksExistence 又标了单行站点——二者互斥", s.id))
		case s.leaksExistence && !s.singleRow:
			out = append(out, fmt.Sprintf("站点 %s 不是单行站点，leaksExistence 不适用", s.id))
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

// fenceAccountState 是**管理/本人行**用来回答「写落地了没有」的窄投影（存活 / 启用 /
// 失效 / 两个代际 / 绑定）。它**不是**「拒绝路径没写」的判据——投影外列（name /
// upstream_key / supplier_user_id / updated_at）的变化在投影里看不见（裁决 C2）。
// 拒绝行的判据是下面的整行快照。
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

// fenceProxyState 同上：管理/本人行的落地判据，不用于拒绝行。
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

// --- 整行快照（拒绝行的唯一「没被改」判据） ---
//
// 为什么不是投影：拒绝路径上的一次**无作用域**写可以只改投影外列（name / upstream_key /
// supplier_user_id / updated_at），投影相等会让整张表全绿（裁决 C2 的实测绕过）。
//
// 读面用**平台管理面**（context.Background()）：带作用域读回会把自己的目标读成不存在，
// 「没变」与「看不见」混为一谈。三个读面都是整实体读（GetAccount / GetProxy /
// GetAccountExt），没有只能投影读的方法，故不需要回落到原生 SQL。
//
// **不存在的行也是可比较的快照**（nil）：对照行打的就是不存在的 id，「写前没有、写后
// 仍然没有」必须与「存在且逐字段相同」一样被证明。

func (f *fenceFixture) accountRow(t *testing.T, id int64) *domain.Account {
	t.Helper()
	a, err := f.repos.Accounts.GetAccount(context.Background(), id)
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	require.NoErrorf(t, err, "整行回读必须成功：account id=%d", id)
	require.NotNil(t, a)
	return a
}

func (f *fenceFixture) proxyRow(t *testing.T, id int64) *domain.Proxy {
	t.Helper()
	p, err := f.repos.Proxies.GetProxy(context.Background(), id)
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	require.NoErrorf(t, err, "整行回读必须成功：proxy id=%d", id)
	return &p
}

// extRow 返回 nil = 无 ext 行（api-key/custom 模板账号的常态）。
func (f *fenceFixture) extRow(t *testing.T, accountID int64) *domain.AccountExt {
	t.Helper()
	e, err := f.repos.AccountExts.GetAccountExt(context.Background(), accountID)
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	require.NoErrorf(t, err, "整行回读必须成功：account_ext account_id=%d", accountID)
	return e
}

// extByKeyAdmin 用平台管理面按组合键读 ext 行（nil = 没有这一行）。
func (f *fenceFixture) extByKeyAdmin(t *testing.T, codexEmail, codexAccountID string) *domain.AccountExt {
	t.Helper()
	e, err := f.repos.AccountExts.FindAccountExtByCodexKey(
		context.Background(), codexEmail, codexAccountID, credential.TypeCodexPAT)
	if errors.Is(err, repository.ErrNotFound) {
		return nil
	}
	require.NoErrorf(t, err, "整行回读必须成功：codex key=%s/%s", codexEmail, codexAccountID)
	return e
}

// fenceAccountUnchanged 断言两次快照**整结构**相同（含 updated_at），返回比较结果。
// 用 require.Equal 而不是逐列：失败信息要能把「哪一列被写了」直接摆出来。
// require 失败即 FailNow，因此走到 return 就等价于「整结构相同」。
func fenceAccountUnchanged(t *testing.T, before, after *domain.Account) *bool {
	t.Helper()
	require.Equal(t, before, after, "账号整行必须逐字段不变（含 name/upstream_key/supplier_user_id）")
	if before != nil && after != nil {
		require.Equal(t, before.UpdatedAt, after.UpdatedAt, "updated_at 不得被写")
	}
	return fenceUnchanged(true)
}

func fenceProxyUnchanged(t *testing.T, before, after *domain.Proxy) *bool {
	t.Helper()
	require.Equal(t, before, after, "代理整行必须逐字段不变（含 name/remark/凭据/timestamps）")
	if before != nil && after != nil {
		require.Equal(t, before.UpdatedAt, after.UpdatedAt, "updated_at 不得被写")
	}
	return fenceUnchanged(true)
}

func fenceExtUnchanged(t *testing.T, before, after *domain.AccountExt) *bool {
	t.Helper()
	require.Equal(t, before, after, "account_ext 整行必须逐字段不变（含两枚凭据列与 identity）")
	return fenceUnchanged(true)
}

// fenceGroupsUnchanged 分组是 m2m 行而不是账号列，必须单独比（排序后）。
func fenceGroupsUnchanged(t *testing.T, before, after []int64) *bool {
	t.Helper()
	require.Equal(t, before, after, "账号的分组集合必须不变")
	return fenceUnchanged(true)
}

// fenceAllUnchanged 把多个比较结果合成一个：任一被改动 ⇒ false。
func fenceAllUnchanged(results ...*bool) *bool {
	ok := true
	for _, r := range results {
		if r == nil || !*r {
			ok = false
		}
	}
	return fenceUnchanged(ok)
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

// fenceAbsentID 是约定的**不存在 id**：克隆库的自增 id 从 1 起，这个值不可能被占用。
// 单行站点的 nonexistent 对照行全部打它。
const fenceAbsentID int64 = 987654321

// fenceRows 是行为表：行 = {站点, 作用域, 调用, 期望}。
//
// 每行都**自带前置状态**（需要新账号/新代理就在行内建），因此一条变异只会让被它打中的
// 站点变红，不会污染邻行（夹具行按固定顺序执行，但行与行之间不共享目标行）。
//
// 每个单行站点都有一条 label:"nonexistent"、compare:"foreign" 的对照行：同一个 ctx、
// 不同的目标（B 的不存在 id / 不存在的键）。它的职责只有一个——证明「越域」与
// 「不存在」给出同一可观测结论（裁决 C3）。
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
				before := f.accountRow(t, f.theirs.ID)
				got, err := f.repos.Accounts.GetAccount(ctx, f.theirs.ID)
				return fenceOutcome{
					err: err, present: got != nil,
					unchanged: fenceAccountUnchanged(t, before, f.accountRow(t, f.theirs.ID)),
				}
			},
		},
		{
			label: "nonexistent", site: "account_repo.GetAccount/ent", scope: asForeign, want: wantNotFound,
			wantErr: repository.ErrNotFound, compare: "foreign",
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				before := f.accountRow(t, fenceAbsentID)
				got, err := f.repos.Accounts.GetAccount(ctx, fenceAbsentID)
				return fenceOutcome{
					err: err, present: got != nil,
					unchanged: fenceAccountUnchanged(t, before, f.accountRow(t, fenceAbsentID)),
				}
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
				before := f.accountRow(t, f.theirs.ID)
				got, err := f.repos.Accounts.GetAccountWithTemplate(ctx, f.theirs.ID)
				return fenceOutcome{
					err: err, present: got != nil,
					unchanged: fenceAccountUnchanged(t, before, f.accountRow(t, f.theirs.ID)),
				}
			},
		},
		{
			label: "nonexistent", site: "account_repo.GetAccountWithTemplate/ent", scope: asForeign, want: wantNotFound,
			wantErr: repository.ErrNotFound, compare: "foreign",
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				before := f.accountRow(t, fenceAbsentID)
				got, err := f.repos.Accounts.GetAccountWithTemplate(ctx, fenceAbsentID)
				return fenceOutcome{
					err: err, present: got != nil,
					unchanged: fenceAccountUnchanged(t, before, f.accountRow(t, fenceAbsentID)),
				}
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
				beforeTheirs, beforeMine := f.accountRow(t, f.theirs.ID), f.accountRow(t, f.mine.ID)
				list, _, err := f.repos.Accounts.ListAccounts(ctx, repository.ListQuery{})
				ids := fenceAccountIDs(list)
				require.Contains(t, ids, f.mine.ID, "本人行必须仍在结果里（证明是行级过滤，不是空表）")
				return fenceOutcome{
					err: err, present: slices.Contains(ids, f.theirs.ID), count: len(list),
					unchanged: fenceAllUnchanged(
						fenceAccountUnchanged(t, beforeTheirs, f.accountRow(t, f.theirs.ID)),
						fenceAccountUnchanged(t, beforeMine, f.accountRow(t, f.mine.ID)),
					),
				}
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
				beforeAcc := f.accountRow(t, f.mineOnTheirProxy.ID)
				beforeProxy := f.proxyRow(t, f.theirProxy)
				list, _, err := f.repos.Accounts.ListAccounts(ctx, repository.ListQuery{})
				acc := fenceAccountOf(list, f.mineOnTheirProxy.ID)
				require.NotNil(t, acc, "账号行本身可见（越域只影响代理装配）")
				require.NotNil(t, acc.ProxyID, "绑定列不得被读路径改写")
				return fenceOutcome{
					err: err, present: acc != nil, attached: acc != nil && acc.Proxy != nil,
					unchanged: fenceAllUnchanged(
						fenceAccountUnchanged(t, beforeAcc, f.accountRow(t, f.mineOnTheirProxy.ID)),
						fenceProxyUnchanged(t, beforeProxy, f.proxyRow(t, f.theirProxy)),
					),
				}
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
				before := f.accountRow(t, acc.ID)
				err := f.repos.Accounts.DeleteAccount(ctx, acc.ID)
				return fenceOutcome{err: err, unchanged: fenceAccountUnchanged(t, before, f.accountRow(t, acc.ID))}
			},
		},
		{
			label: "nonexistent", site: "account_repo.DeleteAccount/ent", scope: asForeign, want: wantUnchanged,
			wantErr: repository.ErrNotFound, compare: "foreign",
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				before := f.accountRow(t, fenceAbsentID)
				err := f.repos.Accounts.DeleteAccount(ctx, fenceAbsentID)
				return fenceOutcome{err: err, unchanged: fenceAccountUnchanged(t, before, f.accountRow(t, fenceAbsentID))}
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
				beforeAcc, beforeGroups := f.accountRow(t, f.theirs.ID), f.accountGroups(t, f.theirs.ID)
				ids, err := f.repos.Accounts.GetAccountGroups(ctx, f.theirs.ID)
				return fenceOutcome{
					err: err, count: len(ids),
					unchanged: fenceAllUnchanged(
						fenceAccountUnchanged(t, beforeAcc, f.accountRow(t, f.theirs.ID)),
						fenceGroupsUnchanged(t, beforeGroups, f.accountGroups(t, f.theirs.ID)),
					),
				}
			},
		},
		{
			label: "nonexistent", site: "account_repo.GetAccountGroups/ent", scope: asForeign, want: wantEmptyList,
			compare: "foreign",
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				beforeAcc, beforeGroups := f.accountRow(t, fenceAbsentID), f.accountGroups(t, fenceAbsentID)
				ids, err := f.repos.Accounts.GetAccountGroups(ctx, fenceAbsentID)
				return fenceOutcome{
					err: err, count: len(ids),
					unchanged: fenceAllUnchanged(
						fenceAccountUnchanged(t, beforeAcc, f.accountRow(t, fenceAbsentID)),
						fenceGroupsUnchanged(t, beforeGroups, f.accountGroups(t, fenceAbsentID)),
					),
				}
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
				beforeAcc, beforeGroups := f.accountRow(t, f.theirs.ID), f.accountGroups(t, f.theirs.ID)
				err := f.repos.Accounts.SetAccountGroups(ctx, f.theirs.ID, []int64{f.otherGroup.ID})
				return fenceOutcome{
					err: err,
					unchanged: fenceAllUnchanged(
						fenceAccountUnchanged(t, beforeAcc, f.accountRow(t, f.theirs.ID)),
						fenceGroupsUnchanged(t, beforeGroups, f.accountGroups(t, f.theirs.ID)),
					),
				}
			},
		},
		{
			label: "nonexistent", site: "account_repo.SetAccountGroups/ent-direct", scope: asForeign, want: wantUnchanged,
			wantErr: repository.ErrNotFound, compare: "foreign",
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				beforeAcc, beforeGroups := f.accountRow(t, fenceAbsentID), f.accountGroups(t, fenceAbsentID)
				err := f.repos.Accounts.SetAccountGroups(ctx, fenceAbsentID, []int64{f.otherGroup.ID})
				return fenceOutcome{
					err: err,
					unchanged: fenceAllUnchanged(
						fenceAccountUnchanged(t, beforeAcc, f.accountRow(t, fenceAbsentID)),
						fenceGroupsUnchanged(t, beforeGroups, f.accountGroups(t, fenceAbsentID)),
					),
				}
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
				before := f.accountRow(t, f.theirs.ID)
				id, missing, err := f.repos.Accounts.FindMissingOwnedAccountID(ctx, []int64{f.mine.ID, f.theirs.ID})
				require.True(t, missing, "混入他方 id 必须报缺失")
				return fenceOutcome{
					err: err, missingID: id, targetID: f.theirs.ID,
					unchanged: fenceAccountUnchanged(t, before, f.accountRow(t, f.theirs.ID)),
				}
			},
		},
		{
			label: "nonexistent", site: "account_repo.FindMissingOwnedAccountID/ent", scope: asForeign, want: wantMissingID,
			compare: "foreign",
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				before := f.accountRow(t, fenceAbsentID)
				id, missing, err := f.repos.Accounts.FindMissingOwnedAccountID(ctx, []int64{f.mine.ID, fenceAbsentID})
				require.True(t, missing, "混入不存在的 id 必须报缺失")
				return fenceOutcome{
					err: err, missingID: id, targetID: fenceAbsentID,
					unchanged: fenceAccountUnchanged(t, before, f.accountRow(t, fenceAbsentID)),
				}
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
				before := f.accountRow(t, acc.ID)
				err := f.repos.Accounts.FailAccountCAS(ctx, acc.ID, before.IdentityRevision, "fence", now, "boom")
				return fenceOutcome{err: err, unchanged: fenceAccountUnchanged(t, before, f.accountRow(t, acc.ID))}
			},
		},
		{
			label: "nonexistent", site: "account_repo.FailAccountCAS/ent", scope: asForeign, want: wantUnchanged,
			wantErr: repository.ErrStaleIdentityRevision, compare: "foreign",
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				before := f.accountRow(t, fenceAbsentID)
				err := f.repos.Accounts.FailAccountCAS(ctx, fenceAbsentID, 1, "fence", now, "boom")
				return fenceOutcome{err: err, unchanged: fenceAccountUnchanged(t, before, f.accountRow(t, fenceAbsentID))}
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
				seeded := f.accountRow(t, acc.ID)
				require.NoError(t, f.repos.Accounts.FailAccountCAS(context.Background(), acc.ID, seeded.IdentityRevision, "fence", now, "boom"))
				before := f.accountRow(t, acc.ID)
				err := f.repos.Accounts.RecoverAccountCAS(ctx, acc.ID, before.LifecycleRevision)
				return fenceOutcome{err: err, unchanged: fenceAccountUnchanged(t, before, f.accountRow(t, acc.ID))}
			},
		},
		{
			label: "nonexistent", site: "account_repo.RecoverAccountCAS/ent", scope: asForeign, want: wantUnchanged,
			wantErr: repository.ErrStaleRevision, compare: "foreign",
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				before := f.accountRow(t, fenceAbsentID)
				err := f.repos.Accounts.RecoverAccountCAS(ctx, fenceAbsentID, 1)
				return fenceOutcome{err: err, unchanged: fenceAccountUnchanged(t, before, f.accountRow(t, fenceAbsentID))}
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
				beforeOwn, beforeForeign := f.accountRow(t, own.ID), f.accountRow(t, foreign.ID)
				err := f.repos.Accounts.DeleteAccountsBatch(ctx, []int64{own.ID, foreign.ID})
				return fenceOutcome{
					err: err,
					unchanged: fenceAllUnchanged(
						fenceAccountUnchanged(t, beforeOwn, f.accountRow(t, own.ID)),
						fenceAccountUnchanged(t, beforeForeign, f.accountRow(t, foreign.ID)),
					),
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
				before := f.accountRow(t, acc.ID)
				patch := repository.AccountPatch{Enabled: &disabled}
				_, err := f.repos.Accounts.UpdateAccountsBatch(ctx, []int64{acc.ID}, patch)
				return fenceOutcome{err: err, unchanged: fenceAccountUnchanged(t, before, f.accountRow(t, acc.ID))}
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
				beforeTheirs, beforeMine := f.proxyRow(t, f.theirProxy), f.proxyRow(t, f.mineProxy)
				list, err := f.repos.Proxies.ListProxies(ctx)
				ids := fenceProxyIDs(list)
				require.Contains(t, ids, f.mineProxy, "本人代理必须仍在结果里")
				return fenceOutcome{
					err: err, present: slices.Contains(ids, f.theirProxy), count: len(list),
					unchanged: fenceAllUnchanged(
						fenceProxyUnchanged(t, beforeTheirs, f.proxyRow(t, f.theirProxy)),
						fenceProxyUnchanged(t, beforeMine, f.proxyRow(t, f.mineProxy)),
					),
				}
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
				before := f.proxyRow(t, f.theirProxy)
				got, err := f.repos.Proxies.GetProxy(ctx, f.theirProxy)
				return fenceOutcome{
					err: err, present: got.ID != 0,
					unchanged: fenceProxyUnchanged(t, before, f.proxyRow(t, f.theirProxy)),
				}
			},
		},
		{
			label: "nonexistent", site: "proxy_repo.GetProxy/proxy", scope: asForeign, want: wantNotFound,
			wantErr: repository.ErrNotFound, compare: "foreign",
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				before := f.proxyRow(t, fenceAbsentID)
				got, err := f.repos.Proxies.GetProxy(ctx, fenceAbsentID)
				return fenceOutcome{
					err: err, present: got.ID != 0,
					unchanged: fenceProxyUnchanged(t, before, f.proxyRow(t, fenceAbsentID)),
				}
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
				before := f.proxyRow(t, f.theirProxy)
				cur, err := f.repos.Proxies.GetProxy(context.Background(), f.theirProxy)
				require.NoError(t, err)
				cur.Remark = "foreign-attempt"
				err = f.repos.Proxies.UpdateProxy(ctx, updateOf(cur, repository.ProxyCredential{}))
				return fenceOutcome{err: err, unchanged: fenceProxyUnchanged(t, before, f.proxyRow(t, f.theirProxy))}
			},
		},
		{
			label: "nonexistent", site: "proxy_repo.UpdateProxy/proxy", scope: asForeign, want: wantUnchanged,
			wantErr: repository.ErrNotFound, compare: "foreign",
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				before := f.proxyRow(t, fenceAbsentID)
				err := f.repos.Proxies.UpdateProxy(ctx, updateOf(domain.Proxy{
					ID: fenceAbsentID, Name: "fence-absent-update",
					Scheme: domain.ProxySchemeSOCKS5, Host: "127.0.0.1", Port: 1080,
				}, repository.ProxyCredential{}))
				return fenceOutcome{err: err, unchanged: fenceProxyUnchanged(t, before, f.proxyRow(t, fenceAbsentID))}
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
				before := f.proxyRow(t, id)
				err := f.repos.Proxies.DeleteProxy(ctx, id)
				return fenceOutcome{err: err, unchanged: fenceProxyUnchanged(t, before, f.proxyRow(t, id))}
			},
		},
		{
			label: "nonexistent", site: "proxy_repo.DeleteProxy/proxy", scope: asForeign, want: wantUnchanged,
			wantErr: repository.ErrNotFound, compare: "foreign",
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				before := f.proxyRow(t, fenceAbsentID)
				err := f.repos.Proxies.DeleteProxy(ctx, fenceAbsentID)
				return fenceOutcome{err: err, unchanged: fenceProxyUnchanged(t, before, f.proxyRow(t, fenceAbsentID))}
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
				beforeProxy, beforeAcc := f.proxyRow(t, pid), f.accountRow(t, bound.ID)
				err := f.repos.Proxies.DeleteProxy(ctx, pid)
				return fenceOutcome{
					err: err,
					unchanged: fenceAllUnchanged(
						fenceProxyUnchanged(t, beforeProxy, f.proxyRow(t, pid)),
						fenceAccountUnchanged(t, beforeAcc, f.accountRow(t, bound.ID)),
					),
				}
			},
		},
		{
			label: "nonexistent", site: "proxy_repo.DeleteProxy/unbind", scope: asForeign, want: wantUnchanged,
			wantErr: repository.ErrNotFound, compare: "foreign",
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				beforeProxy, beforeAcc := f.proxyRow(t, fenceAbsentID), f.accountRow(t, fenceAbsentID)
				err := f.repos.Proxies.DeleteProxy(ctx, fenceAbsentID)
				return fenceOutcome{
					err: err,
					unchanged: fenceAllUnchanged(
						fenceProxyUnchanged(t, beforeProxy, f.proxyRow(t, fenceAbsentID)),
						fenceAccountUnchanged(t, beforeAcc, f.accountRow(t, fenceAbsentID)),
					),
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
				before := f.proxyRow(t, f.theirProxy)
				err := f.repos.Proxies.WriteProxyProbe(ctx, f.theirProxy, now, false, "foreign-attempt", "")
				return fenceOutcome{err: err, unchanged: fenceProxyUnchanged(t, before, f.proxyRow(t, f.theirProxy))}
			},
		},
		{
			label: "nonexistent", site: "proxy_repo.WriteProxyProbe/proxy", scope: asForeign, want: wantUnchanged,
			wantErr: repository.ErrNotFound, compare: "foreign",
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				before := f.proxyRow(t, fenceAbsentID)
				err := f.repos.Proxies.WriteProxyProbe(ctx, fenceAbsentID, now, true, "", "203.0.113.9")
				return fenceOutcome{err: err, unchanged: fenceProxyUnchanged(t, before, f.proxyRow(t, fenceAbsentID))}
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
				beforeAcc := f.accountRow(t, f.theirs.ID)
				beforeExt := f.extByKeyAdmin(t, "fence-theirs@example.com", "fence-codex-theirs")
				ext, err := f.repos.AccountExts.FindAccountExtByCodexKey(ctx, "fence-theirs@example.com", "fence-codex-theirs", credential.TypeCodexPAT)
				return fenceOutcome{
					err: err, present: ext != nil,
					unchanged: fenceAllUnchanged(
						fenceAccountUnchanged(t, beforeAcc, f.accountRow(t, f.theirs.ID)),
						fenceExtUnchanged(t, beforeExt, f.extByKeyAdmin(t, "fence-theirs@example.com", "fence-codex-theirs")),
					),
				}
			},
		},
		{
			label: "nonexistent", site: "ext_codex_repo.accountExtQuery/ent", scope: asForeign, want: wantNotFound,
			wantErr: repository.ErrNotFound, compare: "foreign",
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				before := f.extByKeyAdmin(t, "fence-absent@example.com", "fence-codex-absent")
				ext, err := f.repos.AccountExts.FindAccountExtByCodexKey(ctx, "fence-absent@example.com", "fence-codex-absent", credential.TypeCodexPAT)
				return fenceOutcome{
					err: err, present: ext != nil,
					unchanged: fenceExtUnchanged(t, before, f.extByKeyAdmin(t, "fence-absent@example.com", "fence-codex-absent")),
				}
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
				beforeAcc, beforeExt := f.accountRow(t, acc.ID), f.extRow(t, acc.ID)
				err := f.repos.AccountExts.AdminWriteOAuthRotationCAS(ctx, acc.ID, beforeAcc.LifecycleRevision, "at2", "rt2", nil)
				return fenceOutcome{
					err: err,
					unchanged: fenceAllUnchanged(
						fenceAccountUnchanged(t, beforeAcc, f.accountRow(t, acc.ID)),
						fenceExtUnchanged(t, beforeExt, f.extRow(t, acc.ID)),
					),
				}
			},
		},
		{
			label: "nonexistent", site: "ext_codex_repo.accountOwnerUpdate/ent", scope: asForeign, want: wantUnchanged,
			wantErr: repository.ErrStaleRevision, compare: "foreign",
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				beforeAcc, beforeExt := f.accountRow(t, fenceAbsentID), f.extRow(t, fenceAbsentID)
				err := f.repos.AccountExts.AdminWriteOAuthRotationCAS(ctx, fenceAbsentID, 1, "at2", "rt2", nil)
				return fenceOutcome{
					err: err,
					unchanged: fenceAllUnchanged(
						fenceAccountUnchanged(t, beforeAcc, f.accountRow(t, fenceAbsentID)),
						fenceExtUnchanged(t, beforeExt, f.extRow(t, fenceAbsentID)),
					),
				}
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
				beforeAcc, beforeExt := f.accountRow(t, acc.ID), f.extRow(t, acc.ID)
				_, err := f.repos.AccountExts.UpsertAccountExt(ctx, fencePATExt(acc.ID, "k-foreign"))
				// 加锁读 0 行 ⇒ ErrNotFound：账号行不变，且事务没有建出 ext 行。
				return fenceOutcome{
					err: err, present: f.extRow(t, acc.ID) != nil,
					unchanged: fenceAllUnchanged(
						fenceAccountUnchanged(t, beforeAcc, f.accountRow(t, acc.ID)),
						fenceExtUnchanged(t, beforeExt, f.extRow(t, acc.ID)),
					),
				}
			},
		},
		{
			label: "nonexistent", site: "ext_codex_repo.lockOwnedAccountForUpdate/sql", scope: asForeign, want: wantNotFound,
			wantErr: repository.ErrNotFound, compare: "foreign",
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				beforeAcc, beforeExt := f.accountRow(t, fenceAbsentID), f.extRow(t, fenceAbsentID)
				_, err := f.repos.AccountExts.UpsertAccountExt(ctx, fencePATExt(fenceAbsentID, "k-absent"))
				return fenceOutcome{
					err: err, present: f.extRow(t, fenceAbsentID) != nil,
					unchanged: fenceAllUnchanged(
						fenceAccountUnchanged(t, beforeAcc, f.accountRow(t, fenceAbsentID)),
						fenceExtUnchanged(t, beforeExt, f.extRow(t, fenceAbsentID)),
					),
				}
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
				beforeAcc, beforeExt := f.accountRow(t, f.theirs.ID), f.extRow(t, f.theirs.ID)
				ext, err := f.repos.AccountExts.GetOwnedAccountExt(ctx, f.theirs.ID)
				return fenceOutcome{
					err: err, present: ext != nil,
					unchanged: fenceAllUnchanged(
						fenceAccountUnchanged(t, beforeAcc, f.accountRow(t, f.theirs.ID)),
						fenceExtUnchanged(t, beforeExt, f.extRow(t, f.theirs.ID)),
					),
				}
			},
		},
		{
			label: "nonexistent", site: "ext_codex_repo.GetOwnedAccountExt/ent", scope: asForeign, want: wantNotFound,
			wantErr: repository.ErrNotFound, compare: "foreign",
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				beforeAcc, beforeExt := f.accountRow(t, fenceAbsentID), f.extRow(t, fenceAbsentID)
				ext, err := f.repos.AccountExts.GetOwnedAccountExt(ctx, fenceAbsentID)
				return fenceOutcome{
					err: err, present: ext != nil,
					unchanged: fenceAllUnchanged(
						fenceAccountUnchanged(t, beforeAcc, f.accountRow(t, fenceAbsentID)),
						fenceExtUnchanged(t, beforeExt, f.extRow(t, fenceAbsentID)),
					),
				}
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
				beforeAcc, beforeExt := f.accountRow(t, f.theirs.ID), f.extRow(t, f.theirs.ID)
				cred, err := f.repos.AccountExts.GetOwnedAccountCredential(ctx, f.theirs.ID)
				return fenceOutcome{
					err: err, present: cred != nil,
					unchanged: fenceAllUnchanged(
						fenceAccountUnchanged(t, beforeAcc, f.accountRow(t, f.theirs.ID)),
						fenceExtUnchanged(t, beforeExt, f.extRow(t, f.theirs.ID)),
					),
				}
			},
		},
		{
			label: "nonexistent", site: "ext_codex_repo.GetOwnedAccountCredential/sql", scope: asForeign, want: wantNotFound,
			wantErr: repository.ErrNotFound, compare: "foreign",
			run: func(t *testing.T, f *fenceFixture, ctx context.Context) fenceOutcome {
				beforeAcc, beforeExt := f.accountRow(t, f.theirs.ID), f.extRow(t, f.theirs.ID)
				cred, err := f.repos.AccountExts.GetOwnedAccountCredential(ctx, fenceAbsentID)
				return fenceOutcome{
					err: err, present: cred != nil,
					unchanged: fenceAllUnchanged(
						fenceAccountUnchanged(t, beforeAcc, f.accountRow(t, f.theirs.ID)),
						fenceExtUnchanged(t, beforeExt, f.extRow(t, f.theirs.ID)),
					),
				}
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

import (
	"context"

	"github.com/is7qin/c3api/internal/domain"
)

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

	t.Run("别名/点导入不改变 witness 判定", func(t *testing.T) {
		// 谓词的书写名可以被 import 别名改掉；语法身份不能。
		aliased := fenceParseInjected(t, "account_repo.go", `package repository

import (
	"context"

	dom "github.com/is7qin/c3api/internal/domain"
)

func GetAccount(ctx context.Context, id int64) error {
	_ = accountOwnerPred(dom.AccountScopeFrom(ctx))
	return nil
}
`)
		require.Empty(t, fenceWitnessViolations(t, []fenceSource{aliased}, ownerSites[:1]),
			"别名导入不得让 witness 误报")

		// 点导入后连选择器都没有：裸标识符必须还原成 ent/account 的 SupplierUserID。
		dotted := fenceParseInjected(t, "proxy_repo.go", `package repository

import (
	"context"

	. "github.com/is7qin/c3api/internal/ent/account"
)

func OwnedBy(ctx context.Context, uid int64) error {
	_ = SupplierUserID(uid)
	return nil
}
`)
		sites := []ownerSite{{
			id: "account_repo.GetAccount/ent", file: "proxy_repo.go", method: "OwnedBy",
			kind: kindEntDirect, witness: "account.SupplierUserID(", entry: "OwnedBy", want: wantNotFound,
		}}
		require.Empty(t, fenceWitnessViolations(t, []fenceSource{dotted}, sites),
			"点导入后的裸标识符必须被还原成 account.SupplierUserID")
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

import (
	"context"

	"github.com/is7qin/c3api/internal/domain"
)

func GetAccount(ctx context.Context, id int64) error {
	_ = accountOwnerPred(domain.AccountScopeFrom(ctx))
	return nil
}
`)
		got := fenceUndeclaredViolations(fenceObservedSites(t, []fenceSource{injected}), ownerSites)
		require.Empty(t, got, "已登记方法不得被判违规：%v", got)
	})

	t.Run("import 别名不丢站点", func(t *testing.T) {
		// 裁决 I1 的绕过：把 domain 导入成 dom 后，文本 token 扫描的命中数腰斩。
		// 判据按 import path 还原，因此这个形状必须与直写形态完全等价。
		injected := fenceParseInjected(t, "account_repo.go", `package repository

import (
	"context"

	dom "github.com/is7qin/c3api/internal/domain"
	accountpkg "github.com/is7qin/c3api/internal/ent/account"
)

func (r *AccountRepo) SneakyAlias(ctx context.Context, id int64) error {
	scope := dom.AccountScopeFrom(ctx)
	return r.client.Account.Update().Where(accountpkg.SupplierUserID(scope.OwnerUID)).Exec(ctx)
}
`)
		got := fenceUndeclaredViolations(fenceObservedSites(t, []fenceSource{injected}), ownerSites)
		require.Len(t, got, 1, "别名不得让新站点漏报：%v", got)
		require.Contains(t, got[0], "SneakyAlias")
		require.Contains(t, got[0], "entDirect")
	})

	t.Run("点导入不丢站点", func(t *testing.T) {
		// 点导入后既没有选择器也没有本地名：裸标识符必须还原到对应包。
		injected := fenceParseInjected(t, "account_repo.go", `package repository

import (
	"context"

	. "github.com/is7qin/c3api/internal/domain"
	. "github.com/is7qin/c3api/internal/ent/account"
)

func (r *AccountRepo) SneakyDot(ctx context.Context, id int64) error {
	scope := AccountScopeFrom(ctx)
	return r.client.Account.Update().Where(SupplierUserID(scope.OwnerUID)).Exec(ctx)
}
`)
		got := fenceUndeclaredViolations(fenceObservedSites(t, []fenceSource{injected}), ownerSites)
		require.Len(t, got, 1, "点导入不得让新站点漏报：%v", got)
		require.Contains(t, got[0], "SneakyDot")
		require.Contains(t, got[0], "entDirect")
	})

	t.Run("直写谓词的新方法", func(t *testing.T) {
		injected := fenceParseInjected(t, "account_repo.go", `package repository

import (
	"context"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent/account"
)

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

import (
	"context"

	domainalias "github.com/is7qin/c3api/internal/domain"
)

func (r *UsageRepo) SneakySQL(ctx context.Context, id int64) error {
	q := "UPDATE accounts SET enabled = false WHERE id = $1"
	if s := domainalias.AccountScopeFrom(ctx); s.Set {
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

import (
	"context"

	"github.com/is7qin/c3api/internal/domain"
)

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

import (
	"context"

	"github.com/is7qin/c3api/internal/ent/account"
)

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

	// 存在性对照的**结构**约束：单行站点必须有且只有一条对照行，非单行站点一条也不许有。
	// 结构先红，行为表才不会被一条漏写的对照行悄悄架空。
	compareRows := make(map[string]int, len(rows))
	for _, r := range rows {
		if r.compare != "" {
			require.Equal(t, "foreign", r.compare, "对照行的基准只允许是 foreign：%s/%s", r.site, r.label)
			require.Equal(t, "nonexistent", r.label, "对照行必须打 nonexistent 标签：%s/%s", r.site, r.label)
			require.True(t, seenRow[r.site+"/"+r.compare], "站点 %s 的对照行缺少基准行 foreign", r.site)
			compareRows[r.site]++
		}
	}
	for _, site := range ownerSites {
		want := 0
		if site.singleRow && !site.leaksExistence {
			want = 1
		}
		require.Equal(t, want, compareRows[site.id],
			"站点 %s 的 nonexistent 对照行数量必须是 %d（单行站点证明「越域与不存在同码」，非单行站点没有这种形态）", site.id, want)
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

			outcomes := make(map[string]fenceOutcome, len(mine))
			for _, r := range mine {
				t.Run(r.label, func(t *testing.T) {
					got := r.run(t, f, f.ctxOf(r.scope))
					if r.wantErr != nil {
						require.ErrorIs(t, got.err, r.wantErr, "%s/%s 的错误哨兵必须精确", site.id, r.label)
					}
					requireWant(t, r.want, got)
					outcomes[r.label] = got
				})
			}
			if t.Failed() {
				// 已有红点（含对照行缺基准）：不再叠加一条级联失败，保留单一可归因的输出。
				return
			}

			// 存在性不可区分：nonexistent 与 foreign 的可观测结论逐项相同。错误按哨兵归类
			// （不比文本——文本里带目标 id），空结果形态按 present/count/total/attached，
			// 存在性探测按「是否点名了本行的 targetID」归一。
			for _, r := range mine {
				if r.compare == "" {
					continue
				}
				base, ok := outcomes[r.compare]
				require.Truef(t, ok, "站点 %s 的对照行 %q 缺少基准行 %q 的观测", site.id, r.label, r.compare)
				got, ok := outcomes[r.label]
				require.Truef(t, ok, "站点 %s 的对照行 %q 没有观测结果", site.id, r.label)
				require.Equal(t, fenceObservableOf(base), fenceObservableOf(got),
					"站点 %s：越域（%s）与不存在（%s）必须给出同一可观测结论（不泄漏存在性）",
					site.id, r.compare, r.label)
			}

			// 契约行：**恰好一条**拒绝行（非 admin/owner 作用域）承载站点声明的单值结论。
			// 0 条 ⇒ 期望被改成了别的东西（M6 的形状）；≥2 条 ⇒ 出现"两种结论都算过"的
			// 二选一，正是本规格要禁的东西。对照行有自己的职责（存在性同码），不冒充契约。
			var contract []fenceRow
			for _, r := range mine {
				if r.compare == "" && r.scope != asAdmin && r.scope != asOwner && r.want == site.want {
					contract = append(contract, r)
				}
			}
			require.Len(t, contract, 1,
				"站点 %s 声明的契约 %q 必须恰好被一条拒绝行承载（当前 %d 条）", site.id, site.want, len(contract))
		})
	}
}
