// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/egress"
	"github.com/is7qin/c3api/pkg/logx"
)

// Codex 是 codex SDK 适配层（§1——SDK 调用集中于此，codexsdk import 仅限
// 本文件族）：cred → Auth 账号级缓存 + GenerateImage 包装 + 信封包装 +
// fatal → 统一回调（双源去重）+ 轮转回写。新增能力（流式 / Dial /
// resp）同形态扩展本文件。
type Codex struct {
	// clients 账号 → 传输资源表（条目 + 出口×传输 的 HTTPClient）。**构造期注入的
	// 共享组件**：同一个实例也交给调度器（线程退役 / 账号移除），所以两侧不互相
	// 持有、构造顺序单向（AGENTS.md「禁止事后回填」）。nil = 本适配层自建（无调度器
	// 共享的用法，如纯单位用例；生产装配必须显式传同一个实例）。
	clients *ClientTable
	failure FailureHandler        // 统一失效回调；nil = no-op（测试/未装配）
	rotate  RotationStore         // 轮转回写落库面；nil = 不落库（测试/未装配）
	inval   func(accountID int64) // 回写后失效账号快照条目（下个会话重载新凭据）；nil = 不失效
	log     *logx.Logger          // 回写/失效错误日志；nil = 不记
	// wsCache 每个 client 的 WS 空闲连接上限（构造期参数；见 Options.WSConnCache）。
	wsCache int
	// egress 出口池（构造期注入；M3 取代旧的单个 http.RoundTripper 字段）。
	// SDK 客户端的 transport 是**构造期**参数，所以出口只能按账号在 clientFor
	// 构造时注入（`eg.Codex.Transport`——同一出口派生的关闭自动压缩连接池，见
	// egressCodexTransport）；同一账号
	// 不同出口靠 clientKey 的 proxy 维分桶。nil 且调用面要求出口 ⇒ 明确 error
	// （errEgressNotWired），绝不静默退回底座。
	egress *egress.Pool
	// usage 额度快照拉取的实例级节流与时间参数（S8——原包级全局挂实例，
	// 消除跨调用面共享的可变包级状态）：usageSem 有界并发 semaphore；usageTTL
	// 快照 TTL；usageCooldown 失败冷却。三者构造期定值，测试按实例调整。
	usageSem      chan struct{}
	usageTTL      time.Duration
	usageCooldown time.Duration
}

// (SetTransport setter deleted by hygiene: transport is a construction-time
// dependency — see NewCodex. A post-construction setter leaves the adapter
// observably half-built between New and Set, with cached clients already
// keyed to the wrong transport.)
//
// (The transport field itself became egress *egress.Pool in M3: the transport is
// no longer one process-wide dependency but a property of the account's egress.
// spec-proxy-pool §4.6.)

// RotationStore 轮转回写落库面（repository.AccountExtRepo 满足；接口化供测试
// 注入与装配侧解耦）。部分更新 upsert——仅 codex_oauth_token/
// codex_oauth_refresh_token/codex_oauth_expires_at（expiresAt 为调用方携带的
// 旧值，保旧语义），其余列不动。
//
// **围栏（§3.3b-2 的轮转回写半边）**：写入必须带来源身份代际 K，存储侧以
// `WHERE … AND account.identity_revision = expected` 提交；命中 0 行 = 这一版
// 身份已被管理面替换 ⇒ 返回 repository.ErrStaleIdentityRevision，**不是**重试
// 信号（陈旧轮转不得覆盖新凭据，也不得反过来禁用账号）。
//
// 为什么围栏维度不是旧 RT：RT 每轮都轮换，多实例下同一账号的两个 Auth 各自
// 轮转（DB 的 RT 由先到者写成新的），用旧 RT 作谓词会把合法轮转判成陈旧、让
// DB 永久停在已吊销的那一版；K 只由管理面身份写入推进，SDK 自刷新中性。
type RotationStore interface {
	WriteOAuthRotation(ctx context.Context, accountID int64, expectedIdentityRevision int64, at, rt string, expiresAt *time.Time) error
}

// RotationDeps 轮转回写依赖（§1——main 装配：repository.AccountExts +
// scheduler）。
type RotationDeps struct {
	Store RotationStore
	// InvalidateSnapshot 回写成功后失效调度器 AccountExt 内存快照对应条目
	// （下个会话重载新凭据）；nil = 不失效（测试/未装配）。
	InvalidateSnapshot func(accountID int64)
	// Log 回写/失效错误日志（旋转低频事件，错误恒 Warn 记一条）；nil = 不记。
	Log *logx.Logger
}

// (SetRotationDeps setter deleted by hygiene: same reason — see NewCodex.)

// codexWSConnCacheCapacity 是每个 HTTPClient（即每个账号 × 出口）的空闲 WS 连接
// **总数上限**（仅上限、不预分配；SDK 的 connCache 按需 append，满则淘汰最旧）。
//
// 取 256（设计 §5、§8-9）：旧值 2048 = 2 × 槽位池上界（maxIdentityPoolSlots
// 1024），前提是“每个槽位线程两条长连接”。有对话标识的请求改走 SDK 会话簿后，
// 线程号按对话而非槽位分配，缓存键还带 routing-hint，槽位上界不再是空闲连接数
// 的上界，2 × 1024 只剩放大文件描述符占用（账号数 × 出口数 × 上限）。空闲连接
// 只在同一线程的下一个请求（一轮内后续请求、紧接的下一轮）有复用价值，活跃连接
// 不计入上限；256 覆盖单账号同时活跃的对话线程量级，淘汰只让冷线程下次重拨
// （重拨按 SDK 规则重新预热，不影响正确性）。
//
// **必须显式注入**：SDK 默认 0 = 不复用（每请求一条新连接）——不接这一行，"跨轮
// 复用"整条链就是零收益（spec GAP-7）。
const codexWSConnCacheCapacity = 256

// codexEntry 单账号缓存条目：Auth（HTTP/WS 双面共享——at 缓存/单飞/rt 轮换
// 在 SDK Auth 内）+ 重建判定签名 + fatal 已上报标记（双源去重——回调路径与
// errors.As 路径共享同一 CAS）。
// expiresAt 为构造时凭据携带的旧过期时刻（轮转回调无 expiry，回写保旧
// 用；外部凭据变更 → 重建刷新）。
//
// client 桶**挂在条目上**（键 = 出口 × responses 传输），表只映射 accountID → 当前
// 条目：桶的可见性与条目同生命周期，于是"条目被替换/删除"= "它的桶再也无人可达"。
// 这是「旧凭据的请求被新条目复用」与「账号移除后旧句柄重建可复用桶」两条边界的结构性
// 答案（§3.2 凭据替换 / 账号移除）。回收点：条目重建/移除（整条目摘除）与配置批次
// 的资源指令（`ClientTable.ApplyBatch` 按出口**资源键**保留/回收）——代理地址/密码
// 轮换每次都产生新键，不回收即无界；TTL 只管连接不管这张表。
//
// turn-state 不在条目上：存储属会话（scheduler.codexSession），经 Identity.Sink
// 写入、经 Identity.TurnState 读出。
type codexEntry struct {
	accountID int64
	auth      codexsdk.Auth
	sig       credKey // 凭据签名（外部凭据变更 → 重建）
	// srcRev **本条目**凭据内容的来源身份代际 K（`AccountCredential.IdentityRevision`，
	// 与凭据列同一次快照读）。它只回答**资源归属**问题：`supersededBy` 判"这份快照
	// 是否已被表里的条目超越"、`invalidateOlder` 判"表里的条目是否属于更旧的身份"
	// ——两者都是"要不要动这张表"，不是"这条事件属于哪一代"。
	//
	// **请求级证据不用它**：条目按签名复用、这个值会被后来的同签名请求单调抬升，所以
	// 在飞请求的判决（errors.As 补报、构造失败）用那一版请求自己的 K（`Prepared.srcRev`
	// 或凭据快照的版本），绝不回读这里——请求可能被旧出口/旧配置污染，那条判决不得
	// 升格成新身份的事实。
	//
	// 反过来，**凭据自身**的证据（Auth 判死回调、轮转回写）必须用它：它们说的是这条活
	// 条目此刻的凭据，K 前进只是管理面对同一份凭据内容的重新授权（见 buildAuth /
	// rotateWriteback）。0 = 来源未知（无 Selection 调用面）。
	//
	// 语义是"该凭据内容上**观测到的最大** K"：同签名的新 K 只可能来自不动凭据内容
	// 的身份写入（base_url/template_id/installation），内容既相同，资源归属就该按新
	// K——故复用时**单调抬升**、从不回退（atomic：事件路径无表锁读它）。
	srcRev atomic.Int64
	// owner 构造**本条目**那版凭据的稳定 owner（§3.2「凭据替换」的 ts 清空判据）。
	// 它是条目的稳定字段：请求准备阶段（Prepare）从这里读，不再对请求里的凭据快照
	// 重新散列——否则条目还没重建时，散列给出的是"更新的 owner + 旧的 auth"，owner
	// 与实际出网凭据分叉。owner/Identity/client-auth 因此在同一版本上。
	owner [16]byte
	// clients 本条目私有的 HTTPClient 桶（出口 × responses 传输）。**只由 ClientTable
	// 在表锁内访问**——条目一旦离开表就不可达，迟到写入自然落空。
	clients   map[clientKey]*codexsdk.HTTPClient
	expiresAt *time.Time
	reported  atomic.Bool
	// usage 额度快照缓存（GetUsageSnapshot 5min TTL）+ 失败冷却起点
	// （60s——gate Major 2）：重建（sig 变化）随新条目一并清除；usageErr 只存
	// 分类哨兵（ErrAuthExpired/ErrUpstream），不缓存错误体。
	usage      *domain.CodexUsageSnapshot
	usageAt    time.Time
	usageErrAt time.Time
	usageErr   error
	// usageMu 保护上面四个字段：它们由 usage 拉取协程与快照读取面并发访问，与
	// **条目重建**无关——所以用条目自己的叶锁，不复用表锁（表锁只保护 map 与
	// client 桶的摘除）。
	usageMu sync.Mutex
}

// Options 构造期开关（零值 = 生产默认）。
type Options struct {
	// WSConnCache 每个 HTTPClient 的 WS 空闲连接上限；0 = 生产默认
	// （codexWSConnCacheCapacity）。它只是**可测性**入口：上限是构造期参数，
	// 而"空闲数不超过构造期上限"（G7）只有把小容量注入进去才能行为观测。
	WSConnCache int
}

// NewCodex 构造 codex 适配层。failure 为统一失效回调（适配层构造注册
// WithOnAuthFatal → 回调；nil = 上报 no-op——测试替身形态）；egressPool 为出口池
// （构造期注入，生产 = main 里与调度器 Config.Egress 同一个池；客户端 transport
// 就从这个池的 client 上取，spec-proxy-pool §4.6）；clients 为账号传输资源表
// （构造期注入，生产 = 与 scheduler.Config.RetireThread、scheduler.Config.ClientResources
// 指向同一个实例；nil = 本适配层自建，只适用于不需要跨组件退役的用法）；rotation 为轮转回写面
// （零值 = 不落库/不失效/不记，测试与未装配形态）。
// 全部构造期依赖一次给齐——构造后不存在“半装配”的 Codex，也没有事后回填。
func NewCodex(failure FailureHandler, egressPool *egress.Pool, clients *ClientTable, rotation RotationDeps, opts ...Options) *Codex {
	if clients == nil {
		clients = NewClientTable()
	}
	wsCache := codexWSConnCacheCapacity
	for _, o := range opts {
		if o.WSConnCache > 0 {
			wsCache = o.WSConnCache
		}
	}
	return &Codex{
		clients:       clients,
		failure:       failure,
		egress:        egressPool,
		rotate:        rotation.Store,
		inval:         rotation.InvalidateSnapshot,
		log:           rotation.Log,
		wsCache:       wsCache,
		usageSem:      make(chan struct{}, usageFetchConcurrency),
		usageTTL:      defaultUsageSnapshotTTL,
		usageCooldown: defaultUsageCooldown,
	}
}

// errEgressNotWired 出口池未装配（测试/未装配形态）而调用面又要求一个出口。
// 聚合一处决策：**不静默直连**——装配错误必须作为可诊断的错误浮上来（与 M2
// 装载期 ErrEgressNotWired 同一条纪律，只是这条落在调用面）。
var errEgressNotWired = errors.New("sdkbridge: egress pool not wired")

// resolveEgress 取本次调用生效的出口。调用面（proxy 层）恒给 `sel.Egress`
// （**恒非 nil**：未绑定 = 池的 Direct）；没有 Selection 的内部调用面
// （usage 额度快照）传 nil → 取池的直连出口。池未装配 ⇒ errEgressNotWired。
func (a *Codex) resolveEgress(eg *egress.Client) (*egress.Client, error) {
	if eg != nil {
		return eg, nil
	}
	if a.egress == nil {
		return nil, errEgressNotWired
	}
	return a.egress.Direct(), nil
}

// DirectEgress 本适配层的直连出口（池的 Direct 单例；池未装配 ⇒ nil）。给没有
// Selection 的调用面（usage 快照拉取）与测试面用：它是“直连也是一条出口”的唯一
// 暴露点，不是旁路。
func (a *Codex) DirectEgress() *egress.Client {
	if a.egress == nil {
		return nil
	}
	return a.egress.Direct()
}

// GenerateImage 非流式生图包装（§1）：cred → 缓存取 HTTPClient →
// c.GenerateImage(ctx, toSDKParams(p))；domain↔codexsdk 双向转换集中本文件。
// eg 为本次出口（**必填**：`sel.Egress`，恒非 nil；直连就是池的 Direct）。
// 错误翻译（translateError）：SDK *HTTPError → 网关侧信封错误（EnvelopeError——
// StatusCode()/RawJSON()/Unwrap 链，网关 statusOf/upstreamErrMsg 零改动复用）；
// 注：*HTTPError 自身现在也有 RawJSON()（SDK 侧 WS 错误帧翻译用），包装保留是为了
// 统一 StatusCode() 方法面与 Unwrap 链；
// fatal 五类（errors.As）→ 统一回调单次上报（双源去重）+ 原样透传（SDK 已
// 不包装，errors.As 可命中）；RefreshError 可重试不上报。Codex 端点归 SDK 官方默认。
func (a *Codex) GenerateImage(ctx context.Context, cred *domain.AccountCredential, eg *egress.Client, p *domain.ImageGenParams) (*domain.ImageResponse, error) {
	// 生图不走 responses 面（spec §5）：传输维恒零值 ⇒ 键与改动前同条目。
	e, client, err := a.clientFor(cred, eg, domain.CodexResponsesTransportDefault)
	if err != nil {
		return nil, err
	}
	resp, err := client.GenerateImage(ctx, toSDKParams(p))
	if err != nil {
		return nil, a.translateError(e, cred.IdentityRevision, err)
	}
	return fromSDKResponse(resp), nil
}

// GenerateImageStream 流式生图包装（生产接线——合成流式：SDK 内部非流式
// 调 + 等待期 keepalive 合成 + completed 逐张合成，网关零合成逻辑）：cred →
// 缓存取 HTTPClient → c.GenerateImageStream(ctx, toSDKParams(p), fn)；事件翻
// 译 codexsdk.ImageStreamEvent → domain.ImageStreamEvent（Type/B64JSON/Usage
// 逐字段映射——usage 平铺直透，网关 completed 帧 JSON tag 直透同一口径）；
// 错误翻译同 GenerateImage（translateError——信封/fatal 统一回调/refresh 分
// 类复用；fn 回调错误经 translateError 原样透传——非 SDK 错误不过滤）。
func (a *Codex) GenerateImageStream(ctx context.Context, cred *domain.AccountCredential, eg *egress.Client, p *domain.ImageGenParams, fn func(domain.ImageStreamEvent) error) error {
	// 传输维恒零值（同 GenerateImage：生图不经 responses 面）。
	e, client, err := a.clientFor(cred, eg, domain.CodexResponsesTransportDefault)
	if err != nil {
		return err
	}
	err = client.GenerateImageStream(ctx, toSDKParams(p), func(ev codexsdk.ImageStreamEvent) error {
		// 事件类型显式映射：SDK 升级改事件名 → 未知 Warn + 跳过
		// （不静默透传——未知类型落入网关 default 静默分支则落账 0 张零告警）。
		t, ok := mapStreamEventType(ev.Type)
		if !ok {
			if a.log != nil {
				a.log.Warn("codex image stream: unknown event type skipped", logx.String("type", ev.Type))
			}
			return nil
		}
		var usage *domain.ImageUsage
		if ev.Usage != nil {
			usage = &domain.ImageUsage{
				InputTokens:       ev.Usage.InputTokens,
				InputImageTokens:  ev.Usage.InputImageTokens,
				OutputTokens:      ev.Usage.OutputTokens,
				OutputImageTokens: ev.Usage.OutputImageTokens,
			}
		}
		return fn(domain.ImageStreamEvent{Type: t, B64JSON: ev.B64JSON, Usage: usage})
	})
	if err != nil {
		// 与 GenerateImage 同款（评审修复）：SDK *HTTPError（StatusCode/Raw 是**字段**，
		// 不是 StatusCode()/RawJSON() 方法——RawJSON() 自 SDK 的 WS 错误帧翻译起已存在，
		// 但方法面仍不完整）→ EnvelopeError 包装——网关 statusOf/
		// upstreamBody/streamErrMessage 的协议才能消费（4xx 状态 + 原始 body
		// 透传、SSE error 帧 message 取上游文案）；fatal 五类统一回调单次上报
		// + 原样透传。fn 回调错误（网关写入失败/客户端断开）非 SDK 错误 →
		// translateError 原样透传（不过滤）。
		return a.translateError(e, cred.IdentityRevision, err)
	}
	return nil
}

// Responses 非流式 responses 合成调用（§1）：cred → 缓存取 HTTPClient
// （clientFor——机制复用）→ c.Responses(ctx, payload, id)（SDK 合成非流式——
// 内部无条件 stream:true + SSE 事件聚合重组完整响应体；网关以非流式语义消费，
// 原样转发；网关从合成体顶层 usage 计费）。
//
// id 是**请求级身份**（§2.1）：Session/Meta 进握手头与 client_metadata、TurnState
// 是本次要带的 ts、Sink 是签发值落点。上游签发值（握手头/响应头/事件）只经 Sink
// 写入会话——本函数既不在请求前改 ts，也不在返回后回写：那是把事件签发的新值
// 倒退成请求开始时的快照。
//
// 错误翻译同 GenerateImage（translateError——SDK *HTTPError → 信封；fatal 五类
// 统一回调双源去重；RefreshError/网络原样）。transport 为出站传输开关（spec §5：
// 构造期参数 ⇒ 走 clientFor 的键维度；映射点只有 codexTransport 一处）。
func (a *Codex) Responses(ctx context.Context, cred *domain.AccountCredential, eg *egress.Client, payload []byte, id codexsdk.Identity, transport domain.CodexResponsesTransport) (*codexsdk.HTTPResponse, error) {
	p, err := a.Prepare(cred)
	if err != nil {
		return nil, err
	}
	return a.ResponsesPrepared(ctx, p, eg, payload, id, transport)
}

// ResponsesPrepared 用**已准备句柄**发起非流式 responses 调用（生产热路径）：
// 条目来自该句柄，auth/client 与 owner 同版（§3.2 凭据替换）。语义同 Responses。
func (a *Codex) ResponsesPrepared(ctx context.Context, p Prepared, eg *egress.Client, payload []byte, id codexsdk.Identity, transport domain.CodexResponsesTransport) (*codexsdk.HTTPResponse, error) {
	if p.entry == nil {
		return nil, errUnprepared
	}
	client, err := a.clientForEntry(p.entry, eg, transport)
	if err != nil {
		return nil, err
	}
	resp, err := client.Responses(ctx, payload, id)
	if err != nil {
		return nil, a.translateError(p.entry, p.srcRev, err)
	}
	return resp, nil
}

// Search codex search 端点透传调用（spec 2026-08-13）：cred → 缓存取
// HTTPClient（clientFor——统一 client 形态直接复用，无独立实例问题）→
// e.client.Search(ctx, payload)（官方默认端点 https://chatgpt.com/backend-api/codex/alpha/search；请求/响应体 opaque 零解
// 析——alpha 端点实验性，上游变更网关免疫）。**无头注入**（x-codex-turn-
// metadata 统一不转发——与 resp HTTP 路径现状一致；SDK Search 默认头面无该
// 头）。错误翻译同 Responses（translateError——信封/fatal 统一回调双源去重/
// RefreshError 分类复用）。
func (a *Codex) Search(ctx context.Context, cred *domain.AccountCredential, eg *egress.Client, payload []byte) (*codexsdk.HTTPResponse, error) {
	// 传输维恒零值（Search 不走 responses 面——键与改动前同条目）。
	e, client, err := a.clientFor(cred, eg, domain.CodexResponsesTransportDefault)
	if err != nil {
		return nil, err
	}
	resp, err := client.Search(ctx, payload)
	if err != nil {
		return nil, a.translateError(e, cred.IdentityRevision, err)
	}
	return resp, nil
}

// StreamBody 流式 responses **raw body** 透传：cred → 缓存取 HTTPClient →
// c.StreamBody(ctx, payload, id) 返回**未关闭**的 *http.Response（body 归调用方，
// proxy 侧经 sserelay.Relay 消费后**恰好关闭一次**）。身份语义同 Responses
// （TurnState 是本次要带的 ts、Sink 是签发值落点）。**本方法不写签发值**：本次
// resp.Header 的 ts 与流式取帧期间 `response.metadata` 事件的新值都由 SDK 经 Sink
// 写会话（后者才是更新的值；回写响应头只会把新值倒退成快照）。
// 错误翻译同
// Responses（translateError——信封/fatal 统一回调双源去重/RefreshError 分类复用）。
// transport 语义同 Responses（构造期参数 ⇒ 换值换 client 条目）。
func (a *Codex) StreamBody(ctx context.Context, cred *domain.AccountCredential, eg *egress.Client, payload []byte, id codexsdk.Identity, transport domain.CodexResponsesTransport) (*http.Response, error) {
	p, err := a.Prepare(cred)
	if err != nil {
		return nil, err
	}
	return a.StreamBodyPrepared(ctx, p, eg, payload, id, transport)
}

// StreamBodyPrepared 用**已准备句柄**发起流式 responses 调用（生产热路径）：
// 条目来自该句柄，auth/client 与 owner 同版。语义同 StreamBody。
func (a *Codex) StreamBodyPrepared(ctx context.Context, p Prepared, eg *egress.Client, payload []byte, id codexsdk.Identity, transport domain.CodexResponsesTransport) (*http.Response, error) {
	if p.entry == nil {
		return nil, errUnprepared
	}
	client, err := a.clientForEntry(p.entry, eg, transport)
	if err != nil {
		return nil, err
	}
	resp, err := client.StreamBody(ctx, payload, id)
	if err != nil {
		return nil, a.translateError(p.entry, p.srcRev, err)
	}
	return resp, nil
}

// entryFor cred → 账号级缓存条目（构造冷面——每账号首次/凭据变更后；互斥锁
// + 签名比对，同账号并发请求单飞构造——对齐 SDK OAuth 单飞 refresh 语义）：
//   - 同账号复用（Auth 内 at 缓存/轮转状态保持；sig 相同直接返回）
//   - 仅外部凭据变更（管理面导入/更新——token/rt/pat 任一变化 → sig
//     不同）后重建；**轮转回调写回不重建**（回调写回的是本 Auth 内部已更新
//     的状态，重建丢 at 缓存破坏轮转连续性——写回走管理面通道，不经缓存）
//   - 失效剔除（联动）：fatal 上报后 evict，恢复后重建
//   - 空 rt 防护：codex-oauth 缺 refresh_token → 按失效上报（账号凭据
//     不完整）不 panic（OAuthWithRotation 空 rt 构造 panic）；PAT 走 PAT(key)
//     无此面
//   - 重建 = 新条目构造——usage/usageAt/usageErrAt/usageErr 一并清除（对齐
//     auth 重建；凭据变更后快照重拉）
//
// 条目承载 Auth（HTTP 面 GenerateImage/Stream 与 WS 面 Dial 共享——连接
// per-请求不缓存，Auth 账号级状态跨面复用；HTTPClient 由 clientFor 懒构造）。
func (a *Codex) entryFor(cred *domain.AccountCredential) (*codexEntry, error) {
	e, err := a.clients.Entry(cred.AccountID, credSig(cred), cred.IdentityRevision, func(e *codexEntry) (codexsdk.Auth, error) {
		// owner 随条目一起定型（同一版凭据）：重建/复用都保持 owner 与 auth 同版，
		// 请求准备只需读这个字段（见 Prepared）。
		e.owner = CredOwner(cred)
		return a.buildAuth(cred, e)
	})
	if err != nil {
		// 构造失败（空 rt 等）：锁外上报（上报链要取表锁——锁内调用即重入死锁）。
		// 这是**来源凭据版本**的失败（不是"账号配置的全局事实"）：上报带来源 K 与
		// 来源签名，只有它仍是当前版本时才允许摘掉资源/升级为账号级动作。
		a.reportConfigFailure(cred, err)
		return nil, err
	}
	return e, nil
}

// Prepared 是一次请求的**账号准备句柄**（§3.3 请求准备的第一步）：条目（auth + 该
// 条目私有的 client 桶）。owner 从该条目的**稳定字段**读（`Owner`），不再对请求里的
// 凭据快照重新散列——那是"更新的 owner + 还没换的 auth"的分叉来源。
//
// 值类型、栈上传递：不额外分配，也不引入第二次查表。生命周期：由 `Prepare` 在请求
// 准备阶段取得；owner 对齐、Identity 构造与 client/auth 取用都从**同一个句柄**读。
// 条目之后被替换/删除时它仍是合法指针（在飞请求照常完成），owner/auth 恒为它自己
// 那一版。
type Prepared struct {
	entry *codexEntry
	// srcRev 这条请求**准备时冻结**的来源身份代际 K（`AccountCredential.IdentityRevision`）。
	// 它是本句柄产生的**请求级证据**（errors.As 补报、构造失败）的唯一来源版本：条目按
	// 签名复用、条目上的 `srcRev` 会被后来的同签名 Prepare 单调抬升，所以"此刻条目上
	// 的值"不是这条请求的版本——回读它就会让在飞请求的旧判决顶上新身份。凭据自身的证据
	// 不走这里（Auth 判死回调 / 轮转回写用条目当前 K）。0 = 来源未知（无 Selection 面）。
	//
	// 值类型字段：随句柄在栈上传递，不引入分配。
	srcRev int64
}

// errUnprepared 空句柄的使用错误（调用方忽略了 Prepare 的错误）。装配错误必须显形，
// 不静默回退到"按请求凭据再查一次"（那正是要消除的路径）。
var errUnprepared = errors.New("sdkbridge: codex request prepared without entry")

// Prepare 取本次请求的账号准备句柄：entryFor（无网络）+ 条目稳定字段 + **冻结的来源
// K**。错误语义与适配层调用一致（空 rt/构造失败 → 交给调用方的上游错误分类）。
//
// K 取自**请求自己的凭据快照**（不是条目上的当前值）：条目可以被同签名的后到请求
// 抬升，而这条请求的事件永远属于它准备时的那一代身份。
func (a *Codex) Prepare(cred *domain.AccountCredential) (Prepared, error) {
	e, err := a.entryFor(cred)
	if err != nil {
		return Prepared{}, err
	}
	return Prepared{entry: e, srcRev: cred.IdentityRevision}, nil
}

// Owner 本句柄的凭据 owner（条目构造时定型的稳定字段；零值 = 空句柄）。
func (p Prepared) Owner() [16]byte {
	if p.entry == nil {
		return [16]byte{}
	}
	return p.entry.owner
}

// codexTransport 领域开关 → SDK 传输枚举的**唯一**映射点。
// ws → TransportWS，其余（含 ""/"http"/脏值）→ TransportHTTP：脏值由
// CodexResponsesTransport.Key()/Valid() 在写面与快照面归一，这里 fail-safe
// 到默认面。
//
// 为什么显式映射而不是 `codexsdk.Transport(t)`：那依赖"domain 字符串恰好等于
// SDK 枚举"这个巧合，任一侧改名就静默错配（WS 折成 HTTP 时两臂读数会"一致地
// 看不出差别"，是事后最难定位的故障）。
func codexTransport(t domain.CodexResponsesTransport) codexsdk.Transport {
	if t == domain.CodexResponsesTransportWS {
		return codexsdk.TransportWS
	}
	return codexsdk.TransportHTTP
}

// clientKey 是 HTTPClient 缓存的**纯传输**维度：出口 × responses 传输。两者都是
// SDK 的构造期参数（换代理/换传输后复用旧 client = 请求仍走旧线路）。身份不在这里
// ——身份是请求数据（§3.1），同一条 client 服务该账号全部会话。
type clientKey struct {
	// proxy 出口维（spec-proxy-pool §4.6.3）：= 本次出口的**资源键**
	// （`egress.ResourceKey`：Spec 摘要，直连为全零）。
	proxy domain.EgressResourceKey
	// transport responses 出站传输（spec §5）；取值面 = CodexResponsesTransport.Key()，
	// 默认面取零值 ⇒ 只走 HTTP 的部署键形状与改动前一致。
	transport string
}

// egressDigest 出口资源键（clientKey 的 proxy 维）：直连 → 零值，代理 →
// `Spec.Key()`。实现收在 pkg/egress（配置权威也要算同一个键，不得两份定义）。
func egressDigest(eg *egress.Client) domain.EgressResourceKey {
	return domain.EgressResourceKey(egress.ResourceKey(eg))
}

// clientFor entryFor + HTTPClient 懒构造（GenerateImage/Stream/Responses/Search/
// GetUsage 面——键 = clientKey，同传输组合复用连接池）。
//
// 身份与 turn-state 都不是构造期参数（§3.1）：本次请求带什么由 Identity 决定，
// 签发值由 Sink 落到会话——所以"ts 更新"不构成重建理由，同一条 client 跨轮存活。
// 这也是 S1（连接身份去 ts）在网关侧成立的前提：client 跨轮存活，连接才可能跨轮
// 存活。
//
// NewHTTPClient 为纯构造（无 I/O 无 error）；取表/构造全程在 ClientTable 的锁内。
// 返回选中的客户端供调用面直用。
//
// transport 为 responses 出站传输（spec §5）：SDK 侧同样是**构造期**参数，不入
// 键就会"Selection 说 ws、请求仍走 HTTP POST"。
//
// 出口一律来自调用方（proxy 给 `sel.Egress`，usage 面给池直连）；取 client 只按
// (条目 × 出口 × 传输) 命中或构造，**不定义账号活跃集**——保留还是回收账号资源由
// 配置权威下发的资源指令决定（`ClientTable.ApplyBatch`）。
func (a *Codex) clientFor(cred *domain.AccountCredential, eg *egress.Client, transport domain.CodexResponsesTransport) (*codexEntry, *codexsdk.HTTPClient, error) {
	return a.clientForEgress(cred, eg, transport)
}

// usageClientFor 无 Selection 的辅助面（额度快照）取 client：出口恒为直连。
// 它只按配置权威下发的资源指令范围取/建资源，不参与定义活跃集——否则一次 usage
// 请求就会把在用的代理桶摘掉并永久 Close（"跨窗口不重连"直接失效）。
func (a *Codex) usageClientFor(cred *domain.AccountCredential) (*codexEntry, *codexsdk.HTTPClient, error) {
	return a.clientForEgress(cred, a.DirectEgress(), domain.CodexResponsesTransportDefault)
}

// clientForEgress 是两条调用面共同的本体：条目（entryFor）→ 条目私有桶（Client）。
func (a *Codex) clientForEgress(cred *domain.AccountCredential, eg *egress.Client, transport domain.CodexResponsesTransport) (*codexEntry, *codexsdk.HTTPClient, error) {
	eg, err := a.resolveEgress(eg)
	if err != nil {
		return nil, nil, err
	}
	e, err := a.entryFor(cred)
	if err != nil {
		return nil, nil, err
	}
	return e, a.clientOnEgress(e, eg, transport), nil
}

// clientForEntry 已取得条目句柄后的取 client 面（Prepared 路径用它，**不再查条目**
// ——请求准备与取 client 必须是同一个句柄，否则替换窗口里 owner 与实际 auth 又会
// 分叉）。eg 由 resolveEgress 归一（nil → 池直连；池未装配 ⇒ 错误）。
func (a *Codex) clientForEntry(e *codexEntry, eg *egress.Client, transport domain.CodexResponsesTransport) (*codexsdk.HTTPClient, error) {
	eg, err := a.resolveEgress(eg)
	if err != nil {
		return nil, err
	}
	return a.clientOnEgress(e, eg, transport), nil
}

// clientOnEgress 出口已归一后的取/建 client 本体（条目私有桶；miss 时按该条目的
// auth 构造）。
func (a *Codex) clientOnEgress(e *codexEntry, eg *egress.Client, transport domain.CodexResponsesTransport) *codexsdk.HTTPClient {
	key := clientKey{proxy: egressDigest(eg), transport: transport.Key()}
	return a.clients.Client(e, key, func() *codexsdk.HTTPClient {
		return codexsdk.NewHTTPClient(e.auth,
			// 出口 transport（spec §4.6）：eg 恒非 nil（resolveEgress 保证），用该出口
			// 的 Codex 底座（关闭自动压缩的独立连接池，egressCodexTransport）。底座是
			// 裸 client（`Transport == nil`）时 WithTransport(nil) 等价于"不加
			// Transport"——所以这里恒注入、不写分支（测试路径也走这一条，与生产同构）。
			codexsdk.WithTransport(egressCodexTransport(eg)),
			// responses 出站传输（spec §5）：同样恒注入。WithResponsesTransport
			// (TransportHTTP) 与"不注入"等价，而不写 if 分支的原因正是要让"注入点
			// 被删掉"这种变异在组合用例里现红（删了它，ws 臂会静默跑 HTTP）。
			codexsdk.WithResponsesTransport(codexTransport(transport)),
			// G6：**必须**开 WS 连接复用。SDK 默认 0 = 每请求一条新连接；不接这一行，
			// S1（连接身份去 ts）带来的"可复用"就只是潜力，实际上零复用。
			//
			// 复用面由 SDK 的**读空闲上限**兜底（默认 300s = 上游
			// DEFAULT_STREAM_IDLE_TIMEOUT_MS；只计"一次取帧等待里没有任何可交付帧"，
			// Ping/Pong 不续命）：超时即退役该连接（不归还、不重试），同 key 的下一请求
			// 重新拨号。所以这里**不要**再传 WithWSIdleTimeout 放宽（上游自己 5 分钟就断），
			// 也不要用 WithTimeout 替代——那是整段请求的硬上限，会切断长流。缓存里的空闲
			// 连接不在等待、不计时；取用时才开始算。
			codexsdk.WithWSConnCache(a.wsCache),
		)
	})
}

// egressCodexTransport 取出口的 Codex 出站 transport（关闭自动压缩，egress.Client.Codex）。
//
// `Codex == nil` 只出现在测试里手写的 `egress.Client` 字面量（egress.Pool 构造的
// 出口恒带 Codex）：回退 HTTP 面 transport，保持这些测试的既有出站路径。
func egressCodexTransport(eg *egress.Client) http.RoundTripper {
	if eg.Codex == nil {
		return eg.HTTP.Transport
	}
	return eg.Codex.Transport
}

// RetireThread 退役账号某线程的空闲 WS 连接（线程退休 / 重载丢槽的唯一出口）。
// 实现在 ClientTable（它才是表的所有者）；本方法保留为适配层自用/测试面。
//
// 前置条件由会话池保证：调用时该 threadKey 不可能有在飞 exchange（会话独占租借，
// 退休发生在该请求 `Close` 归还之后），所以只需摘 idle。
func (a *Codex) RetireThread(accountID int64, threadKey string) {
	if a == nil {
		return
	}
	a.clients.RetireThread(accountID, threadKey)
}

// closeWSIdleConns 关闭一批客户端各自的 **WS** 空闲连接（G4）。
//
// 调用纪律：**锁外**调用。SDK 的 clear() 会逐条 terminate，可能在写闸上等待；
// 在 a.mu 内调用会把整个 client 表卡住（spec §3.1-S4）。
//
// 只用 CloseWSIdleConnections 而不是 CloseIdleConnections：后者会对
// `client.Transport.(*http.Transport)` 断言，然后清空其空闲池——网关出口是
// `*httpx.ShardedTransport`（断言失败 ⇒ 静默 no-op，丢了 client 却关不掉连接），
// 而一旦哪天出口是裸 `*http.Transport` 又会打掉全站 h2 连接池（spec §7-5）。
func closeWSIdleConns(clients []*codexsdk.HTTPClient) {
	for _, c := range clients {
		if c != nil {
			c.CloseWSIdleConnections()
		}
	}
}

// —— codex 额度快照（GetUsageSnapshot——TTL 缓存 + 有界并发 + 失败冷却） ——

// ErrAuthExpired 凭据失效分类错误（GetUsageSnapshot 错误面——IsFatal 纯判定
// 零副作用：usage 查询面不重复上报不摘除——凭据失效由会话路径（Dial/
// Responses）经 FatalAuth 上报，防循环；凭据变更后 entry sig 重建自然恢复。
// upstream_error 映射输入）。
var ErrAuthExpired = errors.New("codex: usage auth expired")

// ErrUpstream 上游错误分类错误（GetUsageSnapshot 错误面——*HTTPError/网络/
// RefreshError 等一律保守归本类；upstream_error 映射输入）。
var ErrUpstream = errors.New("codex: usage upstream error")

// usageFetchConcurrency 快照拉取有界并发上限（所有调用面共享节流：管理面
// 几百账号懒加载 + 未来路由面共用同一上限，防 OpenAI 429；只限并发不限速率
// ——速率上限由失败冷却封顶）。实例级 semaphore（Codex.usageSem）容量即此值，
// 挂实例消除包级可变状态（S8）。
const usageFetchConcurrency = 8

// 快照 TTL 与失败冷却默认值（挂 Codex 实例字段 usageTTL/usageCooldown——见
// NewCodex）。
const (
	// defaultUsageSnapshotTTL 快照 TTL（5min 慢变量——上游分钟级更新；滚动查看零上游）。
	defaultUsageSnapshotTTL = 5 * time.Minute
	// defaultUsageCooldown 失败冷却（60s——冷却内直接返回分类错误零上游，封顶
	// 重试率 ≤1 次/账号/分钟；不缓存错误体，冷却后重试）。
	defaultUsageCooldown = 60 * time.Second
)

// GetUsageSnapshot 账号 codex 额度快照（ChatGPT 面 wham/usage）：cred →
// 缓存条目（entryFor——sig 比对/重建，重建时 usage/usageAt/usageErrAt 随新
// 条目一并清除）→ 5min TTL 命中直接返回（零上游）；未命中 → 实例 semaphore
// （容量 8，有界并发）→ SDK GetUsage（e.client 复用——固定 SDK 官方端点
// https://chatgpt.com/backend-api/wham/usage，网关零拼装，test transport 仅 host 重写保留官方 path）→ 白名单收敛映射（fromSDKUsage）→ 写
// e.usage + e.usageAt。失败 → 写 e.usageErrAt（60s 冷却）。
//
// 错误分类**纯判定零副作用**（gate Major 3——不复用 translateError：其 fatal
// 分支 reportFatal → evictIfCurrent 删 entry，冷却随 entry 消失）：IsFatal（fatal 五类
// 穿透信封链——SDK 说凭据死才是凭据死，鉴权面真相唯一来源）→ ErrAuthExpired；
// 其余（*HTTPError/网络/RefreshError，含非致命 401——无判死标记的拒绝属上游
// 面，不从状态码反推鉴权结论）保守归 ErrUpstream。usage 是查询面不是会话面——凭据失效
// 由会话路径上报（防循环），entry 恒保留（fatal 后后续调用仍命中冷却）。
// 命中路径零分配：先按 accountID 查 entry.usage（map 查 + 锁 + 时间
// 比较——不计算 credSig/不重建条目），命中直接返回；未命中才走
// clientFor（entryFor 签名比对/重建）。
//
// 出口：本面**没有 Selection**（额度快照是节点本地的后台拉取，不是某次请求的
// 出栈），故走池的直连出口（`DirectEgress()`，经 usageClientFor）。它**不参与定义
// 账号的活跃集**：请求路径只取/建资源，配置可见性只由配置权威下发的资源指令改写
// （`ClientTable.ApplyBatch`；直连键由权威显式列入保留集，不是表的特例）。
func (a *Codex) GetUsageSnapshot(ctx context.Context, cred *domain.AccountCredential) (*domain.CodexUsageSnapshot, error) {
	if s, ok := a.snapshotCachedFor(cred.AccountID); ok {
		return s, nil
	}
	e, client, err := a.usageClientFor(cred)
	if err != nil {
		// 入口错误分类：oauth 缺 rt 凭据不完整（errCredentialIncomplete）
		// → 凭据失效语义 ErrAuthExpired（不落 default 归 ErrUpstream）。
		if errors.Is(err, errCredentialIncomplete) {
			return nil, ErrAuthExpired
		}
		return nil, err
	}
	if s, ok := a.snapshotCached(e); ok {
		return s, nil
	}
	if err := a.snapshotCooldown(e); err != nil {
		return nil, err
	}
	select {
	case a.usageSem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-a.usageSem }()
	// 双检：semaphore 等待期间并发请求可能已拉取成功/失败（TTL/冷却语义保持
	// ——不重复拉取、不重复报错）。
	if s, ok := a.snapshotCached(e); ok {
		return s, nil
	}
	if err := a.snapshotCooldown(e); err != nil {
		return nil, err
	}
	resp, err := client.GetUsage(ctx)
	if err != nil {
		// 取消/超时短路（task review 2026-08-18 Important 1）：ctx 取消 →
		// 不写失败冷却（误记会锁死该账号 60s——管理面批量拉取切页触发面）、
		// 返回 ctx 错误本身保留取消身份（不误归 ErrUpstream）。
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		a.recordUsageFailure(e, err)
		return nil, classifyUsageErr(err)
	}
	s := fromSDKUsage(resp)
	e.usageMu.Lock()
	e.usage = s
	e.usageAt = time.Now()
	// 成功清冷却态（死状态不留：哨兵仅在 usageErrAt 新鲜时被读取，但
	// 状态机卫生——检查顺序调整不误服旧哨兵）。
	e.usageErrAt = time.Time{}
	e.usageErr = nil
	e.usageMu.Unlock()
	return s, nil
}

// snapshotCachedFor 命中路径零分配快查：按 accountID 查 entry + TTL
// 判定——命中直接返回缓存实例，不经过 clientFor/credSig/
// entryFor（重建判定）。未命中/无 entry → nil（走完整路径）。
func (a *Codex) snapshotCachedFor(accountID int64) (*domain.CodexUsageSnapshot, bool) {
	e := a.clients.Get(accountID)
	if e == nil {
		return nil, false
	}
	e.usageMu.Lock()
	defer e.usageMu.Unlock()
	if e.usage != nil && time.Since(e.usageAt) < a.usageTTL {
		return e.usage, true
	}
	return nil, false
}

// snapshotCached 快照 TTL 命中判定（5min 慢变量；e.usage nil = 未拉取过）。
func (a *Codex) snapshotCached(e *codexEntry) (*domain.CodexUsageSnapshot, bool) {
	e.usageMu.Lock()
	defer e.usageMu.Unlock()
	if e.usage != nil && time.Since(e.usageAt) < a.usageTTL {
		return e.usage, true
	}
	return nil, false
}

// snapshotCooldown 失败冷却判定（60s）：冷却中 → 返回分类哨兵（
// ErrAuthExpired/ErrUpstream——非错误体，冷却后重试拉取新错误）；冷却外 →
// nil（可拉取）。
func (a *Codex) snapshotCooldown(e *codexEntry) error {
	e.usageMu.Lock()
	defer e.usageMu.Unlock()
	if !e.usageErrAt.IsZero() && time.Since(e.usageErrAt) < a.usageCooldown {
		return e.usageErr
	}
	return nil
}

// recordUsageFailure 失败冷却起点（gate Major 2）：写 usageErrAt + 分类哨兵
// （不缓存错误体）。
func (a *Codex) recordUsageFailure(e *codexEntry, err error) {
	e.usageMu.Lock()
	e.usageErrAt = time.Now()
	e.usageErr = classifyUsageErr(err)
	e.usageMu.Unlock()
}

// classifyUsageErr 错误分类纯判定（gate Major 3——零副作用不上报不摘除）。
// 边界单一原则：鉴权面真相唯一来源是 SDK 致命分类——IsFatal（fatal 五类穿透
// 信封链：RefreshOAuthError/AuthPermanentlyRevokedError/AccountDisabledError/
// CallbackDeliveryError）→ ErrAuthExpired；其余一律 ErrUpstream。非致命 401
// （无判死标记的拒绝——上游未宣告凭证死亡）不在此反推鉴权结论：真死亡必带
// 致命标记走 fatal 路径并经 OnAuthFatal 禁用，无标记则按上游抖动冷却重试自愈
// （的 401 特判已随 SDK PAT 判死上线退役——补丁理由消失即删）。
func classifyUsageErr(err error) error {
	if IsFatal(err) {
		return ErrAuthExpired
	}
	return ErrUpstream
}

// fromSDKUsage codexsdk.UsageStatus → domain.CodexUsageSnapshot 白名单收敛
// 映射（spec 变更 3——砍四留四：approx_*/瞬时布尔/派生状态/RateLimitReached
// 一律不进契约；每块 nil → 不出字段；ResetAt Unix 秒 → time.Time；ResetAt 0
// （上游主窗口省略）→ nil 不出字段；Balance 空串 → nil 不出字段——零填充语义）。
func fromSDKUsage(u *codexsdk.UsageStatus) *domain.CodexUsageSnapshot {
	if u == nil {
		return nil
	}
	s := &domain.CodexUsageSnapshot{PlanType: u.PlanType}
	if rl := u.RateLimit; rl != nil && rl.PrimaryWindow != nil {
		s.RateLimit = &domain.CodexRateLimit{UsedPercent: rl.PrimaryWindow.UsedPercent}
		if rl.PrimaryWindow.ResetAt > 0 {
			t := time.Unix(int64(rl.PrimaryWindow.ResetAt), 0).UTC()
			s.RateLimit.ResetAt = &t
		}
	}
	if c := u.Credits; c != nil && c.Balance != nil && *c.Balance != "" {
		b := *c.Balance
		s.Credits = &domain.CodexCredits{Balance: &b}
	}
	if sc := u.SpendControl; sc != nil && sc.IndividualLimit != nil {
		l := sc.IndividualLimit
		s.SpendControl = &domain.CodexSpendControl{
			Limit: l.Limit, Used: l.Used, Remaining: l.Remaining,
			UsedPercent: l.UsedPercent, RemainingPercent: l.RemainingPercent,
		}
	}
	return s
}

// Dial 建立到上游的 Responses WebSocket 连接（§2 接线）：cred → Auth 缓存
// 取（entryFor——账号级长存：at 缓存/单飞/rt 轮换在 Auth 内，与 HTTP 面共享；
// WS 连接本身 per-请求不缓存）→ codexsdk.Dial(ctx, auth, id, opts...)。id 是本次
// 连接的身份（握手头 + 帧内 metadata + ts 落点）；opts 由网关侧组装
// （codex_responses_ws.go——WithPingInterval(0) 等；SDK 侧按 codex 形状归一 WS
// 帧）；端点归 SDK 官方默认（wss://chatgpt.com/backend-api/codex/responses，
// transport 观察为 https）。错误翻译（translateDialError）：
//   - *DialError → 信封包装（EnvelopeError——StatusCode()/RawJSON()/Unwrap
//     链，Refreshed 语义保留：已轮转重连一次仍失败 → 网关避免双份刷新）
//   - 裸错误（Dial 401 轮转路径 refresh 失败——client.go:391-394 透传，不包
//     DialError）→ translateError 既有双分支：fatal 类（RefreshOAuthError /
//     AccountDisabledError）→ 统一回调单次上报 + 原样透传（网关"该请求不转
//     移"，IsFatal 判定）；RefreshError/网络 → 原样透传（网关正常 failover）
func (a *Codex) Dial(ctx context.Context, cred *domain.AccountCredential, eg *egress.Client, id codexsdk.Identity, opts ...codexsdk.Option) (*codexsdk.Client, error) {
	p, err := a.Prepare(cred)
	if err != nil {
		return nil, err
	}
	return a.DialPrepared(ctx, p, eg, id, opts...)
}

// DialPrepared 用**已准备句柄**拨号（生产 WS 车道热路径）：auth 来自该句柄，与 owner
// 同版（§3.2 凭据替换）。语义同 Dial。
//
// 读帧空闲上限（仅 WS）不在这里设：SDK 默认 5 分钟（对齐上游
// DEFAULT_STREAM_IDLE_TIMEOUT_MS；只计"一次取帧等待里没有任何可交付帧"，Ping/Pong
// 不续命），由 SDK 单点拥有。**不要**用 codexsdk.WithTimeout 或调用方 ctx 期限替代
// 它——那是整段请求的硬上限，会把长流切成 DeadlineExceeded；也不要在 opts 里放宽
// WithWSIdleTimeout：上游自己 5 分钟就断，网关宽于上游只会攒死连接。
func (a *Codex) DialPrepared(ctx context.Context, p Prepared, eg *egress.Client, id codexsdk.Identity, opts ...codexsdk.Option) (*codexsdk.Client, error) {
	e := p.entry
	if e == nil {
		return nil, errUnprepared
	}
	eg, err := a.resolveEgress(eg)
	if err != nil {
		return nil, err
	}
	// 出口 transport 与 Codex HTTP 面同一注入点（spec §4.6.4）：ws 握手走
	// `HTTPClient.Do`，transport 上的 Proxy 是代理唯一的注入面。eg 恒非 nil；
	// `Transport == nil`（裸底座）时 WithTransport(nil) 等价 SDK 默认。
	opts = append(opts, codexsdk.WithTransport(egressCodexTransport(eg)))
	c, err := codexsdk.Dial(ctx, e.auth, id, opts...)
	if err != nil {
		return nil, a.translateDialError(e, p.srcRev, err)
	}
	return c, nil
}

// translateDialError Dial 错误翻译（§5 错误契约）：*DialError → 信封
// （Refreshed 保留）；其余（refresh 失败裸错误）复用 translateError 双分支
// （fatal → 统一回调 + 原样；RefreshError/网络 → 原样 → 网关 failover 分类）。
func (a *Codex) translateDialError(e *codexEntry, srcRev int64, err error) error {
	if de, ok := errors.AsType[*codexsdk.DialError](err); ok {
		env := NewEnvelopeError(de.StatusCode, "", de)
		env.Refreshed = de.Refreshed
		return env
	}
	return a.translateError(e, srcRev, err)
}

// buildAuth 按 cred 构造 SDK Auth（构造前校验）：
//   - codex-oauth：OAuthWithRotation(rt, WithOnAuthFatal(统一回调) [,
//     WithInitialAccessToken(at)])——过期判定在网关侧构造前：OAuthExpiresAt 已
//     过期 → 不传 WithInitialAccessToken（SDK 走初始 at 缺省路径，首请求前用
//     rt 换取——auth_oauth.go:106-109 只判非空不判过期，401 自愈）；未过期/
//     未知（nil）→ 预置单参 at 避免首调用强制 refresh
//   - WithOnTokenRotated（§1）：每次 refresh 成功产出新 at+rt → account_ext
//     部分更新回写（幂等；回调在 SDK 单飞内串行——同账号并发轮转不重复回写）
//   - codex-pat：PAT(key, WithPATOnAuthFatal(统一回调))——PAT 致命 401 在 SDK
//     内分类后同走 OnAuthFatal 禁用链路（毒化 + 单次上报，与 OAuth 双源去重共用）
//   - 空 rt（oauth 缺 refresh_token）→ 上报失效（凭据不完整）并返回错误，不
//     panic
//   - 账号标识：DB 显式值经 WithOAuthAccountID/WithPATAccountID 透传（SDK 不
//     派生——派生点唯一在落库，管理面导入行/单账号保存）；空 = 不发头。变更 →
//     credSig 变化 → 客户端重建（账号身份变更语义）
func (a *Codex) buildAuth(cred *domain.AccountCredential, e *codexEntry) (codexsdk.Auth, error) {
	// 判死回调的**事件来源 K = 条目当前 K**（回调触发时才读，不捕获构造期的值）：
	// 与轮转回写同一条规则——凭据自身的证据落在"这条活条目此刻被授权的那一代"上。
	//
	// 为什么必须用当前值（fail-open 的成因）：K 只在身份类字段变更时推进，凭据内容
	// 可以不变 ⇒ 条目按签名复用、Auth 不重建 ⇒ 捕获的构造期 K 会永远停在旧代际。而
	// `reportCredentialEvent` **先 CAS 去重再走围栏**：带旧 K 的回调会先吃掉那唯一
	// 一次上报机会、再被存储围栏判陈旧；随后请求级翻译（带请求自己的当前 K，本该
	// 成功）被去重跳过 ⇒ 真正判死的凭据永远不禁用账号，账号带病继续跑。
	//
	// 不复用这段逻辑的地方是 `translateError` / `translateDialError`：它们带的是请求
	// 句柄**冻结**的 K（请求可能被旧出口/旧配置污染，那条保护不能丢）。
	if cred.PATKey != "" {
		return codexsdk.PAT(cred.PATKey,
			// 统一回调装配（与 OAuth 同源）：SDK 判死（PAT 401 致命体分类）→
			// reportFatal 上报禁用（双源去重单次）
			codexsdk.WithPATOnAuthFatal(func(fatal error) { a.reportFatal(e, e.srcRev.Load(), fatal) }),
			// 账号标识（DB 显式值——SDK 不派生；空 = 不发头，向后兼容）
			codexsdk.WithPATAccountID(cred.CodexAccountID),
		), nil
	}
	if cred.OAuthRefreshToken == "" {
		return nil, errCredentialIncomplete // 上报在 clientFor 锁外执行（见 clientFor）
	}
	e.expiresAt = cred.OAuthExpiresAt // 回写保旧（SDK 回调无 expiry）
	opts := []codexsdk.OAuthOption{
		// 统一回调装配（§3）：SDK 判死（RT 判死码 / token 端点 401 / 账号
		// 禁用 / AT 401 判死 / 回调连续失败）→ 双源去重单次上报
		codexsdk.WithOnAuthFatal(func(fatal error) { a.reportFatal(e, e.srcRev.Load(), fatal) }),
		codexsdk.WithOnTokenRotated(func(at, rt string) { a.rotateWriteback(e, at, rt) }),
	}
	if atUsable(cred) {
		opts = append(opts, codexsdk.WithInitialAccessToken(cred.OAuthToken))
	}
	// 账号标识（DB 显式值——SDK 不派生；空 = 不发头，向后兼容）
	opts = append(opts, codexsdk.WithOAuthAccountID(cred.CodexAccountID))
	return codexsdk.OAuthWithRotation(cred.OAuthRefreshToken, opts...), nil
}

// errCredentialIncomplete 凭据不完整（构造前校验——oauth 类型缺
// refresh_token；按失效处理上报——账号凭据不完整，不 panic）。
var errCredentialIncomplete = errors.New("codexsdk: credentials incomplete (oauth missing refresh_token, account needs re-import)")

// atUsable 初始 at 预置判定（过期判定在网关侧构造前）：at 非空且未过期（nil
// 过期时刻 = 未知 → 视为可用，401 自愈兜底）。
func atUsable(cred *domain.AccountCredential) bool {
	if cred.OAuthToken == "" {
		return false
	}
	if cred.OAuthExpiresAt == nil {
		return true
	}
	return cred.OAuthExpiresAt.After(time.Now())
}

// 身份/版本字段矩阵（每个字段影响哪个生命周期；三套判据不可混用）：
//
//	字段                  | 条目/Auth 重建 | 会话 ts（owner） | 会话/线程       | 说明
//	--------------------- | ------------- | ---------------- | --------------- | ----
//	AT 轮转（SDK 刷新）    | 否            | 否               | 否              | 派生缓存：SDK 内存已更新，DB 回写不换身份
//	RT 轮转（SDK / 管理面） | 是            | 否               | 否              | Auth 构造输入变了；上游对话身份未变
//	PAT 变更              | 是            | 是               | 否              | PAT 既是秘密也是身份
//	上游账号 ID 改绑       | 是            | 是               | 否              | 跨账号串台风险
//	installation 变更     | 否            | 否               | 是（会话重建）   | 会话身份配置：线程按新 installation 重开、ts 自然为空
//	出口 / 传输变更        | 否            | 否               | 否              | clientKey 层（出口 × 传输）
//
// 三套判据：credKey = Auth 构造输入（RT/PAT/上游账号）；CredOwner = 会话连续性
// 身份（上游账号/PAT，**是 credKey 字段的子集**）；clientKey = 纯传输（出口/传输）。
// AT 是派生值，不参与任何一套——入键就等于"每个刷新周期重建一次、清一次 ts"。
// installation 不参与 owner：它是会话身份配置，由会话/线程版本承担（池重建即新
// 线程、ts 为空）——把它放进 owner 会让"条目何时重建"决定 owner 值（冻结的 owner
// 与请求快照分叉），正是要消除的不自洽。
//
// credKey 是外部凭据签名（重建判定）的**可比较值键**：逐字段 `==` 比较，不依赖
// 「OAuth token/PAT 为 base64url 字符集」的脆弱约定。
type credKey struct {
	oauthRefresh   string
	patKey         string
	codexAccountID string
}

// credSig 凭据签名（Auth 重建判定）：Auth 构造输入变了（管理面换 RT/PAT、改绑上游
// 账号）→ 重建。**AT 不参与**：它是派生缓存，SDK 每次 refresh 都会换，入键会让
// "token 轮转"退化成"每轮重建"；过期时刻同样不参与（构造时的初始 at 预置决策已经
// 生效，过期 at 由 SDK 401 自愈轮转）。
func credSig(c *domain.AccountCredential) credKey {
	return credKey{
		oauthRefresh:   c.OAuthRefreshToken,
		patKey:         c.PATKey,
		codexAccountID: c.CodexAccountID,
	}
}

// CredOwner 账号凭据的 **owner**（§3.2「凭据替换」的 ts 清空判据）——**条目构造期**
// 的派生函数：entryFor 在建条目时把它写进 codexEntry.owner，请求准备阶段只读该字段
// （Prepared.Owner）。**不要**在请求路径上对请求里的凭据快照重新散列——那会在"条目
// 还没重建、凭据已经换了"的窗口里给出更新的 owner 与旧的 auth。
//
// 纯函数：不读缓存、不加锁、**零分配**（栈上两个累加器）。owner 只含**稳定身份标识**
// ——CodexAccountID + PAT，**排除 AT 与 RT**：两者都会被 OAuth 刷新改写（RT 也会
// 轮转），参与 owner 就等于每个刷新周期清一次该账号全部会话的 ts。会话的 ts 属于
// 同一条上游对话；刷新只换令牌，不换对话身份。
//
// owner 的字段是 credKey 的**子集**：任何 owner 维度变化都会同时重建条目，因此
// 冻结在条目上的 owner 恒等于当前凭据快照的 owner——结构上不存在"新 owner + 旧
// auth"的分叉。installation 不属于此集合（会话身份配置，见字段矩阵）。
//
// 为什么非密码学哈希就够：比较是**账号内**的（会话池按账号建，owner 只和同一会话
// 上一次对齐的值比），所以它只回答"这份凭据还是不是同一版"，跨账号碰撞在语义上
// 无法发生；而"同一账号伪造自己凭据的碰撞"能拿到的只是自己那一条会话的旧 ts。
// 逐字段喂入并夹一个分隔字节，避免 ("ab","c") 与 ("a","bc") 同值。
func CredOwner(cred *domain.AccountCredential) [16]byte {
	const (
		offsetA = 0xcbf29ce484222325 // FNV-1a 64 偏移基
		offsetB = 0x9e3779b97f4a7c15 // 第二通道：不同初值，降低结构碰撞
		prime   = 0x100000001b3
	)
	var out [16]byte
	if cred == nil {
		return out
	}
	a, b := uint64(offsetA), uint64(offsetB)
	mix := func(s string) {
		for i := 0; i < len(s); i++ {
			a = (a ^ uint64(s[i])) * prime
			b = (b + uint64(s[i])) * prime
			b ^= b >> 31
		}
		a = (a ^ 0xff) * prime
		b = (b + 0xff) * prime
	}
	// 字段顺序即语义：上游账号 → PAT（两者都在 credKey 里 ⇒ owner 与 auth 同版）。
	for _, s := range [2]string{cred.CodexAccountID, cred.PATKey} {
		mix(s)
	}
	binary.LittleEndian.PutUint64(out[0:8], a)
	binary.LittleEndian.PutUint64(out[8:16], b)
	return out
}

// reportCredentialEvent 单条**凭据版本**事件的上报与资源回收（fatal / WS 判死帧
// 共用）：CAS 去重 —— 同一条目的同一 fatal 只上报一次（回调路径与 errors.As 路径
// 双源去重）。
//
// 关键约束（§3.3b-2）：事件在产生时绑定了**来源条目**与**来源 K**。两道门：
//  1. 本地门 `isCurrent(e)`（表锁内指针比较）——条目已被换掉 ⇒ 这条事件属于旧
//     版本，不发账号级动作也不回收资源；
//  2. 持久化门：上报携带 `SourceRevision = srcRev`（来源 K，由调用方按句柄/构造版本
//     传入），由 failure 链以它作 expectedRevision（存储侧 CAS）。第 1 道门挡不住
//     "管理面已换凭据、还没有任何请求重建条目"这段窗口——指针仍是旧的，只有 K 才能
//     揭穿；而"同签名身份写入把条目 K 抬到更大"的窗口里，指针也仍是旧的，所以 K
//     只能来自事件自己的来源，**不能回读条目当前值**。
//
// 资源回收用 evictIfCurrent（锁内比较指针），同样是"只在仍是当前版本时生效"。
//
// 账号级禁用与单凭据撤销是两种事件：本函数处理后者（且只在来源版本仍当前时升级
// 为账号级动作）；整账号不可用走 reportConfigFailure（同样带来源版本）。
func (a *Codex) reportCredentialEvent(e *codexEntry, srcRev int64, fatal error, evict bool) {
	if e == nil || fatal == nil {
		return
	}
	if !e.reported.CompareAndSwap(false, true) {
		return
	}
	if a.clients.isCurrent(e) && a.failure != nil {
		a.failure(CredentialFailure{AccountID: e.accountID, SourceRevision: srcRev, Cause: fatal})
	}
	if evict {
		a.clients.evictIfCurrent(e)
	}
}

// reportConfigFailure 构造失败的上报（如 oauth 缺 refresh_token）：这是**来源
// 凭据版本**的失败，不是"账号配置的全局事实"——请求快照的坏配置不得删掉管理员
// 刚修好的当前条目（§3.3b-2）。
//
// 判定用同一套来源门：表里已有属于**更新身份**的条目（K 更大），或同一代身份下
// 已有另一份**构造成功**的凭据（签名不同 ⇒ 迟到的这份不是当前内容）⇒ 这条错误
// 只上抛，不做账号级动作、不动现有资源。否则：
//   - 上报带来源 K（failure 链据此围栏：K 已被推进则拒）；
//   - 资源动作走 invalidateOlder（仅当表里条目属于更旧的身份才摘），绝不做
//     无版本的"按 id 删当前"。
func (a *Codex) reportConfigFailure(cred *domain.AccountCredential, fatal error) {
	if cred == nil || fatal == nil {
		return
	}
	sig := credSig(cred)
	if supersededBy(a.clients.Get(cred.AccountID), cred.IdentityRevision, sig) {
		return // 陈旧凭据快照的构造失败：当前条目保留，错误照常上抛给调用方
	}
	if a.failure != nil {
		a.failure(CredentialFailure{AccountID: cred.AccountID, SourceRevision: cred.IdentityRevision, Cause: fatal})
	}
	a.clients.invalidateOlder(cred.AccountID, cred.IdentityRevision)
}

// reportFatal fatal 统一上报（双源去重核心）：rotationAuth 路径同一 fatal 既
// 触发 WithOnAuthFatal 又随返回错误 errors.As 命中——**以回调为准去重、单次
// 上报**（CAS 胜者上报；败者并发调用/errors.As 补报路径跳过）。上报后失效
// 剔除（联动——账号已判死，缓存条目随弃，管理面恢复后重建）。
//
// srcRev 是这一 fatal 的**来源 K**，两条路径的取法不同（这是刻意的）：
//   - SDK 判死回调（凭据自身的证据）取**条目当前** K——见 buildAuth；
//   - errors.As 补报（请求级证据）取**请求句柄冻结**的 K（`Prepared.srcRev` 或凭据
//     快照的版本），请求可能被旧出口/旧配置污染，不得升格成新身份的判决。
func (a *Codex) reportFatal(e *codexEntry, srcRev int64, fatal error) {
	a.reportCredentialEvent(e, srcRev, fatal, true)
}

// FatalPrepared 显式终止 + 单次上报（§3——WS 业务判死事件帧接线，relay 解析
// 帧后调用；唯一跨边界点）：
//   - 来源版本门：只对**本连接 Prepare 得到的条目**生效。条目已被替换（管理面换
//     凭据）⇒ 这条帧属于旧凭据，直接 no-op——旧连接的错误不得毒化新凭据的 Auth。
//   - 上报走 reportCredentialEvent(e, p.srcRev, fatal, false)：来源 K 来自**这个
//     句柄**（准备时冻结），与 errors.As 路径共享 CAS 双源去重。
//   - **判据拆成两句**：**是否毒化**看"有没有请求观测到新代"（`e.srcRev`）；
//     **是否保留条目不看它**——两条分支都摘条目。
//   - `e.srcRev.Load() == p.srcRev`（观测上同代）：毒化 `e.auth` + 上报。毒化是在飞
//     请求的快速失败面，只保留给这一档。
//   - 不相等（条目已被同签名身份写入抬到更新的代）：**不毒化**共享 Auth（毒化它是
//     条目级动作，旧代证据毒化它会让新身份带着旧判决跑），照常以 `p.srcRev` 上报。
//   - 为什么"观测同代"不足以保留条目：`e.srcRev` 是**被请求观测到**的 K，不是库里
//     当前的 K——它只在同签名 Prepare 命中复用且 K 更大时才抬升。管理面把库里的 K
//     推进之后若没有请求 Prepare，旧句柄看到的仍是旧值，于是陈旧证据会走"同代"档：
//     毒化 + 上报 + 围栏按 K 不等拒（CAS 已消费、毒化态落不了库）⇒ 条目若留着就是
//     永久黑洞（poisoned=true、failed_at=nil、evicted=false），账号在库里一直"启用"。
//     判决被拒后**没有第二次机会**：唯一的第二次机会来自条目重建（下一条请求按库里
//     当前 K 重建、重新得出结论并上报）。所以无论哪一档都摘条目。
//   - 代价（有意接受）：不再"毒化态保留"——毒化过的条目会被摘掉，同凭据的下一条
//     请求重建一份新 Auth 再判一次。用一次重建换"不留黑洞"。
//   - 为什么不能"两边都改用条目当前 K"：这条证据是**某一次请求**产生的（可能来自旧
//     出口/旧配置的伪 401），按当前 K 上报就会把旧判决升格成新身份的事实（B1 倒退）。
//     K 必须来自 `p.srcRev`，只在**是否毒化**上参考观测到的代际。
//
// 空句柄（未 Prepare / 未装配）→ no-op：没有来源版本就没有可施加的事件。
func (a *Codex) FatalPrepared(p Prepared, fatal error) {
	e := p.entry
	if e == nil || fatal == nil {
		return // 防御：无来源/无错误不上报
	}
	if !a.clients.isCurrent(e) {
		return // 旧版本的判死帧：不毒化当前 Auth、不升级为账号级事件
	}
	// 跨代证据：照常以**请求自己的** K 上报（存储围栏会把它判成陈旧，fail-closed），
	// 但**不毒化**共享 Auth——它是条目级动作，只属于当前那一代。随后摘掉这条条目，
	// 让下一条请求按当前代重建并重新得出结论。
	if e.srcRev.Load() != p.srcRev {
		a.reportCredentialEvent(e, p.srcRev, fatal, false)
		a.clients.evictIfCurrent(e)
		return
	}
	// 观测同代：毒化这条共享 Auth（在飞请求快速失败），上报后**同样摘条目**——
	// "观测同代"证明不了"库里同代"，留着毒化态就是一个判决落不了库、又没人会再判的
	// 黑洞（见函数注释）。
	e.auth.Fatal(fatal)
	a.reportCredentialEvent(e, p.srcRev, fatal, false)
	a.clients.evictIfCurrent(e)
}

// rotateWriteback 轮转回写（§1——SDK OnTokenRotated 回调；在 SDK 单飞内
// 串行执行——同账号并发轮转天然单飞，无需额外互斥）：
//   - **来源门（本地）**：条目已不是当前版本 ⇒ 这次轮转属于旧凭据，直接丢弃
//     （写进去就是"旧 Auth 覆盖管理员刚换上的新凭据"）
//   - account_ext 部分更新 upsert（codex_oauth_token + codex_oauth_refresh_token +
//     codex_oauth_expires_at 保旧——携带 e.expiresAt 构造时旧值），**带存储围栏**：
//     `expectedIdentityRevision = e.srcRev`（**条目当前**的身份代际），命中 0 行 ⇒
//     身份已被管理面替换 ⇒ repository.ErrStaleIdentityRevision
//
// 为什么这里可以（也必须）用当前 K：轮转换的是**这个条目自己的** AT/RT，不是某一次
// 请求的证据。K 只在身份类字段（base_url / template_id / installation）变更时推进
// ——凭据内容（RT/PAT/上游账号）可以完全不变，于是条目按签名复用、Auth 不重建；若
// 回写还带"构造这条 Auth 时的 K"，K 前进之后该条目的**每一次**轮转都会被判陈旧、
// 永不落库（重启后 DB 停在已被轮换掉的旧 RT）。K 前进是管理面对同一份凭据内容的
// 重新授权，写入自然落在新 K 上。旧条目的迟到轮转由上面的 `isCurrent` 门挡住。
//   - 陈旧围栏（ErrStaleIdentityRevision）：这是**被拒的陈旧事件**，不是投递失败
//     ——记一条 Warn、不回写快照、不 panic、**不得反过来禁用账号**（旧凭据的轮转
//     结果失配，新凭据自己会继续工作）
//   - 其余失败 → panic（SDK 契约：回调失败 = 令牌持久化中断信号——callRotate
//     recover 后记 pending 下次 refresh 前重试，连续达阈值 →
//     CallbackDeliveryError fatal → 统一回调摘除；fail-closed）
//   - 成功后失效调度器 AccountExt 内存快照条目（下个会话重载新凭据；
//     失效失败仅 Warn 不阻断——令牌已落库，适配层 Auth 内存新 at 自愈）
//   - **回调本身不重建缓存**：回调写回的是本 Auth 内部已更新的状态（at 缓存/rt
//     轮换已在 SDK 内生效）。但下一次 Prepare 会因 RT 变化而重建 Auth（RT 是 Auth
//     构造输入，见 credKey 的矩阵）：新 Auth 用刚落库的 AT/RT 起步，不丢轮转结果。
//     ts 不因此清空——owner 不含 AT/RT，同一条上游对话的会话继续复用。
//
// 回调在 SDK 单飞内阻塞并发等待者——必须快速返回（本地 upsert 毫秒级）；
// 用固定短超时兜底 PG 故障（超时 → 重试链接管）。
func (a *Codex) rotateWriteback(e *codexEntry, at, rt string) {
	if a.rotate == nil {
		return // 未装配（测试形态）：no-op
	}
	if e == nil {
		return
	}
	if at == "" || rt == "" {
		return // 盲写防御：SDK 已保证非空（响应缺 refresh 时回调收旧 rt），双保险
	}
	if !a.clients.isCurrent(e) {
		return // 旧凭据的轮转：当前条目已换，写进去就是覆盖新凭据
	}
	// 围栏取**条目当前** K（不是构造这条 Auth 时的 K）：见上方"为什么这里可以用
	// 当前 K"。isCurrent 之后读，拿到的是这条活条目此刻被授权的那一代。
	srcRev := e.srcRev.Load()
	ctx, cancel := context.WithTimeout(context.Background(), rotateWritebackTimeout)
	defer cancel()
	err := a.rotate.WriteOAuthRotation(ctx, e.accountID, srcRev, at, rt, e.expiresAt)
	if err != nil {
		if errors.Is(err, repository.ErrStaleIdentityRevision) {
			// 存储围栏拒绝：这一版身份已被管理面替换（旧 Auth 在途 refresh 迟到的
			// 结果）。丢弃即可——新凭据的令牌由新条目自己维护。
			if a.log != nil {
				a.log.Warn("codex rotation writeback rejected by identity fence",
					logx.Int64("account_id", e.accountID), logx.Int64("source_identity_revision", srcRev))
			}
			return
		}
		if a.log != nil {
			// 运维信号即时留痕（重试至多 3 次各记一条——DB 故障持续期的
			// 真实告警；fatal 上报后账号摘除，告警自停）
			a.log.Warn("codex rotation writeback failed", logx.Int64("account_id", e.accountID), logx.Error(err))
		}
		// 契约：回调失败 → panic → SDK recover → pending 重试 → 达阈值
		// CallbackDeliveryError fatal（令牌无法持久化 = 账号失效信号）
		panic(err)
	}
	if a.inval != nil {
		a.inval(e.accountID)
	}
}

// rotateWritebackTimeout 轮转回写固定超时（回调在 SDK 单飞内阻塞并发等待
// 者——本地 upsert 毫秒级，3s 兜底 PG 故障；超时 → 重试链接管）。
const rotateWritebackTimeout = 3 * time.Second

// translateError SDK 错误 → 网关侧错误翻译（错误契约）：
//   - fatal 五类（errors.As——RefreshOAuthError / AuthPermanentlyRevokedError /
//     AccountDisabledError / CallbackDeliveryError）→ 双源去重单次上报（回调
//     路径 CAS 已胜出则此处跳过；PAT/无回调路径此处补报）——原样透传不包装
//     （SDK 已保证 errors.As 可命中）。srcRev 是这条错误的**来源 K**（调用面带
//     请求句柄或凭据快照的版本），与回调路径共享去重但不共享"当前值"。
//   - RefreshError → 不上报（可重试——对齐 SDK 语义 auth_errors.go:53-58，
//     网关按既有 failover 分类处理）
//   - *HTTPError → 信封包装（EnvelopeError：StatusCode()/RawJSON()/Unwrap 链；
//     *HTTPError 自身已有 RawJSON()，包装保留是为了 StatusCode() 方法面与 Unwrap 链）
//   - 其余（网络/解析等）原样透传（code 0 连接级分类）
//
// 背压（codexsdk.ErrWSBackpressure）是**传输层**错误而非协议终态：上游没坏、
// 下游消费太慢。sentinel 换 sentinel（不按字符串匹配），并保留原始错误链——
// `errors.Is(mapped, domain.ErrUpstreamBackpressure)` 与
// `errors.Is(mapped, codexsdk.ErrWSBackpressure)` 同时为真，调用方既能按网关
// 语义分类（不误判成网络失败/鉴权失败去重试与冷却账号），又能回溯 SDK 原始面。
// 包装风格与 repository/usage_repo.go:86 的 ErrPartitionUnavailable 一致。
func (a *Codex) translateError(e *codexEntry, srcRev int64, err error) error {
	if errors.Is(err, codexsdk.ErrWSBackpressure) {
		return fmt.Errorf("%w: %w", domain.ErrUpstreamBackpressure, err)
	}
	if f := asFatal(err); f != nil {
		// 双源去重（与 reportFatal 同语义，直接复用）：
		// CAS 在回调路径已胜出则此处跳过（单次上报）；PAT/无回调路径此处补报
		a.reportFatal(e, srcRev, f)
		return err
	}
	if he, ok := errors.AsType[*codexsdk.HTTPError](err); ok {
		return NewEnvelopeError(he.StatusCode, string(he.Raw), he)
	}
	return err
}

// asFatal 判定 SDK 错误是否为账号级终止类（fatal 五类，errors.As 穿透信封
// 包装链——EnvelopeError.Unwrap 保留链）。RefreshError 不在 fatal 集（可重试
// ——auth_errors.go:53-58 语义）。
func asFatal(err error) error {
	var (
		re *codexsdk.RefreshOAuthError
		ap *codexsdk.AuthPermanentlyRevokedError
		ad *codexsdk.AccountDisabledError
		cd *codexsdk.CallbackDeliveryError
	)
	if errors.As(err, &re) || errors.As(err, &ap) || errors.As(err, &ad) || errors.As(err, &cd) {
		return err
	}
	return nil
}

// IsFatal 网关侧 fatal 判定（§5：Dial 裸错误 fatal 类 → 该请求不转移，由
// handleCodexDialError 调用）：与 asFatal 同构导出（errors.As 穿透信封链）。
func IsFatal(err error) bool { return asFatal(err) != nil }

// --- domain ↔ codexsdk 双向转换（集中本文件 + 转换单测防漂移） ---

// mapStreamEventType codexsdk 流式事件类型 → domain 类型化常量（显式
// 映射：SDK 升级改事件名 → ok=false——调用方 Warn + 跳过，不静默透传落账
// 0 张；case 用 SDK 常量防漂移）。
func mapStreamEventType(t string) (domain.ImageStreamEventType, bool) {
	switch t {
	case codexsdk.ImageStreamEventCompleted:
		return domain.ImageStreamEventCompleted, true
	case codexsdk.ImageStreamEventKeepalive:
		return domain.ImageStreamEventKeepalive, true
	default:
		return "", false
	}
}

// toSDKParams domain.ImageGenParams → codexsdk.ImageGenParams（字段同构；
// nil 指针/空切片语义保留——可选字段不发）。SDK 只读消费转换产物，无别名风险。
func toSDKParams(p *domain.ImageGenParams) *codexsdk.ImageGenParams {
	if p == nil {
		return nil
	}
	s := &codexsdk.ImageGenParams{
		Model:      p.Model,
		Prompt:     p.Prompt,
		N:          p.N,
		Size:       p.Size,
		Quality:    p.Quality,
		Background: p.Background,
	}
	if len(p.Images) > 0 {
		s.Images = make([]codexsdk.ImageRef, len(p.Images))
		for i := range p.Images {
			s.Images[i] = codexsdk.ImageRef{ImageURL: p.Images[i].ImageURL, Raw: p.Images[i].Raw}
		}
	}
	return s
}

// fromSDKResponse codexsdk.ImageResponse → domain.ImageResponse（字段同构平铺；
// usage 缺失 → nil——网关 per-image 分量兜底）。
func fromSDKResponse(r *codexsdk.ImageResponse) *domain.ImageResponse {
	out := &domain.ImageResponse{
		Created:      r.Created,
		Background:   r.Background,
		OutputFormat: r.OutputFormat,
		Quality:      r.Quality,
		Size:         r.Size,
	}
	if len(r.Data) > 0 {
		out.Data = make([]domain.Image, len(r.Data))
		for i := range r.Data {
			out.Data[i] = domain.Image{B64JSON: r.Data[i].B64JSON}
		}
	}
	if r.Usage != nil {
		out.Usage = &domain.ImageUsage{
			InputTokens:       r.Usage.InputTokens,
			InputImageTokens:  r.Usage.InputImageTokens,
			OutputTokens:      r.Usage.OutputTokens,
			OutputImageTokens: r.Usage.OutputImageTokens,
		}
	}
	return out
}

// --- domain.ImageResponse → 上游 wire 序列化（客户端转发 + 计费提取共用） ---

// imageDataWire / imageUsageWire 是上游 images 端点响应 wire 形态（嵌套 usage
// details——对齐 codex-sdk imageResponseWire / billing.ImageUsageFromResponse
// 提取路径：usage.input/output_tokens_details.image_tokens + data 数组长）。
type imageDataWire struct {
	B64JSON *string `json:"b64_json"`
}

type imageTokensWire struct {
	ImageTokens int64 `json:"image_tokens"`
}

type imageUsageWire struct {
	InputTokens   int64            `json:"input_tokens"`
	OutputTokens  int64            `json:"output_tokens"`
	InputDetails  *imageTokensWire `json:"input_tokens_details"`
	OutputDetails *imageTokensWire `json:"output_tokens_details"`
}

type imageResponseWire struct {
	Created      int64           `json:"created"`
	Background   *string         `json:"background,omitempty"`
	Data         []imageDataWire `json:"data"`
	OutputFormat *string         `json:"output_format,omitempty"`
	Quality      *string         `json:"quality,omitempty"`
	Size         *string         `json:"size,omitempty"`
	Usage        *imageUsageWire `json:"usage,omitempty"`
}

// MarshalImageResponse 把 domain.ImageResponse 序列化为上游 wire 形态
// （客户端转发与计费提取共用同一字节——billing.ImageUsageFromResponse 与
// API-key 直连同口径：data 长 = 张数 + usage image_tokens）。usage 缺失 → 不
// 输出 usage 字段（上游未提供语义——per-image 分量兜底）。
func MarshalImageResponse(r *domain.ImageResponse) ([]byte, error) {
	w := imageResponseWire{
		Created:      r.Created,
		Background:   r.Background,
		OutputFormat: r.OutputFormat,
		Quality:      r.Quality,
		Size:         r.Size,
	}
	if r.Data != nil {
		w.Data = make([]imageDataWire, len(r.Data))
		for i := range r.Data {
			w.Data[i] = imageDataWire{B64JSON: r.Data[i].B64JSON}
		}
	} else {
		w.Data = []imageDataWire{}
	}
	if r.Usage != nil {
		w.Usage = &imageUsageWire{
			InputTokens:  r.Usage.InputTokens,
			OutputTokens: r.Usage.OutputTokens,
		}
		if r.Usage.InputImageTokens != 0 {
			w.Usage.InputDetails = &imageTokensWire{ImageTokens: r.Usage.InputImageTokens}
		}
		if r.Usage.OutputImageTokens != 0 {
			w.Usage.OutputDetails = &imageTokensWire{ImageTokens: r.Usage.OutputImageTokens}
		}
	}
	return json.Marshal(w)
}
