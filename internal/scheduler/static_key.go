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
	"github.com/is7qin/c3api/pkg/egress"
)

// planKey 是编译计划/决策的有效性判据（值语义）：已编译候选在预留与复用
// 检查中比较的是它，不是叶子指针。字段集 = 计划的全部静态输入——门禁与
// Selection 装配从当前叶读到的每个静态事实都落在这里；反之，落在 payloadKey
// 的载荷变化只换新叶子内容，不作废在途计划。
//
// 比较规则第 2 项（`CandidateFingerprint` 本身按值比较）在这里**未接线**：
// 它按值覆盖 accountID/templateID/credential_type/生效 baseURL/stripImageTools/
// 上游鉴权键/凭据摘要/installation_id/codexAccountID，这些事实**均已逐项**落在
// 下列字段里（凭据原文折叠为 credentialPairDigest：hashCredentialPair），
// 故覆盖面无缺口。
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
	// 该语义由凭据面的 credentialPairDigest 承载（见该字段）。改前它是独立的
	// string 字段，本单元把它与 pat 原文一起折叠为摘要。
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
	// port/认证四项的 egress.Spec.Key()。**身份类**（换出口 = 换上游看到的身份
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
	// codexEmail 落在这里：AccountCandidateFingerprint 把 Ext.CodexEmail 读入并
	// 传给 CandidateFingerprint——它位于决策路径，故 email 是决策输入。当前
	// hashFields 的实参清单里还没有它（死参数），所以今天 email 只是保守多失效
	// （email 仅管理面改写，SDK 自动刷新不碰它）；一旦有人把它真正接进指纹，
	// 判据无需改动即仍然正确。
	//
	// codexInstallation 落在这里：AccountCandidateFingerprint 把
	// Ext.CodexIdentity.InstallationID 传给 CandidateFingerprint（账号级稳定项）。
	// 身份三件套 session/thread/window 已退役为运行时槽状态（scheduler 槽位池），
	// **不再**进键——否则每次槽轮换都会作废在途计划。
	//
	// credentialPairDigest 是「上游鉴权键 + PAT」这对凭据**原文**的 32 字节摘要。
	// 它排在 planKey **末尾**——详见结构体末尾的字段声明与那里的比较顺序说明。
	codexAccountID    string
	codexInstallation string
	codexEmail        string
	// bpsProfileDigest is the canonical digest of the opaque BPS profile. It is
	// independent from Codex installation identity and participates in the
	// routing/leaf fence for TypeBPS only.
	bpsProfileDigest string

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

	// credentialPairDigest 是「上游鉴权键 + PAT」这对凭据**原文**的 32 字节摘要
	// （hashCredentialPair：8 字节大端长度前缀 + 内容，再 sha256）。改前它们是两枚
	// 独立 string 字段（上游键无条件入键、PAT 只在非 BPS 分支入键）；本单元把原文
	// 从键里去掉，但**等价关系逐字节不变**——摘要吃同一份"进键 PAT"（ext 缺失或
	// 模板为 BPS 时为 ""），带长度前缀使 ("ab","c") 与 ("a","bc") 仍不同键。
	//
	// 为什么 PAT 轮转必须打断计划：domain/routing.go 的 stableCredentialDigest 对
	// codex-pat 就是 sha256(pat)，pat 轮转确实改变候选指纹；而 codex-oauth 分支返回
	// 常量 ⇒ OAuth 令牌不是决策输入（它落在 payloadKey）。两个分支的差异正是
	// "载荷 vs 决策输入"的分界线。
	//
	// 为什么不复用 stableCredentialDigest（那会**合并今天不同的键**）：它按凭据类型
	// 分派——codex-oauth 恒回常量、静态类型只看上游键而忽略残留 ext 的 pat、空值
	// 直接返 error、BPS 返 error。构造函数不能新增失败路径，也不能把"上游键变而
	// pat 不变"与"pat 变而上游键不变"折叠成同一类。口径与用例见
	// static_key_credential_test.go 的 1125 组等价类测试。
	//
	// **为什么它在末尾：比较顺序优化，不是字段归属变更。** `==` 对结构体按字段逐个
	// 比，而 32 字节数组是这里最贵的单字段比较（定长数组比较优先于 string 段）：放在
	// 末位后，凡在其它维度上就能判"不等"的分支（如组归属变更）永远不会付这笔钱；
	// 反之它若在数组块中间，"只差更晚那个数组"的比较会多走一届。它与
	// `acc.UpstreamKey` / `Ext.CodexPATKey` 的映射关系不变（static_read_closure_test.go
	// 的映射也不因此改动），只是声明位置变了。
	credentialPairDigest [32]byte
}

// specOfDomain 把领域代理对象降为出口规格值（egress.Spec）——**唯一转换点已
// 下沉到 domain**（spec account-model-test §4.2：调度静态键与探测执行共用
// (*domain.Proxy).EgressSpec，不得在别处重新拼 scheme/host/port）。nil（未绑定）
// = 零值 Spec（摘要 = 空规格摘要，直连是一个确定状态，不是缺失）。
func specOfDomain(p *domain.Proxy) egress.Spec {
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
type payloadKey struct {
	codexOAuthToken   string // 凭据值：上游鉴权访问令牌
	codexOAuthRefresh string // 凭据值：刷新令牌
	codexOAuthExpires int64  // 凭据值：访问令牌过期时刻（UnixNano；0 = 未设置）
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
// 一次构建，与 repository.TemplateLoadScope 同寿命。
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
func newSnapshotStatic(acc domain.Account, tpl *domain.Template, groupIDs []int64, eg *egress.Client, digests *templateDigestCache) *snapshotStatic {
	av := &snapshotStatic{acc: acc, tpl: tpl, groupIDs: append([]int64(nil), groupIDs...), egress: eg}
	// 唯一决策位在**构造期一次定型**（不是装载器的活儿，也不是热路径的判断）：
	// 之后候选过滤与计划 fence 都只读这一个 bool。放在 key 派生之前，使
	// `k.routable = av.routable` 与字段派生同源、不可能失配。
	av.routable = routableFor(av.acc.Enabled, av.acc.ProxyID, av.acc.Proxy)
	// 会话路径的两项账号级事实同样构造期定型（预留只拷字段，零分配）。仅 Codex
	// 凭据填：非 Codex 面不走会话簿，零值即"无会话路径事实"。
	if av.tpl != nil && av.tpl.CredentialType.IsCodex() {
		av.cacheDomain = cacheDomainForAccount(av.acc.ID, av.acc.CacheDomain)
		av.installationID = installationIDOf(av.acc.Ext)
	}
	if av.tpl != nil && av.tpl.CredentialType.IsBPS() && av.acc.Ext != nil {
		av.bpsProfileDigest = domain.BPSProfileHash(av.acc.Ext.BPSProfile)
	}
	var k planKey
	k.accountID = av.acc.ID
	k.name = av.acc.Name
	k.templateID = av.acc.TemplateID
	if av.tpl != nil {
		if av.tpl.CredentialType.IsBPS() {
			k.baseURL = domain.BPSDefaultEndpoint
		} else {
			k.baseURL = av.tpl.BaseURL
		}
	}
	if (av.tpl == nil || !av.tpl.CredentialType.IsBPS()) && av.acc.BaseURL != nil && *av.acc.BaseURL != "" {
		k.baseURL = *av.acc.BaseURL
	}
	// 凭据原文不再单独入键，但读点与改前逐行同源（闭包门禁把 `$S.acc.UpstreamKey`
	// 与 `$S.acc.Ext.CodexPATKey` 都登记到 credentialPairDigest 上）：上游键与 PAT
	// 在下面合成一枚摘要，不再各占一枚 string 字段。
	k.maxConcurrency = av.acc.MaxConcurrency
	k.enabled = av.acc.Enabled
	k.cacheDomain = normalizeCacheDomain(av.acc.CacheDomain)
	k.upstreamCostMultiplierBp = domain.MultBp(av.acc.UpstreamCostMultiplierBp)
	k.identityRevision = av.acc.IdentityRevision
	k.supplierUserID = av.acc.SupplierUserID
	k.proxyDigest = specOfDomain(av.acc.Proxy).Key()
	k.routable = av.routable
	// 凭据面：模板为 BPS 时 PAT 不进键（BPS 分支从来不写它），故进键 pat 保持 ""；
	// 其余模板形状（含 tpl == nil ⇒ isBPS 为假）沿用 pre-BPS 语义。
	var credentialPAT string
	if ext := av.acc.Ext; ext != nil {
		isBPS := av.tpl != nil && av.tpl.CredentialType.IsBPS()
		if isBPS {
			// BPS needs the verified upstream account claim for auth, but it
			// never consumes Codex email/PAT/installation identity.
			k.codexAccountID = derefString(ext.CodexAccountID)
		} else {
			// Preserve the pre-BPS static-key semantics for all other legacy
			// template shapes. BPS is the only provider-specific exclusion.
			k.codexAccountID = derefString(ext.CodexAccountID)
			k.codexEmail = derefString(ext.CodexEmail)
			credentialPAT = derefString(ext.CodexPATKey)
			if id := ext.CodexIdentity; id != nil {
				k.codexInstallation = id.InstallationID
			}
		}
	}
	// 上游键无条件参与（与改前一致），PAT 只在非 BPS 分支参与：两者合成一枚摘要，
	// 每次重载一次 sha256，与模板侧 5 项摘要同量级。
	k.credentialPairDigest = hashCredentialPair(av.acc.UpstreamKey, credentialPAT)
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
		if tpl.CredentialType.IsBPS() {
			k.bpsProfileDigest = av.bpsProfileDigest
		}
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

// payloadKeyOf 由快照静态视图派生载荷判据：只读 OAuth 令牌三元组。av 为 nil
// 时返回零值。
func payloadKeyOf(av *snapshotStatic) payloadKey {
	var k payloadKey
	if av == nil {
		return k
	}
	if ext := av.acc.Ext; ext != nil {
		k.codexOAuthToken = derefString(ext.CodexOAuthToken)
		k.codexOAuthRefresh = derefString(ext.CodexOAuthRefreshToken)
		if ext.CodexOAuthExpiresAt != nil {
			k.codexOAuthExpires = ext.CodexOAuthExpiresAt.UnixNano()
		}
	}
	return k
}

// staticKeyOf 由快照静态视图派生比较键。av 为 nil（首轮无旧快照）时返回零值键；
// 零值键只在两侧都为空时相等（首轮无复用分支，oldByID 为空 map）。
//
// 两枚子键从**同一份**快照派生，字段归属只声明一次：planKeyOf 读决策输入，
// payloadKeyOf 读载荷。
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

// hashCredentialPair 把「上游鉴权键 + PAT」这对凭据原文折叠为 planKey 的 32 字节摘要
// （planKey 去载荷：键里不再出现凭据原文本身）。
//
// 为什么自己写而**不复用** domain/routing.go 的 stableCredentialDigest：那枚函数编码的
// 是**候选指纹**的语义，不是"键相等"的语义——它按凭据类型分派（codex-oauth 恒回常量、
// 静态类型只看上游键而忽略残留 ext 的 pat、空值直接返 error、BPS 返 error）。拿它来算
// planKey 会把今天**不同**的键合并，并且给唯一构造函数新增失败路径。
//
// 编码：8 字节大端长度前缀 + 内容，两段依次为上游键、PAT，再 sha256。长度前缀是承重的
// ——裸拼接会把 ("ab","c") 与 ("a","bc") 折成同摘要，而它们在改前是两个不同的 string
// 字段。分段顺序同样承重：(上游键="x", pat="") ≠ (上游键="", pat="x")。
//
// 分配：常见凭据（两段合计 ≤ 240 字节）走栈上缓冲，零堆分配；更长时退化一次 make。
func hashCredentialPair(upstream, pat string) [32]byte {
	const inlineCap = 256 // 16 字节前缀 + 两段内容
	var inline [inlineCap]byte
	total := 16 + len(upstream) + len(pat)
	buf := inline[:]
	if total > inlineCap {
		buf = make([]byte, total)
	}
	n := 0
	binary.BigEndian.PutUint64(buf[n:], uint64(len(upstream)))
	n += 8
	n += copy(buf[n:], upstream)
	binary.BigEndian.PutUint64(buf[n:], uint64(len(pat)))
	n += 8
	n += copy(buf[n:], pat)
	return sha256.Sum256(buf[:n])
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
