// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// 本文件是 factsDigest（U40）的**行为**判据。字节由 static_facts_kat_test.go 钉住，
// 形状由 static_plan_facts_gate_test.go 钉住，这里钉的是**关系**：
//
//  1. 敏感度：六维各变、其余恒定 ⇒ 摘要必须跟着变（同维内两两不等），且摘要相等
//     ⟺ 生效事实元组相等——**经唯一构造函数出键**，不是在编码器上手搓 ProviderFacts。
//     这一条是「值真的进了编码」的唯一证据：门禁只管读点（谁读了哪个字段），管不了
//     读到的值有没有被送进摘要（M5 之后，键字段集与读点集都还是对的）。
//  2. 不变性：20 个非 provider 维逐一变化 ⇒ 摘要逐字节不变。防「把 K/templateID 掺
//     进摘要」这类隐性冗余依赖（那会让无关变更作废在途计划）。
//  3. 单射：跨通道不等、槽位不混（("ab","c") vs ("a","bc")）。这两条在段序被换后
//     仍然绿——与 KAT 互补：KAT 钉字节，这里钉相等关系。
//  4. 分配：最坏**合法**形状零堆分配；跨过 512B 上界恰好一次 make。

// factsDigestSink 是分配断言的汇：把结果存到包级变量，防编译器把整轮调用消掉。
var factsDigestSink [32]byte

// factsInput 是一枚账号的**原始输入**（六维各一格）。构造统一走 newSnapshotStatic，
// 测试不直接拼 ProviderFacts——否则测的就不是「构造函数读到的事实」。
type factsInput struct {
	ct        credential.Type // 通道维：由类型经 credential.ChannelOf 派生
	accountID string          // ext.CodexAccountID
	inst      string          // ext.CodexIdentity.InstallationID
	profile   string          // ext.BPSProfile 的原始 JSON（空 = 无）
	upstream  string          // acc.UpstreamKey
	pat       string          // ext.CodexPATKey
}

// build 经唯一构造函数出一枚 planKey（每次调用都新建对象，模拟每轮重载）。
func (in factsInput) build() planKey {
	tpl := &domain.Template{
		ID:               3,
		BaseURL:          "https://tpl.example/v1",
		CredentialType:   in.ct,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
		Models:           []string{"m-a"},
	}
	ext := &domain.AccountExt{
		CodexAccountID: new(in.accountID),
		CodexIdentity:  &domain.CodexIdentity{InstallationID: in.inst},
		CodexPATKey:    new(in.pat),
	}
	if in.profile != "" {
		ext.BPSProfile = []byte(in.profile)
	}
	a := domain.Account{
		ID: 7, Name: "acct-7", TemplateID: tpl.ID, Template: tpl,
		UpstreamKey: in.upstream, MaxConcurrency: 4, Enabled: true, IdentityRevision: 5,
		Ext: ext,
	}
	return planKeyOf(newSnapshotStatic(a, tpl, []int64{3, 9}, directEgress(), nil))
}

// factsTuple 是**生效**的六元事实（经快照派生之后的口径）。
//
// 它是本测试的独立陈述（不是从构造函数抄的）：installation 只在 codex 通道成型
// （$S.installationID 的定型条件），PAT 只在非 BPS 形状进键，profile 摘要只对 BPS
// 非空。摘要相等必须 ⟺ 它相等。
type factsTuple struct {
	channel    credential.Channel
	accountID  string
	inst       string
	bpsProfile string
	upstream   string
	pat        string
}

func (in factsInput) effective() factsTuple {
	ch := credential.ChannelOf(in.ct)
	t := factsTuple{channel: ch, accountID: in.accountID, upstream: in.upstream}
	if ch == credential.ChannelCodex {
		t.inst = in.inst
	}
	if ch != credential.ChannelBPS {
		t.pat = in.pat
	}
	ext := &domain.AccountExt{}
	if in.profile != "" {
		ext.BPSProfile = []byte(in.profile)
	}
	t.bpsProfile = domain.BPSProfileDigest(in.ct, ext)
	return t
}

func (t factsTuple) String() string {
	return "channel=" + itoa(int(t.channel)) +
		" account=" + t.accountID +
		" install=" + t.inst +
		" bps=" + t.bpsProfile +
		" upstream=" + t.upstream +
		" pat=" + t.pat
}

// TestFactsDigestSensitivity 是 U40 的核心判据：六维 × 3 值，其余恒定。
//
// 两条断言（缺一就是假绿）：
//   - 同维内 3 份摘要**两两不等**（6 维 × C(3,2) = 18 次比较）；
//   - 摘要相等 ⟺ 生效事实元组相等（18 份全配对比较）。跨维允许与基线重复——每维
//     必有一个取值等于基线，那是**正确**的语义，不是漏洞。
//
// 另钉一条今天的语义（免得它被当成断言漏洞）：BPS 通道对 installationID 与 pat
// 不敏感。理由不是"实现没做"，而是 BPS 分支本来就不消费这两件事（$S.installationID
// 只在 codex 通道定型、BPS 的进键 PAT 恒为 ""），键里的事实不得比叶子带着的事实更细。
func TestFactsDigestSensitivity(t *testing.T) {
	base := factsInput{
		ct:        credential.TypeCodexPAT,
		accountID: "acct-A",
		inst:      "inst-A",
		profile:   `{"profile_id":"p","device_id":"d"}`,
		upstream:  "sk-A",
		pat:       "pat-A",
	}
	with := func(mut func(in *factsInput)) factsInput {
		in := base
		mut(&in)
		return in
	}

	dims := []struct {
		name  string
		cases []factsInput
	}{
		{"channel", []factsInput{
			with(func(in *factsInput) { in.ct = credential.TypeAPIKey }),
			with(func(in *factsInput) { in.ct = credential.TypeCodexPAT }),
			with(func(in *factsInput) { in.ct = credential.TypeBPS }),
		}},
		{"codexAccountID", []factsInput{
			with(func(in *factsInput) { in.accountID = "acct-A" }),
			with(func(in *factsInput) { in.accountID = "acct-B" }),
			with(func(in *factsInput) { in.accountID = "acct-C" }),
		}},
		{"installationID", []factsInput{
			with(func(in *factsInput) { in.inst = "inst-A" }),
			with(func(in *factsInput) { in.inst = "inst-B" }),
			with(func(in *factsInput) { in.inst = "inst-C" }),
		}},
		// profile 摘要只在 BPS 通道生效（非 BPS 恒 ""），所以这一维必须在 BPS 上下文里取
		// 三个值——在 codex 上下文里它根本不是事实，三条同键才是正确结论。
		{"bpsProfileDigest", []factsInput{
			with(func(in *factsInput) { in.ct = credential.TypeBPS; in.profile = `{"profile_id":"p1","device_id":"d"}` }),
			with(func(in *factsInput) { in.ct = credential.TypeBPS; in.profile = `{"profile_id":"p2","device_id":"d"}` }),
			with(func(in *factsInput) { in.ct = credential.TypeBPS; in.profile = `{"profile_id":"p3","device_id":"d"}` }),
		}},
		{"upstreamKey", []factsInput{
			with(func(in *factsInput) { in.upstream = "sk-A" }),
			with(func(in *factsInput) { in.upstream = "sk-B" }),
			with(func(in *factsInput) { in.upstream = "sk-C" }),
		}},
		{"pat", []factsInput{
			with(func(in *factsInput) { in.pat = "pat-A" }),
			with(func(in *factsInput) { in.pat = "pat-B" }),
			with(func(in *factsInput) { in.pat = "pat-C" }),
		}},
	}
	require.Len(t, dims, 6, "六维：channel / codexAccountID / installationID / bpsProfileDigest / upstreamKey / pat")

	type sample struct {
		label  string
		key    planKey
		digest [32]byte
		tuple  factsTuple
	}
	var all []sample
	withinDimComparisons := 0
	for _, dim := range dims {
		require.Len(t, dim.cases, 3, "%s 维必须取 3 个值", dim.name)
		got := make([]sample, 0, 3)
		for i, in := range dim.cases {
			key := in.build()
			got = append(got, sample{
				label:  dim.name + "#" + itoa(i),
				key:    key,
				digest: key.factsDigest,
				tuple:  in.effective(),
			})
		}
		// 同维内：3 份摘要两两不等（C(3,2) = 3 次比较）。
		for i := 0; i < len(got); i++ {
			for j := i + 1; j < len(got); j++ {
				require.NotEqual(t, got[i].digest, got[j].digest,
					"同维两值必须不同键：%s（%s）", dim.name, got[i].tuple.String()+" vs "+got[j].tuple.String())
				require.NotEqual(t, got[i].key, got[j].key,
					"同维两值必须不同键（整枚 planKey）：%s", dim.name)
				withinDimComparisons++
			}
		}
		// 前提非空：这一维的三个生效元组必须互不相同，否则上面的"两两不等"是在证明
		// 一个不成立的比较基准。
		require.NotEqual(t, got[0].tuple, got[1].tuple, "%s 维的两个取值必须产生不同的生效事实", dim.name)
		require.NotEqual(t, got[1].tuple, got[2].tuple, "%s 维的两个取值必须产生不同的生效事实", dim.name)
		all = append(all, got...)
	}
	require.Equal(t, 18, len(all), "6 维 × 3 值")
	require.Equal(t, 18, withinDimComparisons, "同维两两比较次数")

	// 摘要相等 ⟺ 生效事实元组相等（全配对；跨维允许与基线重复）。
	equalPairs, unequalPairs := 0, 0
	for i := 0; i < len(all); i++ {
		for j := i + 1; j < len(all); j++ {
			wantEq := all[i].tuple == all[j].tuple
			gotEq := all[i].digest == all[j].digest
			require.Equal(t, wantEq, gotEq,
				"摘要相等必须 ⟺ 生效事实元组相等：%s vs %s（事实相等=%v，摘要相等=%v）",
				all[i].tuple.String(), all[j].tuple.String(), wantEq, gotEq)
			if wantEq {
				equalPairs++
			} else {
				unequalPairs++
			}
		}
	}
	require.Positive(t, equalPairs, "跨维与基线重复是**正确**语义：必须真的存在相等的类")
	require.Positive(t, unequalPairs, "判据不能靠恒相等通过")
	t.Logf("敏感度：18 份摘要、%d 次同维比较、%d 次全配对（相等 %d / 不等 %d）",
		withinDimComparisons, len(all)*(len(all)-1)/2, equalPairs, unequalPairs)

	// 今天的语义（不是漏洞）：BPS 通道对 installationID 与 pat 不敏感——两者都不在
	// BPS 推给摘要的六段里（installation 只在 codex 通道成型，BPS 的进键 PAT 恒 ""）。
	bpsBase := with(func(in *factsInput) { in.ct = credential.TypeBPS })
	bpsNoisy := with(func(in *factsInput) {
		in.ct = credential.TypeBPS
		in.inst = "inst-OTHER"
		in.pat = "pat-RESIDUAL"
	})
	require.NotEqual(t, bpsBase.inst+"|"+bpsBase.pat, bpsNoisy.inst+"|"+bpsNoisy.pat,
		"前提：两枚 BPS 输入的 installation/pat 确实不同")
	require.Equal(t, bpsBase.build().factsDigest, bpsNoisy.build().factsDigest,
		"BPS 对 installationID 与 pat 不敏感（两者都不是 BPS 进摘要的事实）")
}

// TestFactsDigestInvariance 把摘要钉在「provider 事实」这一维上：其余 20 个 planKey
// 维逐一变化，摘要必须**逐字节**不变。
//
// 为什么需要它：把 identityRevision 或 templateID 顺手掺进摘要，敏感度测试全绿
// （那些维本来就会变），但每一次无关变更都会让摘要换代 ⇒ 复用分支失效、在途计划
// 被白作废 —— 摘要就成了「比改前更细的键」，正是这个单元要消灭的东西。
//
// 20 = 22 个 planKey 字段 − factsDigest − credentialType。credentialType 不在表里
// 不是遗漏：它就是 channel 维（经 credential.ChannelOf），按定义属于 factsDigest。
func TestFactsDigestInvariance(t *testing.T) {
	inputs := func() (domain.Account, *domain.Template, []int64) {
		tpl := &domain.Template{
			ID:               3,
			BaseURL:          "https://tpl.example/v1",
			CredentialType:   credential.TypeCodexOAuth,
			StripImageTools:  true,
			Models:           []string{"m-b", "m-a"},
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
			FormatModels:     map[domain.RequestFormat][]string{domain.FormatOpenAIResponses: {"m-a"}},
			ModelMapping:     domain.ModelMapping{"a": {MappedModel: "b", Mode: domain.ModelMappingModeExplicit}},
			Upstream:         &domain.TemplateUpstream{Paths: map[string]string{"responses": "v1/responses"}},
		}
		pat := "pat-A"
		a := domain.Account{
			ID: 7, Name: "acct-7", TemplateID: tpl.ID, Template: tpl,
			UpstreamKey: "sk-A", MaxConcurrency: 4, Enabled: true,
			CacheDomain: new("cache.example"), UpstreamCostMultiplierBp: new(12000),
			IdentityRevision: 5, SupplierUserID: 11,
			Ext: &domain.AccountExt{
				CodexAccountID: new("acct-A"),
				CodexPATKey:    &pat,
				CodexIdentity:  &domain.CodexIdentity{InstallationID: "inst-A"},
			},
		}
		return a, tpl, []int64{3, 9}
	}
	build := func(a domain.Account, tpl *domain.Template, gids []int64) planKey {
		return planKeyOf(newSnapshotStatic(a, tpl, gids, directEgress(), nil))
	}
	baseKey := build(inputs())
	baseDigest := baseKey.factsDigest
	require.NotEqual(t, [32]byte{}, baseDigest, "摘要必须非零（否则不变性恒真）")

	cases := []struct {
		name   string
		mutate func(a *domain.Account, tpl *domain.Template, gids *[]int64)
	}{
		{"accountID", func(a *domain.Account, _ *domain.Template, _ *[]int64) { a.ID = 8 }},
		{"name", func(a *domain.Account, _ *domain.Template, _ *[]int64) { a.Name = "other" }},
		{"templateID", func(a *domain.Account, _ *domain.Template, _ *[]int64) { a.TemplateID = 4 }},
		{"baseURL", func(a *domain.Account, _ *domain.Template, _ *[]int64) { a.BaseURL = new("https://override/v1") }},
		{"maxConcurrency", func(a *domain.Account, _ *domain.Template, _ *[]int64) { a.MaxConcurrency = 8 }},
		{"enabled", func(a *domain.Account, _ *domain.Template, _ *[]int64) { a.Enabled = false }},
		{"cacheDomain", func(a *domain.Account, _ *domain.Template, _ *[]int64) { a.CacheDomain = nil }},
		{"upstreamCostMultiplierBp", func(a *domain.Account, _ *domain.Template, _ *[]int64) { a.UpstreamCostMultiplierBp = new(9000) }},
		{"identityRevision", func(a *domain.Account, _ *domain.Template, _ *[]int64) { a.IdentityRevision = 6 }},
		{"supplierUserID", func(a *domain.Account, _ *domain.Template, _ *[]int64) { a.SupplierUserID = 42 }},
		{"proxyDigest", func(a *domain.Account, _ *domain.Template, _ *[]int64) {
			a.ProxyID = new(int64(1))
			a.Proxy = &domain.Proxy{ID: 1, Scheme: domain.ProxySchemeSOCKS5, Host: "127.0.0.1", Port: 1080, Enabled: true}
		}},
		// routable 由代理状态派生，所以这一维没法只翻它自己（翻 enabled 同样会带上它）。
		// 两个位都是 planKey 字段，摘要在两种翻法下都不得变。
		{"routable", func(a *domain.Account, _ *domain.Template, _ *[]int64) {
			a.ProxyID = new(int64(1))
			a.Proxy = &domain.Proxy{ID: 1, Enabled: false}
		}},
		{"stripImageTools", func(_ *domain.Account, tpl *domain.Template, _ *[]int64) { tpl.StripImageTools = false }},
		{"modelsDigest", func(_ *domain.Account, tpl *domain.Template, _ *[]int64) { tpl.Models = []string{"m-a"} }},
		{"formatModelsKey", func(_ *domain.Account, tpl *domain.Template, _ *[]int64) { tpl.FormatModels = nil }},
		{"supportedFormats", func(_ *domain.Account, tpl *domain.Template, _ *[]int64) {
			tpl.SupportedFormats = []domain.RequestFormat{domain.FormatOpenAIChat}
		}},
		{"modelMappingKey", func(_ *domain.Account, tpl *domain.Template, _ *[]int64) { tpl.ModelMapping = nil }},
		{"upstreamPathsKey", func(_ *domain.Account, tpl *domain.Template, _ *[]int64) { tpl.Upstream = nil }},
		{"codexResponsesTransport", func(_ *domain.Account, tpl *domain.Template, _ *[]int64) {
			tpl.CodexResponsesTransport = domain.CodexResponsesTransportWS
		}},
		{"groupIDsDigest", func(_ *domain.Account, _ *domain.Template, gids *[]int64) { *gids = []int64{3} }},
	}
	require.Len(t, cases, 20, "20 个非 provider 维（22 − factsDigest − credentialType）")

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, tpl, gids := inputs()
			tc.mutate(&a, tpl, &gids)
			got := build(a, tpl, gids)
			// 先证明变异真的改了键——否则"摘要不变"是空转（变异写错时它会静默绿）。
			require.NotEqual(t, baseKey, got, "变异必须真的改变 planKey：%s", tc.name)
			require.Equal(t, baseDigest, got.factsDigest,
				"factsDigest 不得吸收 %s：它是 provider 事实的摘要，不是整枚键的摘要", tc.name)
		})
	}

	// baseURL 有第二个源（模板值），同一个 planKey 字段；一并钉住。
	t.Run("baseURL (template)", func(t *testing.T) {
		a, tpl, gids := inputs()
		tpl.BaseURL = "https://tpl2.example/v1"
		got := build(a, tpl, gids)
		require.NotEqual(t, baseKey, got, "模板 baseURL 变更必须换键")
		require.Equal(t, baseDigest, got.factsDigest, "factsDigest 不得吸收 baseURL")
	})
}

// TestFactsDigestChannelInjective 钉住通道维的单射：三个通道两两不同摘要。
//
// 为什么单独一条：段 0 是**标签**而不是枚举值，标签写错/漏写都会让「同一份事实在
// 两个通道上同键」——那会把 codex 与 BPS 的身份混成一类，而两者在上游是不同账号。
// 这条在段序被换后仍然绿（与 KAT 互补）。
func TestFactsDigestChannelInjective(t *testing.T) {
	base := ProviderFacts{CodexAccountID: "acct-A", InstallationID: "inst-A", BPSProfileDigest: "prof-A"}
	channels := []credential.Channel{credential.ChannelStaticKey, credential.ChannelCodex, credential.ChannelBPS}
	seen := make(map[[32]byte]credential.Channel, len(channels))
	for _, ch := range channels {
		f := base
		f.Channel = ch
		d := FactsDigest(f, "sk-A", "pat-A")
		if prev, ok := seen[d]; ok {
			t.Fatalf("通道 %d 与通道 %d 落到同一摘要：通道维没有进编码", ch, prev)
		}
		seen[d] = ch
	}
	require.Len(t, seen, len(channels), "三个通道必须三份不同摘要")
	// 非空性：六段全同、只换通道时摘要必须真的变（上面已隐含；这里显式钉一条，
	// 让"标签被写成常量"这种退化直接命中）。
	staticFacts := base
	staticFacts.Channel = credential.ChannelStaticKey
	codexFacts := base
	codexFacts.Channel = credential.ChannelCodex
	require.NotEqual(t, FactsDigest(staticFacts, "sk-A", "pat-A"), FactsDigest(codexFacts, "sk-A", "pat-A"))
}

// TestFactsDigestSlotExclusive 钉住槽位不混：长度前缀使碎片边界进编码。
//
// ("ab","c") 与 ("a","bc") 裸拼接后同为 "abc"——它们在改前是两枚不同的 string 字段
// （两枚不同的键），改后也必须是两枚不同的摘要。同理 ("x","") 与 ("","x")。
func TestFactsDigestSlotExclusive(t *testing.T) {
	f := ProviderFacts{Channel: credential.ChannelCodex, CodexAccountID: "acct-A"}
	require.NotEqual(t, FactsDigest(f, "ab", "c"), FactsDigest(f, "a", "bc"),
		"槽位歧义：(\"ab\",\"c\") 与 (\"a\",\"bc\") 不得折成同一摘要")
	require.NotEqual(t, FactsDigest(f, "x", ""), FactsDigest(f, "", "x"),
		"槽位歧义：(\"x\",\"\") 与 (\"\",\"x\") 不得折成同一摘要")
	require.NotEqual(t, FactsDigest(f, "", ""), FactsDigest(f, " ", ""),
		"空值与单空格不得同键（与改前的两枚 string 字段一致）")
}

// factsDigestWorstLegal 是**最坏合法形状**：每一维都取真实字段的长度上界。
//
//	channel 10（chan:codex）
//	codexAccountID 43（OpenAI 账号 ID）
//	installationID 36（UUID）
//	bpsProfileDigest 0（codex 通道没有 profile）
//	upstreamKey 128（上游鉴权键上界）
//	pat 20
//	六段前缀 48
//
// 合计 285B ≤ 512 ⇒ 必须走栈上缓冲、零堆分配。真实形状比它更小，所以"最坏合法"
// 成立时常见形状自然也成立。
func factsDigestWorstLegal() (ProviderFacts, string, string) {
	return ProviderFacts{
			Channel:        credential.ChannelCodex,
			CodexAccountID: strings.Repeat("a", 43),
			InstallationID: "3f1c9a24-8b6d-4e07-9c1a-2d5b8e7f0a13",
		},
		"sk-live-" + strings.Repeat("0123456789abcdef", 7) + strings.Repeat("f", 8), // 8 + 112 + 8 = 128
		strings.Repeat("p", 20)
}

// factsDigestOverflowShape 是溢出分支的形状：前像 547B（> 512 上界，恰好一次 make）。
// 取值与 static_facts_kat_test.go 的 overflow_547 向量一致——KAT 钉的字节、基准测的
// 形状、分配断言用的输入是**同一份**。
func factsDigestOverflowShape() (ProviderFacts, string, string) {
	return ProviderFacts{
			Channel:        credential.ChannelCodex,
			CodexAccountID: "acct_9f2c41d8e7b3",
			InstallationID: "3f1c9a24-8b6d-4e07-9c1a-2d5b8e7f0a13",
		},
		"sk-live-" + strings.Repeat("0123456789abcdef", 26),
		"pat-" + strings.Repeat("f", 8)
}

// TestFactsDigestWorstCaseAllocs 用 testing.AllocsPerRun 钉住两件事：
//
//   - 最坏**合法**形状 **0 分配**。这条不是"性能优化验收"，是编码形状的硬约束：
//     `sha256.New()` + `h.Sum(nil)`（domain.hashFields 的路子）恒 1 次 32B 分配，
//     `[]byte(s)` 逃逸再贡献若干——一旦有人"复用"那枚函数，本断言立刻红。
//   - 跨过 512B 上界**恰好一次** make（多一次说明长度算错、做了二次扩容）。
func TestFactsDigestWorstCaseAllocs(t *testing.T) {
	f, upstream, pat := factsDigestWorstLegal()
	require.LessOrEqual(t, factsDigestPreimageLen(f, upstream, pat), factsDigestInlineCap,
		"最坏合法形状必须落在栈上上界内（上界 = 8*6 + Σ|seg| ≤ 512）")
	allocs := testing.AllocsPerRun(1000, func() {
		factsDigestSink = FactsDigest(f, upstream, pat)
	})
	require.Zero(t, allocs, "最坏合法形状必须零堆分配（inlineCap ≥ 512 + sha256.Sum256）")

	t.Run("溢出恰好一次分配", func(t *testing.T) {
		of, oupstream, opat := factsDigestOverflowShape()
		require.Greater(t, factsDigestPreimageLen(of, oupstream, opat), factsDigestInlineCap,
			"溢出形状必须真的跨过栈上上界")
		allocs := testing.AllocsPerRun(100, func() {
			factsDigestSink = FactsDigest(of, oupstream, opat)
		})
		require.Equal(t, 1.0, allocs, "超出 512B 只允许一次 make（多余分配说明长度算错或发生了扩容）")
	})
}
