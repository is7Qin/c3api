// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package credential

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件的两种锁，形状刻意不同（golden 只证明「表 == 手抄副本」，不是「表 == 现实」）：
//
//	4.1 TestTypeBehaviorFrozen —— 经**方法**读（调用方真正消费的面），期望值手抄
//	4.4 TestCredentialPkgOwnsNoTypePredicateHelper —— **结构**门禁（反向：不许在包内
//	    新开按类型判定的入口）
//
// credential_test.go 的 TestBPSProviderCapabilities / TestTypeValid 仍保留：它们钉的是
// 单类型方法与非法输入，与这里的全集合矩阵不重复。

type wantBehavior struct {
	valid, validTplExt, validAcctExt bool
	isCodex, isBPS, isRespProvider   bool
	usesStaticKey                    bool
	// supportsContinuation 是**能力**轴（能不能用 previous_response_id 续接），
	// 与上面的 isCodex/isBPS（通道轴）分开手抄：两列今天取值一致，而 golden 必须
	// 能分开漂移，否则「能力 = 通道」这个错误假设没有任何一格钉得住。
	supportsContinuation bool
	group                []Type
}

// typeBehaviorGolden 是 6 个类型的 12 项行为 + 身份组的手抄矩阵，也是本包**唯一的
// 跨类型锁**：表里加/删行而漏改这里 → 红。它提升成包级变量（不是留在用例里）是为了
// 让 TestCapsTablesSplitByAxis 能拿它做**键集合**断言：拆表后的类型集合必须与这份
// golden 恰好相同，而不是「表自己和自己相等」。
var typeBehaviorGolden = map[Type]wantBehavior{
	TypeAPIKey: {
		valid: true, validTplExt: false, validAcctExt: false,
		isCodex: false, isBPS: false, isRespProvider: false,
		usesStaticKey: true, supportsContinuation: false,
		group: []Type{TypeAPIKey},
	},
	TypeResponsesSpecial: {
		valid: true, validTplExt: true, validAcctExt: false,
		isCodex: false, isBPS: false, isRespProvider: false,
		usesStaticKey: true, supportsContinuation: false,
		group: []Type{TypeResponsesSpecial},
	},
	TypeCustom: {
		valid: true, validTplExt: true, validAcctExt: false,
		isCodex: false, isBPS: false, isRespProvider: false,
		usesStaticKey: true, supportsContinuation: false,
		group: []Type{TypeCustom},
	},
	TypeCodexOAuth: {
		valid: true, validTplExt: true, validAcctExt: true,
		isCodex: true, isBPS: false, isRespProvider: true,
		usesStaticKey: false, supportsContinuation: true,
		group: []Type{TypeCodexOAuth, TypeCodexPAT},
	},
	TypeCodexPAT: {
		valid: true, validTplExt: true, validAcctExt: true,
		isCodex: true, isBPS: false, isRespProvider: true,
		usesStaticKey: false, supportsContinuation: true,
		group: []Type{TypeCodexOAuth, TypeCodexPAT},
	},
	TypeBPS: {
		valid: true, validTplExt: true, validAcctExt: true,
		isCodex: false, isBPS: true, isRespProvider: true,
		usesStaticKey: false, supportsContinuation: false,
		group: []Type{TypeBPS},
	},
}

// TestTypeBehaviorFrozen 冻结 6 个类型的 12 项行为 + 身份组。
//
// 为什么断言方法而不是表本身：今天比较表，明天表和 golden 一起改就全绿——
// 而且那样锁的是实现面，不是调用方消费面。改表若不同步改这里，diff 会落在两种形状上
// （表 vs 方法期望），review 才看得见。
func TestTypeBehaviorFrozen(t *testing.T) {
	want := typeBehaviorGolden
	// 表键集合与 golden 类型集合绑定（拆表后仍绑 typeCapsTable：类型集合只在那张表里）：
	// 加/删表行而漏改这里 → 红（漏掉一行不再等于静默走零值）。
	require.Len(t, typeCapsTable, len(want), "typeCapsTable 与 golden 的类型集合必须一致")

	for typ, w := range want {
		require.Equal(t, w.valid, typ.Valid(), "%q.Valid", typ)
		require.Equal(t, w.validTplExt, typ.ValidTemplateExt(), "%q.ValidTemplateExt", typ)
		require.Equal(t, w.validAcctExt, typ.ValidAccountExt(), "%q.ValidAccountExt", typ)
		require.Equal(t, w.isCodex, typ.IsCodex(), "%q.IsCodex", typ)
		require.Equal(t, w.isBPS, typ.IsBPS(), "%q.IsBPS", typ)
		require.Equal(t, w.isRespProvider, typ.IsResponsesProvider(), "%q.IsResponsesProvider", typ)
		require.Equal(t, w.usesStaticKey, typ.UsesStaticKey(), "%q.UsesStaticKey", typ)
		require.Equal(t, w.supportsContinuation, typ.SupportsContinuation(), "%q.SupportsContinuation", typ)
		require.Equal(t, w.group, typ.IdentityGroup(), "%q.IdentityGroup：身份组决定账号查重作用域，不能按名称猜", typ)
	}
}

// TestContinuationCapabilityIsNotChannel 钉住「能力 ≠ 通道」：表内一个**未来类型**
// 的替身——通道是 codex、但 supportsContinuation=false。
//
// 为什么需要它：今天 codex 通道恰好全都支持续接，于是
// `SupportsContinuation() ≡ ChannelOf(t)==ChannelCodex` 这个等价式在**每一个**真实
// 类型上成立，任何把它写成通道投影的实现都会全绿通过。这条测试把两列拆开，等价式
// 当场失效——调度层的计划判定依赖的正是这个分叉（它读能力位，不读通道）。
//
// 注册方式：typeCapsTable 是包私有的，所以替身只能在这里临时插入并在 Cleanup 里删除；
// 表键集合的相等断言（TestTypeBehaviorFrozen / TestChannelFrozen）在删除后照旧成立
// （本包不并行跑用例，见 AGENTS.md）。
//
// 替身按拆分后的形状构造：通道（身份）在 typeCapsTable 的 ch 列，能力位单独给 false——
// 这正是「能力位留在 typeCapsTable」的现场证明：若把它挪进 channelCapsTable，本夹具
// 的 codex 通道将无法与「不支持续接」共存，测试当场写不出来。
func TestContinuationCapabilityIsNotChannel(t *testing.T) {
	const future Type = "codex-future-no-continuation"
	typeCapsTable[future] = typeCaps{ch: ChannelCodex, validTplExt: true, supportsContinuation: false}
	t.Cleanup(func() { delete(typeCapsTable, future) })

	require.Equal(t, ChannelCodex, ChannelOf(future),
		"夹具的通道必须是 codex——否则这条测试证明不了两条轴能分开")
	require.True(t, future.Valid())
	require.True(t, future.IsCodex(), "通道是 codex ⇒ 通道投影 IsCodex 必须为真")
	require.True(t, future.IsResponsesProvider())
	require.False(t, future.SupportsContinuation(),
		"codex 通道 + supportsContinuation=false 必须共存：能力位不是通道投影")

	// 反向对照：真实 codex 类型的能力位为真——上面那条 false 不是「表外零值兜底」。
	require.True(t, TypeCodexOAuth.SupportsContinuation())
	require.True(t, TypeCodexPAT.SupportsContinuation())
	for _, typ := range []Type{TypeAPIKey, TypeResponsesSpecial, TypeCustom, TypeBPS} {
		require.False(t, typ.SupportsContinuation(), "%q：今天只有 codex 族支持续接", typ)
	}
}

// TestKnownTypesMirrorsCapsTable KnownTypes 是"全类型枚举"的对外入口，跨包的跨表
// 完整性断言（domain.TestEveryKnownTypeHasFormatPolicyRow）靠它。它必须与 caps 表
// **恰好**同集合、规范序、且是副本（调用方改不动权威表）。
func TestKnownTypesMirrorsCapsTable(t *testing.T) {
	got := KnownTypes()
	require.Len(t, got, len(typeCapsTable), "KnownTypes 必须与 typeCapsTable 同大小")
	require.True(t, sort.SliceIsSorted(got, func(i, j int) bool { return got[i] < got[j] }),
		"KnownTypes 必须规范序（字典序）：%v", got)

	seen := make(map[Type]bool, len(got))
	for _, typ := range got {
		require.False(t, seen[typ], "KnownTypes 不得有重复项：%q", typ)
		seen[typ] = true
		require.True(t, typ.Valid(), "%q 来自表内 ⇒ Valid() 必真", typ)
	}
	for typ := range typeCapsTable {
		require.True(t, seen[typ], "typeCapsTable 里的 %q 必须出现在 KnownTypes", typ)
	}

	// 副本语义：改写返回切片不影响下一次调用。
	first := KnownTypes()
	require.NotEmpty(t, first)
	authoritative := first[0]
	first[0] = Type("tampered")
	second := KnownTypes()
	require.Equal(t, authoritative, second[0], "KnownTypes 必须返回副本")
}

// TestChannelFrozen 独立 golden：期望表是**字面量**，不许用 IsCodex/IsBPS 反推——
// 那会变成同源重言（表整体错时两边一起错，测试照绿）。
func TestChannelFrozen(t *testing.T) {
	want := map[Type]Channel{
		TypeAPIKey:           ChannelStaticKey,
		TypeResponsesSpecial: ChannelStaticKey,
		TypeCustom:           ChannelStaticKey,
		TypeCodexOAuth:       ChannelCodex,
		TypeCodexPAT:         ChannelCodex,
		TypeBPS:              ChannelBPS,
	}
	require.Len(t, typeCapsTable, len(want), "typeCapsTable 与 Channel golden 的类型集合必须一致")
	for typ, ch := range want {
		require.Equal(t, ch, ChannelOf(typ), "%q 的执行通道漂移", typ)
	}
}

// TestChannelOfInvalidType 表外（非法）类型落到 ChannelStaticKey——与原先
// if/else-if/else 的 else 分支严格等价，而且**不是 fallback**：通道是零值，但
// Valid() 必须拒绝，credentialFor 会在取凭据前显式报错，不会拿静态 key 路径把非法
// 类型洗白。
func TestChannelOfInvalidType(t *testing.T) {
	for _, typ := range []Type{"", "bogus", "codex_oauth"} {
		require.Equal(t, ChannelStaticKey, ChannelOf(typ), "%q 不在表内 → 零值通道", typ)
		require.False(t, typ.Valid(), "%q 必须被 Valid() 拒绝", typ)
	}
}

// TestCapsTablesSplitByAxis 钉住「拆表」本身：哪一列属于哪条轴、两张表的键集合恰好是什么。
//
// 断言的**不是** AST 形状，而是类型系统 + 运行时集合：
//   - channelCapsTable 的键类型是 Channel ⇒ `map[Type]channelCaps` 根本编译不过，
//     「把类型塞进通道表」在编译期已被排除；
//   - 运行时键集合必须恰好是三个合法 Channel（空表或漏一行都红）；
//   - 每个类型的 ch 都必须在通道表里有行——这是拆表**新引入**的缺口：新通道只加常量、
//     不加通道行，ValidAccountExt 会静默恒 false（零值），而不是报错；
//   - typeCapsTable 的键集合必须恰好等于 golden 的类型集合（加/删类型漏改 golden 即红）。
func TestCapsTablesSplitByAxis(t *testing.T) {
	// ① 键类型（类型系统层面的事实，运行时再复述一次，防「顺手换成 map[string]」）。
	require.Equal(t, reflect.TypeOf(Channel(0)), reflect.TypeOf(channelCapsTable).Key(),
		"channelCapsTable 的键必须是 Channel：键就是通道，Type 不能进来")
	require.Equal(t, reflect.TypeOf(channelCaps{}), reflect.TypeOf(channelCapsTable).Elem())
	require.Equal(t, reflect.TypeOf(Type("")), reflect.TypeOf(typeCapsTable).Key(),
		"typeCapsTable 的键必须是 Type：逐类型声明的只有类型级列")

	// ② 通道表键集合 == 三个合法 Channel，且非空。
	wantChannels := []Channel{ChannelStaticKey, ChannelCodex, ChannelBPS}
	require.NotEmpty(t, channelCapsTable, "channelCapsTable 为空：ValidAccountExt 会对全类型恒 false")
	require.Len(t, channelCapsTable, len(wantChannels), "channelCapsTable 的键必须恰好是三个合法 Channel")
	for _, ch := range wantChannels {
		_, ok := channelCapsTable[ch]
		require.True(t, ok, "channelCapsTable 缺 %d 这一行", ch)
	}

	// ③ 每个类型的通道都有通道行（拆表后新增的静默缺口）。
	for typ, c := range typeCapsTable {
		_, ok := channelCapsTable[c.ch]
		require.True(t, ok,
			"%q 的通道 %d 在 channelCapsTable 里没有行：ValidAccountExt 会静默走零值 false", typ, c.ch)
	}

	// ④ 类型表键集合 == golden 类型集合（逐键双向断言，不是只比长度——长度相等的一对
	// 增删仍然会漏）。
	require.Len(t, typeCapsTable, len(typeBehaviorGolden), "typeCapsTable 与 golden 的类型集合必须一致")
	for typ := range typeBehaviorGolden {
		_, ok := typeCapsTable[typ]
		require.True(t, ok, "%q 在 golden 里但不在 typeCapsTable 里", typ)
	}
}

// 这里曾有一条 TestCapsColumnInvariants（对 capsTable 求「跨列不变量」）。caps 塌到
// 2 列（ch + validTplExt）后它不可重建：其余 4 个能力位已不存在，而剩下的两列之间
// **没有**函数关系——api_key（validTplExt=false）与 responses-special / custom
// （true）的 ch 同为 ChannelStaticKey。任何「ch ⇔ validTplExt」的断言都是错的，
// 所以不接受「保留仍成立的部分」这种空话，整条删除。
//
// S3b 拆两表后，这条的替代品**不是**又一条跨列不变量，而是结构：validAcctExt 已搬进
// `map[Channel]channelCaps`，「通道是 static、却声明账号 ext 可用」在类型上不可表达
// （见 TestCapsTablesSplitByAxis）；剩下三列（ch / validTplExt /
// supportsContinuation）之间仍然没有函数关系，不复活那条断言。
//
// 代价与接受理由：**golden 是唯一的跨类型锁**——表与 golden 被同步改错时不再有第二
// 把锁。补偿它的是 TestPredicatesOnUnknownType：那条锁的是表外语义（golden 不覆盖
// 的维度），恰是通道派生最容易翻车的地方。

// TestPredicatesOnUnknownType 表外类型（非法/未注册）上，**8 个布尔谓词全部必须
// false**——逐谓词显式枚举，不写「全部谓词」的模糊断言。
//
// 为什么单独锁：拆表后通道轴谓词仍全部由 typeCapsTable 的 ch 派生，而 ChannelOf 的零值
// 正是 ChannelStaticKey（它在 channelCapsTable 里有一行真实记录，不是「查不到」）。
// 最容易的错法是把 ok 守卫丢掉、写成 ChannelOf(t) == ChannelStaticKey——那会让
// UsesStaticKey() 对非法类型从 false 翻成 true；而 service/account.go:45,221 拿它决定
// upstream_key 是否必填，**没有** Valid() 前置守卫，是真实的行为变化。golden 只覆盖
// 表内 6 个类型，抓不到这一类。
func TestPredicatesOnUnknownType(t *testing.T) {
	predicates := []struct {
		name string
		fn   func(Type) bool
	}{
		{"Valid", Type.Valid},
		{"ValidTemplateExt", Type.ValidTemplateExt},
		{"ValidAccountExt", Type.ValidAccountExt},
		{"IsCodex", Type.IsCodex},
		{"IsBPS", Type.IsBPS},
		{"IsResponsesProvider", Type.IsResponsesProvider},
		{"UsesStaticKey", Type.UsesStaticKey},
		{"SupportsContinuation", Type.SupportsContinuation},
	}
	for _, unknown := range []Type{"", "bogus", "codex_oauth"} {
		for _, p := range predicates {
			require.False(t, p.fn(unknown), "%q.%s：表外类型必须 false（ok 守卫）", unknown, p.name)
		}
	}
}

// TestIdentityGroupOnUnknownType IdentityGroup**不是**布尔谓词，单独一档：表外类型
// 只与自己同组，不许被并进 Codex 组或 BPS 组（那会让非法类型的账号去重作用域命中
// 真实账号）。
func TestIdentityGroupOnUnknownType(t *testing.T) {
	require.Equal(t, []Type{Type("")}, Type("").IdentityGroup())
	require.Equal(t, []Type{Type("bogus")}, Type("bogus").IdentityGroup())
	require.Equal(t, []Type{Type("codex_oauth")}, Type("codex_oauth").IdentityGroup())
}

// typePredicateMethods 是本轮保留签名的三个谓词（84 处调用点零改动）。它们是**只读
// 投影**，不许成为新判定的样板。
var typePredicateMethods = map[string]bool{
	"IsCodex":             true,
	"IsBPS":               true,
	"IsResponsesProvider": true,
}

// typeNameFragments 出现在函数名里即视为「按类型判定的新入口」。
var typeNameFragments = []string{"Codex", "BPS", "ResponsesProvider"}

// TestCredentialPkgOwnsNoTypePredicateHelper 反向门禁：堵「在 credential 包加个
// helper，让 proxy 合法地按类型分支」这条路——那会让 proxy 的单通道收口形同虚设，
// 只是把散落的分支搬了个家。
//
// 规则：① 方法：除三个既有谓词外不得再有 Is* 形态的新谓词；② 方法名与包级导出函数名
// 都不得含 Codex / BPS / ResponsesProvider。
//
// 已知残余（名字型门禁的固有边界，靠 review 兜底，不假装已覆盖）：
//   - 换名：如 `NeedsSDKEntry`，既无 `Is` 前缀也不含三段名，本门禁放行；
//   - 挪包：把 helper 放到别的包（如 `internal/domain`），本门禁只扫本包，放行。
func TestCredentialPkgOwnsNoTypePredicateHelper(t *testing.T) {
	names := credentialSourceFiles(t)
	hit := func(name string) (string, bool) {
		for _, frag := range typeNameFragments {
			if strings.Contains(name, frag) {
				return frag, true
			}
		}
		return "", false
	}
	for _, name := range names {
		fset := token.NewFileSet()
		src, err := os.ReadFile(name)
		require.NoError(t, err)
		af, err := parser.ParseFile(fset, name, src, 0)
		require.NoError(t, err, "解析 %s", name)
		for _, decl := range af.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if fn.Recv != nil {
				if typePredicateMethods[fn.Name.Name] {
					continue
				}
				require.False(t, strings.HasPrefix(fn.Name.Name, "Is"),
					"%s：新增谓词方法 %s——类型的形态只由 typeCapsTable/channelCapsTable 投影，不要在 credential 包再开按类型判定的入口",
					fset.Position(fn.Pos()), fn.Name.Name)
				frag, found := hit(fn.Name.Name)
				require.False(t, found, "%s：方法名 %s 含 %q——同上的「按类型分支」入口", fset.Position(fn.Pos()), fn.Name.Name, frag)
				continue
			}
			if !fn.Name.IsExported() {
				continue
			}
			frag, found := hit(fn.Name.Name)
			require.False(t, found, "%s：包级导出函数 %s 含 %q——proxy 会拿它绕过 ChannelOf，单通道收口即失效",
				fset.Position(fn.Pos()), fn.Name.Name, frag)
		}
	}
}

// credentialSourceFiles 列出 internal/credential 全部**非测试** `.go`（排序；扫面为空
// 即失败——空转的门禁比没有门禁更危险）。
func credentialSourceFiles(t *testing.T) []string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	require.True(t, ok)
	entries, err := os.ReadDir(filepath.Dir(self))
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		names = append(names, filepath.Join(filepath.Dir(self), name))
	}
	sort.Strings(names)
	require.NotEmpty(t, names, "internal/credential 非测试源文件为空：扫面在空转")
	return names
}
