// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// 静态读闭包门禁：把"键覆盖消费面"从人工核验变成机械断言。
//
// 叶子复用判据是 `staticKeyOf(old) == staticKeyOf(new)`。判据一旦真的生效，无关重载
// 上就会**首次**复用旧叶子——任何"读了静态字段却不在键里"的消费点立刻变成陈旧读。
// 故必须枚举"谁读了快照的哪些静态事实"，并断言每个读都落在 planKey ∪ payloadKey。
//
// 为什么根标识符不能全局按名字收：`av` 被 `*snapshotStatic` 之外的类型共用（同包里
// 还有 `*RoutingView` 的 `cur`/`v`），按名字全局收集会把无关类型的选择器混进来。
// 故扫描面先收在**显式的函数集**内，函数内再按根绑定枚举：根 = 该函数的
// `*snapshotStatic` 参数/接收者、`X := <...>.static.Load()`、`X := &snapshotStatic{}`。
// 局部别名（`ext := av.acc.Ext`）解析到规范路径。
//
// 为什么粒度必须是**标量解引用路径**而不是字段名：`av.acc` 这类整结构体读确实存在
// （有 15 处，既是深路径的前缀也是真实读取），以字段名判定会把载荷算成"被读"，
// 门不可执行。故路径终到标量，整结构体/指针/切片读单独分类（container / field）。
//
// **构造期 vs 消费期**（这条区分是删 `snapshotStatic.proxyDigest` 的前提，不许放宽）：
// 本门按**函数**登记读路径，而"谁在什么时候读"分两类，判据相同但含义不同：
//   - 构造/判据期：`newSnapshotStatic`（唯一构造函数）与 `staticKeyOf`（叶子复用判据）
//     允许读**原料**（如 `$S.acc.Proxy` 整指针交给 `specOfDomain`）——它们跑的时机
//     是 reload，叶子尚未发布或正在比较，读原料不构成陈旧读。
//   - 消费期：发布之后的读者（`Select`/预留谓词/事件/编译事实）只允许读**已登记**
//     的派生位（键字段或随叶定型的标量）——任何消费点想读原料（例如 `$S.acc.Proxy.*`
//     下钻）都必须先在这里登记并声明它落在哪个键字段，否则本次断言（③ 的"键字段集与
//     消费到的字段集相等"）直接失败。
// 因此"删掉一个字段"只能通过**删掉它的读路径**来完成；登记表不会因为字段消失而
// 自动放过任何消费点。

const (
	snapRootAlias = "$S" // 规范根名：所有 *snapshotStatic 根归一为它
)

// staticReaderFuncs 是允许读快照静态事实的函数全集，值写明它为何要读。断言是**相等**
// （双向）：新增一个读静态视图的函数 ⇒ 失败；某个函数不再读 ⇒ 陈旧条目 ⇒ 失败。
var staticReaderFuncs = map[string]string{
	"Classify":                   "事件分类：按账号的 template_id 归质量键",
	"InvalidateAccount":          "取账号所属组集合做定向重载",
	"invalidateOneIntoLocked":    "单组折叠：复用/替换叶子并登记引用集",
	"IsLatched":                  "latch 谓词按 (指纹, K) 判定",
	"MarkResult":                 "运行结果记账：状态、K、template_id、事件组",
	"ProbeAccount":               "探针：把整个账号交给候选指纹权威",
	"Runtimes":                   "运行时视图：并发上限与名字",
	"attachCompilerFacts":        "把逐账号编译事实挂到静态根",
	"buildIdentityPools":         "按静态叶重建 codex 槽位池：凭据类型、K、installation",
	"buildRoutes":                "路由索引：模板的格式集/模型集（按模板事实去重）",
	"buildSnapshots":             "构造快照：复用判定需读旧叶子",
	"deriveCompilerAccountFacts": "编译事实派生（决策输入）",
	"eventGID":                   "事件组 ID（group_ids 的确定性派生）",
	"failureEvent":               "失效事件构造：K、template_id、事件组",
	"fullLocked":                 "账号配置快照（全量）：账号全集 → 当前出口（读叶子 egress）",
	"indexRoutesForAccount":      "账号的组引用集登记",
	"mergedLocked":               "账号配置快照（组级增量）：用工作根叶子 egress 覆盖基线",
	"newAccountSnapshot":         "叶子构造：账号 ID",
	"newSnapshotStatic":          "唯一构造函数：一次派生并持有 planKey 与载荷摘要（读全部源字段，写 $S.key / $S.payload）",
	"onRuleFailure":              "失效事件的 fail-closed fence（K）",
	"payloadKeyOf":               "载荷投影：只读回构造期缓存的 $S.payload（零调用、零 $S.acc 路径）",
	"planKeyOf":                  "决策输入投影",
	"reloadLocked":               "全量重载：K 变化检测",
	"reserveOnView":              "预留谓词与 Selection 装配",
	"staticKeyOf":                "叶子复用判据",
}

// 路径分类：field 落到某个键字段；derived 由另一条路径覆盖；container 是整结构体/
// 指针/切片读取（前缀、nil 检查、整体交给权威），本身不是标量判据。
const (
	staticPathField     = "field"
	staticPathDerived   = "derived"
	staticPathContainer = "container"
)

type staticReadSpec struct {
	class  string
	detail string
}

// staticReadPaths 是扫描面上允许出现的**全部**静态读路径（根名已归一为 `$S`）。
// 断言是**相等**（双向）。`field` 类的 detail 必须是 planKey ∪ payloadKey 的字段名，
// 且这些字段名之集必须**恰好**等于两枚键的全部字段——既证明键覆盖消费面，也证明键
// 里没有消费者读不到的冗余字段。
var staticReadPaths = map[string]staticReadSpec{
	// --- 整结构体/指针/切片读取 ---
	"$S.acc":         {staticPathContainer, "整账号：深路径前缀、整体交给候选指纹权威（ProbeAccount 的 &av.acc）"},
	"$S.tpl":         {staticPathContainer, "模板指针：nil 检查与前缀"},
	"$S.acc.Ext":     {staticPathContainer, "凭据扩展指针：nil 检查 + 装配到 Selection.Ext 的穿透把手（收窄属计划 fence 消费面收口的范围）"},
	"$S.acc.Proxy":   {staticPathContainer, "出口代理展开指针：**只有构造/判据期**读它（newSnapshotStatic 定型 routable、staticKeyOf 重算出口摘要），整交给 specOfDomain；消费期不得下钻（摘要入键后由 proxyDigest 承载）"},
	"$S.acc.ProxyID": {staticPathField, "proxyDigest"}, // 绑定关系（nil = 直连）与 $S.acc.Proxy 同源承载：绑定变化 ⇒ 摘要变；可用性位另见 $S.routable
	"$S.routable":    {staticPathField, "routable"},    // 唯一可用性决策位（**快照事实**，不是领域字段）：构造期一次定型（enabled ∧（未绑定 ∨ 绑定可用）），候选过滤只读它
	"$S.cacheDomain": {staticPathField, "cacheDomain"}, // 生效缓存域（构造期由 $S.acc.ID + $S.acc.CacheDomain 定型）：预留只拷进 Selection.CacheDomain
	// installationID：构造期由 installationIDOf 定型（**仅 codex 通道**），此后有两个
	// 读者——预留拷进 Selection.InstallationID，构造器把同一个值送进 factsDigest 的
	// installation 段。键里承载它的是 factsDigest，不是独立字段（U40）。
	"$S.installationID":   {staticPathField, "factsDigest"},
	"$S.bpsProfileDigest": {staticPathField, "factsDigest"}, // BPS profile canonical digest，构造期冻结后进入 factsDigest 的 bpsProfileDigest 段
	"$S.egress":           {staticPathContainer, "恒非 nil 的出口 client 指针：直接交给 Selection（热路径零判断）、组级重派生时原样接续、prune 时取摘要"},

	// --- 账号侧标量 ---
	"$S.acc.ID":                       {staticPathField, "accountID"},
	"$S.acc.Name":                     {staticPathField, "name"},
	"$S.acc.TemplateID":               {staticPathField, "templateID"},
	"$S.acc.BaseURL":                  {staticPathField, "baseURL"},
	"$S.acc.UpstreamKey":              {staticPathField, "factsDigest"}, // 上游鉴权键：factsDigest 的 upstreamKey 段（消费面另有 Selection.UpstreamKey 的数据面透传，同源于本路径）
	"$S.acc.MaxConcurrency":           {staticPathField, "maxConcurrency"},
	"$S.acc.Enabled":                  {staticPathField, "enabled"},
	"$S.acc.CacheDomain":              {staticPathField, "cacheDomain"},
	"$S.acc.UpstreamCostMultiplierBp": {staticPathField, "upstreamCostMultiplierBp"},
	"$S.acc.IdentityRevision":         {staticPathField, "identityRevision"},
	"$S.acc.SupplierUserID":           {staticPathField, "supplierUserID"},

	// --- 账号凭据面（决策输入子集）：五条读点全部落到同一枚 factsDigest ---
	//
	// U40 把 codexAccountID / codexInstallation / bpsProfileDigest / credentialPairDigest
	// 折叠成一枚摘要；`field` 类的 detail 因此从四个名字收敛成一个。这不是放宽：
	// ③ 的「键字段集 ≡ 消费字段集」仍是双向相等断言，five-paths→one-field 只是把
	// 「哪条读点落哪枚字段」重新算了一遍。
	//
	// 两条路径随字段一起删除，且**只能**靠删读路径删字段：
	//   - $S.acc.Ext.CodexEmail：email 不是决策输入（CandidateFingerprint 的 hashFields
	//     实参清单里从来没有它），读点从 newSnapshotStatic 删除；
	//   - $S.acc.Ext.CodexIdentity（连同 .InstallationID）：installation 改由
	//     $S.installationID 叶标量提供（该标量本来就在），构造器不再下钻身份指针。
	//     这一条同时解释了容器路径为什么消失——它的唯一读者就是被删掉的那次下钻。
	"$S.acc.Ext.CodexAccountID": {staticPathField, "factsDigest"}, // factsDigest 的 codexAccountID 段
	"$S.acc.Ext.CodexPATKey":    {staticPathField, "factsDigest"}, // pat 原文：factsDigest 的 pat 段（BPS 分支不读 ⇒ 不进 BPS 的键）
	// $S.acc.Ext.BPSProfile 不再出现：S3a 把这条派生收口进 domain.BPSProfileDigest，
	// scheduler 只读冻结后的 $S.bpsProfileDigest（上一段）。若谁在控制面重新直接读
	// profile，本表与实际读路径集的**相等**判据会立刻红——删行不是放宽，是把声明
	// 收紧到"实际没有这条读"。

	// --- 账号凭据面（载荷：只影响叶子内容是否新鲜） ---
	//
	// 三枚明文字段在 P3a 折叠成一枚 payloadDigest（与 U40 对 provider 事实做的同一件
	// 事）。`field` 类的 detail 因此从三个名字收敛成一个——这**不是放宽**：③ 的
	// 「键字段集 ≡ 消费字段集」仍是双向相等断言，只是把"哪条读点落哪枚字段"重新算了
	// 一遍。读者同时也收紧了：这三条路径如今唯一的读者是唯一构造函数（见
	// payloadProjectionFuncs 的双向相等断言）——payloadKeyOf 已不再读原文。
	"$S.acc.Ext.CodexOAuthToken":        {staticPathField, "payloadDigest"},
	"$S.acc.Ext.CodexOAuthRefreshToken": {staticPathField, "payloadDigest"},
	"$S.acc.Ext.CodexOAuthExpiresAt":    {staticPathField, "payloadDigest"},

	// --- 模板侧源字段（候选指纹 I 不覆盖的那些） ---
	"$S.tpl.ID":                      {staticPathField, "templateID"}, // 与 $S.acc.TemplateID 同源：加载器按 template_id 联结，键用账号侧拼写覆盖同一事实
	"$S.tpl.BaseURL":                 {staticPathField, "baseURL"},
	"$S.tpl.CredentialType":          {staticPathField, "credentialType"},
	"$S.tpl.StripImageTools":         {staticPathField, "stripImageTools"},
	"$S.tpl.Models":                  {staticPathField, "modelsDigest"},
	"$S.tpl.FormatModels":            {staticPathField, "formatModelsKey"},
	"$S.tpl.SupportedFormats":        {staticPathField, "supportedFormats"},
	"$S.tpl.ModelMapping":            {staticPathField, "modelMappingKey"},
	"$S.tpl.UpstreamPaths":           {staticPathField, "upstreamPathsKey"},
	"$S.tpl.CodexResponsesTransport": {staticPathField, "codexResponsesTransport"},

	// --- 组归属 ---
	"$S.groupIDs": {staticPathField, "groupIDsDigest"},

	// --- 派生缓存位点（写目标 + 读回；非源字段） ---
	"$S.key":     {staticPathDerived, "canonical digest 复用位点：newSnapshotStatic 单次 av.key=k 写入、planKeyOf 读回；源字段读经 $S.acc.*/$S.tpl.*/$S.groupIDs 完整登记"},
	"$S.payload": {staticPathDerived, "载荷摘要缓存位点（与 $S.key 同款，P3a）：newSnapshotStatic 单次 av.payload=… 写入、payloadKeyOf 读回；源字段读经 $S.acc.Ext.CodexOAuth* 完整登记（三条都是 field 行，落在 payloadDigest 上）"},

	// --- planKey 的**逐字段**消费（buildRoutes 的路由事实去重键） ---
	//
	// 基线里 `$S.key` 只有整结构体一条派生登记（planKeyOf 读回整枚键比较）。buildRoutes
	// 把 5 项模板侧投影逐字段读进 tplRouteFactsKey，读点于是从"整体交出去比较"变成
	// "逐字段消费"——必须逐条登记，这是门禁正确履职，不是绕开：少一条即红。
	// detail 写成对应的 planKey 字段名，于是 ③（键字段集 ≡ 消费字段集）仍然成立。
	"$S.key.credentialType":   {staticPathField, "credentialType"},
	"$S.key.modelsDigest":     {staticPathField, "modelsDigest"},
	"$S.key.formatModelsKey":  {staticPathField, "formatModelsKey"},
	"$S.key.supportedFormats": {staticPathField, "supportedFormats"},
	"$S.key.modelMappingKey":  {staticPathField, "modelMappingKey"},
}

// payloadProjectionFuncs 是允许读**凭据原文**的函数全集：载荷只用于上游鉴权，任何
// 决策/装配路径读它都意味着"token 刷新会 churn 计划"。P3a 之后读取面收在**唯一构造
// 函数**一处——原文在构造期折叠成摘要（$S.payload），此后全包只剩摘要。
//
// 判据是**相等**（双向，照抄 proxyMaterialPathFuncs 的 violation 形态）：
//   - 多一个读者 ⇒ 红（"只有构造函数读凭据原文"就不再是机械事实）；
//   - 少一个读者 ⇒ 红（登记表腐化成"曾经读过"，下一个人会以为缓存路径还在）。
//
// 为什么不能只靠行为用例（payload_key_test.go 的 H3/H4）：一个"payloadKeyOf 仍读
// av.acc.Ext、构造函数也写了缓存"的实现会让 H3/H4 全绿——缓存写了没人读，比较期
// 仍在碰明文。这条与 payload_digest_gate_test.go 的 H2（payloadKeyOf 体内零调用、
// 零 $S.acc 路径）是同一条不变式的两个方向：一个钉读者集合，一个钉函数体形状。
var payloadProjectionFuncs = map[string]string{
	"newSnapshotStatic": "唯一构造函数：三条凭据原文读点在这里一次折叠成 payloadDigest（$S.payload）",
}

// payloadReadPaths 返回"落在载荷字段上的读路径"全集：staticReadPaths 的 field 行
// 与 payloadKey 的字段集交叉。它给下面两条判据同一份输入，避免"路径表里漏写一条"
// 静默缩小扫描面（那是 ③ 在扫的东西，这里不重复兜底）。
func payloadReadPaths(payloadFields map[string]bool) map[string]bool {
	out := map[string]bool{}
	for p, spec := range staticReadPaths {
		if spec.class == staticPathField && payloadFields[spec.detail] {
			out[p] = true
		}
	}
	return out
}

// payloadReaderUnregistered 报告"读了凭据原文但没在白名单里"的函数（空 = 合规）。
//
// 与 proxyMaterialUnregisteredReaders 同样的分工：这一侧可以用一小段真实源码证明
// （负例把绕过写成函数体、走完整扫描链再交给同一个判据），陈旧那一侧只能用注入。
func payloadReaderUnregistered(pathFuncs map[string]map[string]bool, payloadFields map[string]bool) []string {
	var out []string
	for p := range payloadReadPaths(payloadFields) {
		for fn := range pathFuncs[p] {
			if _, ok := payloadProjectionFuncs[fn]; !ok {
				out = append(out, fmt.Sprintf(
					"未登记的凭据原文读者：%s 读 %s——原文只在 newSnapshotStatic 读一次并折叠成 $S.payload；消费期请读摘要，别读原文", fn, p))
			}
		}
	}
	sort.Strings(out)
	return out
}

// payloadReaderStale 报告"白名单声称读了、扫描面上却没读"的条目（空 = 合规）。
func payloadReaderStale(pathFuncs map[string]map[string]bool, payloadFields map[string]bool) []string {
	paths := payloadReadPaths(payloadFields)
	var out []string
	for fn, why := range payloadProjectionFuncs {
		if strings.TrimSpace(why) == "" {
			out = append(out, fmt.Sprintf("白名单缺理由：%s", fn))
		}
		reads := false
		for p := range paths {
			if pathFuncs[p][fn] {
				reads = true
			}
		}
		if !reads {
			out = append(out, fmt.Sprintf("陈旧条目：白名单声称 %s 读凭据原文，但扫描面上它已不再读", fn))
		}
	}
	sort.Strings(out)
	return out
}

// payloadReaderViolations 是上面两条的合并（主断言用）：相等性判据必须双向。
func payloadReaderViolations(pathFuncs map[string]map[string]bool, payloadFields map[string]bool) []string {
	out := append(payloadReaderUnregistered(pathFuncs, payloadFields), payloadReaderStale(pathFuncs, payloadFields)...)
	sort.Strings(out)
	return out
}

// proxyMaterialPathFuncs 是**代理原料**路径的读者白名单：路径 → 函数 → 为什么它可以读。
//
// 为什么需要它（C5/S12）：上面 ①–④ 对载荷有函数级限制，对代理原料却只有路径级登记，
// 于是下面这几段可编译的代码不会让任何断言变红（都是真实评审给出的形状）：
//
//	// ① 写进 reserveOnView（**已登记**在 staticReaderFuncs 里）
//	if !routableFor(av.acc.Enabled, av.acc.ProxyID, av.acc.Proxy) { return false }
//	// ② 整账号逃逸：形参不是快照根，按名字根本看不见
//	if !func(a domain.Account) bool { return routableFor(a.Enabled, a.ProxyID, a.Proxy) }(av.acc) { return false }
//	// ③ 整指针拷贝：链为空，既不是根也不是别名
//	m := av; _ = m.acc.Proxy
//
// 三条路径都已登记（② 绿）、`proxyDigest` / `enabled` 都是键字段（③ 绿）、
// `$S.acc.Proxy` 是 container 不进键字段账，而 routableFor 的形参不是 `*snapshotStatic`
// 根——扫描器不追进去。于是"出口原料只许在 reload 期读"这条删掉
// `snapshotStatic.proxyDigest` 的前提，此前只是**声称**。
//
// 判据是**相等**（双向）：多一个读者 ⇒ 红；少一个 ⇒ 红（登记表腐化成"曾经读过"）。
//
// **能力的边界（写在明处，别过度声称）**：本表钉的是"谁读"，钉不住"读了用来干什么"。
// 一个已经为装配而读某字段的函数，可以把同一次读拿去做别的用途——典型例子是在
// `reserveOnView` 里写 `if av.acc.ProxyID != nil { return false }`：它不新增任何读者，
// 五条断言全绿，却把"绑了代理的账号"整批拒掉。这层由**行为用例**守，不是由本门守：
// `TestSelectEgressAlwaysResolved`（绑了可用代理的账号必须被选中，且 Egress 是池内该规格
// 的 client）与 `TestSelectSkipsUnroutableProxy`（绑了不可用代理的必须不被选中）——
// 任何改变结论的过滤器都会让它们红。两边合起来才是完整判据：门禁管形状，用例管结论。
var proxyMaterialPathFuncs = map[string]map[string]string{
	"$S.acc.Proxy": {
		"newSnapshotStatic": "构造期：展开的代理整交给 routableFor 与 specOfDomain（可用性与摘要都在此时定型）",
		"staticKeyOf":       "判据期：重算出口摘要（proxyDigest），叶子是否复用取决于它",
	},
	"$S.acc.ProxyID": {
		"newSnapshotStatic": "构造期：区分「未绑定（nil）」与「绑定但展开缺失」，routableFor 的输入之一",
		"reserveOnView":     "消费期：把**同一片叶子**上的 proxy_id 抄进 Selection 作审计证据（与 Selection.Egress 同代，不会指错出口）",
	},
	"$S.acc.Enabled": {
		"newSnapshotStatic": "构造期：routable 的输入（enabled ∧（未绑定 ∨ 绑定可用）），决策的唯一派生点",
		"Runtimes":          "消费期：管理面运行时视图**回显** enabled（展示事实，不参与派发判定）",
	},
}

// proxyMaterialUnregisteredReaders 报告"读了原料但没在白名单里"的读者（空 = 合规）。
//
// 两个方向拆成两个函数，因为它们能被证明的方式不同：
//   - **未登记读者**可以用一小段真实源码证明（把绕过写成函数体、解析、扫描）——测的是
//     扫描器与判据的组合；
//   - **陈旧条目**（白名单声称某人读、代码里已经不读）在现实代码里不成立，只能用注入
//     真实扫描结果来证明。
//
// 合成一条清单会让注入式负例把整张表都报成"陈旧"，所以这里不接受同一份输入判两件事。
func proxyMaterialUnregisteredReaders(pathFuncs map[string]map[string]bool) []string {
	var out []string
	for path, allowed := range proxyMaterialPathFuncs {
		for fn := range pathFuncs[path] {
			if _, ok := allowed[fn]; !ok {
				out = append(out, fmt.Sprintf("未登记读者：%s 读 %s——原料只许在构造/判据期读，消费期请读键或派生位", fn, path))
			}
		}
	}
	sort.Strings(out)
	return out
}

// proxyMaterialStaleEntries 报告"白名单声称读了、扫描面上却没读"的条目（空 = 合规）。
func proxyMaterialStaleEntries(pathFuncs map[string]map[string]bool) []string {
	var out []string
	for path, allowed := range proxyMaterialPathFuncs {
		for fn, why := range allowed {
			if strings.TrimSpace(why) == "" {
				out = append(out, fmt.Sprintf("白名单缺理由：%s 读 %s", fn, path))
			}
			if !pathFuncs[path][fn] {
				out = append(out, fmt.Sprintf("陈旧条目：白名单声称 %s 读 %s，但扫描面上它已不再读", fn, path))
			}
		}
	}
	sort.Strings(out)
	return out
}

// proxyMaterialReaderViolations 是上面两条的合并（主断言用）：相等性判据必须双向。
func proxyMaterialReaderViolations(pathFuncs map[string]map[string]bool) []string {
	out := append(proxyMaterialUnregisteredReaders(pathFuncs), proxyMaterialStaleEntries(pathFuncs)...)
	sort.Strings(out)
	return out
}

// clonePathFuncs 深拷贝"路径 → 函数集"，供负例在真实扫描结果上做最小注入。
func clonePathFuncs(in map[string]map[string]bool) map[string]map[string]bool {
	out := make(map[string]map[string]bool, len(in))
	for path, funcs := range in {
		cp := make(map[string]bool, len(funcs))
		for fn := range funcs {
			cp[fn] = true
		}
		out[path] = cp
	}
	return out
}

// --- 静态读的语法解析 ---

// snapStaticLoadCall 报告 e 是否为 `<recv>.static.Load()`。`accountSnapshot.static` 是
// 本包唯一带 `static` 字段的 atomic.Pointer，故该形态唯一指向 *snapshotStatic。
func snapStaticLoadCall(e ast.Expr) bool {
	call, ok := e.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Load" {
		return false
	}
	recv, ok := sel.X.(*ast.SelectorExpr)
	return ok && recv.Sel.Name == "static"
}

func snapStar(e ast.Expr) bool {
	st, ok := e.(*ast.StarExpr)
	if !ok {
		return false
	}
	id, ok := st.X.(*ast.Ident)
	return ok && id.Name == "snapshotStatic"
}

// snapRootPrefix 报告一个**类型表达式**是不是"静态视图根"，并给出它的规范前缀：
//
//	*snapshotStatic / snapshotStatic  → "$S"
//	domain.Account / *domain.Account  → "$S.acc"
//
// 为什么要认 `domain.Account`：这是"整账号逃逸"的形状。`av.acc` 本身就是已登记
// 的 container 读，把整个账号交给一个局部闭包之后，闭包里的 `a.Proxy` 根标识符是
// 形参名、既不是快照根也不是别名 —— 按名字枚举就完全看不见（绕过：在候选闭包里写
// `func(a domain.Account) bool { return routableFor(a.Enabled, a.ProxyID, a.Proxy) }(av.acc)`）。
// 把这类形参也登记成根，闭包内的字段读就仍归到**外层函数**名下，与直接读
// `av.acc.Proxy` 同等对待。
//
// 同样的道理覆盖 `x := av` / `m := *av` 这类整指针/整结构体拷贝（见 snapRoots）。
func snapRootPrefix(typ ast.Expr) (string, bool) {
	if st, ok := typ.(*ast.StarExpr); ok {
		typ = st.X
	}
	switch v := typ.(type) {
	case *ast.Ident:
		if v.Name == "snapshotStatic" {
			return snapRootAlias, true
		}
	case *ast.SelectorExpr:
		if pkg, ok := v.X.(*ast.Ident); ok && pkg.Name == "domain" && v.Sel.Name == "Account" {
			return snapRootAlias + ".acc", true
		}
	}
	return "", false
}

func snapComposite(e ast.Expr) bool {
	if ue, ok := e.(*ast.UnaryExpr); ok {
		e = ue.X
	}
	cl, ok := e.(*ast.CompositeLit)
	if !ok {
		return false
	}
	id, ok := cl.Type.(*ast.Ident)
	return ok && id.Name == "snapshotStatic"
}

// snapChain 把表达式解析为（根标识符, 选择器链）。内联的 `<recv>.static.Load()` 链
// 解析为规范根 snapRootAlias。
func snapChain(e ast.Expr) (root string, parts []string, ok bool) {
	for {
		switch v := e.(type) {
		case *ast.SelectorExpr:
			parts = append([]string{v.Sel.Name}, parts...)
			e = v.X
		case *ast.ParenExpr:
			e = v.X
		case *ast.StarExpr:
			e = v.X
		case *ast.IndexExpr:
			e = v.X
		case *ast.CallExpr:
			if snapStaticLoadCall(v) {
				return snapRootAlias, parts, true
			}
			return "", nil, false
		case *ast.Ident:
			return v.Name, parts, true
		default:
			return "", nil, false
		}
	}
}

// snapRoots 收集函数内所有"静态视图根"，值是规范前缀（`$S`）。
//
// 登记面（缺一个就是一条绕过）：
//   - 具名参数/接收者：`*snapshotStatic`、`snapshotStatic`；
//   - `X := <recv>.static.Load()`、`X := &snapshotStatic{…}`；
//   - **整拷贝**：`x := av`、`m := *av`（选择器链为空且右值解析到某个根）。
//
// 局部闭包的形参**不在这里**登记：它不是函数级事实而是**作用域级**事实（内层同名形参
// 会遮蔽外层变量），放在这里会让整段函数体按内层类型解释。闭包形参见 snapPaths 的
// 分作用域遍历。
func snapRoots(fn *ast.FuncDecl) map[string]string {
	roots := map[string]string{snapRootAlias: snapRootAlias}
	for _, fl := range []*ast.FieldList{fn.Recv, fn.Type.Params} {
		if fl == nil {
			continue
		}
		for _, p := range fl.List {
			prefix, ok := snapRootPrefix(p.Type)
			// 顶层函数取 `*domain.Account` 时不认：它读的是调用方给的值，不是快照
			// （`CandidateFingerprint` 这类纯领域函数正是如此）。整账号逃逸只在
			// **闭包形参**上认，见 funcLitRoots。
			if !ok || prefix != snapRootAlias {
				continue
			}
			for _, nm := range p.Names {
				if _, exists := roots[nm.Name]; !exists {
					roots[nm.Name] = prefix
				}
			}
		}
	}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		as, ok := n.(*ast.AssignStmt)
		if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
			return true
		}
		lhs, ok := as.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}
		if snapStaticLoadCall(as.Rhs[0]) || snapComposite(as.Rhs[0]) {
			roots[lhs.Name] = snapRootAlias
			return true
		}
		// 整拷贝：`x := av` / `m := *av` —— 链为空但右值就是某个根。
		if r, parts, ok := snapChain(as.Rhs[0]); ok && len(parts) == 0 {
			if prefix := roots[r]; prefix != "" {
				roots[lhs.Name] = prefix
			}
		}
		return true
	})
	return roots
}

// snapAliases 把局部别名（`ext := av.acc.Ext`）解析到规范路径。迭代到不动点，
// 因为别名可以链式（`id := ext.CodexIdentity`）。
func snapAliases(fn *ast.FuncDecl, roots map[string]string) map[string]string {
	alias := map[string]string{}
	for range 8 {
		changed := false
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			as, ok := n.(*ast.AssignStmt)
			if !ok || len(as.Lhs) != 1 || len(as.Rhs) != 1 {
				return true
			}
			lhs, ok := as.Lhs[0].(*ast.Ident)
			if !ok || roots[lhs.Name] != "" {
				return true
			}
			r, parts, ok := snapChain(as.Rhs[0])
			if !ok || len(parts) == 0 {
				return true
			}
			var canon string
			if prefix := roots[r]; prefix != "" {
				canon = prefix + "." + strings.Join(parts, ".")
			} else {
				a, ok := alias[r]
				if !ok {
					return true
				}
				canon = a + "." + strings.Join(parts, ".")
			}
			if alias[lhs.Name] != canon {
				alias[lhs.Name] = canon
				changed = true
			}
			return true
		})
		if !changed {
			break
		}
	}
	return alias
}

// snapPaths 枚举函数内出现的全部静态读路径。作为方法调用接收者的选择器不算字段读
// （`av.eventGID()` 是派生方法，不是快照字段）。
//
// **按作用域遍历**：进入 `func(…) {…}` 时用它的形参派生一个新的根集合（内层遮蔽外层），
// 出闭包即丢弃。这是"整账号逃逸"（`func(a domain.Account) bool {…}(av.acc)`）被看见
// 的唯一正确做法——按名字全局登记会把闭包外的同名局部变量（如 `a := byID[…]`）按内层
// 类型解释，凭空造出 `$S.acc.static` 这类不存在的路径。读到的路径仍归到**外层函数**名下。
func snapPaths(fn *ast.FuncDecl, roots map[string]string, alias map[string]string) map[string]bool {
	callFuns := map[ast.Node]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if _, isSel := c.Fun.(*ast.SelectorExpr); isSel {
				callFuns[c.Fun] = true
			}
		}
		return true
	})
	paths := map[string]bool{}
	var walk func(n ast.Node, scope map[string]string)
	walk = func(n ast.Node, scope map[string]string) {
		ast.Inspect(n, func(node ast.Node) bool {
			if node != n {
				if fl, ok := node.(*ast.FuncLit); ok {
					walk(fl.Body, funcLitRoots(scope, fl.Type.Params))
					return false
				}
			}
			se, ok := node.(*ast.SelectorExpr)
			if !ok || callFuns[node] {
				return true
			}
			r, parts, ok := snapChain(se)
			if !ok || len(parts) == 0 {
				return true
			}
			if prefix := scope[r]; prefix != "" {
				paths[prefix+"."+strings.Join(parts, ".")] = true
				return true
			}
			if a, ok := alias[r]; ok {
				paths[a+"."+strings.Join(parts, ".")] = true
			}
			return true
		})
	}
	walk(fn.Body, roots)
	return paths
}

// funcLitRoots 用闭包形参派生新的根集合（内层遮蔽外层，且**只**在闭包子树里生效）。
// 认 `domain.Account` 是因为把整账号交给闭包后，闭包里的 `a.Proxy` 按名字找不到任何根；
// 顶层函数取 `*domain.Account` 时不认（那是纯领域函数，读的是调用方给的值，不是快照）。
func funcLitRoots(parent map[string]string, fl *ast.FieldList) map[string]string {
	if fl == nil {
		return parent
	}
	var out map[string]string // 惰性派生：闭包没有这类形参时直接复用父作用域
	for _, p := range fl.List {
		prefix, ok := snapRootPrefix(p.Type)
		if !ok {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(parent)+1)
			for k, v := range parent {
				out[k] = v
			}
		}
		for _, nm := range p.Names {
			out[nm.Name] = prefix
		}
	}
	if out == nil {
		return parent
	}
	return out
}

// --- 断言 ---

// staticReadScan 是一次扫描的全部产物：枚举结果（函数集 / 路径集 / 路径 × 函数）
// 加两枚键的字段集。抽成结构体的原因不是"好看"——是**负例必须喂真实扫描结果**：
// 只加正例等于自证，而负例若手搓一个 map，被测的就不是扫描器与判据的组合了。
type staticReadScan struct {
	funcs         map[string]bool            // 读过静态视图的函数全集
	paths         map[string]bool            // 出现过的静态读路径全集
	pathFuncs     map[string]map[string]bool // 路径 → 读者函数集
	planFields    map[string]bool            // planKey 的字段集
	payloadFields map[string]bool            // payloadKey 的字段集
}

// collectStaticReads 遍历扫描面，枚举每个函数读了哪些静态字段。
//
// 判据（顺序即下方 ①–④ 与 ⑤ 的输入）：
//   - 函数内部按根绑定枚举（`*snapshotStatic` 参数/接收者、`.static.Load()` 绑定、
//     `&snapshotStatic{}`），局部别名解析到规范路径 `$S`；
//   - 只登记**具名根或实际路径**的函数（roots 恒含规范根，故只有内联链不算读）。
func collectStaticReads(t *testing.T) staticReadScan {
	t.Helper()
	root := gateRoot(t)
	srcs := gateSources(t, root, []string{"internal/scheduler"})

	planShape, ok := gateStructShape(t, srcs, "internal/scheduler", "planKey")
	require.True(t, ok, "planKey 必须存在")
	payloadShape, ok := gateStructShape(t, srcs, "internal/scheduler", "payloadKey")
	require.True(t, ok, "payloadKey 必须存在")

	scan := staticReadScan{
		funcs:         map[string]bool{},
		paths:         map[string]bool{},
		pathFuncs:     map[string]map[string]bool{},
		planFields:    make(map[string]bool, len(planShape.fields)),
		payloadFields: make(map[string]bool, len(payloadShape.fields)),
	}
	for f := range planShape.fields {
		scan.planFields[f] = true
	}
	for f := range payloadShape.fields {
		scan.payloadFields[f] = true
	}
	for _, src := range srcs {
		for _, decl := range src.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			roots := snapRoots(fn)
			paths := snapPaths(fn, roots, snapAliases(fn, roots))
			// roots 恒含规范根（内联链），故只有**具名根**（*snapshotStatic 参数/
			// 接收者/绑定）或实际路径才算读了静态视图。两者都没有的函数不登记。
			if len(paths) == 0 && len(roots) == 1 {
				continue
			}
			scan.funcs[fn.Name.Name] = true
			for p := range paths {
				scan.paths[p] = true
				if scan.pathFuncs[p] == nil {
					scan.pathFuncs[p] = map[string]bool{}
				}
				scan.pathFuncs[p][fn.Name.Name] = true
			}
		}
	}
	return scan
}

// TestStaticReadClosureCoversEveryConsumerPath 枚举面上每一个静态读都必须落在
// planKey ∪ payloadKey 的字段上，且键字段与消费字段必须**一一对上**。
func TestStaticReadClosureCoversEveryConsumerPath(t *testing.T) {
	scan := collectStaticReads(t)
	observedFuncs, observedPaths, pathFuncs := scan.funcs, scan.paths, scan.pathFuncs

	// ① 读静态视图的函数集必须**恰好**是声明的允许集。
	gotFuncs := make([]string, 0, len(observedFuncs))
	for f := range observedFuncs {
		gotFuncs = append(gotFuncs, f)
	}
	sort.Strings(gotFuncs)
	require.Equal(t, sortedKeys(staticReaderFuncs), gotFuncs,
		"读快照静态事实的函数集与声明允许集必须相等：新增读取点必须先声明它落在哪个键字段")

	// ② 读路径集必须**恰好**是声明的路径集。
	gotPaths := make([]string, 0, len(observedPaths))
	for p := range observedPaths {
		gotPaths = append(gotPaths, p)
	}
	sort.Strings(gotPaths)
	declaredPaths := make([]string, 0, len(staticReadPaths))
	for p := range staticReadPaths {
		declaredPaths = append(declaredPaths, p)
	}
	sort.Strings(declaredPaths)
	require.Equal(t, declaredPaths, gotPaths,
		"静态读路径集与声明路径集必须相等")

	// ③ field 类路径的键字段必须存在，且覆盖面**恰好**等于两枚键的字段全集。
	planFields, payloadFields := scan.planFields, scan.payloadFields
	allKeyFields := map[string]bool{}
	for f := range planFields {
		allKeyFields[f] = true
	}
	for f := range payloadFields {
		allKeyFields[f] = true
	}
	consumed := map[string]bool{}
	for p, spec := range staticReadPaths {
		require.NotEmpty(t, strings.TrimSpace(spec.detail), "路径分类必须写明理由：%s", p)
		switch spec.class {
		case staticPathField:
			require.True(t, allKeyFields[spec.detail],
				"路径 %s 声称落在键字段 %s，但该字段不在 planKey ∪ payloadKey", p, spec.detail)
			consumed[spec.detail] = true
		case staticPathDerived, staticPathContainer:
			// 派生/整结构体：由另一条路径或权威函数覆盖，不占键字段。
		default:
			t.Fatalf("路径 %s 的分类未知：%q", p, spec.class)
		}
	}
	gotConsumed := make([]string, 0, len(consumed))
	for f := range consumed {
		gotConsumed = append(gotConsumed, f)
	}
	sort.Strings(gotConsumed)
	wantConsumed := make([]string, 0, len(allKeyFields))
	for f := range allKeyFields {
		wantConsumed = append(wantConsumed, f)
	}
	sort.Strings(wantConsumed)
	require.Equal(t, wantConsumed, gotConsumed,
		"键字段集与消费到的字段集必须相等：键里不得有消费者读不到的字段，消费面也不得读到键外的字段")

	// ④ 凭据原文只允许被**唯一构造函数**读到——决策/装配路径读到 token 意味着
	// "SDK 刷新 token 会 churn 计划"，而比较期读原文会让"构造期缓存"退化成本地投影。
	// 判据是相等（双向）：这条同时钉住"缓存写了有人读"与"没有第二个读者"。
	require.Empty(t, payloadReaderViolations(pathFuncs, payloadFields),
		"凭据原文的读者必须与 payloadProjectionFuncs **相等**：新增读者要说明它为什么必须在消费期读明文（正常答案是：不需要）")

	// ⑤ 代理原料的"路径 × 函数"白名单：原料（acc.Proxy / acc.ProxyID / acc.Enabled）
	// 只许在构造/判据期读，消费期只能读键与派生位。① 只证明"函数登记过"，④ 只管载荷，
	// 故这一条是"出口原料不漏到消费期"在**形状**上的机械判据（"读了用来干什么"由
	// account_proxy_test.go 的行为用例守，见 proxyMaterialPathFuncs 注释的边界）。
	require.Empty(t, proxyMaterialReaderViolations(pathFuncs),
		"代理原料的读者必须与白名单**相等**：新增读者要在这里登记并写明它为何必须在消费期读")
}

// TestProxyMaterialGateRejectsConsumptionTimeRead 负例：证明 ⑤ 的判据真的会拒。
//
// 只加正例等于自证——判据写反方向、或比对的两侧总是空集，正例一样是绿的。故这里分两层：
//   - **真实源码**：把绕过的函数体当成一段真代码解析出来，走完整的 解析 → roots → paths
//     → pathFuncs 链，再交给同一个判据。测的是扫描器与判据的组合，而不只是比较器；
//   - **登记表腐化**：这一侧只能用注入，因为现实里表是对的（"某个函数不再读了"只在
//     有人删了读取点之后才发生），而它要证明的是"表跟着代码腐化时门会红"。
//
// 覆盖的形状（前两条是 oracle 第二轮/第三轮实际给出的可编译绕过）：
//
//	① 直接重派生：routableFor(av.acc.Enabled, av.acc.ProxyID, av.acc.Proxy)
//	② 整账号逃逸：func(a domain.Account) bool { … }(av.acc)   ← 形参不是快照根
//	③ 整指针拷贝：m := av; _ = m.acc.Proxy                     ← 链为空但不是根
//
// 另有一个**对照**：同一层里读一个不在白名单上的普通字段（`av.acc.ID`）不报错——否则
// "永远返回违规"的实现也能通过上面三条。
func TestProxyMaterialGateRejectsConsumptionTimeRead(t *testing.T) {
	scan := collectStaticReads(t)
	require.Empty(t, proxyMaterialReaderViolations(scan.pathFuncs),
		"真实扫描结果必须本来就合规，否则后面的负例证明不了任何东西")

	rejects := func(t *testing.T, body string, wantPaths ...string) {
		t.Helper()
		got := proxyMaterialUnregisteredReaders(proxyMaterialReadsOf(t, body))
		require.NotEmpty(t, got, "这个函数体必须被拒：%s", body)
		joined := strings.Join(got, "\n")
		require.Contains(t, joined, "injected", "违规必须点名注入的函数：%s", joined)
		for _, p := range wantPaths {
			require.Contains(t, joined, p, "违规必须点名被越界读取的路径：%s", joined)
		}
	}

	t.Run("直接重派生 routable", func(t *testing.T) {
		rejects(t, `
	if !routableFor(av.acc.Enabled, av.acc.ProxyID, av.acc.Proxy) {
		return
	}`, "$S.acc.Proxy", "$S.acc.Enabled")
	})

	t.Run("整账号逃逸到闭包", func(t *testing.T) {
		rejects(t, `
	if !func(a domain.Account) bool {
		return routableFor(a.Enabled, a.ProxyID, a.Proxy)
	}(av.acc) {
		return
	}`, "$S.acc.Proxy", "$S.acc.Enabled")
	})

	t.Run("整指针拷贝", func(t *testing.T) {
		rejects(t, `
	m := av
	_ = m.acc.Proxy
	_ = m.acc.Enabled`)
	})

	t.Run("对照：读非原料字段不报", func(t *testing.T) {
		got := proxyMaterialUnregisteredReaders(proxyMaterialReadsOf(t, `
	if av.acc.ID == 0 {
		return
	}
	_ = av.tpl.ID`))
		require.Empty(t, got, "普通字段读取与出口原料无关，不该被这条判据拦：%v", got)
	})

	t.Run("登记表腐化", func(t *testing.T) {
		pathFuncs := clonePathFuncs(scan.pathFuncs)
		delete(pathFuncs["$S.acc.Proxy"], "staticKeyOf")
		got := proxyMaterialStaleEntries(pathFuncs)
		require.Len(t, got, 1, "只该报一条陈旧条目：%v", got)
		require.Contains(t, got[0], "staticKeyOf")
		require.Contains(t, got[0], "陈旧条目")
	})
}

// proxyMaterialReadsOf 把一段**函数体源码**送进真正的扫描链（解析 → snapRoots →
// snapPaths），返回"路径 → 读者函数集"。函数名固定 `injected`，形参固定
// `av *snapshotStatic`：这样注入体与 `reserveOnView` 的可见形状一致（都能拿到 av），
// 但断言只关心"这个读者没有登记"。
//
// 不做类型检查（parser 不解析 import）：`domain.Account` 这类限定类型名在语法层就够
// ——本门判的是语法形状，不是类型。
func proxyMaterialReadsOf(t *testing.T, body string) map[string]map[string]bool {
	t.Helper()
	src := "package scheduler\n\nfunc injected(av *snapshotStatic) {\n" + body + "\n}\n"
	f, err := parser.ParseFile(token.NewFileSet(), "injected.go", src, parser.SkipObjectResolution)
	require.NoError(t, err)
	require.Len(t, f.Decls, 1)
	fn, ok := f.Decls[0].(*ast.FuncDecl)
	require.True(t, ok, "注入的源码必须只有一处函数声明")
	roots := snapRoots(fn)
	paths := snapPaths(fn, roots, snapAliases(fn, roots))
	out := make(map[string]map[string]bool, len(paths))
	for p := range paths {
		out[p] = map[string]bool{fn.Name.Name: true}
	}
	return out
}

// TestEveryStaticLoadSiteIsDeclared `.static.Load()` 是取得静态视图的**唯一**入口。
// 每个调用点都必须落在声明的读者函数内——否则一个"取了视图但暂时只读了几何事实"
// 的函数会绕过上面那张表。
func TestEveryStaticLoadSiteIsDeclared(t *testing.T) {
	root := gateRoot(t)
	srcs := gateSources(t, root, []string{"internal/scheduler"})
	var sites []string
	for _, src := range srcs {
		for _, decl := range src.file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			hit := false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok && snapStaticLoadCall(call) {
					hit = true
				}
				return true
			})
			if hit {
				sites = append(sites, fn.Name.Name)
			}
		}
	}
	sort.Strings(sites)
	require.NotEmpty(t, sites, "扫描面必须至少有一处静态视图取用点")
	for _, f := range sites {
		require.Contains(t, staticReaderFuncs, f,
			"%s 取了静态视图却未在 staticReaderFuncs 中声明", f)
	}
}

// TestPayloadProjectionIsTheOnlyCredentialReader payloadKey 的字段集必须与载荷投影
// 函数实际读到的路径**一一对应**：投影多读一个字段（该字段不在键里）会让键漏掉它，
// 少读一个（键里有但没人读）会让键带上无人消费的字段。
func TestPayloadProjectionIsTheOnlyCredentialReader(t *testing.T) {
	root := gateRoot(t)
	srcs := gateSources(t, root, []string{"internal/scheduler"})
	payloadShape, ok := gateStructShape(t, srcs, "internal/scheduler", "payloadKey")
	require.True(t, ok, "payloadKey 必须存在")

	declared := map[string]bool{}
	for p, spec := range staticReadPaths {
		if spec.class == staticPathField && payloadShape.fields[spec.detail] {
			declared[spec.detail] = true
			require.True(t, strings.HasPrefix(p, snapRootAlias+".acc.Ext.CodexOAuth"),
				"载荷字段 %s 的读路径必须落在凭据三元组上，实际是 %s", spec.detail, p)
		}
	}
	want := make([]string, 0, len(payloadShape.fields))
	for f := range payloadShape.fields {
		want = append(want, f)
	}
	sort.Strings(want)
	got := make([]string, 0, len(declared))
	for f := range declared {
		got = append(got, f)
	}
	sort.Strings(got)
	require.Equal(t, want, got, "payloadKey 字段集与声明为载荷的读路径必须相等")
	require.NotEmpty(t, payloadProjectionFuncs, "载荷投影白名单不得为空")
	for f, why := range payloadProjectionFuncs {
		require.NotEmpty(t, strings.TrimSpace(why), "载荷投影白名单必须写明理由：%s", f)
		require.Contains(t, staticReaderFuncs, f, "载荷投影函数必须是声明的静态读者：%s", f)
	}
}

// TestPayloadReaderGateRejectsConsumptionTimeRead 负例：证明 ④ 的双向判据真的会拒。
//
// 只加正例等于自证，而 ④ 的两个方向能被证明的方式不同：
//   - **未登记读者**用一小段真实源码证明：把它当函数体解析、走完整的
//     解析 → roots → paths → pathFuncs 链，再交给同一个判据；
//   - **陈旧条目**（白名单声称某人读、代码里已经不读）在现实里不成立，只能用注入
//     真实扫描结果来证明。
//
// 注入的形状正是 M3 变异（"缓存写了没人读"）的可编译形态：构造函数照写
// $S.payload，而某个消费期函数仍去读 av.acc.Ext 的三枚明文字段。
func TestPayloadReaderGateRejectsConsumptionTimeRead(t *testing.T) {
	root := gateRoot(t)
	srcs := gateSources(t, root, []string{"internal/scheduler"})
	payloadShape, ok := gateStructShape(t, srcs, "internal/scheduler", "payloadKey")
	require.True(t, ok, "payloadKey 必须存在")
	payloadFields := payloadShape.fields
	require.NotEmpty(t, payloadFields, "载荷字段集不得为空（否则下面的判据恒绿）")

	scan := collectStaticReads(t)
	require.Empty(t, payloadReaderViolations(scan.pathFuncs, payloadFields),
		"真实扫描结果必须本来就合规，否则后面的负例证明不了任何东西")

	rejects := func(t *testing.T, body string, wantPaths ...string) {
		t.Helper()
		got := payloadReaderUnregistered(proxyMaterialReadsOf(t, body), payloadFields)
		require.NotEmpty(t, got, "这个函数体必须被拒：%s", body)
		joined := strings.Join(got, "\n")
		require.Contains(t, joined, "injected", "违规必须点名注入的函数：%s", joined)
		for _, p := range wantPaths {
			require.Contains(t, joined, p, "违规必须点名被越界读取的路径：%s", joined)
		}
		require.Contains(t, joined, "$S.payload", "红点必须指出正确去处（构造期缓存的摘要）")
	}

	t.Run("消费期读明文（M3 的可编译形态）", func(t *testing.T) {
		rejects(t, `_ = derefString(av.acc.Ext.CodexOAuthToken)`, "$S.acc.Ext.CodexOAuthToken")
	})
	t.Run("消费期读 expires", func(t *testing.T) {
		rejects(t, `_ = av.acc.Ext.CodexOAuthExpiresAt`, "$S.acc.Ext.CodexOAuthExpiresAt")
	})
	t.Run("对照：读摘要不报", func(t *testing.T) {
		require.Empty(t, payloadReaderUnregistered(proxyMaterialReadsOf(t, `_ = av.payload`), payloadFields),
			"摘要本身不是凭据原文：读它不构成消费期明文读")
	})
	t.Run("登记表腐化", func(t *testing.T) {
		pathFuncs := clonePathFuncs(scan.pathFuncs)
		for p := range payloadReadPaths(payloadFields) {
			delete(pathFuncs[p], "newSnapshotStatic")
		}
		got := payloadReaderStale(pathFuncs, payloadFields)
		require.Len(t, got, 1, "只该报一条陈旧条目：%v", got)
		require.Contains(t, got[0], "newSnapshotStatic")
		require.Contains(t, got[0], "陈旧条目")
	})
}

// --- buildRoutes 的形状门禁（spec §4.3 末的两条） ---
//
// 上面 ①–⑤ 管"读点落在哪枚键的哪个字段"，管不了"某枚键的某个字段被删/被改摘要之后
// 消费面会不会静默走样"。buildRoutes 的代价模型把这两件事绑在一起：它按
// tplRouteFactsKey（planKey 的 5 项模板侧投影）去重，于是
//
//   - 正方向（新增读点）：模板多读一个字段 ⇒ 去重键不再覆盖消费面 ⇒ 错合并、丢桶；
//   - 反方向（planKey 被删/改）：去重键少一维 ⇒ 两个本该分开的模板被合并 ⇒ 同样丢桶，
//     而 15 条形状用例喂的是**模板**、不是 planKey，一行都不会红。
//
// 所以两侧各一条形状门禁，判据都在 buildRoutesShapeViolations 里（纯函数，负例喂真实
// 扫描结果才不是自证）。这两条只看 buildRoutes **自己**的读点；"把读点搬进 helper"这条
// 跨函数逃逸面由下面的调用面门禁（TestBuildRoutesCallsNoHelperOutsideItsWhitelist）守。

// buildRoutesTemplateReadWhitelist 是 buildRoutes 允许读到的模板路径全集。
//
// `tplRouteFactsKey` 的 5 项覆盖面 = 这条白名单。谁给 buildRoutes 加第 7 个模板读点
// （例如 StripImageTools / BaseURL），门禁立刻红，逼他回答"去重键要不要同步"。
// CredentialType / SupportedFormats 在 tplSupportsFormat 里被读（扫描器不追进形参），
// 列在这里是因为**语义上**它们就是白名单的一部分：谁把它内联进来也不该变红。
var buildRoutesTemplateReadWhitelist = []string{
	"$S.tpl",
	"$S.tpl.Models",
	"$S.tpl.FormatModels",
	"$S.tpl.SupportedFormats",
	"$S.tpl.ModelMapping",
	"$S.tpl.CredentialType",
}

// buildRoutesPlanKeyReadWhitelist 是 buildRoutes 允许读到的 planKey 子路径：**恰好**
// tplRouteFactsKey 的 5 项，一条不多一条不少。
var buildRoutesPlanKeyReadWhitelist = []string{
	"$S.key.credentialType",
	"$S.key.modelsDigest",
	"$S.key.formatModelsKey",
	"$S.key.supportedFormats",
	"$S.key.modelMappingKey",
}

// buildRoutesShapeViolations 是两条形状门禁的判据本体（空 = 合规）：
//   - 模板读点 ⊆ buildRoutesTemplateReadWhitelist；
//   - planKey 子路径集**恰好**等于 buildRoutesPlanKeyReadWhitelist；
//   - buildRoutes 的 `$S.*` 读点全集 ⊆ 上面两者的并 ∪ {`$S.key`}（"只许读这些"）。
//
// `$S.key` 自身（不带子路径）是允许的：`k := av.key` 把投影整体取出来一次，逐字段
// 消费是那 5 条子路径；挡住整枚键的**比较**没有意义（planKeyOf 本来就这么用），挡住的
// 是"读了一个没登记的维度"。
func buildRoutesShapeViolations(paths []string) []string {
	tplAllow := make(map[string]bool, len(buildRoutesTemplateReadWhitelist))
	for _, p := range buildRoutesTemplateReadWhitelist {
		tplAllow[p] = true
	}
	keyAllow := make(map[string]bool, len(buildRoutesPlanKeyReadWhitelist))
	for _, p := range buildRoutesPlanKeyReadWhitelist {
		keyAllow[p] = true
	}

	var out []string
	keyed := make(map[string]bool, len(keyAllow))
	for _, p := range paths {
		switch {
		case keyAllow[p]:
			keyed[p] = true
		case p == snapRootAlias+".key":
			// 投影整体取用（k := av.key）；逐字段消费由下面那 5 条覆盖。
		case strings.HasPrefix(p, snapRootAlias+".key."):
			out = append(out, fmt.Sprintf("未登记的 planKey 子路径：%s——去重键是那 5 项的投影，多读一维就要先回答「去重键要不要同步」", p))
		case tplAllow[p]:
		case strings.HasPrefix(p, snapRootAlias+".tpl."):
			out = append(out, fmt.Sprintf("越界模板读点：%s——tplRouteFactsKey 的 5 项覆盖面 = 模板白名单，新增读点必须同步去重键", p))
		default:
			out = append(out, fmt.Sprintf("越界静态读点：%s——buildRoutes 只许读模板白名单 6 条与 planKey 的 5 项投影", p))
		}
	}
	for _, p := range buildRoutesPlanKeyReadWhitelist {
		if !keyed[p] {
			out = append(out, fmt.Sprintf("去重键维度消失：buildRoutes 不再读 %s——planKey 字段被删或被改摘要（例如 modelMappingKey 只对键取摘要）会让路由侧静默错合并，而形状用例喂的是模板，一行都不会红", p))
		}
	}
	sort.Strings(out)
	return out
}

// funcDeclOf 在扫描面上按名字取顶层函数声明（多条同名声明取最后一条，与解析序一致）。
func funcDeclOf(t *testing.T, srcs []gateFile, name string) *ast.FuncDecl {
	t.Helper()
	var fn *ast.FuncDecl
	for _, src := range srcs {
		for _, decl := range src.file.Decls {
			d, ok := decl.(*ast.FuncDecl)
			if ok && d.Body != nil && d.Name.Name == name {
				fn = d
			}
		}
	}
	require.NotNil(t, fn, "扫描面上必须存在函数 %s", name)
	return fn
}

// staticPathsOfFunc 用**同一个扫描器**（snapRoots / snapAliases / snapPaths）枚举指定
// 顶层函数读到的静态路径。正例与负例因此测的是同一件东西：扫描器 × 判据。
func staticPathsOfFunc(t *testing.T, srcs []gateFile, name string) []string {
	t.Helper()
	fn := funcDeclOf(t, srcs, name)
	roots := snapRoots(fn)
	paths := snapPaths(fn, roots, snapAliases(fn, roots))
	out := make([]string, 0, len(paths))
	for p := range paths {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// TestBuildRoutesReadsOnlyItsDedupedTemplateFacts 正例：buildRoutes 的实际读点必须
// 落在"模板白名单 6 条 ∪ planKey 5 项投影"内，且 planKey 那 5 项**恰好**全在。
func TestBuildRoutesReadsOnlyItsDedupedTemplateFacts(t *testing.T) {
	root := gateRoot(t)
	srcs := gateSources(t, root, []string{"internal/scheduler"})
	paths := staticPathsOfFunc(t, srcs, "buildRoutes")

	require.Empty(t, buildRoutesShapeViolations(paths),
		"buildRoutes 的静态读点越界：%v", paths)
	// 非空性：判据不能靠"什么都没读到"通过。
	tplReads := 0
	for _, p := range paths {
		if strings.HasPrefix(p, snapRootAlias+".tpl") {
			tplReads++
		}
	}
	require.Greater(t, tplReads, 1, "buildRoutes 必须真的读模板事实（读点：%v）", paths)
}

// TestBuildRoutesShapeGateRejectsDrift 负例：把绕过的函数体当真实源码走完整扫描链，再
// 交给同一条判据。只加正例等于自证——判据写反或两侧恒空时正例一样是绿的。
func TestBuildRoutesShapeGateRejectsDrift(t *testing.T) {
	pathsOf := func(body string) []string {
		t.Helper()
		got := proxyMaterialReadsOf(t, body)
		out := make([]string, 0, len(got))
		for p := range got {
			out = append(out, p)
		}
		sort.Strings(out)
		return out
	}
	compliant := `
	_ = av.tpl
	_ = av.tpl.Models
	_ = av.tpl.FormatModels
	_ = av.tpl.SupportedFormats
	_ = av.tpl.ModelMapping
	_ = av.tpl.CredentialType
	k := av.key
	_ = k.credentialType
	_ = k.modelsDigest
	_ = k.formatModelsKey
	_ = k.supportedFormats
	_ = k.modelMappingKey`
	require.Empty(t, buildRoutesShapeViolations(pathsOf(compliant)),
		"对照：恰好落在白名单内的读点不得被拒")

	t.Run("第 7 个模板读点", func(t *testing.T) {
		got := buildRoutesShapeViolations(pathsOf(compliant + "\n\t_ = av.tpl.StripImageTools"))
		require.Len(t, got, 1, "只该报一条越界模板读点：%v", got)
		require.Contains(t, got[0], "$S.tpl.StripImageTools")
		require.Contains(t, got[0], "越界模板读点")
	})

	t.Run("多读一条 planKey 子路径", func(t *testing.T) {
		// 夹具用**现存的**键字段（baseURL）：注入一个已删字段只会测到扫描器的语法层，
		// 证明不了"真实字段被多读一维"这条判据。
		got := buildRoutesShapeViolations(pathsOf(compliant + "\n\t_ = k.baseURL"))
		require.Len(t, got, 1, "只该报一条未登记子路径：%v", got)
		require.Contains(t, got[0], "$S.key.baseURL")
	})

	t.Run("去重键维度消失", func(t *testing.T) {
		got := buildRoutesShapeViolations(pathsOf(strings.Replace(compliant, "\n\t_ = k.modelMappingKey", "", 1)))
		require.Len(t, got, 1, "只该报一条维度消失：%v", got)
		require.Contains(t, got[0], "$S.key.modelMappingKey")
		require.Contains(t, got[0], "去重键维度消失")
	})
}

// --- buildRoutes 的调用面门禁（spec §4.4 末：不得把阅读点搬进 helper） ---
//
// 上面两条形状门禁只看 buildRoutes **一个函数体**里出现的 `$S.*` 路径。这把"把模板读点
// 搬进 helper"留成了一条近乎免费的绕过面——oracle Important-1 的变异：
//
//	for _, m := range tpl.Models { models[m] = struct{}{} }
//
// 抽成 `func addModels(tpl *domain.Template, dst map[string]struct{})` 之后，buildRoutes
// 里只剩 `tpl := av.tpl`（container，在白名单里）与几处 dummy 子读：路径集照样合规，
// 而真正的消费面已经在门禁之外。扫描器不追进被调函数（也不该追——那就是写第二个编译器）。
//
// 所以这里守**资格**而不是**追值**：buildRoutes 直接调用到的每个名字都必须显式登记，
// 并写明它为什么可以出现。断言是相等（双向）：多一个 ⇒ 红（可能就是新的逃逸面）；
// 少一个 ⇒ 红（白名单腐化：有人删了调用却留着旧条目）。
//
// 只做**一层**（buildRoutes 的直接调用），不做全调用图传播：
//   - 一层已经堵死"抽一个 helper 把读点带走"这条实际路径——任何新 helper 都必然出现在
//     这一层，因为 Go 没有隐式调用；
//   - 全调用图会把门禁变成第二个编译器（指针别名、接口派发、泛型实例化都要建模），维护
//     成本远高于它挡住的剩余风险。
//
// 边界（说清，否则会被当成"证明了消费闭包"）：**领域方法**（`tpl.HasModelSpace()`）读
// 什么字段由 `domain` 包自己的测试守；本门禁守的是"模板事实被搬出 buildRoutes"这条
// 包内 helper 逃逸面，不是 `domain.Template` 的内部实现。

// buildRoutesCall 的可调用者形态：未限定标识符 = 包内/同包函数，选择器 = 方法或跨包函数。
const (
	buildRoutesCallFunc   = "func"
	buildRoutesCallMethod = "method"
)

type buildRoutesCall struct {
	kind string
	name string
}

// buildRoutesCallSpec 是白名单的一条：kind 必须对上（同名换形态 = 换被调者），why 必填。
type buildRoutesCallSpec struct {
	kind string
	why  string
}

// buildRoutesCallWhitelist 是 buildRoutes 允许直接调用到的名字全集。理由写在这里，因为
// 下一个人只会读这一张表——"为什么它可以出现"比"它出现了"更重要。
var buildRoutesCallWhitelist = map[string]buildRoutesCallSpec{
	"tplSupportsFormat": {buildRoutesCallFunc,
		"BPS 门 ∧ SupportedFormats：唯一允许的模板判定入口（spec §4.4 第 3 条），只读 CredentialType/SupportedFormats"},
	"HasModelSpace": {buildRoutesCallMethod,
		"`domain.Template` 的领域方法：模板模型空间是否非空。领域方法读哪些字段由 domain 包自己的测试守（本门禁不追进领域方法）"},
	"Load": {buildRoutesCallMethod,
		"`a.static.Load()`：静态视图的唯一取用入口，取视图本身不读模板；每个取用点另由 TestEveryStaticLoadSiteIsDeclared 守"},
	"RoutableFormats": {buildRoutesCallMethod,
		"`domain.RoutableFormats()`：全仓唯一的可路由格式枚举（不是快照事实），只调一次、不进循环"},
}

// goBuiltinCalls 是 Go 内建（含预声明类型名的转换）：它们不是包内函数，永远不可能是逃逸
// 面，故不进集合——否则白名单要为每次 make/len 加一行噪音。
var goBuiltinCalls = map[string]bool{
	"append": true, "cap": true, "clear": true, "close": true, "complex": true,
	"copy": true, "delete": true, "imag": true, "len": true, "make": true,
	"max": true, "min": true, "new": true, "panic": true, "print": true,
	"println": true, "real": true, "recover": true,
	// 预声明类型名的转换（语法上是 CallExpr）。
	"bool": true, "byte": true, "complex64": true, "complex128": true,
	"error": true, "float32": true, "float64": true, "int": true, "int8": true,
	"int16": true, "int32": true, "int64": true, "rune": true, "string": true,
	"uint": true, "uint8": true, "uint16": true, "uint32": true, "uint64": true,
	"uintptr": true,
}

// collectBuildRoutesCalls 枚举一个函数体里**直接**出现的调用。函数字面量内部的调用也算
// （闭包里照样能读 tpl），这正是"一层"该覆盖的范围：只看被调名字，不看调用深度。
func collectBuildRoutesCalls(body *ast.BlockStmt) []buildRoutesCall {
	seen := map[buildRoutesCall]bool{}
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch fn := unwrapCallFun(call.Fun).(type) {
		case *ast.Ident:
			if !goBuiltinCalls[fn.Name] {
				seen[buildRoutesCall{buildRoutesCallFunc, fn.Name}] = true
			}
		case *ast.SelectorExpr:
			// 只记方法名，不记接收者表达式：接收者是谁由读点门禁（`$S.*` 路径集）守，
			// 这里若记整串表达式，改一个局部变量名就会让门禁红——那是噪音，不是保护。
			seen[buildRoutesCall{buildRoutesCallMethod, fn.Sel.Name}] = true
		}
		return true
	})
	out := make([]buildRoutesCall, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

// unwrapCallFun 剥掉括号与泛型实例化，拿到真正的被调表达式。
func unwrapCallFun(e ast.Expr) ast.Expr {
	for {
		switch x := e.(type) {
		case *ast.ParenExpr:
			e = x.X
		case *ast.IndexExpr: // f[T](...)
			e = x.X
		case *ast.IndexListExpr: // f[T1, T2](...)
			e = x.X
		default:
			return e
		}
	}
}

// buildRoutesCallViolations 是调用面门禁的判据本体（空 = 合规）：观测集与白名单**相等**。
//
// 红点信息必须直接给出处置办法，否则下一个人只会去白名单里加一行——那正是这条门禁要
// 拦下来的反射动作。
func buildRoutesCallViolations(calls []buildRoutesCall) []string {
	var out []string
	observed := make(map[string]bool, len(calls))
	for _, c := range calls {
		observed[c.name] = true
		spec, known := buildRoutesCallWhitelist[c.name]
		switch {
		case !known:
			out = append(out, fmt.Sprintf(
				"给 buildRoutes 加了新的包内调用 `%s`：若 %s 接收 *domain.Template/planKey，模板读点就逃出了本门禁——"+
					"要么别抽这个 helper（spec §4.4 的默认选择），要么把 %s 的读路径补进 staticReadPaths 并把这里当成有意变更",
				c.name, c.name, c.name))
		case spec.kind != c.kind:
			out = append(out, fmt.Sprintf(
				"buildRoutes 调用 `%s` 的形态变了（白名单登记为 %s，实际是 %s）：同名换形态等于换了被调者，请重答一次「这会不会把模板读点搬出去」",
				c.name, spec.kind, c.kind))
		}
	}
	names := make([]string, 0, len(buildRoutesCallWhitelist))
	for name := range buildRoutesCallWhitelist {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if observed[name] {
			continue
		}
		out = append(out, fmt.Sprintf(
			"白名单陈旧：buildRoutes 不再调用 `%s`（原登记：%s）——有意删除请删掉这条登记；被改名请把新名字补进来并重答一次逃逸面问题",
			name, buildRoutesCallWhitelist[name].why))
	}
	sort.Strings(out)
	return out
}

// TestBuildRoutesCallsNoHelperOutsideItsWhitelist 正例：buildRoutes 的直接调用集必须
// **恰好**等于显式白名单（多一个靠 buildRoutesCallViolations 的未知分支，少一个靠陈旧
// 分支——两者都是相等断言的一半）。
func TestBuildRoutesCallsNoHelperOutsideItsWhitelist(t *testing.T) {
	root := gateRoot(t)
	srcs := gateSources(t, root, []string{"internal/scheduler"})
	calls := collectBuildRoutesCalls(funcDeclOf(t, srcs, "buildRoutes").Body)
	require.NotEmpty(t, calls, "扫描面必须真的抓到 buildRoutes 的调用（否则下面的相等断言是空转）")
	require.Empty(t, buildRoutesCallViolations(calls),
		"buildRoutes 的调用面越界；观测到：%v", calls)
	for name, spec := range buildRoutesCallWhitelist {
		require.NotEmpty(t, strings.TrimSpace(spec.why), "调用白名单必须写明理由：%s", name)
	}
}

// TestBuildRoutesCallGateRejectsNewHelper 负例：把 oracle 实际描述的绕过（把模型空间
// 循环抽成 `addModels(tpl, dst)`）当成**真源码**解析出来，走同一条判据。只加正例等于
// 自证——判据写反、或两侧恒空时正例一样是绿的。
func TestBuildRoutesCallGateRejectsNewHelper(t *testing.T) {
	callsOf := func(body string) []buildRoutesCall {
		t.Helper()
		src := "package scheduler\n\nfunc injected(a *accountSnapshot, tpl *domain.Template, models map[string]struct{}) {\n" +
			body + "\n}\n"
		f, err := parser.ParseFile(token.NewFileSet(), "injected.go", src, parser.SkipObjectResolution)
		require.NoError(t, err)
		require.Len(t, f.Decls, 1)
		fn, ok := f.Decls[0].(*ast.FuncDecl)
		require.True(t, ok, "注入的源码必须只有一处函数声明")
		return collectBuildRoutesCalls(fn.Body)
	}

	compliant := `
	formats := domain.RoutableFormats()
	av := a.static.Load()
	_ = av
	_ = formats
	space := tpl.HasModelSpace()
	_ = space
	for _, format := range formats {
		if !tplSupportsFormat(tpl, format) {
			continue
		}
	}`
	require.Empty(t, buildRoutesCallViolations(callsOf(compliant)),
		"对照：恰好落在白名单内的调用不得被拒")

	t.Run("模型空间循环抽成 helper", func(t *testing.T) {
		got := buildRoutesCallViolations(callsOf(compliant + "\n\taddModels(tpl, models)"))
		require.Len(t, got, 1, "只该报一条越界调用：%v", got)
		require.Contains(t, got[0], "addModels")
		require.Contains(t, got[0], "*domain.Template", "红点必须点明逃逸面长什么样")
		require.Contains(t, got[0], "staticReadPaths", "红点必须给出处置办法")
	})

	t.Run("白名单陈旧", func(t *testing.T) {
		got := buildRoutesCallViolations(callsOf("\tav := a.static.Load()\n\t_ = av"))
		require.NotEmpty(t, got, "少调用也必须红：否则白名单会烂成注释")
		joined := strings.Join(got, "\n")
		require.Contains(t, joined, "白名单陈旧")
		require.Contains(t, joined, "tplSupportsFormat")
	})

	t.Run("同名换形态", func(t *testing.T) {
		// 白名单把 RoutableFormats 登记为方法（`domain.RoutableFormats()`）；不限定调用
		// 是另一回事（同包内真有这么个函数时），必须重新回答一次。
		got := buildRoutesCallViolations(callsOf(compliant + "\n\tRoutableFormats()"))
		joined := strings.Join(got, "\n")
		require.Contains(t, joined, "形态变了")
		require.Contains(t, joined, "RoutableFormats")
	})
}
