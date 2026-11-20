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
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 本文件的三种锁，形状刻意不同（golden 只证明「表 == 手抄副本」，不是「表 == 现实」）：
//
//	4.1  TestTypeBehaviorFrozen      —— 经**方法**读（调用方真正消费的面），期望值手抄
//	4.1b TestCapsColumnInvariants    —— 表内**跨列不变量**，不依赖任何副本
//	4.4  TestCredentialPkgOwnsNoTypePredicateHelper —— **结构**门禁（反向：不许在包内
//	     新开按类型判定的入口）
//
// credential_test.go 的 TestBPSProviderCapabilities / TestTypeValid 仍保留：它们钉的是
// 单类型方法与非法输入，与这里的全集合矩阵不重复。

type wantBehavior struct {
	valid, validTplExt, validAcctExt bool
	isCodex, isBPS, isRespProvider   bool
	usesAcctExt, usesStaticKey       bool
	fixedEndpoint, respFmt           bool
	group                            []Type
}

// TestTypeBehaviorFrozen 冻结 6 个类型的 11 项行为 + 身份组。
//
// 为什么断言方法而不是 capsTable 本身：今天比较表，明天表和 golden 一起改就全绿——
// 而且那样锁的是实现面，不是调用方消费面。改表若不同步改这里，diff 会落在两种形状上
// （表 vs 方法期望），review 才看得见。
func TestTypeBehaviorFrozen(t *testing.T) {
	want := map[Type]wantBehavior{
		TypeAPIKey: {
			valid: true, validTplExt: false, validAcctExt: false,
			isCodex: false, isBPS: false, isRespProvider: false,
			usesAcctExt: false, usesStaticKey: true,
			fixedEndpoint: false, respFmt: false,
			group: []Type{TypeAPIKey},
		},
		TypeResponsesSpecial: {
			valid: true, validTplExt: true, validAcctExt: false,
			isCodex: false, isBPS: false, isRespProvider: false,
			usesAcctExt: false, usesStaticKey: true,
			fixedEndpoint: false, respFmt: true,
			group: []Type{TypeResponsesSpecial},
		},
		TypeCustom: {
			valid: true, validTplExt: true, validAcctExt: false,
			isCodex: false, isBPS: false, isRespProvider: false,
			usesAcctExt: false, usesStaticKey: true,
			fixedEndpoint: false, respFmt: true,
			group: []Type{TypeCustom},
		},
		TypeCodexOAuth: {
			valid: true, validTplExt: true, validAcctExt: true,
			isCodex: true, isBPS: false, isRespProvider: true,
			usesAcctExt: true, usesStaticKey: false,
			fixedEndpoint: true, respFmt: true,
			group: []Type{TypeCodexOAuth, TypeCodexPAT},
		},
		TypeCodexPAT: {
			valid: true, validTplExt: true, validAcctExt: true,
			isCodex: true, isBPS: false, isRespProvider: true,
			usesAcctExt: true, usesStaticKey: false,
			fixedEndpoint: true, respFmt: true,
			group: []Type{TypeCodexOAuth, TypeCodexPAT},
		},
		TypeBPS: {
			valid: true, validTplExt: true, validAcctExt: true,
			isCodex: false, isBPS: true, isRespProvider: true,
			usesAcctExt: true, usesStaticKey: false,
			fixedEndpoint: true, respFmt: true,
			group: []Type{TypeBPS},
		},
	}
	// 表键集合与 golden 类型集合绑定：加/删表行而漏改这里 → 红（漏掉一行不再等于
	// 静默走零值）。
	require.Len(t, capsTable, len(want), "capsTable 与 golden 的类型集合必须一致")

	for typ, w := range want {
		require.Equal(t, w.valid, typ.Valid(), "%q.Valid", typ)
		require.Equal(t, w.validTplExt, typ.ValidTemplateExt(), "%q.ValidTemplateExt", typ)
		require.Equal(t, w.validAcctExt, typ.ValidAccountExt(), "%q.ValidAccountExt", typ)
		require.Equal(t, w.isCodex, typ.IsCodex(), "%q.IsCodex", typ)
		require.Equal(t, w.isBPS, typ.IsBPS(), "%q.IsBPS", typ)
		require.Equal(t, w.isRespProvider, typ.IsResponsesProvider(), "%q.IsResponsesProvider", typ)
		require.Equal(t, w.usesAcctExt, typ.UsesAccountExt(), "%q.UsesAccountExt", typ)
		require.Equal(t, w.usesStaticKey, typ.UsesStaticKey(), "%q.UsesStaticKey", typ)
		require.Equal(t, w.fixedEndpoint, typ.RequiresFixedEndpoint(), "%q.RequiresFixedEndpoint", typ)
		require.Equal(t, w.respFmt, typ.SupportsResponsesFormat("openai-responses"), "%q.SupportsResponsesFormat(openai-responses)", typ)
		require.Equal(t, w.respFmt, typ.SupportsResponsesFormat("openai-responses-ws"), "%q.SupportsResponsesFormat(openai-responses-ws)", typ)
		// 格式白名单与能力位无关：responses-special/codex/bps 只允许两种格式是硬约束。
		require.False(t, typ.SupportsResponsesFormat("openai-chat"), "%q.SupportsResponsesFormat(openai-chat)", typ)
		require.Equal(t, w.group, typ.IdentityGroup(), "%q.IdentityGroup：身份组决定账号查重作用域，不能按名称猜", typ)
	}
}

// TestCapsColumnInvariants 表内跨列不变量，直接对 capsTable 求值——**不依赖 golden**。
// 这是唯一能在「表 + golden 被同步改错」时仍然咬人的断言。
func TestCapsColumnInvariants(t *testing.T) {
	for typ, c := range capsTable {
		static := c.ch == ChannelStaticKey
		// 通道决定秘密来源：静态通道只读 upstream_key，SDK 通道只读 account_ext。
		require.Equal(t, static, c.usesStaticKey, "%q：static 通道 ⇔ 需要 account.upstream_key", typ)
		require.Equal(t, !static, c.usesAccountExt, "%q：SDK 通道 ⇔ 秘密/profile 在 account_ext", typ)
		require.Equal(t, !static, c.fixedEndpoint, "%q：SDK 通道 ⇔ 自带上游端点", typ)
		require.Equal(t, !static, c.validAcctExt, "%q：账号 ext 子表只为 SDK 通道存在", typ)
		// 秘密只能在一个地方：两边同时为真意味着同一份凭据有两个断言来源。
		require.False(t, c.usesAccountExt && c.usesStaticKey, "%q：account_ext 与 upstream_key 互斥", typ)
		// 能服务 responses 的类型都必须有模板 ext（当前设计事实：格式校验/flags 挂在
		// template_ext 上）。
		if c.responsesFormats {
			require.True(t, c.validTplExt, "%q：responsesFormats ⇒ 模板 ext 子表可用", typ)
		}
	}
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
					"%s：新增谓词方法 %s——类型的形态只由 capsTable 投影，不要在 credential 包再开按类型判定的入口",
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
