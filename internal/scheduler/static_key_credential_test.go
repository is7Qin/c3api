// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// TestPlanKeyCredentialPairEquivalenceClasses 是 planKey 凭据面的**等价类参照
// 测试**，也是 frozenPlanKey 在凭据维度上的 oracle 继任者（见该函数的注释）。
//
// 断言口径（5 类型 × 15 表示 × 15 表示 = 1125 组，两两比较共 1 265 625 次）：
//
//	新键相等  ⟺  改前**进键**的 (上游鉴权键, PAT) 对逐字节相等 ∧ 凭据类型相同
//
// "改前进键的那对凭据"由 frozenPlanKey 逐行冻结（它不参与实现）：上游鉴权键无条件
// 入键；PAT 只在「ext 存在 ∧ 模板非 BPS」分支入键。因此这条判据同时锁住两个方向：
//   - 不得**合并**今天不同的键（大小写、尾空格、拼接歧义、残留 ext 的 pat）；
//   - 不得**拆分**今天相同的键（BPS 的残留 pat 今天不进键，改后也不许进）。
//
// 类型作为元组的一维：跨类型的两枚键今天就不可能相等（credentialType 是独立字段），
// 故"同类 + 同对"才是等价。
func TestPlanKeyCredentialPairEquivalenceClasses(t *testing.T) {
	// 15 种凭据表示：空、退化空白、拼接歧义的碎片（"ab"/"c" vs "a"/"bc"）、大小写、
	// 前后空白与制表、内嵌 NUL（挡"用 NUL 当分隔符"的写法）、多字节 UTF-8、长串。
	reps := []string{
		"",          // 空值
		" ",         // 单个空格：非空但退化
		"a",         // ┐
		"ab",        // │ ("ab","c") 与 ("a","bc")
		"c",         // │ 在网格里各出现一次
		"bc",        // ┘
		"abc",       // 与上面两对的裸拼接相同：长度前缀必须把它们分开
		"pat-abc",   // 常规 pat
		"PAT-ABC",   // 大小写差异
		"pat-abc ",  // 尾空格
		" pat-abc",  // 前空格
		"pat-abc\t", // 尾制表符
		"ab\x00c",   // 内嵌 NUL
		"パット",       // 多字节 UTF-8
		"sk-" + "0123456789abcdef0123456789abcdef", // 长串（跨过 16 字节前缀的常见长度）
	}
	types := []struct {
		name string
		cred credential.Type
	}{
		// 5 类型恰好覆盖改前构造函数的三条分支：BPS / 非 BPS × 静态族 / Codex 族。
		// responses-special 与 api_key 在凭据面走同一条分支（非 BPS、静态族），
		// 逐字节同源，故不另占一维；BPS 与 codex-oauth 是必测的两条边。
		{"api_key", credential.TypeAPIKey},
		{"custom", credential.TypeCustom},
		{"codex-pat", credential.TypeCodexPAT},
		{"codex-oauth", credential.TypeCodexOAuth},
		{"bps", credential.TypeBPS},
	}
	require.Len(t, types, 5, "5 类型")
	require.Len(t, reps, 15, "15 种表示")

	// 夹具：除 (类型, 上游鉴权键, pat) 外的全部事实恒定——否则键的差异可能来自别的
	// 维度，等价类断言就不是在讲凭据面了。每次调用都新建对象（模拟每轮重载）。
	fixture := func(ct credential.Type, upstream, pat string) (domain.Account, *domain.Template, []int64) {
		tpl := &domain.Template{
			ID:               3,
			BaseURL:          "https://tpl.example/v1",
			CredentialType:   ct,
			StripImageTools:  true,
			Models:           []string{"m-b", "m-a"},
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatOpenAIResponses},
			FormatModels:     map[domain.RequestFormat][]string{domain.FormatOpenAIChat: {"m-a", "m-b"}},
			ModelMapping:     domain.ModelMapping{"alias": {MappedModel: "up", Mode: domain.ModelMappingModeExplicit}},
			Upstream:         &domain.TemplateUpstream{Paths: map[string]string{"openai-chat": "v1/chat/completions"}},
		}
		acc := domain.Account{
			ID: 7, Name: "acct-7", TemplateID: tpl.ID, UpstreamKey: upstream,
			MaxConcurrency: 4, Enabled: true, IdentityRevision: 5,
			Template: tpl,
			Ext: &domain.AccountExt{
				CodexAccountID: new("acct-up"),
				CodexEmail:     new("e@example.com"),
				CodexPATKey:    new(pat), // 恒非 nil："" 与 nil 在改前同键（同一枚 string）
				CodexIdentity:  &domain.CodexIdentity{InstallationID: "inst"},
				BPSProfile:     []byte(`{"profile_id":"p","device_id":"d"}`),
			},
		}
		return acc, tpl, []int64{3, 9}
	}
	build := func(ct credential.Type, upstream, pat string) planKey {
		acc, tpl, gids := fixture(ct, upstream, pat)
		return planKeyOf(newSnapshotStatic(acc, tpl, gids, directEgress(), nil))
	}

	type cell struct {
		credName      string
		upstream, pat string
		key           planKey
		pair          frozenCredentialPair
	}
	cells := make([]cell, 0, len(types)*len(reps)*len(reps))
	for _, ty := range types {
		for _, upstream := range reps {
			for _, pat := range reps {
				acc, tpl, gids := fixture(ty.cred, upstream, pat)
				key := planKeyOf(newSnapshotStatic(acc, tpl, gids, directEgress(), nil))
				_, pair, _ := frozenPlanKey(acc, tpl, gids)
				// 口径自证：冻结推导的 pair 就是原文本身（BPS 分支今天不写 pat 字段，
				// 故为 ""）。这条把"等价类"钉在**原始三元组**上，而不是钉在某个中间量上。
				wantPair := frozenCredentialPair{upstream: upstream, pat: pat}
				if ty.cred.IsBPS() {
					wantPair.pat = ""
				}
				require.Equal(t, wantPair, pair, "%s upstream=%q pat=%q: 冻结的进键 pair", ty.name, upstream, pat)
				cells = append(cells, cell{credName: ty.name, upstream: upstream, pat: pat, key: key, pair: pair})
			}
		}
	}
	require.Len(t, cells, 1125, "5 类型 × 15 表示 × 15 表示")

	// 两两比较：新键相等 ⟺ (类型, 进键 pair) 相同。
	var mismatches []string
	equalPairs, unequalPairs := 0, 0
	for i := range cells {
		for j := range cells {
			a, b := cells[i], cells[j]
			wantEq := a.credName == b.credName && a.pair == b.pair
			gotEq := a.key == b.key
			if wantEq {
				equalPairs++
			} else {
				unequalPairs++
			}
			if wantEq == gotEq {
				continue
			}
			if len(mismatches) < 10 {
				mismatches = append(mismatches, fmt.Sprintf(
					"%s(u=%q,p=%q) vs %s(u=%q,p=%q): 期望相等=%v，实得=%v（进键 pair %+v vs %+v）",
					a.credName, a.upstream, a.pat, b.credName, b.upstream, b.pat, wantEq, gotEq, a.pair, b.pair))
			}
		}
	}
	require.Empty(t, mismatches, "等价类失配（新键相等必须 ⟺ 改前进键的 (上游键, pat, 类型) 逐字节相等）")
	// 双向非空：判据不能靠"永远不等"或"永远相等"通过。
	require.Positive(t, equalPairs, "必须存在相等的等价类（否则判据只是恒不等）")
	require.Positive(t, unequalPairs, "必须存在不相等的类（否则判据只是恒相等）")
	t.Logf("1125 组、%d 次两两比较：相等 %d / 不等 %d", len(cells)*len(cells), equalPairs, unequalPairs)

	// 点名的边界各来一条独立断言：失配时不必去读 126 万次比较的日志。
	apiKey, bps, oauth := credential.TypeAPIKey, credential.TypeBPS, credential.TypeCodexOAuth
	require.NotEqual(t, build(apiKey, "ab", "c"), build(apiKey, "a", "bc"),
		"裸拼接歧义：(\"ab\",\"c\") 与 (\"a\",\"bc\") 今天不同键，改后也不得合并")
	require.NotEqual(t, build(apiKey, "sk", "pat-abc"), build(apiKey, "sk", "PAT-ABC"),
		"大小写必须区分（今天就是两枚不同的 string）")
	require.NotEqual(t, build(apiKey, "sk", "pat-abc"), build(apiKey, "sk", "pat-abc "),
		"尾空格必须区分")
	require.NotEqual(t, build(apiKey, "sk ", "pat-abc"), build(apiKey, "sk", "pat-abc"),
		"上游键的尾空格必须区分")
	require.NotEqual(t, build(apiKey, "", ""), build(apiKey, "", " "),
		"空值与单空格必须区分")
	require.Equal(t, build(apiKey, "sk", ""), build(apiKey, "sk", ""),
		"同一对凭据必须同键（自反性）")
	require.NotEqual(t, build(bps, "sk-1", ""), build(bps, "sk-2", ""),
		"BPS 的上游键仍进键")
	require.Equal(t, build(bps, "sk", "pat-residual"), build(bps, "sk", "other-residual"),
		"BPS 的残留 ext pat 今天不进键，改后也不得进（否则等于新增一次失效）")
	require.NotEqual(t, build(oauth, "sk", "pat-a"), build(oauth, "sk", "pat-b"),
		"codex-oauth 的 pat 今天仍进 planKey（常量摘要只作用于候选指纹，不作用于键）")

	// 网格之外的分支补充：ext 缺失。改前该分支根本不写 PAT 字段，故 PAT 维度恒为 ""
	// ⇒ 同一上游键下所有 pat 表示同键，而上游键之间仍不同键。
	t.Run("扩展：ext 缺失时 PAT 表示不参与等价类", func(t *testing.T) {
		var first planKey
		for i, pat := range reps {
			acc, tpl, gids := fixture(apiKey, "sk", pat)
			acc.Ext = nil
			got := planKeyOf(newSnapshotStatic(acc, tpl, gids, directEgress(), nil))
			if i == 0 {
				first = got
				continue
			}
			require.True(t, first == got, "ext 缺失时 pat=%q 不得改变 planKey（今天该分支不写该字段）", pat)
		}
		acc, tpl, gids := fixture(apiKey, "sk-other", "")
		acc.Ext = nil
		require.False(t, first == planKeyOf(newSnapshotStatic(acc, tpl, gids, directEgress(), nil)),
			"ext 缺失时上游鉴权键仍必须进键")
	})
}
