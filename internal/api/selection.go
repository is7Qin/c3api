// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package api

import (
	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/egress"
)

// Selection 是一次派发的**只读事实集**：数据面读字段，绝不回调控制面。
//
// 形状契约（spec 2026-11-23 P3b §2.1，形状 A）：
//   - 只读事实保持**字段**（实测 20 个；brief 记 19，漏了 ProxyID）。数据面有
//     238 处直读；换成访问器会把热路径从一次字段加载变成一次方法调用（20 个
//     访问器还会扩大冻结面），不采纳。
//   - 控制面私有的租约/会话/池状态收进唯一的 Handle 字段（一个接口 = 16B）。
//     释放归还、会话推进、凭据投影只能经 SelectionHandle 的 4 个方法。
//   - 尺寸预算：unsafe.Sizeof(Selection{}) == 264 ≤ 288。三个私有指针（24B）收成
//     一个接口（16B），272→264 仍落在同一个 288B 分配档 ⇒ 每次预留 **0 额外堆字节**
//     （门禁：internal/api 的 TestSelectionSizeBudget 与 scheduler 的 3 allocs/328 B 基准）。
//
// 数据面**不得**写字段，句柄一律经 HandleOrDetached 取（nil / 零值 Selection ⇒
// 分离句柄，四方法 no-op/零值），不得直接对 Handle 调方法。Selection 的唯一生产
// 构造点是控制面预留（scheduler.reserveOnView），那里 Handle 与对象同时装配。
type Selection struct {
	AccountID       int64
	TemplateID      int64
	BaseURL         string
	Format          domain.RequestFormat
	UpstreamKey     string
	CredentialType  credential.Type
	Model           string // 已应用模型映射
	StripImageTools bool
	// SkipContinuation 本次派发是否**跳过** previous_response_id 的续接解析
	// （构造期定型：预留装配点按两条逐 Type 能力位算好，见 scheduler.skipContinuationFor）。
	//
	// 今天只有 BPS 为真：BPS 既没有服务端续接、也不吃 c3api 的绑定账本，id 原样留在
	// 请求里由 SDK 丢弃（c3api 不做 body 工作）。它与能力位
	// credential.SupportsContinuation 是**两个问题**：能力位回答"这个类型能不能原生
	// 续接"（计划级、决定要不要 fail-closed 解析），本政策位回答"这次派发要不要连
	// 绑定账本都不碰"（请求级）。
	SkipContinuation bool
	// Upstream custom 的出站上游修饰（逐格式路径 + request_patch，同一结构体
	// 指针；只读引用快照对象，不复制、不修改；nil = 未配置 = 全部回落官方默认
	// 端点、无请求修饰）。map 8B → 指针 8B：Sizeof 仍 264（§5.1）。
	Upstream *domain.TemplateUpstream
	// CodexResponsesTransport 原生 codex responses 的出站传输（构造期参数，
	// 来自 tpl.CodexResponsesTransport 原值；消费方只调 Key()/Name()）。
	//
	// 为什么是构造期而不是热路径判断：它由模板配置决定、随快照定型；热路径
	// 多一次分支只为读一个已经定型的事实。它进 planKey（换值 ⇒ 换叶子）与
	// sdkbridge 的 client 缓存键（换值 ⇒ 换 client），两处缺一即静默失效。
	CodexResponsesTransport domain.CodexResponsesTransport
	Ext                     *domain.AccountExt
	CandidateFingerprint    string
	// Conversation 本次预留走**会话路径**（= AttemptPlanIdentity.Conversation）：
	// 未认领槽位（handle 侧 session 恒 nil），身份由 SDK 会话簿按对话合成。proxy
	// 只读这一位决定走会话簿，不读未导出的 session。
	Conversation bool
	// CacheDomain 选中账号的**生效**缓存域（私有域 = `private:<账号ID>`）、
	// InstallationID 选中账号的 installation：会话路径构造会话键与请求的来源
	// （不认领槽位后 Thread 是零值，不能从那里取）。均为快照构建期定型的
	// 账号静态视图字段，预留时只做字段拷贝（零分配）。
	CacheDomain      string
	InstallationID   string
	ModelMappingMode domain.ModelMappingMode
	// SupplierFinance 选中本账号时捕获的不可变财务上下文（spec 2026-10-09
	// §4.2/§4.6）：供给准入门放行候选时**同一次视图读取**捕获归属/分成/代数，
	// 随本 Selection 携带；收尾（含 failover 获胜尝试）只按该捕获值落账——
	// 不再回查 owner。
	SupplierFinance domain.SupplierFinance
	// Egress 本次选中账号的**出口 client**（**恒非 nil**：未绑定 = Pool.Direct()）。
	// 快照期已解析好，出栈处只读这个指针并把它交给 aiclient/sdkbridge（M3）——
	// 0 次 nil 判断、0 次 map 读、0 分配（spec §4.2 的 G5 零开销要求）。
	Egress *egress.Client
	// ProxyID 本次选中账号绑定的出口代理行 id（nil = 直连）。与 Egress **同一次
	// 快照读**取（同一字段来源 `acc.ProxyID`），只服务失败面的可观测性：网络级
	// 失败时 err_logs.message 要写 `proxy_id=<id>`（spec §5.1/§5.2），而 Spec 是
	// 摘要值化后的连接规格、按设计不带行 id（同规格不同行会共用同一个 client）。
	// 它**不参与**任何决策：可用性由 `Routable` 一位决定，出口由 `Egress` 决定。
	ProxyID *int64
	// Handle 控制面私有租约/会话句柄。数据面只经 HandleOrDetached 取它、只许调
	// 4 个方法（见 SelectionHandle）；nil = 分离 selection（无租约），只出现在
	// 没有控制面装配的测试夹具里。
	Handle SelectionHandle
}
