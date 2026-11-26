// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// frozenCredentialPair 是 acc58144（本单元之前）**真正进 planKey** 的那对凭据原文。
// 口径逐行取自改前的构造函数：上游鉴权键无条件入键；PAT 只在「ext 存在 ∧ 模板非
// BPS」分支入键（BPS 分支从来不写该字段），因此 BPS / 无 ext 时 pat 恒为 ""。
type frozenCredentialPair struct {
	upstream string
	pat      string
}

// frozenPlanKey re-implements the ORIGINAL plan-key derivation independently of
// planKeyOf / newSnapshotStatic (a frozen old-algorithm oracle) **except** for the
// provider facts + credential pair, which it returns as the frozen raw facts
// instead of writing a digest into a key field.
//
// 凭据维度的 oracle 地位已被 static_key_credential_test.go 的 1125 组等价类测试
// **接管**（本单元：planKey 去载荷）。理由不是省事：改前凭据面是两枚 string 字段，
// 判据就是"逐字节相等"；改后键里只剩 sha256 摘要，在包内再"独立"写一份同款
// 长度前缀 + sha256 只是把实现抄一遍（自证），既证不出等价关系，也证不出"没有
// 合并今天不同的键"。等价类测试断言的是**关系**——新键相等 ⟺ 这对原文逐字节
// 相等——比逐值相等更强：它同时锁住两个方向（不得合并、不得拆分）。
//
// 本函数因此只证其余维度（账号/模板/组归属/出口/可用性）仍与改前同源：比较时把
// 构造函数产出的 factsDigest 抹零，再与这里的 planKey 逐字段相等；摘要本身改由
// 「冻结的事实选择 + FactsDigest」交叉验证（编码由 KAT 单独钉住）。
func frozenPlanKey(acc domain.Account, tpl *domain.Template, gids []int64) (planKey, frozenCredentialPair, ProviderFacts) {
	var k planKey
	var pair frozenCredentialPair
	var facts ProviderFacts
	k.accountID = acc.ID
	k.name = acc.Name
	k.templateID = acc.TemplateID
	if tpl != nil {
		if tpl.CredentialType.IsBPS() {
			k.baseURL = domain.BPSDefaultEndpoint
		} else {
			k.baseURL = tpl.BaseURL
		}
	}
	if (tpl == nil || !tpl.CredentialType.IsBPS()) && acc.BaseURL != nil && *acc.BaseURL != "" {
		k.baseURL = *acc.BaseURL
	}
	pair.upstream = acc.UpstreamKey
	k.maxConcurrency = acc.MaxConcurrency
	k.enabled = acc.Enabled
	k.cacheDomain = normalizeCacheDomain(acc.CacheDomain)
	k.upstreamCostMultiplierBp = domain.MultBp(acc.UpstreamCostMultiplierBp)
	k.identityRevision = acc.IdentityRevision
	k.supplierUserID = acc.SupplierUserID
	// 出口规格摘要：编码由 pkg/egress 的 TestEgressSpecOf 单独冻结；这里只断言
	// 构造函数与冻结推导同源。
	k.proxyDigest = specOfDomain(acc.Proxy).Key()
	// 可用性决策位：独立重述（不调用 routableFor——oracle 必须是**另一份**推导，
	// 否则构造函数与 oracle 同源，比对就退化成自证）。
	k.routable = acc.Enabled && (acc.ProxyID == nil || (acc.Proxy != nil && acc.Proxy.Enabled))
	// provider 事实（U40）：按改前的三条分支冻结「进 factsDigest 的那六段」。
	// 编码不在 oracle 里重写（那是把实现抄一遍）——KAT 钉字节，这里只钉**事实选择**：
	// BPS 不吃 Codex installation/PAT，installation 只在 codex 通道成型（$S.installationID
	// 的定型条件），profile 摘要只对 BPS 非空。
	if tpl != nil {
		facts.Channel = credential.ChannelOf(tpl.CredentialType)
		if facts.Channel == credential.ChannelCodex {
			facts.InstallationID = installationIDOf(acc.Ext)
		}
		facts.BPSProfileDigest = domain.BPSProfileDigest(tpl.CredentialType, acc.Ext)
	}
	if ext := acc.Ext; ext != nil {
		facts.CodexAccountID = derefString(ext.CodexAccountID)
		if facts.Channel != credential.ChannelBPS {
			pair.pat = derefString(ext.CodexPATKey)
		}
	}
	if tpl != nil {
		k.credentialType = tpl.CredentialType
		k.stripImageTools = tpl.StripImageTools
		k.modelsDigest = digestStrings(tpl.Models)
		k.formatModelsKey = digestFormatModels(tpl.FormatModels)
		k.supportedFormats = digestFormats(tpl.SupportedFormats)
		k.modelMappingKey = digestModelMapping(tpl.ModelMapping)
		// custom 上游修饰（Paths + request_patch）同样进键（编码由
		// TestDigestTemplateUpstreamKAT 单独冻结）。
		k.upstreamPathsKey = digestTemplateUpstream(tpl.Upstream)
		k.codexResponsesTransport = tpl.CodexResponsesTransport.Key()
	}
	k.groupIDsDigest = digestInt64s(gids)
	return k, pair, facts
}

// requirePlanKeyMatchesFrozen 断言 got 与冻结推导除**factsDigest** 外逐字段相等，
// 并断言 factsDigest 确实由构造期写入、且等于「冻结的事实选择」的摘要。
//
// 为什么要抹掉那一维：摘要值不能由 oracle 独立重算（包内再写一份长度前缀 + sha256
// 只是把实现抄一遍）；字节由 static_facts_kat_test.go 的 KAT 钉住，关系由
// TestPlanKeyCredentialPairEquivalenceClasses 的 1125 组等价类钉住。这里保留的是
// **事实选择**那一维：frozenPlanKey 独立陈述「哪六段进摘要」（BPS 排除 installation/PAT、
// installation 只在 codex 通道成型），把它喂给编码器再与构造器的输出比。
func requirePlanKeyMatchesFrozen(t *testing.T, got planKey, acc domain.Account, tpl *domain.Template, gids []int64, msgAndArgs ...any) {
	t.Helper()
	want, pair, facts := frozenPlanKey(acc, tpl, gids)
	require.NotEqual(t, [32]byte{}, got.factsDigest,
		"factsDigest 必须由构造期写入：零值意味着 provider 事实面整个逃逸了冻结比对")
	require.Equal(t, FactsDigest(facts, pair.upstream, pair.pat), got.factsDigest,
		"factsDigest 必须与冻结的 provider 事实口径一致（改前分支语义：BPS 不吃 pat/installation）")
	got.factsDigest = [32]byte{}
	require.Equal(t, want, got, msgAndArgs...)
}

// TestFrozenPlanKeyOracleMatchesConstructor checks the cached key against the
// frozen old-algorithm oracle for nil/empty inputs, groupIDs order permutation,
// duplicates, and content changes, and confirms the sensitivity that the key is
// meant to carry.
//
// 凭据维度（本次改动的维度）在这里只断言两件事：冻结推导的**进键 pair** 与手写
// 期望一致（BPS 残留 pat 被排除、无 ext 时为 ""），以及构造函数确实写了非零摘要
// （否则下面的抹零比较会让凭据维度整个逃逸）。凭据维度的**等价类**由
// static_key_credential_test.go 的 1125 组参照测试承担。
func TestFrozenPlanKeyOracleMatchesConstructor(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m-a", "m-b"})
	tpl.FormatModels = map[domain.RequestFormat][]string{domain.FormatOpenAIChat: {"m-a"}}
	tpl.ModelMapping = map[string]domain.ModelMappingEntry{"m-a": {MappedModel: "x", Mode: domain.ModelMappingModeExplicit}}
	a := *acc(7, tpl, 4)
	a.Ext = &domain.AccountExt{CodexAccountID: new("cid"), CodexEmail: new("e@x"), CodexPATKey: new("pat")}
	a.SupplierUserID = 42

	// BPS 模板：残留 ext pat **不进键**（改前也不进），上游鉴权键与 profile 摘要进键。
	bpsTpl := tplWith(domain.FormatOpenAIResponses, []string{"m-a"})
	bpsTpl.CredentialType = credential.TypeBPS
	bpsAcc := *acc(8, bpsTpl, 4)
	bpsAcc.Ext = &domain.AccountExt{
		CodexAccountID: new("cid"), CodexEmail: new("e@x"), CodexPATKey: new("pat-residual"),
		BPSProfile: []byte(`{"profile_id":"p","device_id":"d"}`),
	}

	// 无 ext：PAT 维度恒为 ""（改前该分支根本不写字段）。
	noExt := *acc(9, tpl, 4)
	noExt.Ext = nil

	// ext 存在但没有 pat：同样恒为 ""，与"pat 为空串"同键（改前就是同一枚 string）。
	noPat := *acc(10, tpl, 4)
	noPat.Ext = &domain.AccountExt{CodexAccountID: new("cid")}

	// ws 出站传输：改前也进键（模板开关变更必须换新叶子）。
	wsTpl := *tpl
	wsTpl.CodexResponsesTransport = domain.CodexResponsesTransportWS

	cases := []struct {
		name string
		acc  domain.Account
		tpl  *domain.Template
		gids []int64
		pair frozenCredentialPair
	}{
		{"nil-tpl-empty-gids", domain.Account{ID: 1}, nil, nil, frozenCredentialPair{}},
		{"base", a, tpl, []int64{3, 1, 2}, frozenCredentialPair{upstream: a.UpstreamKey, pat: "pat"}},
		{"permuted", a, tpl, []int64{1, 2, 3}, frozenCredentialPair{upstream: a.UpstreamKey, pat: "pat"}},
		{"duplicates", a, tpl, []int64{1, 1, 2}, frozenCredentialPair{upstream: a.UpstreamKey, pat: "pat"}},
		{"bps residual pat excluded", bpsAcc, bpsTpl, nil, frozenCredentialPair{upstream: bpsAcc.UpstreamKey}},
		{"no ext", noExt, tpl, nil, frozenCredentialPair{upstream: noExt.UpstreamKey}},
		{"ext without pat", noPat, tpl, nil, frozenCredentialPair{upstream: noPat.UpstreamKey}},
		{"ws transport", a, &wsTpl, nil, frozenCredentialPair{upstream: a.UpstreamKey, pat: "pat"}},
	}
	for _, tc := range cases {
		got := planKeyOf(newSnapshotStatic(tc.acc, tc.tpl, tc.gids, directEgress(), nil))
		want, pair, facts := frozenPlanKey(tc.acc, tc.tpl, tc.gids)
		require.Equal(t, tc.pair, pair, "%s: 冻结推导的凭据口径", tc.name)
		// 非空性：构造函数真的写了摘要。否则下面"抹零后相等"会让 provider 事实面整个逃逸。
		require.NotEqual(t, [32]byte{}, got.factsDigest, "%s: 摘要必须非零值", tc.name)
		// 事实选择与构造器同源：BPS 排除 pat/installation、codex 的 installation 进摘要。
		require.Equal(t, FactsDigest(facts, pair.upstream, pair.pat), got.factsDigest,
			"%s: factsDigest 必须与冻结的 provider 事实口径一致", tc.name)
		got.factsDigest = [32]byte{}
		require.Equal(t, want, got, "%s: 除 factsDigest 外的全部维度必须与冻结推导同源", tc.name)
	}

	// Permutation of the same groupID set is invariant (canonical digest).
	require.Equal(t,
		planKeyOf(newSnapshotStatic(a, tpl, []int64{3, 1, 2}, directEgress(), nil)),
		planKeyOf(newSnapshotStatic(a, tpl, []int64{1, 2, 3}, directEgress(), nil)),
		"groupIDs permutation must not change the key")
	// Raw duplicate elements are sensitive (digest does not de-dup).
	require.NotEqual(t,
		planKeyOf(newSnapshotStatic(a, tpl, []int64{1, 2}, directEgress(), nil)),
		planKeyOf(newSnapshotStatic(a, tpl, []int64{1, 1, 2}, directEgress(), nil)))

	// Mapping-mode change is sensitive.
	tplMode := *tpl
	tplMode.ModelMapping = map[string]domain.ModelMappingEntry{"m-a": {MappedModel: "x", Mode: domain.ModelMappingModeImplicit}}
	require.NotEqual(t,
		planKeyOf(newSnapshotStatic(a, tpl, nil, directEgress(), nil)),
		planKeyOf(newSnapshotStatic(a, &tplMode, nil, directEgress(), nil)),
		"mapping mode change must change the key")
	// Model-content change is sensitive.
	tplModels := *tpl
	tplModels.Models = []string{"m-a", "m-c"}
	require.NotEqual(t,
		planKeyOf(newSnapshotStatic(a, tpl, nil, directEgress(), nil)),
		planKeyOf(newSnapshotStatic(a, &tplModels, nil, directEgress(), nil)),
		"model content change must change the key")
	// custom upstream (paths + request_patch) is part of the key: content and
	// presence both count (the encoding itself is frozen separately by
	// TestDigestTemplateUpstreamKAT; here we only pin that the constructor
	// actually folds the field in).
	tplPaths := *tpl
	tplPaths.Upstream = &domain.TemplateUpstream{Paths: map[string]string{"openai-chat": "v1/chat/completions"}}
	tplPathsOther := *tpl
	tplPathsOther.Upstream = &domain.TemplateUpstream{Paths: map[string]string{"openai-chat": "v2/chat/completions"}}
	require.NotEqual(t,
		planKeyOf(newSnapshotStatic(a, tpl, nil, directEgress(), nil)),
		planKeyOf(newSnapshotStatic(a, &tplPaths, nil, directEgress(), nil)),
		"setting an upstream path must change the key")
	require.NotEqual(t,
		planKeyOf(newSnapshotStatic(a, &tplPaths, nil, directEgress(), nil)),
		planKeyOf(newSnapshotStatic(a, &tplPathsOther, nil, directEgress(), nil)),
		"changing an upstream path must change the key")
	// Same content in a *different* map instance must not change the key either
	// (comparing one instance with itself would assert nothing).
	tplPathsSame := *tpl
	tplPathsSame.Upstream = &domain.TemplateUpstream{Paths: map[string]string{"openai-chat": "v1/chat/completions"}}
	require.Equal(t,
		planKeyOf(newSnapshotStatic(a, &tplPaths, nil, directEgress(), nil)),
		planKeyOf(newSnapshotStatic(a, &tplPathsSame, nil, directEgress(), nil)),
		"identical upstream paths must not change the key")

	// payloadKey (OAuth rotation) must NOT affect planKey.
	aRot := a
	aRot.Ext = &domain.AccountExt{CodexAccountID: new("cid"), CodexEmail: new("e@x"), CodexPATKey: new("pat"),
		CodexOAuthToken: new("new-at")}
	require.Equal(t,
		planKeyOf(newSnapshotStatic(a, tpl, nil, directEgress(), nil)),
		planKeyOf(newSnapshotStatic(aRot, tpl, nil, directEgress(), nil)),
		"OAuth payload rotation must not change planKey")
}
