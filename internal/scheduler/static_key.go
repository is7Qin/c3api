// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"crypto/sha256"
	"encoding/binary"
	"slices"
	"sort"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/egressapi"
)

// planKey 是编译计划/决策的有效性判据（值语义）：已编译候选在预留与复用
// 检查中比较的是它，不是叶子指针。字段集 = 计划的全部静态输入——门禁与
// Selection 装配从当前叶读到的每个静态事实都落在这里；反之，落在 payloadKey
// 的载荷变化只换新叶子内容，不作废在途计划。
//
// 比较规则第 2 项（`CandidateFingerprint` 本身按值比较）在这里**未接线**：
// 它按值覆盖 accountID/templateID/credential_type/生效 baseURL/stripImageTools/
// 上游鉴权键/凭据摘要/installation_id/codexAccountID，这些事实**均已逐项**落在
// 下列字段里（provider 身份事实与凭据原文折叠为 factsDigest：FactsDigest），
// 故覆盖面无缺口。
//
// **provider 事实不再逐项命名**（U40）：`codexAccountID` / `codexInstallation` /
// `codexEmail` / `bpsProfileDigest` / `credentialPairDigest` 五枚按 provider 命名的
// 字段折叠成一枚 `factsDigest`（字段数 26 → 22）。加 provider 只改 ProviderFacts 的
// 槽位与编码段，本结构体的字段数恒定——provider 知识不再长进调度核心键的形状里。
// `codexEmail` 顺手删掉：它不是决策输入（domain.CandidateFingerprint 的 hashFields
// 实参清单里从来没有它），此前唯一的作用是「改 email 白重建一枚叶子」。
//
// 两枚 struct 都只含值语义字段（标量 / `[32]byte` 摘要），`==` 仍是唯一算子
// （沿用 static_key_test.go 的反射守卫）。
type planKey struct {
	// --- §5.5 账号侧（静态快照消费 = 是） ---
	accountID  int64
	name       string
	templateID int64
	baseURL    string // 生效 baseURL：账号覆盖优先，否则模板值（覆盖率最广，同时覆盖 §5.5 的 base_url 行与 §5.5b 的 Template.BaseURL 行）
	// 声明表里的 upstream_key 行不单独占字段：它**只**因会把凭据身份换掉而进键，
	// 该语义由 factsDigest 承载（上游鉴权键是它的段 4，见 static_facts.go）。
	// 改前它是独立的 string 字段，U39 把它与 pat 原文一起折叠为凭据 pair 摘要，
	// U40 再把那枚摘要并入 factsDigest。
	maxConcurrency           int
	enabled                  bool
	cacheDomain              string // 规范化：nil 与 "" 对消费者等价（cacheDomainForAccount）
	upstreamCostMultiplierBp int
	identityRevision         int64 // K（spec §5.5 注：K 会进编译事实/候选/wire）
	// supplierUserID 供应商归属（spec 2026-10-09 §2.5/§4.6）：**非身份类**
	// （归属变更不推进 K），但它是**供给准入门**的静态判据——带归属账号在财务
	// 快照未就绪时不得入选调度。落在键内使归属变更换新叶子 ⇒ 预留谓词读到的是
	// 当前归属（否则叶子复用会带来陈旧归属读，正是静态读闭包门要防的缺陷）。
	supplierUserID int64
	// proxyDigest 出口代理规格摘要（spec-proxy-pool §3.4/§4.1）：scheme/host/
	// port/认证四项的 egressapi.Spec.Key()。**身份类**（换出口 = 换上游看到的身份
	// 第三维 ⇒ 本来就会推 K），但摘要仍独立进键：K 只证「变过」，计划还需要
	// 「现在走哪条线」这一事实；未绑定 = 零值 Spec 的摘要（与任何真实代理不同）。
	proxyDigest [16]byte
	// routable 账号当前**能不能被派发**（唯一决策位，spec §3.4/§4.1/§4.7）。
	// 派生规则：enabled ∧（未绑定 ∨ 绑定可用），由 newSnapshotStatic 一次定型。
	//
	// 为什么它必须进键：代理 `enabled` 翻转**不碰任何账号字段**（proxy 行自己变了，
	// account 的 K/C 都不动）。若只有 proxyDigest 而没有这一位，"同一个 spec、可用性
	// 从 true 变 false"的两次重载会键判等 ⇒ 复用旧叶子 ⇒ 旧叶子仍带着"可派发"的
	// 结论继续入选。它是 K 与 proxyDigest 之外的**第三个变化源**，必须独立入键。
	routable bool

	// --- 凭据面消费子集（决策输入部分） ---
	//
	// 这一面改前是五枚按 provider 命名的字段（codexAccountID / codexInstallation /
	// codexEmail / bpsProfileDigest / credentialPairDigest），本单元全部折叠进
	// 结构体末尾的 factsDigest。它们读到的静态事实**一条都没少**——读路径仍全在
	// newSnapshotStatic，只是落点从五枚字段换成同一枚摘要（静态读闭包门禁的
	// 「键字段集 ≡ 消费字段集」因此仍是 22 项的相等断言，不是放宽）。

	// --- §5.5b(1) 模板侧源字段（I 不覆盖的那些） ---
	credentialType   credential.Type
	stripImageTools  bool
	modelsDigest     [32]byte
	formatModelsKey  [32]byte
	supportedFormats [32]byte
	modelMappingKey  [32]byte
	// upstreamPathsKey custom 的逐格式路径（credential_type=custom 专用）。
	// 路径直接决定上游 URL，改配置必须换新计划，否则旧计划继续打到旧端点。
	upstreamPathsKey [32]byte
	// codexResponsesTransport 原生 codex responses 的出站传输（spec §5）。
	// 归一取 Key()：""/"http" 同键，"ws" 独立键，非法值折默认面。
	//
	// 为什么必须进键：`sameStatic := staticKeyOf(oldAv) == staticKeyOf(av)` 命中时
	// 整片静态叶子被复用，`reserveOnView` 从 `cur.static.Load()` 读 `curAv.tpl`
	// ⇒ 键里没有该维度时，模板把开关改成 ws 之后仍然读旧 `tpl`，**开关永远不生效**。
	codexResponsesTransport string

	// --- §5.5 group_ids 行（Slice 类事实以规范序摘要入键） ---
	groupIDsDigest [32]byte

	// factsDigest 是「provider 身份事实 + 凭据原文」的 32 字节摘要（U40）：
	// 取代 codexAccountID / codexInstallation / bpsProfileDigest /
	// credentialPairDigest，并删掉死字段 codexEmail。加 provider 只改
	// ProviderFacts 的槽位与编码段，本结构体字段数恒定。
	//
	// 摘要**不承载「哪个事实变了」**：凭据轮转与身份变更在摘要上不可区分。
	// 这是有意的——两者都只要求「换新叶子」；OAuth 令牌轮转走 payloadKey，
	// 身份变更走 K。
	//
	// 为什么它仍在**末尾**：`==` 对结构体按字段逐个比，32 字节定长数组是这里最贵
	// 的单字段比较；放末位后，凡在其它维度上就能判"不等"的分支（如组归属变更）
	// 永远不会付这笔钱。它与 $S.acc.UpstreamKey / $S.acc.Ext.CodexAccountID /
	// $S.acc.Ext.CodexPATKey / $S.installationID / $S.bpsProfileDigest 的映射关系
	// 不变（static_read_closure_test.go 的五条路径都指到这一枚字段），只是表示与
	// 位置变了。
	factsDigest [32]byte
}

// specOfDomain 把领域代理对象降为出口规格值（egressapi.Spec）——**唯一转换点已
// 下沉到 domain**（spec account-model-test §4.2：调度静态键与探测执行共用
// (*domain.Proxy).EgressSpec，不得在别处重新拼 scheme/host/port）。nil（未绑定）
// = 零值 Spec（摘要 = 空规格摘要，直连是一个确定状态，不是缺失）。
func specOfDomain(p *domain.Proxy) egressapi.Spec {
	return p.EgressSpec()
}

// routableFor 派生唯一决策位 Routable：**enabled ∧（未绑定 ∨ 绑定可用）**。
//
// 四个装载状态在这里收敛成一个 bool，不可用的**原因**（代理禁用 vs 绑定缺失）不
// 进决策面——需要时按 `ProxyID != nil && Proxy == nil`（缺失）/ `Proxy != nil &&
// !Proxy.Enabled`（禁用）现推，只用于日志与回显（spec §4.7、§5.1）。
//
// 形参是**值/指针本身**而不是 `*snapshotStatic`：让 `Proxy.Enabled` 这一跳发生在
// 纯函数里，调用点因此可以把整指针交进来，不必自己下钻成 `$S.acc.Proxy.Enabled`。
//
// 但函数边界**本身不是门禁**：`routableFor(av.acc.Proxy.Enabled, ...)` 一样编译通过，
// 扫描器也不会追进形参——把"原料只许在 reload 期读"寄托在这条约定上是不成立的。
// 拦住它的是 static_read_closure_test.go 的"路径 × 函数"断言（proxyMaterialPathFuncs）：
// 谁在消费期读 `$S.acc.Proxy` / `$S.acc.ProxyID` / `$S.acc.Enabled`，那条断言就红，除非
// 在表里写明理由。
//
// 那条断言管**形状**（谁读），管不了**用途**：已经为装配而读的字段可以被拿去做别的
// 判断（`if av.acc.ProxyID != nil { return false }` 不新增任何读者）。所以"绑了可用代理
// 的账号必须仍被选中、绑了不可用代理的必须不被选中"由行为用例断言
// （TestSelectEgressAlwaysResolved / TestSelectSkipsUnroutableProxy）——改变结论的
// 过滤器在那里红。门禁与用例各守一半，缺一不可。
func routableFor(enabled bool, proxyID *int64, px *domain.Proxy) bool {
	if !enabled {
		return false
	}
	if proxyID == nil {
		return true // 未绑定 = 直连（出口恒可用，不会"缺失"）
	}
	// 绑定：行被软删/不存在（px == nil）或代理被禁用 ⇒ 不可用（fail-closed，
	// spec L8：残留窗口固定按不可用处置，与"删除即解绑"不冲突）。
	return px != nil && px.Enabled
}

// payloadKey 是只影响"叶子内容是否新鲜"的载荷：OAuth 访问令牌三元组只用于
// 上游鉴权（stableCredentialDigest 对 codex-oauth 返回常量，token/refresh/
// expiry 完全不进指纹），不是任何路由决策的输入。载荷变化必须换新叶子
// （SDK 拿到新 token，staticKey 变）但不得作废在途计划（planKey 不变）。
//
// 三枚明文字段（codexOAuthToken / codexOAuthRefresh / codexOAuthExpires）已折叠成
// 一枚 32 字节摘要（P3a）：键类型里不再出现凭据原文。摘要由 newSnapshotStatic 在
// **构造期一次定型**（payloadDigestOf），$S.payload 持有，payloadKeyOf 只读回——
// 比较路径因此不碰令牌原文，也不会因为源对象被就地改写而漂移。
//
// 摘要**不承载「哪个令牌变了」**：轮转只换叶子，不作废在途计划（那是 K 的职责）。
// [32]byte 是这里唯一允许的载体：[32]byte 可比较（`==` 仍是键的唯一算子），且与
// 其它事实族（factsDigest / groupIDsDigest）同一种表示。
type payloadKey struct {
	payloadDigest [32]byte
}

// staticKey 是快照静态事实的**可比较值类型**（spec §5.5b「结构保证」）：
// sameStatic := staticKeyOf(old) == staticKeyOf(new)，算子是 `==`。
//
// 叶子复用判据 = 决策输入 ∪ 载荷：凭据值（含 OAuth token）必须入键——否则
// 凭据轮转后键判等 ⇒ 复用旧叶 ⇒ 上游拿旧令牌 401（实测：
// TestInvalidateAccountReloadsExt 失败）。而「token 刷新不该改计划有效性」
// 由 planKey 承担：两件事不共用一个比较。
//
// §5.5b 比较规则第 6 项（快照容器完备性）：snapshotStatic 的每个字段都落入键或由
// 已比较项派生——acc → 账号侧行；tpl → 模板侧行；groupIDs → group_ids 行；
// gid → min(groupIDs) 纯派生（见 buildSnapshots）。
//
// **本类型必须不含任何指针/切片/映射字段**：含指针的 struct 仍然「可比较」，
// 但 `==` 对指针只比**地址**；键是每次重载重建的（buildSnapshots 每次 new
// snapshotStatic），只要键里有一个指向「该次重载新建对象」的指针（state.go 的
// tpl/Ext，routing_compiler_candidates.go 内嵌的 account/static），键就
// **恒不等** ⇒ 规则重新变惰性——即本 spec 要消灭的那个缺陷。Slice/Map 类事实一律
// 以**规范序 32 字节摘要**（[32]byte，kind = reflect.Array，保持 struct 可比较）入键。
// 守卫见 static_key_test.go。
type staticKey struct {
	planKey
	payloadKey
}

// tplDigests 是模板侧的 5 项摘要——**只**依赖模板内容，与账号/组归属无关
// （缓存单位是"一枚模板对象"，混进任何账号侧事实都会让同模板的第二个账号
// 读到第一个账号的结果）。
type tplDigests struct {
	modelsDigest     [32]byte
	formatModelsKey  [32]byte
	supportedFormats [32]byte
	modelMappingKey  [32]byte
	upstreamPathsKey [32]byte
}

// templateDigestCache 是**一次 reload / 一个失效批次**作用域内的模板摘要表。
// 键 = *domain.Template 指针：第一步（35142e74）之后，同一次载入里同一个
// template_id 只有一枚 domain 对象，指针因此是免费且精确的键（templateID 在
// 手工夹具里会撞——同 ID 不同内容不得合并，见 build_routes_equiv_test.go）。
//
// 生命周期必须与作用域同寿，不能更长：摘要是模板内容的函数。跨作用域复用
// 同一张表，模板对象一旦被换内容（或同 ID 换了一枚新对象），命中就返回**陈旧
// 摘要** ⇒ planKey 与指针内容分叉 ⇒ 旧叶被复用而消费者读到新内容。作用域 =
// 一次构建，与批次的载入会话同寿命（scheduler 侧是 batchState；会话自持的模板表
// 在实现包内，scheduler 看不见它）。
//
// 计数器是验收仪器（spec §4.3）：misses = 不同模板数，hits = 构造次数 − 不同
// 模板数。没有它们，"收下缓存但从不读"（cheat_noop）会让全部不变量绿。
type templateDigestCache struct {
	entries map[*domain.Template]tplDigests
	hits    int
	misses  int
}

// newTemplateDigestCache 建一张空的摘要表。写成 var 而非 func **只为**让验收
// 用例抓住 invalidateGroupsLocked 内部建的那一张：批次作用域无法从外部观测，
// :188/:245 若漏传缓存，唯一可观测面就是命中数少一截。生产路径从不改写它。
var newTemplateDigestCache = func() *templateDigestCache {
	return &templateDigestCache{entries: make(map[*domain.Template]tplDigests)}
}

// newSnapshotStatic 是 snapshotStatic 的**唯一构造函数**：一次派生全部 planKey
// 并随不可变叶持有（planKeyOf 直接返回 av.key）。派生**内联**在本函数内（不得抽
// 独立函数——否则静态读闭包门禁的读者归属变为该函数名）；先组装局部 k 再**单次**
// av.key = k 赋值（不得逐字段写 av.key.x = …，否则门禁新增 $S.key.* 读路径）。
// tpl 可为 nil：模板侧摘要取零值（保留零键契约，不 panic）。
// digests 可为 nil：nil = 逐次计算（不缓存、不计数，既有语义逐字节不变）；非 nil =
// 该作用域内每枚模板只派生一次（5 项摘要占每叶 214 allocs 的 209）。
//
// 生效 baseURL 与编译器同源（routing_compiler_candidates.go 的 baseURL 派生）：模板
// 值为底，账号覆盖非空时优先。两者必须是同一套优先级，否则判据会在编译器认为
// 「变化了」的场景下判等（或反之），复用分支与编译事实就此分叉。
//
// eg 是**已解析好的**出口 client（buildSnapshots 解析、恒非 nil）：把"解析"留在
// 根不在静态读观测集内的 buildSnapshots，把"使用"留在热路径（spec §4.3）。
func newSnapshotStatic(acc domain.Account, tpl *domain.Template, groupIDs []int64, eg *egressapi.Client, digests *templateDigestCache) *snapshotStatic {
	av := &snapshotStatic{acc: acc, tpl: tpl, groupIDs: append([]int64(nil), groupIDs...), egress: eg}
	// 唯一决策位在**构造期一次定型**（不是装载器的活儿，也不是热路径的判断）：
	// 之后候选过滤与计划 fence 都只读这一个 bool。放在 key 派生之前，使
	// `k.routable = av.routable` 与字段派生同源、不可能失配。
	av.routable = routableFor(av.acc.Enabled, av.acc.ProxyID, av.acc.Proxy)
	// 会话路径的两项账号级事实同样构造期定型（预留只拷字段，零分配）。仅 Codex
	// 凭据填：非 Codex 面不走会话簿，零值即"无会话路径事实"。
	if av.tpl != nil && credential.ChannelOf(av.tpl.CredentialType) == credential.ChannelCodex {
		av.cacheDomain = cacheDomainForAccount(av.acc.ID, av.acc.CacheDomain)
		av.installationID = installationIDOf(av.acc.Ext)
	}
	// BPS profile 摘要：类型门与 ext 判空都在 domain.BPSProfileDigest 内（非 BPS
	// 账号残留的 profile 不算摘要）。tpl == nil 时不调用——没有模板就没有类型。
	if av.tpl != nil {
		av.bpsProfileDigest = domain.BPSProfileDigest(av.tpl.CredentialType, av.acc.Ext)
	}
	// 载荷摘要：三枚凭据原文的读点**只在这里**（P3a）。改前它们在 payloadKeyOf 里
	// 逐次读——那与"构造期缓存"直接矛盾（缓存写了没人读，且每次比较都要付一遍明文
	// 解引用）。搬进来之后，"调度包里只有构造函数读 OAuth 令牌"成为机械事实
	// （static_read_closure_test.go 的 payloadProjectionFuncs 是**双向相等**断言）。
	//
	// 空三元组也照样取摘要（ext == nil 与全空字段因此同键，沿用改前语义：nil 与
	// ptr("") 同键、nil expires 与 UnixNano 0 同键）。代价是一次 24B 前像的 sha256
	// ——构造期每叶一次，换来比较期零明文读。
	var (
		oauthToken   string
		oauthRefresh string
		oauthExpires int64
	)
	if ext := av.acc.Ext; ext != nil {
		oauthToken = derefString(ext.CodexOAuthToken)
		oauthRefresh = derefString(ext.CodexOAuthRefreshToken)
		if ext.CodexOAuthExpiresAt != nil {
			oauthExpires = ext.CodexOAuthExpiresAt.UnixNano()
		}
	}
	av.payload = payloadKey{payloadDigest: payloadDigestOf(oauthToken, oauthRefresh, oauthExpires)}
	var k planKey
	k.accountID = av.acc.ID
	k.name = av.acc.Name
	k.templateID = av.acc.TemplateID
	// 生效 baseURL：优先级只在 domain.EffectiveBaseURL 里定义一次（路由的
	// AccountCandidateFingerprint 读的是同一个函数）。BPS 固定端点忽略账号覆盖；
	// tpl == nil 时账号覆盖仍然生效（零键形状的既有语义）。
	k.baseURL = domain.EffectiveBaseURL(av.tpl, &av.acc)
	// 凭据原文不再单独入键，但读点与改前逐行同源（闭包门禁把 `$S.acc.UpstreamKey`
	// 与 `$S.acc.Ext.CodexPATKey` 都登记到 factsDigest 上）：provider 身份事实与
	// 凭据原文在下面合成一枚摘要，不再各占一枚字段。
	k.maxConcurrency = av.acc.MaxConcurrency
	k.enabled = av.acc.Enabled
	k.cacheDomain = normalizeCacheDomain(av.acc.CacheDomain)
	k.upstreamCostMultiplierBp = domain.MultBp(av.acc.UpstreamCostMultiplierBp)
	k.identityRevision = av.acc.IdentityRevision
	k.supplierUserID = av.acc.SupplierUserID
	k.proxyDigest = specOfDomain(av.acc.Proxy).Key()
	k.routable = av.routable
	// provider 事实：**读点全部在这里**（编码器只吃已读好的值，见 static_facts.go）。
	// 三个来源各有归属，没有一个是从已删字段里"顺手拿"的：
	//   - channel：模板凭据类型的通道投影（tpl == nil ⇒ 零值 ChannelStaticKey，
	//     与表外类型落同一面，沿用改前 tpl == nil 的"非 BPS"语义）；
	//   - installation：$S.installationID 叶标量（构造期只在 codex 通道定型）；
	//   - bpsProfileDigest：$S.bpsProfileDigest 叶标量（域函数已按类型门判过）。
	// PAT 仍只在非 BPS 形状进键（BPS 分支从来不读它）——该分支差异与改前逐字节
	// 一致，只是落点从独立字段变成摘要的一段。
	facts := ProviderFacts{
		Channel:          credential.ChannelStaticKey,
		InstallationID:   av.installationID,
		BPSProfileDigest: av.bpsProfileDigest,
	}
	if av.tpl != nil {
		facts.Channel = credential.ChannelOf(av.tpl.CredentialType)
	}
	var credentialPAT string
	if ext := av.acc.Ext; ext != nil {
		facts.CodexAccountID = derefString(ext.CodexAccountID)
		if facts.Channel != credential.ChannelBPS {
			credentialPAT = derefString(ext.CodexPATKey)
		}
	}
	// 上游键无条件参与（与改前一致）。
	k.factsDigest = FactsDigest(facts, av.acc.UpstreamKey, credentialPAT)
	if tpl := av.tpl; tpl != nil {
		k.credentialType = tpl.CredentialType
		k.stripImageTools = tpl.StripImageTools
		// 模板侧 5 项摘要：作用域内每枚模板只算一次。命中路径必须与逐次计算
		// **逐字节**相同；miss 分支就是 nil 语义本身（同一份代码，不另写一份）。
		d, hit := tplDigests{}, false
		if digests != nil {
			d, hit = digests.entries[tpl]
		}
		if !hit {
			d = tplDigests{
				modelsDigest:     digestStrings(tpl.Models),
				formatModelsKey:  digestFormatModels(tpl.FormatModels),
				supportedFormats: digestFormats(tpl.SupportedFormats),
				modelMappingKey:  digestModelMapping(tpl.ModelMapping),
				upstreamPathsKey: digestUpstreamPaths(tpl.UpstreamPaths),
			}
			if digests != nil {
				digests.entries[tpl] = d
			}
		}
		if digests != nil {
			if hit {
				digests.hits++
			} else {
				digests.misses++
			}
		}
		k.modelsDigest = d.modelsDigest
		k.formatModelsKey = d.formatModelsKey
		k.supportedFormats = d.supportedFormats
		k.modelMappingKey = d.modelMappingKey
		k.upstreamPathsKey = d.upstreamPathsKey
		k.codexResponsesTransport = tpl.CodexResponsesTransport.Key()
	}
	k.groupIDsDigest = digestInt64s(av.groupIDs)
	av.key = k
	return av
}

// planKeyOf 由快照静态视图读取计划有效性判据（canonical digest 复用：直接返回
// newSnapshotStatic 一次派生的 av.key）。av 为 nil（首轮无旧快照）时返回零值键；
// 调用方不得依赖零值比较做缺席判定——fence 必须显式处理缺席。
func planKeyOf(av *snapshotStatic) planKey {
	if av == nil {
		return planKey{}
	}
	return av.key
}

// payloadKeyOf 由快照静态视图读取**构造期缓存**的载荷摘要（$S.payload）：零调用、
// 零 $S.acc 路径。av 为 nil（首轮无旧快照）时返回零值键。
//
// 它不再派生任何东西——这是 P3a 的全部意义：载荷判据在叶构造时定型一次，比较期
// 只是读回一枚 [32]byte（改前是每次比较都解引用三枚明文字段）。
func payloadKeyOf(av *snapshotStatic) payloadKey {
	if av == nil {
		return payloadKey{}
	}
	return av.payload
}

// staticKeyOf 由快照静态视图派生比较键。av 为 nil（首轮无旧快照）时返回零值键；
// 零值键只在两侧都为空时相等（首轮无复用分支，oldByID 为空 map）。
//
// 两枚子键从**同一份**快照派生，字段归属只声明一次：planKeyOf 读决策输入（含
// 构造期定型的 factsDigest），payloadKeyOf 读载荷摘要（$S.payload）。两枚都是缓存
// 读回——本函数体内不存在任何摘要计算（sha256 / FactsDigest / payloadDigestOf），
// 这条由 payload_digest_gate_test.go 的 H1（AST 门禁）机械保证：谁在这里现场算
// 摘要，谁就把"构造期一次定型"变成了"比较期再次派生"，键与叶会从两条路径分叉。
//
// 出口摘要这一项**从原料重算**（`specOfDomain(acc.Proxy).Key()`），不读 av.key 的
// 缓存位：复用比较与构造期键因此各自独立地从 acc.Proxy 派生——任一处漏写出口都不会
// 被另一处掩盖。其余字段仍是缓存键的读回（它们的源是标量/切片，构造期已一次定型；
// 逐项重算只会把每次 reload 的成本翻倍，而不增加任何证据）。
func staticKeyOf(av *snapshotStatic) staticKey {
	k := staticKey{planKey: planKeyOf(av), payloadKey: payloadKeyOf(av)}
	if av != nil {
		k.planKey.proxyDigest = specOfDomain(av.acc.Proxy).Key()
	}
	return k
}

// normalizeCacheDomain 把 nil 与 "" 归并为同一个空串：cacheDomainForAccount
// （cache_affinity.go 的 cacheDomainForAccount）对二者都回私有域，消费者等价 ⇒ 键不得制造比
// 消费面更细的差异。
func normalizeCacheDomain(d *string) string {
	if d == nil {
		return ""
	}
	return *d
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// digestStrings 对字符串列表取规范序摘要（先排序，再长度前缀串联，最后 sha256）。
func digestStrings(items []string) [32]byte {
	cp := append([]string(nil), items...)
	sort.Strings(cp)
	return digestParts(len(cp), func(i int) []byte { return []byte(cp[i]) })
}

// digestInt64s 对 int64 列表取规范序摘要（排序 + 长度前缀串联 + sha256）。
func digestInt64s(items []int64) [32]byte {
	cp := append([]int64(nil), items...)
	slices.Sort(cp)
	var buf [8]byte
	return digestParts(len(cp), func(i int) []byte {
		binary.BigEndian.PutUint64(buf[:], uint64(cp[i]))
		return buf[:]
	})
}

// digestFormats 对格式列表取规范序摘要（排序按字符串序，与域内 SupportedFormats
// 的元素语义一致）。
func digestFormats(items []domain.RequestFormat) [32]byte {
	cp := append([]domain.RequestFormat(nil), items...)
	slices.Sort(cp)
	return digestParts(len(cp), func(i int) []byte { return []byte(cp[i]) })
}

// digestFormatModels 对 format → 模型列表映射取规范序摘要：键排序，值列表各自
// 排序，键与每个元素都带长度前缀，防「键值边界」歧义。
func digestFormatModels(m map[domain.RequestFormat][]string) [32]byte {
	if len(m) == 0 {
		return digestParts(0, nil)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, string(k))
	}
	sort.Strings(keys)
	// 每个键固定贡献「键 + 值个数」两个 part，每个值贡献一个 part；预先算好容量。
	n := 0
	for _, k := range keys {
		n += 2 + len(m[domain.RequestFormat(k)])
	}
	parts := make([][]byte, 0, n)
	for _, k := range keys {
		parts = append(parts, []byte(k))
		vals := append([]string(nil), m[domain.RequestFormat(k)]...)
		sort.Strings(vals)
		parts = append(parts, []byte(itoa(len(vals))))
		for _, v := range vals {
			parts = append(parts, []byte(v))
		}
	}
	return digestParts(len(parts), func(i int) []byte { return parts[i] })
}

// digestUpstreamPaths 对 custom 路径表取规范序摘要：键排序后每项贡献「键」
// 与「值」两个 part，配合 digestParts 的 8 字节大端长度前缀消除拼接歧义
// （裸拼接会让 {"ab":"c"} 与 {"a":"bc"} 同摘要，改路径后计划不失效）。
func digestUpstreamPaths(m map[string]string) [32]byte {
	if len(m) == 0 {
		return digestParts(0, nil)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([][]byte, 0, len(keys)*2)
	for _, k := range keys {
		parts = append(parts, []byte(k), []byte(m[k]))
	}
	return digestParts(len(parts), func(i int) []byte { return parts[i] })
}

// digestModelMapping 对模型映射取规范序摘要（键排序；条目的映射目标与模式都入摘要）。
func digestModelMapping(m domain.ModelMapping) [32]byte {
	if len(m) == 0 {
		return digestParts(0, nil)
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([][]byte, 0, len(keys)*3)
	for _, k := range keys {
		e := m[k]
		parts = append(parts, []byte(k), []byte(e.MappedModel), []byte(itoa(int(e.Mode))))
	}
	return digestParts(len(parts), func(i int) []byte { return parts[i] })
}

// digestParts 把 parts（个数 n）按「长度前缀 + 内容」串联后 sha256。n 也计入摘要，
// 8 字节大端长度前缀消除拼接歧义（["ab","c"] 与 ["a","bc"] 摘要不同）。
func digestParts(n int, part func(i int) []byte) [32]byte {
	h := sha256.New()
	var lenBuf [8]byte
	binary.BigEndian.PutUint64(lenBuf[:], uint64(n))
	h.Write(lenBuf[:])
	for i := range n {
		p := part(i)
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(p)))
		h.Write(lenBuf[:])
		h.Write(p)
	}
	var out [32]byte
	h.Sum(out[:0])
	return out
}

// itoa 是 strconv.Itoa 的本地零依赖替代（摘要热路径，避免引入 strconv）。
func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
