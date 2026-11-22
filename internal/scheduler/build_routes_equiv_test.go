// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"fmt"
	"math/rand"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// 本文件是 buildRoutes 的**行为等价锚点**。上半部分是旧算法的化石：
// buildRoutesReference / modelSetReference 逐字保留"按账号重算模板事实"的旧实现，
// **只用于差分断言，不得被生产代码引用**（生产引用会让死代码复活；差分断言只比
// sortedRouteKeys，故化石永远与生产实现同语义、同键集）。

// modelSetReference 是旧 modelSet 的化石：组内所有账号模板的可服务模型并集。
func modelSetReference(accs []*accountSnapshot) map[string]struct{} {
	set := make(map[string]struct{})
	for _, a := range accs {
		tpl := a.static.Load().tpl
		if tpl == nil {
			continue
		}
		for _, m := range tpl.Models {
			set[m] = struct{}{}
		}
		for _, list := range tpl.FormatModels {
			for _, m := range list {
				set[m] = struct{}{}
			}
		}
		for m := range tpl.ModelMapping {
			set[m] = struct{}{}
		}
	}
	return set
}

// buildRoutesReference 是旧 buildRoutes 的化石（按 model × format × account 重算
// 模板事实）。只作差分断言的对照实现，**不得被生产代码引用**。
func buildRoutesReference(accs []*accountSnapshot) map[routeKey]*route {
	routes := make(map[routeKey]*route)
	formats := domain.RoutableFormats()
	for model := range modelSetReference(accs) {
		for _, format := range formats {
			var t1, t2 []*accountSnapshot
			for _, a := range accs {
				tpl := a.static.Load().tpl
				if tpl == nil || !tpl.FormatSupports(format, model) {
					continue
				}
				if tpl.Serves(model) {
					t1 = append(t1, a)
				} else if !tpl.HasModelSpace() {
					t2 = append(t2, a) // 全模型账号：tier2 兜底
				}
				// 白名单账号未命中 → 跳过（不建路由 → 404）
			}
			if len(t1) == 0 && len(t2) == 0 {
				continue
			}
			routes[routeKey{format, model}] = &route{}
		}
	}
	for _, format := range formats {
		var t2 []*accountSnapshot
		for _, a := range accs {
			tpl := a.static.Load().tpl
			if tpl == nil || !tplSupportsFormat(tpl, format) {
				continue
			}
			if tpl.HasModelSpace() {
				continue // 白名单账号不参与未知模型回落（默认桶仅全模型账号）
			}
			t2 = append(t2, a)
		}
		if len(t2) == 0 {
			continue
		}
		routes[routeKey{format, ""}] = &route{}
	}
	return routes
}

// --- 夹具 ---

// routeAccount 造一个已挂静态叶的账号快照（tpl 可为 nil）。
func routeAccount(id int64, tpl *domain.Template) *accountSnapshot {
	return newAccountSnapshot(
		newSnapshotStatic(domain.Account{ID: id}, tpl, nil, directEgress(), nil),
		&accState{status: domain.StatusActive},
	)
}

// routePool 由模板列表造组内账号池：**每个账号一份独立叶**（生产形状：逐账号转换
// 领域对象），nil 元素对应 `tpl == nil` 的账号。同一枚指针重复传入则共享模板。
func routePool(tpls ...*domain.Template) []*accountSnapshot {
	accs := make([]*accountSnapshot, 0, len(tpls))
	for i, tpl := range tpls {
		accs = append(accs, routeAccount(int64(i+1), tpl))
	}
	return accs
}

// routePoolString 是差分失败时的池快照（"参考实现的报错形状"：一眼看出是哪份模板
// 让两个实现分叉）。
func routePoolString(pool []*accountSnapshot) string {
	var b strings.Builder
	for i, a := range pool {
		tpl := a.static.Load().tpl
		if tpl == nil {
			fmt.Fprintf(&b, "\n  [%d] tpl=nil", i)
			continue
		}
		fmt.Fprintf(&b, "\n  [%d] cred=%q formats=%v models=%v formatModels=%v mapping=%v",
			i, tpl.CredentialType, tpl.SupportedFormats, tpl.Models, tpl.FormatModels, tpl.ModelMapping)
	}
	return b.String()
}

// requireRoutesEquivalent 断言生产实现与化石实现的**键集**相等。只比 sortedRouteKeys：
// map 的值是每次新分配的 &route{}，比 map 本身永远不等。
func requireRoutesEquivalent(t *testing.T, tag string, pool []*accountSnapshot) {
	t.Helper()
	require.Equal(t, sortedRouteKeys(buildRoutesReference(pool)), sortedRouteKeys(buildRoutes(pool)),
		"%s：路由键集必须与化石实现逐键相等；池：%s", tag, routePoolString(pool))
}

var allRoutable = domain.RoutableFormats()

// --- 形状表（spec §4.5 第 3 条：每一行都必须存在，缺一行就是漏一种语义） ---
//
// wantBucket / noBucket 是**该形状自己的**定向断言（化石差分是主判据，这两列把形状
// 名字与它所钉的推论连起来，避免"两个实现一起错"时差分静默）。每条形状跑正序与
// 反序（账号切片倒置）：行序是隐式契约（"blanket 就地展开"在形状 4 绿、形状 5 红），
// 倒序把它变成显式的。
type routeShapeCase struct {
	name       string
	tpls       []*domain.Template
	wantBucket []routeKey
	noBucket   []routeKey
}

func routeShapeCases() []routeShapeCase {
	bpsTwin := func() (*domain.Template, *domain.Template) {
		// 唯一差别是 CredentialType：其余四维（Models/FormatModels/SupportedFormats/
		// ModelMapping）完全相同 ⇒ 去重键只剩 credType 能区分它们。
		bps := &domain.Template{
			CredentialType:   credential.TypeBPS,
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses, domain.FormatAnthropic},
			Models:           []string{"x"},
		}
		apikey := &domain.Template{
			CredentialType:   credential.TypeAPIKey,
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses, domain.FormatAnthropic},
			Models:           []string{"x"},
		}
		return bps, apikey
	}
	bps13, apikey13 := bpsTwin()

	shared := tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"})
	sharedDup := tplWith(domain.FormatOpenAIChat, []string{"gpt-4o"})

	sameIDDiffContentA := tplWith(domain.FormatOpenAIChat, []string{"a"})
	sameIDDiffContentA.ID = 7
	sameIDDiffContentB := tplWith(domain.FormatOpenAIChat, []string{"b"})
	sameIDDiffContentB.ID = 7

	return []routeShapeCase{{
		name: "1 单模板基线与默认桶缺席",
		tpls: []*domain.Template{tplWith(domain.FormatOpenAIChat, []string{"gpt-4o", "gpt-4o-mini"})},
		wantBucket: []routeKey{
			{domain.FormatOpenAIChat, "gpt-4o"},
			{domain.FormatOpenAIChat, "gpt-4o-mini"},
		},
		noBucket: []routeKey{{domain.FormatOpenAIChat, ""}},
	}, {
		name: "2 逐格式白名单限制",
		tpls: []*domain.Template{{
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatAnthropic},
			Models:           []string{"gpt-4o", "special"},
			FormatModels:     map[domain.RequestFormat][]string{domain.FormatAnthropic: {"special"}},
		}},
		wantBucket: []routeKey{
			{domain.FormatOpenAIChat, "gpt-4o"},
			{domain.FormatOpenAIChat, "special"},
			{domain.FormatAnthropic, "special"},
		},
		noBucket: []routeKey{{domain.FormatAnthropic, "gpt-4o"}},
	}, {
		name: "3 仅 ModelMapping 非空即有模型空间",
		tpls: []*domain.Template{{
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
			ModelMapping:     domain.ModelMapping{"alias-x": {MappedModel: "up-x", Mode: domain.ModelMappingModeExplicit}},
		}},
		wantBucket: []routeKey{{domain.FormatOpenAIChat, "alias-x"}},
		noBucket:   []routeKey{{domain.FormatOpenAIChat, ""}},
	}, {
		name: "4 全模型账号展开到全组模型并集",
		tpls: []*domain.Template{
			{SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatOpenAIResponses}},
			tplWith(domain.FormatOpenAIChat, []string{"w1", "w2"}),
		},
		wantBucket: []routeKey{
			{domain.FormatOpenAIChat, ""},
			{domain.FormatOpenAIResponses, ""},
			{domain.FormatOpenAIChat, "w1"},
			{domain.FormatOpenAIResponses, "w2"},
		},
	}, {
		name: "5 blanket 不得为未声明的格式建桶",
		tpls: []*domain.Template{
			{SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}},
			{SupportedFormats: []domain.RequestFormat{domain.FormatAnthropic}, Models: []string{"a"}},
		},
		// blanket 账号只声明了 chat ⇒ anthropic 上连默认桶都不许有；(anthropic,"a") 是
		// 那个白名单账号自己贡献的，合法存在（这正是"blanket 不得为 anthropic 建桶"
		// 与"白名单在该格式上有桶"必须分开看的理由）。
		wantBucket: []routeKey{
			{domain.FormatOpenAIChat, ""},
			{domain.FormatOpenAIChat, "a"},
			{domain.FormatAnthropic, "a"},
		},
		noBucket: []routeKey{{domain.FormatAnthropic, ""}, {domain.FormatOpenAIResponses, ""}},
	}, {
		name:       "6 tpl==nil 账号必须跳过",
		tpls:       []*domain.Template{nil, tplWith(domain.FormatOpenAIChat, []string{"m"})},
		wantBucket: []routeKey{{domain.FormatOpenAIChat, "m"}},
		noBucket:   []routeKey{{domain.FormatOpenAIChat, ""}},
	}, {
		name: "7 BPS 格式门（不是裸 SupportedFormats 包含）",
		tpls: []*domain.Template{{
			CredentialType:   credential.TypeBPS,
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses, domain.FormatAnthropic},
			Models:           []string{"x"},
		}},
		wantBucket: []routeKey{{domain.FormatOpenAIResponses, "x"}},
		noBucket:   []routeKey{{domain.FormatAnthropic, "x"}},
	}, {
		name:       "8 两账号共享同一枚模板指针",
		tpls:       []*domain.Template{shared, shared},
		wantBucket: []routeKey{{domain.FormatOpenAIChat, "gpt-4o"}},
	}, {
		name:       "9 两账号内容相同但指针不同（生产形状）",
		tpls:       []*domain.Template{shared, sharedDup},
		wantBucket: []routeKey{{domain.FormatOpenAIChat, "gpt-4o"}},
	}, {
		name:       "10 同一 ID 不同内容不得按 ID 去重",
		tpls:       []*domain.Template{sameIDDiffContentA, sameIDDiffContentB},
		wantBucket: []routeKey{{domain.FormatOpenAIChat, "a"}, {domain.FormatOpenAIChat, "b"}},
	}, {
		name: "11 Models 含空串：与默认桶同键",
		tpls: []*domain.Template{tplWith(domain.FormatOpenAIChat, []string{"", "m"})},
		wantBucket: []routeKey{
			{domain.FormatOpenAIChat, ""},
			{domain.FormatOpenAIChat, "m"},
		},
	}, {
		name: "12 空非 nil 的 FormatModels 列表 = 该格式全禁",
		tpls: []*domain.Template{{
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatAnthropic},
			Models:           []string{"a"},
			FormatModels:     map[domain.RequestFormat][]string{domain.FormatAnthropic: {}},
		}},
		wantBucket: []routeKey{{domain.FormatOpenAIChat, "a"}},
		noBucket:   []routeKey{{domain.FormatAnthropic, "a"}},
	}, {
		name:       "13 去重键必须含 credType",
		tpls:       []*domain.Template{bps13, apikey13},
		wantBucket: []routeKey{{domain.FormatOpenAIResponses, "x"}, {domain.FormatAnthropic, "x"}},
	}, {
		name: "14 模型空间必须含 FormatModels 列表",
		tpls: []*domain.Template{
			{SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}},
			{
				SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
				FormatModels:     map[domain.RequestFormat][]string{domain.FormatAnthropic: {"x"}},
			},
		},
		wantBucket: []routeKey{{domain.FormatOpenAIChat, ""}, {domain.FormatOpenAIChat, "x"}},
	}, {
		name: "15 未配置 FormatModels 的格式走整个模型空间",
		tpls: []*domain.Template{{
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatOpenAIResponses},
			FormatModels:     map[domain.RequestFormat][]string{domain.FormatOpenAIChat: {"x"}},
		}},
		wantBucket: []routeKey{
			{domain.FormatOpenAIChat, "x"},
			{domain.FormatOpenAIResponses, "x"},
		},
	}}
}

func TestBuildRoutesShapesMatchTheFossil(t *testing.T) {
	for _, tc := range routeShapeCases() {
		t.Run(tc.name, func(t *testing.T) {
			reversed := slices.Clone(tc.tpls)
			slices.Reverse(reversed)
			orders := []struct {
				order string
				tpls  []*domain.Template
			}{{"正序", tc.tpls}, {"反序", reversed}}
			for _, o := range orders {
				order, tpls := o.order, o.tpls
				pool := routePool(tpls...)
				requireRoutesEquivalent(t, tc.name+"（"+order+"）", pool)

				routes := buildRoutes(pool)
				for _, k := range tc.wantBucket {
					require.Contains(t, routes, k, "%s（%s）必须建桶 (%s,%q)", tc.name, order, k.format, k.model)
				}
				for _, k := range tc.noBucket {
					require.NotContains(t, routes, k, "%s（%s）不得建桶 (%s,%q)", tc.name, order, k.format, k.model)
				}
			}
		})
	}
	require.Len(t, routeShapeCases(), 15, "形状表必须 15 条：删一行就是漏一种语义")
}

// --- 随机差分（真正的网） ---

var (
	routeRandomCredTypes = []credential.Type{
		credential.TypeAPIKey, credential.TypeCustom, credential.TypeCodexPAT, credential.TypeBPS, "",
	}
	routeRandomModels = []string{"gpt-4o", "gpt-4o-mini", "special", "claude-3", ""}
	// 含非路由格式（search）：SupportedFormats 可以声明它，但 buildRoutes 不为它建桶。
	routeRandomFormats = append(domain.RoutableFormats(), domain.FormatOpenAISearch)
)

// randomRouteList 从 pool 里取 0–n 个元素（允许重复、允许空切片非 nil）。
func randomRouteList[T any](rng *rand.Rand, pool []T, n int) []T {
	out := make([]T, 0, n)
	for range rng.Intn(n + 1) {
		out = append(out, pool[rng.Intn(len(pool))])
	}
	return out
}

func randomRouteTemplate(rng *rand.Rand) *domain.Template {
	tpl := &domain.Template{
		CredentialType:   routeRandomCredTypes[rng.Intn(len(routeRandomCredTypes))],
		SupportedFormats: randomRouteList(rng, routeRandomFormats, 3),
		Models:           randomRouteList(rng, routeRandomModels, 3),
	}
	if rng.Intn(2) == 0 {
		tpl.FormatModels = make(map[domain.RequestFormat][]string, 2)
		for range 1 + rng.Intn(2) {
			f := routeRandomFormats[rng.Intn(len(routeRandomFormats))]
			// 0 个元素 = **空非 nil** 列表（该格式全禁）；nil 与空在 FormatSupports 里同义，
			// 但 map 键存在与否分叉出两条分支，两种都必须出现。
			tpl.FormatModels[f] = randomRouteList(rng, routeRandomModels, 2)
		}
	}
	if rng.Intn(3) == 0 {
		tpl.ModelMapping = make(domain.ModelMapping, 2)
		for range 1 + rng.Intn(2) {
			mode := domain.ModelMappingModeExplicit
			if rng.Intn(2) == 0 {
				mode = domain.ModelMappingModeImplicit
			}
			tpl.ModelMapping[routeRandomModels[rng.Intn(len(routeRandomModels))]] = domain.ModelMappingEntry{
				MappedModel: routeRandomModels[rng.Intn(len(routeRandomModels))],
				Mode:        mode,
			}
		}
	}
	return tpl
}

func randomRoutePool(rng *rand.Rand) []*accountSnapshot {
	n := 1 + rng.Intn(6)
	tpls := make([]*domain.Template, 0, n)
	for range n {
		switch r := rng.Intn(10); {
		case r < 2:
			tpls = append(tpls, nil) // tpl == nil 账号混入
		case r < 4 && len(tpls) > 0:
			// 内容相同、指针不同：生产形状，专打去重命中路径
			prev := tpls[rng.Intn(len(tpls))]
			if prev == nil {
				tpls = append(tpls, randomRouteTemplate(rng))
				continue
			}
			cp := *prev
			tpls = append(tpls, &cp)
		default:
			tpls = append(tpls, randomRouteTemplate(rng))
		}
	}
	return routePool(tpls...)
}

// TestBuildRoutesRandomizedDifferential 是去重键正确性的**组合**网：表驱动天然测不到
// "credType × Models × FormatModels × Mapping × SupportedFormats × 账号序"的交叉，
// 固定种子随机池能。种子写死 ⇒ 失败可复现（池内容会打印出来）。
func TestBuildRoutesRandomizedDifferential(t *testing.T) {
	const cases = 2400
	rng := rand.New(rand.NewSource(20261121))
	for i := range cases {
		pool := randomRoutePool(rng)
		requireRoutesEquivalent(t, fmt.Sprintf("随机池 #%d（正序）", i), pool)
		rev := slices.Clone(pool)
		slices.Reverse(rev)
		requireRoutesEquivalent(t, fmt.Sprintf("随机池 #%d（反序）", i), rev)
	}
}

// --- 去重键自身的性质（spec §6 风险表的反方向） ---

// routeFactsKeyOf 用生产构造函数派生去重键：测的是"模板侧源字段 → planKey → 投影"
// 这条真实链路，而不是手搓的 struct。位置参数**故意**与 tplRouteFactsKey 的字段顺序
// 对齐：谁往键里加第 6 维，这里编译失败，逼他补一条注入用例。
func routeFactsKeyOf(tpl *domain.Template) tplRouteFactsKey {
	av := newSnapshotStatic(domain.Account{ID: 1}, tpl, nil, directEgress(), nil)
	k := av.key
	return tplRouteFactsKey{k.credentialType, k.modelsDigest, k.formatModelsKey, k.supportedFormats, k.modelMappingKey}
}

func routeFactsBaseTemplate() *domain.Template {
	return &domain.Template{
		CredentialType:   credential.TypeAPIKey,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatAnthropic},
		Models:           []string{"gpt-4o", "special"},
		FormatModels:     map[domain.RequestFormat][]string{domain.FormatAnthropic: {"special"}},
		ModelMapping:     domain.ModelMapping{"alias": {MappedModel: "up-alias", Mode: domain.ModelMappingModeExplicit}},
	}
}

// TestTplRouteFactsKeyCoversEveryRoutingFact 生产构造链（`newSnapshotStatic`）上的五维
// 性质：内容相同 ⇒ 键相等；任一 routing fact 不同 ⇒ 键不等（5 维各注入一次）。
//
// 口径（oracle Important-3）：这**不是**独立的摘要 oracle——它复用生产构造链算出的摘要，
// 故摘要算法本身写错时它也可能一起绿；独立语义网是化石实现上的形状用例与随机差分。
// 这条只回答"当前生产摘要的粒度是否分得开这 5 个维度"，而那正是去重键的前提。
//
// 反方向（planKey 字段被删/改）由 static_read_closure_test.go 的形状门禁守；
// 两条合起来才排除"错合并"。
func TestTplRouteFactsKeyCoversEveryRoutingFact(t *testing.T) {
	base := routeFactsBaseTemplate()
	k0 := routeFactsKeyOf(base)

	same := routeFactsBaseTemplate()
	require.Equal(t, k0, routeFactsKeyOf(same), "内容相同（指针不同）⇒ 去重键必须相等（生产形状靠它去重）")

	mutations := []struct {
		name   string
		mutate func(*domain.Template)
	}{
		{"credentialType", func(tpl *domain.Template) { tpl.CredentialType = credential.TypeBPS }},
		{"Models", func(tpl *domain.Template) { tpl.Models = []string{"gpt-4o"} }},
		{"FormatModels", func(tpl *domain.Template) {
			tpl.FormatModels[domain.FormatAnthropic] = []string{"gpt-4o"}
		}},
		{"SupportedFormats", func(tpl *domain.Template) {
			tpl.SupportedFormats = []domain.RequestFormat{domain.FormatAnthropic}
		}},
		{"ModelMapping", func(tpl *domain.Template) {
			tpl.ModelMapping["alias2"] = domain.ModelMappingEntry{
				MappedModel: "up-alias2", Mode: domain.ModelMappingModeImplicit,
			}
		}},
	}
	for _, m := range mutations {
		tpl := routeFactsBaseTemplate()
		m.mutate(tpl)
		require.NotEqual(t, k0, routeFactsKeyOf(tpl),
			"%s 是 routing fact：变了去重键就必须变，否则两个语义不同的模板会被合并", m.name)
	}
}

// --- 生产形状（A3000_M100）上的等价性 ---

const (
	routeBenchAccounts = 3000
	routeBenchModels   = 100
)

// routeBenchDomainAccounts 是 A3000_M100 的生产形状夹具，**两个维度都写死**
// （spec §4.5 第 4 条）：
//
//   - **每账号独立 `*domain.Template` 指针**：U22 第一步之后生产上同一次载入里同
//     template_id 已共享同一枚指针，但本夹具**刻意保持每账号一份**：它证明
//     tplRouteFactsKey 不靠指针身份（共享指针 / 同内容不同指针 / 同 ID 不同内容
//     三种形状由本文件下方的等价用例覆盖）⇒
//     指针去重在生产恒不命中，夹具必须让它在基准里也占不到便宜；
//   - **顺序恶意**：2999 个不命中热模型 m-000 的白名单账号在前（每个各持 99 个模型，
//     在"查询模型 = m-000"上必然失配），真正服务 m-000…m-099 的模板排**最后** ——
//     按"首个命中就停"写的假优化拿不到按账号数计的便宜。
//
// 顺序只影响基准（旧实现无早退）；键集与顺序无关，等价性断言因此不依赖夹具顺序。
func routeBenchDomainAccounts() []*domain.Account {
	shadow := make([]string, 0, routeBenchModels-1)
	for i := 1; i < routeBenchModels; i++ {
		shadow = append(shadow, fmt.Sprintf("m-%03d", i))
	}
	all := make([]string, 0, routeBenchModels)
	for i := range routeBenchModels {
		all = append(all, fmt.Sprintf("m-%03d", i))
	}
	formats := domain.RoutableFormats()
	tpl := func(models []string, withFormatModels bool) *domain.Template {
		t := &domain.Template{
			SupportedFormats: slices.Clone(formats),
			Models:           slices.Clone(models),
		}
		if withFormatModels {
			// 逐格式白名单让旧实现的 Serves 多扫一遍列表（§4.1 第 1 项的第二半成本）。
			t.FormatModels = map[domain.RequestFormat][]string{domain.FormatAnthropic: slices.Clone(models)}
		}
		return t
	}

	accs := make([]*domain.Account, 0, routeBenchAccounts)
	for i := range routeBenchAccounts - 1 {
		accs = append(accs, &domain.Account{
			ID: int64(i + 1), Template: tpl(shadow, true), UpstreamKey: "k",
			Enabled: true, MaxConcurrency: 2, LifecycleRevision: 1, IdentityRevision: 1,
		})
	}
	accs = append(accs, &domain.Account{
		ID: routeBenchAccounts, Template: tpl(all, false), UpstreamKey: "k",
		Enabled: true, MaxConcurrency: 2, LifecycleRevision: 1, IdentityRevision: 1,
	})
	return accs
}

// routeBenchPool 由同一批领域账号派生快照池（每账号一份独立叶）。
func routeBenchPool(tb testing.TB) []*accountSnapshot {
	tb.Helper()
	accs := routeBenchDomainAccounts()
	pool := make([]*accountSnapshot, 0, len(accs))
	for _, a := range accs {
		pool = append(pool, routeAccount(a.ID, a.Template))
	}
	return pool
}

// TestBuildRoutesEquivalenceA3000M100 在生产形状（3000 账号 × 100 模型）上跑一次
// 全量差分：既有"去重会不会丢桶"的整组证据，也是基准夹具的自检（夹具退化 ⇒ 这里先红）。
func TestBuildRoutesEquivalenceA3000M100(t *testing.T) {
	pool := routeBenchPool(t)
	requireRoutesEquivalent(t, "A3000_M100", pool)
	require.Len(t, buildRoutes(pool), len(domain.RoutableFormats())*routeBenchModels,
		"5 个格式 × 100 个模型，白名单账号不产生默认桶")
}
