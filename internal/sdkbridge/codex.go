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
	"math"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/codexproto"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/storeerr"
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
	// 构造时注入（`eg.NoCompression().Transport`——同一出口派生的关闭自动压缩连接池，见
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
	// usageNow usage 面的时间源（构造期 = time.Now）。TTL、失败冷却与"哪份观测
	// 更新鲜"全读它——门禁要求"推进时间"而不是 sleep，测试在启动任何拉取协程
	// **之前**替换这个函数；数据面不写它，故不需要锁。
	usageNow func() time.Time
	// usageFlightMu/usageFlights 同 `(accountID, K)` 的在飞拉取登记（设计 §5.1
	// 的单飞键 = `(accountID, identityRevision)`；§5.2 的上界表"同键并发 N 次 ⇒
	// 1 次上游"）。双检只能防**已完成**拉取的重复：semaphore 等槽期间落地的结果
	// 会被二次快照判定拦住，但 N 个请求同时通过双检、一起阻塞在**尚未返回**的
	// 首拉上时，双检什么也拦不住（实测 8 并发同 K ⇒ 8 次上游）。所以这里必须有
	// 一张显式的在飞表。
	usageFlightMu sync.Mutex
	usageFlights  map[usageFlightKey]*usageFlight
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
// 身份已被管理面替换 ⇒ 返回 storeerr.ErrStaleIdentityRevision，**不是**重试
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
	// 分类哨兵（ErrAuthExpired/ErrUpstream/ErrUsageStale），不缓存错误体。
	usage   *domain.CodexUsageSnapshot
	usageAt time.Time
	// usageRev 这份 usage/usageWindows 是**哪一代身份 K** 拉到的
	// （`AccountCredential.IdentityRevision`）。命中条件因此是
	// `usageRev == k && 新鲜`，而**不是** `srcRev == k`：
	//
	// srcRev 是资源归属的单调观测值（见上），会被后来的同签名请求抬升、也从不
	// 回退——用它判命中会让 K=3 的请求命中 K=2 写下的 usage（条目按签名复用，
	// 换代不重建）。usageRev 只由拉取方写、只记**实际发起那次拉取**的 K。
	usageRev int64
	// usageErrAt/usageErr/usageErrRev 失败冷却三元组：起点、分类哨兵、
	// **这次失败属于哪一代 K**。冷却只挡同代际（`usageErrRev == k && 新鲜`）；
	// 换代后立刻允许补拉一次——否则 base_url/installation 这类不动凭据字节的
	// 身份写入会被上一代的 60s 冷却错误阻挡。三者同一临界区翻页，不撕裂。
	usageErrAt  time.Time
	usageErr    error
	usageErrRev int64
	// usageWindows 是与 usage/usageAt **同一次**成功拉取得到的窗口事实
	// （`UsageStatus.QuotaState()`，冷却判决的唯一输入——见 ExhaustedUntil）。
	// 它不随 usage 快照结构走：`usage` 是收敛后的 domain 契约（不含窗口列表），
	// 窗口原样留在这里，避免为了判决再去解析 Raw 或加宽 domain 契约。
	//
	// 生命周期与 usage 完全同步：成功拉取时与 usage/usageAt/usageRev **同一临界区**
	// 写入；**TTL 命中路径不重写**（命中只读缓存，窗口事实属于那次拉取的时刻，把它
	// 按"现在"重写就等于伪造新鲜度）；失败不写也不清（上一份事实仍描述那次
	// 观测，读到它只会给出已经过期的 reset，被 ExhaustedUntil 的时刻校验剔除）；
	// 条目重建（凭据签名变化）随新条目归 nil——窗口事实属于旧身份的那次会话。
	usageWindows []codexsdk.QuotaWindow
	// usageMu 保护上面七个字段：它们由 usage 拉取协程与快照读取面并发访问，与
	// **条目重建**无关——所以用条目自己的叶锁，不复用表锁（表锁只保护 map 与
	// client 桶的摘除）。数据、时刻、代际标签三件套必须在这个锁里一起翻页，
	// 读面才不会看到"新快照 + 旧代际"或"旧窗口 + 新时刻"。
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
		usageNow:      time.Now,
		usageFlights:  make(map[usageFlightKey]*usageFlight),
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
			a.log.Warn("codex image stream: unknown event type skipped", logx.String("type", ev.Type))
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

// ResponsesPrepared 用**已准备句柄**发起非流式 responses 调用（生产热路径）：
// 条目来自该句柄，auth/client 与 owner 同版（§3.2 凭据替换）。调用方必须先
// `Prepare`——未 Prepared 的入口已删除，绕过这条边界在类型层不可表达。
//
// 语义（§1）：响应是 SDK **合成**的非流式体（内部无条件 stream:true + SSE 事件
// 聚合重组完整响应体；网关以非流式语义消费，原样转发；网关从合成体顶层 usage
// 计费）。id 是**请求级身份**（§2.1）：Session/Meta 进握手头与 client_metadata、
// TurnState 是本次要带的 ts、Sink 是签发值落点。上游签发值（握手头/响应头/事件）
// 只经 Sink 写入会话——本函数既不在请求前改 ts，也不在返回后回写：那是把事件签发
// 的新值倒退成请求开始时的快照。
//
// 错误翻译同 GenerateImage（translateError——SDK *HTTPError → 信封；fatal 五类
// 统一回调双源去重；RefreshError/网络原样）。transport 为出站传输开关（spec §5：
// 构造期参数 ⇒ 走 clientFor 的键维度；映射点只有 codexTransport 一处）。
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

// StreamBodyPrepared 用**已准备句柄**发起流式 responses 调用（生产热路径）：
// 条目来自该句柄，auth/client 与 owner 同版。调用方必须先 `Prepare`。
//
// 语义（§1）：返回**未关闭**的 *http.Response（body 归调用方，proxy 侧经
// sserelay.Relay 消费后**恰好关闭一次**）。身份语义同 ResponsesPrepared
// （TurnState 是本次要带的 ts、Sink 是签发值落点）。**本方法不写签发值**：本次
// resp.Header 的 ts 与流式取帧期间 `response.metadata` 事件的新值都由 SDK 经
// Sink 写会话（后者才是更新的值；回写响应头只会把新值倒退成快照）。
// 错误翻译同 ResponsesPrepared；transport 语义同 ResponsesPrepared（构造期
// 参数 ⇒ 换值换 client 条目）。
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

// egressCodexTransport 取出口的"关闭自动压缩"出站 transport（egress.Client.NoCompression()）。
//
// `Codex == nil` 只出现在测试里手写的 `egress.Client` 字面量（egress.Pool 构造的
// 出口恒带 Codex）：回退 HTTP 面 transport，保持这些测试的既有出站路径。
func egressCodexTransport(eg *egress.Client) http.RoundTripper {
	if eg.NoCompression() == nil {
		return eg.HTTP().Transport
	}
	return eg.NoCompression().Transport
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

// —— codex 额度快照（GetUsageSnapshot——代际绑定 + 单飞 + TTL/冷却 + 有界并发） ——

// —— usage 错误面（分类的三层形状，改动前必读） ——
//
// 每一次 GetUsageSnapshot 失败都同时携带**两种判定**（同一条 %w 链上）：
//
//	err = fmt.Errorf("%w: %w", &codexproto.Error{Kind, Err: cause}, 旧哨兵)
//	         ↑ 消费方（handler）用 errors.As 认这个类型  ↑ 老调用方用 errors.Is 认这个值
//
// 为什么是两层而不是只留一种：
//   - **类型**（internal/codexproto.Error）住中立叶子包，消费方（handler）判 Kind
//     就不必 import 本包——这正是本仓库"生产方与消费方都不认识对方的包"的范式
//     （与 internal/pricingresult.FetchError 同构）；
//   - **旧哨兵**是本包既有的公开面，本包自己的几十条 require.ErrorIs 与
//     QuotaDeadline / worker 等调用方仍认它。删掉它是行为破坏，不是解耦。
//     As 与 Is 相互独立，两条判定互不依赖，共存无副作用。
//
// ⚠ 本包的 ErrUpstream 与 serviceerr.ErrUpstream **同名、不同值、同词义**（仓库
// 第二例）。判"codex 用量上游失败"只许用本包的值或 codexproto.ErrKindUpstream，
// 不要拿 serviceerr 的那个取代它。

// ErrAuthExpired 凭据失效分类错误（GetUsageSnapshot 错误面——IsFatal 纯判定
// 零副作用：usage 查询面不重复上报不摘除——凭据失效由会话路径（Dial/
// Responses）经 FatalAuth 上报，防循环；凭据变更后 entry sig 重建自然恢复。
// upstream_error 映射输入）。
//
// 保留为**值**（老调用方与老测试认它）；新的判定请用 &codexproto.Error 的 Kind。
var ErrAuthExpired = errors.New("codex: usage auth expired")

// ErrUpstream 上游错误分类错误（GetUsageSnapshot 错误面——*HTTPError/网络/
// RefreshError 等一律保守归本类；upstream_error 映射输入）。
//
// 保留为**值**（老调用方与老测试认它）；新的判定请用 &codexproto.Error 的 Kind。
var ErrUpstream = errors.New("codex: usage upstream error")

// ErrUsageStale 本次观测已不属于当前身份代际：拉取在途期间同签名的身份写入
// 把 K 抬过了拉取起点（`entry.srcRev` 前进、`cred` 字节不变 ⇒ 条目不重建）。
// 结果**已丢弃**：快照、窗口事实、时刻、`usageRev` 与失败冷却一律未写。
//
// 调用方语义：按「无事实」处理——它既不是上游错误也不是凭据错误，而是"这一趟
// 白跑了"（不延长冷却、不判账号失败；下一次按新 K 重新拉）。为什么必须丢弃：
// 把旧代际的观测写进缓存并标成新一代，就是让新身份命中它从未观测过的事实
// （反例：K=3 命中 K=2 写的 usage）。
var ErrUsageStale = errors.New("codex: usage snapshot superseded by identity change")

// usageFetchConcurrency 快照拉取有界并发上限（所有调用面共享节流：管理面
// 几百账号懒加载 + 未来路由面共用同一上限，防 OpenAI 429；只限并发不限速率
// ——速率上限由失败冷却封顶）。实例级 semaphore（Codex.usageSem）容量即此值，
// 挂实例消除包级可变状态（S8）。**只有 leader 抢槽**（见 usageFlightKey）：
// follower 不占并发额度，否则 8 个 follower 就能把槽占满而 leader 永远拿不到。
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

// usageFlightKey 单飞键 = `(accountID, K)`（设计 §5.1/§5.2）。
//
// **不含凭据签名**：K 只由管理面身份写入推进，所以同账号同代际的并发请求必然
// 看同一份凭据内容；把签名放进来只会让"同代际、快照里字节不同的两次调用"并发
// 打两次上游（签名是重建判据，不是观测代际）。反向也不成立——同签名不同 K 必须
// **分开**：那是两代身份，各自的观测不得互相冒充。
type usageFlightKey struct {
	accountID int64
	rev       int64
}

// usageFollowRetries follower 重试次数：leader 的 ctx 死了不等于本调用被取消。
// 上界必须有——否则"每次 leader 都被取消"会变成不退出的重试环；2 次足够覆盖
// 一次偶然的连接断开，又不会把调用方预算耗在同一个坏上游上。
const usageFollowRetries = 2

// usageFlight 一次在飞拉取：`done` 关闭后 snap/err 可读（happens-before 由
// close 建立）。结果对所有 follower 共享——与 singleflight 同语义：连同错误
// 一起共享，**但 leader 自己的 ctx 死亡不是 follower 的结论**（它说明的是
// leader 的连接断了，不是 follower 的调用被取消——见 usageFollowRetries）。
type usageFlight struct {
	done chan struct{}
	snap *domain.CodexUsageSnapshot
	err  error
}

// GetUsageSnapshot 账号 codex 额度快照（ChatGPT 面 wham/usage）：cred →
// 缓存条目（entryFor——sig 比对/重建，重建时 usage/usageAt/usageRev/
// usageErrAt/usageErr/usageErrRev 随新条目一并清除）→ 5min TTL **且同代际**
// 命中直接返回（零上游）；未命中 → `(accountID, K)` 单飞（follower 只等结论，
// 不占并发额度；leader 的 ctx 死亡时有界重试，不连坐——见 usageFollowRetries）
// → leader 抢实例 semaphore（容量 8，有界并发）→ SDK GetUsage
// （e.client 复用——固定 SDK 官方端点
// https://chatgpt.com/backend-api/wham/usage，网关零拼装，test transport 仅 host 重写保留官方 path）→ 白名单收敛映射（fromSDKUsage）→ 写
// e.usage + e.usageAt + e.usageRev + e.usageWindows（同一临界区）。失败 → 写
// e.usageErrAt + e.usageErr + e.usageErrRev（60s **同代际**冷却）。
//
// **代际**：命中要 `usageRev == cred.IdentityRevision`（`srcRev` 是
// 资源归属的单调观测值，不能当观测代际）；失败冷却只挡 `usageErrRev == K` 的
// 那一代（换代立即允许补拉一次）；拉取期间 K 前进 ⇒ 结果丢弃并返回
// ErrUsageStale（见函数内注释）。
//
// 错误分类**纯判定零副作用**（gate Major 3——不复用 translateError：其 fatal
// 分支 reportFatal → evictIfCurrent 删 entry，冷却随 entry 消失）：IsFatal（fatal 五类
// 穿透信封链——SDK 说凭据死才是凭据死，鉴权面真相唯一来源）→ ErrAuthExpired；
// 其余（*HTTPError/网络/RefreshError，含非致命 401——无判死标记的拒绝属上游
// 面，不从状态码反推鉴权结论）保守归 ErrUpstream。usage 是查询面不是会话面——凭据失效
// 由会话路径上报（防循环），entry 恒保留（fatal 后后续调用仍命中冷却）。
//
// **错误面的形状**（消费方判定依据，见文件上方「usage 错误面」）：分类结果 =
// `fmt.Errorf("%w: %w", &codexproto.Error{Kind, Err}, 旧哨兵)`。用 errors.As 取
// Kind 与用 errors.Is 认哨兵**都**成立，两者互不依赖；ctx 取消/超时与
// ErrUsageStale 不在此列（它们不是"分类"，原样透传）。
//
// 命中路径零分配：先按 accountID 查 entry.usage（map 查 + 锁 + 时间与代际
// 比较——不计算 credSig/不重建条目），命中直接返回；未命中才走
// clientFor（entryFor 签名比对/重建）。
//
// 出口：本面**没有 Selection**（额度快照是节点本地的后台拉取，不是某次请求的
// 出栈），故走池的直连出口（`DirectEgress()`，经 usageClientFor）。它**不参与定义
// 账号的活跃集**：请求路径只取/建资源，配置可见性只由配置权威下发的资源指令改写
// （`ClientTable.ApplyBatch`；直连键由权威显式列入保留集，不是表的特例）。
func (a *Codex) GetUsageSnapshot(ctx context.Context, cred *domain.AccountCredential) (*domain.CodexUsageSnapshot, error) {
	// 本请求代表的身份代际：整条路径只认这一个值（不回读条目上的当前 K——那会
	// 把别的请求的推进算到这次观测头上）。
	startK := cred.IdentityRevision
	if s, ok := a.snapshotCachedFor(cred.AccountID, startK); ok {
		return s, nil
	}
	e, client, err := a.usageClientFor(cred)
	if err != nil {
		// 入口错误分类：oauth 缺 rt 凭据不完整（errCredentialIncomplete）
		// → 凭据失效语义 ErrAuthExpired（不落 default 归 ErrUpstream）。形状与其他
		// 分类点一致（类型 + 旧哨兵同链），消费方只需一种判定路径；原因原样进
		// codexproto.Error.Err。入口错误没打上游，不进冷却三元组——直接返回。
		if errors.Is(err, errCredentialIncomplete) {
			return nil, fmt.Errorf("%w: %w",
				&codexproto.Error{Kind: codexproto.ErrKindAuthExpired, Err: err}, ErrAuthExpired)
		}
		return nil, err
	}
	if s, ok := a.snapshotCached(e, startK); ok {
		return s, nil
	}
	if err := a.snapshotCooldown(e, startK); err != nil {
		return nil, err
	}
	// 单飞：同 `(accountID, K)` 的并发调用只让一个去打上游，其余等它的结论。
	key := usageFlightKey{accountID: cred.AccountID, rev: startK}
	var flight *usageFlight
	for attempts := 0; ; {
		f, leader := a.joinUsageFlight(key)
		if leader {
			flight = f
			break
		}
		select {
		case <-f.done:
			// 只有「别人的 ctx 到期 / 被取消」且本调用仍活着时才重试；自己的取消
			// 原样返回。重试即重新 join（不复制 flight、不手工关 done）：要么自己
			// 成为新 leader，要么并到别人的新 flight 上。
			if ctx.Err() == nil && isFlightCtxErr(f.err) && attempts < usageFollowRetries {
				attempts++
				continue
			}
			return f.snap, f.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	// leader：任何返回路径都必须摘除登记并发布结论——否则同键后来者会永远等一个
	// 不会关闭的 done。
	var (
		snap *domain.CodexUsageSnapshot
		ferr error
	)
	defer func() { a.finishUsageFlight(key, flight, snap, ferr) }()
	select {
	case a.usageSem <- struct{}{}:
	case <-ctx.Done():
		ferr = ctx.Err()
		return nil, ferr
	}
	defer func() { <-a.usageSem }()
	// 双检：等槽期间同键的前一位 leader（已结束、登记已摘）或别的代际可能已经
	// 写好结论（TTL/冷却语义保持——不重复拉取、不重复报错）。
	if s, ok := a.snapshotCached(e, startK); ok {
		snap = s
		return s, nil
	}
	if err := a.snapshotCooldown(e, startK); err != nil {
		ferr = err
		return nil, err
	}
	// 拉取起点两件事：`fetchStartedAt` 是本次观测"有多新"的上界（新鲜度比较，
	// 见 commitUsage）；`seenAtStart` 是当时的归属代际观测值（判"本次拉取期间
	// 身份有没有前进"，见下）。
	fetchStartedAt := a.now()
	seenAtStart := e.srcRev.Load()
	resp, err := client.GetUsage(ctx)
	if err != nil {
		// 取消/超时短路（task review 2026-08-18 Important 1）：ctx 取消 →
		// 不写失败冷却（误记会锁死该账号 60s——管理面批量拉取切页触发面）、
		// 返回 ctx 错误本身保留取消身份（不误归 ErrUpstream）。
		if ctxErr := ctx.Err(); ctxErr != nil {
			ferr = ctxErr
			return nil, ctxErr
		}
		ferr = classifyUsageErr(err)
		a.recordUsageFailure(e, startK, err)
		return nil, ferr
	}
	s := fromSDKUsage(resp)
	// windows 在锁外算：`QuotaState()` 是纯映射（无 IO、不碰条目），而临界区只
	// 该包住"数据 + 时刻 + 代际标签一起翻页"这件事本身（见 commitUsage）——
	// 否则读面可能看到"新快照 + 旧窗口"（或反之），那就是把两次观测拼成一次事实。
	windows := resp.QuotaState()
	// 本次拉取期间身份前进 ⇒ 丢弃结果。不变量与证明：
	//  - 任何写入都带 `usageRev = startK` 的诚实标签，而命中要求 `usageRev == k`
	//    ⇒ 旧代的观测永远不会被新代际消费（写错标签才是唯一的破口）；
	//  - 判据取"开始时的 srcRev 观测值"：`srcRev` 是单调值，开始**之前**就被抬到
	//    startK 之上的情形（seen == seenAtStart）不是本次拉取造成的，不丢弃——
	//    其安全性由 commitUsage 的单调闸门保证（新一代已写过 ⇒ 旧代不降级）；
	//  - 于是丢弃只覆盖"在途期间换代"这一种确定情形，ErrUsageStale 的语义不被
	//    扩大（它仍只表示"这趟白跑了"，不含"写不进去"）。
	if seen := e.srcRev.Load(); seen > startK && seen > seenAtStart {
		ferr = ErrUsageStale
		return nil, ferr
	}
	snap = a.commitUsage(e, s, windows, startK, fetchStartedAt)
	return snap, nil
}

// QuotaDeadline 该账号当前"额度打满后最晚可信 reset"（冷却截止候选）。
//
// 语义分三段，**都复用** GetUsageSnapshot 这一条拉取路径——本面绝不另开拉取
// （那会让"一次 429 一次上游"的上界失效，见设计 §5.2）：
//
//  1. `GetUsageSnapshot(ctx, cred)`：命中 TTL/失败冷却就零上游，未命中才拉；
//  2. 读该账号条目里那次成功拉取留下的窗口事实（`usageWindows`）——**只读本
//     代际的那份**（`usageRev == cred.IdentityRevision`）；
//  3. `ExhaustedUntil(windows, now)`：卫生校验 + 取最晚 reset。
//
// 步骤 2 的代际校验不是装饰：GetUsageSnapshot 与读窗口之间隔着一次表查询，若
// 期间另一个代际的拉取写回了条目，读到的就是**别人的**窗口事实——用它算截止
// 等于把 A 代的冷却时长建在 B 代的观测上。归属不符 ⇒ 当作"没有窗口事实"。
//
// 错误面原样透传（ErrAuthExpired / ErrUpstream / ErrUsageStale / ctx 错误）：
// 调用方据此决定「补拉失败 ⇒ 冷却时长不变长」，本函数不替它降级成 false。
//
// 返回 false 的四种来由都合法且不可区分（调用方一律"不延长"）：没有窗口
// 事实（未拉过/条目已重建）、窗口属于别的代际、窗口都没打满、事实全被卫生
// 条件剔除。
func (a *Codex) QuotaDeadline(ctx context.Context, cred *domain.AccountCredential) (time.Time, bool, error) {
	if _, err := a.GetUsageSnapshot(ctx, cred); err != nil {
		return time.Time{}, false, err
	}
	e := a.clients.Get(cred.AccountID)
	if e == nil {
		return time.Time{}, false, nil
	}
	// 只取切片头（SDK 每次 QuotaState() 都返回新切片，元素由本包独占），
	// 临界区里不做映射/判决——两件事都不需要互斥。
	e.usageMu.Lock()
	windows := e.usageWindows
	rev := e.usageRev
	e.usageMu.Unlock()
	if rev != cred.IdentityRevision {
		return time.Time{}, false, nil
	}
	until, ok := ExhaustedUntil(windows, a.now())
	return until, ok, nil
}

// snapshotCachedFor 命中路径零分配快查：按 accountID 查 entry + **同代际**
// TTL 判定——命中直接返回缓存实例，不经过 clientFor/credSig/entryFor（重建
// 判定）。未命中/无 entry/代际不符 → nil（走完整路径）。
//
// k 是必须的：条目按凭据签名复用，K 前进时条目**不会**重建（凭据字节没变），
// 少了这一维就会把上一代的快照当成这一代的事实返回。
func (a *Codex) snapshotCachedFor(accountID int64, k int64) (*domain.CodexUsageSnapshot, bool) {
	e := a.clients.Get(accountID)
	if e == nil {
		return nil, false
	}
	e.usageMu.Lock()
	defer e.usageMu.Unlock()
	if e.usage != nil && e.usageRev == k && a.now().Sub(e.usageAt) < a.usageTTL {
		return e.usage, true
	}
	return nil, false
}

// snapshotCached 快照命中判定（5min 慢变量 + 同代际；e.usage nil = 未拉取过）。
func (a *Codex) snapshotCached(e *codexEntry, k int64) (*domain.CodexUsageSnapshot, bool) {
	e.usageMu.Lock()
	defer e.usageMu.Unlock()
	if e.usage != nil && e.usageRev == k && a.now().Sub(e.usageAt) < a.usageTTL {
		return e.usage, true
	}
	return nil, false
}

// snapshotCooldown 失败冷却判定（60s，**同代际**）：`usageErrRev == k` 且新鲜 →
// 返回分类哨兵（ErrAuthExpired/ErrUpstream——非错误体，冷却后重试拉取新错误）；
// 其余（别的代际的失败、已过冷却、从未失败）→ nil（可拉取）。
//
// 换代就放行（`usageErrRev != k` ⇒ nil）是裁定的一部分：base URL/installation
// 这类不动凭据字节的身份写入之后，旧代际的 60s 冷却不得挡住新代际的第一次补拉。
func (a *Codex) snapshotCooldown(e *codexEntry, k int64) error {
	e.usageMu.Lock()
	defer e.usageMu.Unlock()
	if e.usageErrRev == k && !e.usageErrAt.IsZero() && a.now().Sub(e.usageErrAt) < a.usageCooldown {
		return e.usageErr
	}
	return nil
}

// recordUsageFailure 失败冷却起点（gate Major 2）：同一临界区写
// usageErrAt + 分类哨兵 + **失败所属代际**（不缓存错误体）——三元组必须一起
// 翻页，否则读面可能把 A 代的失败时间配上 B 代的标签。
func (a *Codex) recordUsageFailure(e *codexEntry, k int64, err error) {
	sentinel := classifyUsageErr(err)
	now := a.now()
	e.usageMu.Lock()
	defer e.usageMu.Unlock()
	// 单调闸门（与 commitUsage 同一条纪律）：新一代的失败记录已经写过
	// ⇒ 旧代的失败不得把它降级——哪怕这只是一段冷却，它也会让新代际被一段自己
	// 没经历过的冷却挡住。
	if e.usageErrRev > k {
		return
	}
	e.usageErrAt = now
	e.usageErr = sentinel
	e.usageErrRev = k
}

// commitUsage 成功结果提交：数据、时刻、代际标签（以及冷却清零）在**同一**
// usageMu 临界区里一起翻页——读面永远看不到"新快照 + 旧标签"或"旧窗口 + 新
// 时刻"。
//
// 新鲜度（设计 §5.3）：同代际缓存里已有**不比本次拉取开始更旧**的结果 ⇒ 本次
// 观测不算更新鲜，不覆盖，直接把缓存里那份交回调用方（它属于同一代际，代表
// 同一事实且更近）。判据取**拉取开始时刻**而不是返回时刻：返回顺序由网络决定，
// 开始时刻才是"这次观测最晚可能有多旧"的上界——用返回时刻比较等于让"先发后到"
// 的旧观测覆盖新观测。
//
// 比较用 `!usageAt.Before(fetchStartedAt)`（即 **>=**，不是严格的 After）：
// `time.Now()` 的粒度在 Windows 上可以粗到毫秒级，两个真正交错的观测会拿到
// **逐纳秒相等**的时刻（本用例在 CI 与本地都实际撞到过）。相等时无法判先后，
// 保守选"保留缓存"——否则"后返回者覆盖先返回者"会以秒级概率重现。
//
// 单调：`usageRev` 只许单调不减——本次观测属于比缓存**更旧**的代际
// （k < usageRev，即拉取开始前凭据快照就已落后）时照样不落缓存，只把观测交回
// 调用方。它与新鲜度是两条正交的闸门：新鲜度比"时刻"，单调比"代际"。
func (a *Codex) commitUsage(e *codexEntry, s *domain.CodexUsageSnapshot, windows []codexsdk.QuotaWindow, k int64, fetchStartedAt time.Time) *domain.CodexUsageSnapshot {
	now := a.now()
	e.usageMu.Lock()
	defer e.usageMu.Unlock()
	// 单调闸门：新一代已经写过 ⇒ 旧代的观测不得把它降级（usageRev 只
	// 许单调不减）。本次结果仍然交回调用方（它问的就是 k 这一代），只是不落缓存。
	if e.usageRev > k {
		return s
	}
	if e.usageRev == k && !e.usageAt.Before(fetchStartedAt) {
		return e.usage
	}
	e.usage = s
	e.usageAt = now
	e.usageRev = k
	e.usageWindows = windows
	// 成功清冷却态（死状态不留：哨兵仅在"同代际 + 新鲜"时被读取，但状态机
	// 卫生——检查顺序调整不误服旧哨兵；代际标签一并归零）。
	e.usageErrAt = time.Time{}
	e.usageErr = nil
	e.usageErrRev = 0
	return s
}

// now usage 面的时间源（构造期 = time.Now）。集中一处：TTL、失败冷却与新鲜度
// 判定共用同一个时钟，测试才能靠"推进时间"而不是 sleep 来判门禁。
func (a *Codex) now() time.Time {
	if a.usageNow == nil {
		return time.Now()
	}
	return a.usageNow()
}

// joinUsageFlight 加入 `(accountID, K)` 的在飞拉取。leader=true 表示由本次调用
// 去打上游；false 表示等 leader 的结论（**不占 semaphore 槽**——follower 若也
// 抢槽，8 个 follower 就能把容量占满、leader 永远拿不到，直接死锁）。
func (a *Codex) joinUsageFlight(key usageFlightKey) (*usageFlight, bool) {
	a.usageFlightMu.Lock()
	defer a.usageFlightMu.Unlock()
	if f := a.usageFlights[key]; f != nil {
		return f, false
	}
	f := &usageFlight{done: make(chan struct{})}
	a.usageFlights[key] = f
	return f, true
}

// finishUsageFlight 摘除在飞登记并发布结论（先写结论再关 done：follower 的
// 读取由 close 建立 happens-before）。摘除只在登记仍是自己时进行——同一键的
// 后一位 leader 可能已经接手。
func (a *Codex) finishUsageFlight(key usageFlightKey, f *usageFlight, snap *domain.CodexUsageSnapshot, err error) {
	a.usageFlightMu.Lock()
	if a.usageFlights[key] == f {
		delete(a.usageFlights, key)
	}
	a.usageFlightMu.Unlock()
	f.snap = snap
	f.err = err
	close(f.done)
}

// isFlightCtxErr leader 的结论是不是"它自己的 ctx 死了"（取消 / 超时）。这种情形下
// follower 自己的 ctx 仍然活着：把别人的取消当成自己的结论就是白丢一次补拉。
func isFlightCtxErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// classifyUsageErr 错误分类纯判定（gate Major 3——零副作用不上报不摘除）。
// 边界单一原则：鉴权面真相唯一来源是 SDK 致命分类——IsFatal（fatal 五类穿透
// 信封链：RefreshOAuthError/AuthPermanentlyRevokedError/AccountDisabledError/
// CallbackDeliveryError）→ ErrAuthExpired；其余一律 ErrUpstream。非致命 401
// （无判死标记的拒绝——上游未宣告凭证死亡）不在此反推鉴权结论：真死亡必带
// 致命标记走 fatal 路径并经 OnAuthFatal 禁用，无标记则按上游抖动冷却重试自愈
// （的 401 特判已随 SDK PAT 判死上线退役——补丁理由消失即删）。
//
// 返回值形状见上方「usage 错误面」：**类型 + 旧哨兵**两层同链。cause 原样放进
// codexproto.Error.Err（诊断可用、不丢失底层原因）；`usageErr` 只缓存这个包装
// 结果（不是 error 体，见 failure 冷却三元组注释）。
//
// 一个入口都在这里：正常拉取失败（GetUsageSnapshot）与冷却态回放
// （snapshotCooldown 读到的正是本函数上一次的产出）用同一种形状，消费方不需要
// 区分"这次的"还是"续上的"。
func classifyUsageErr(err error) error {
	kind := codexproto.ErrKindUpstream
	sentinel := ErrUpstream
	if IsFatal(err) {
		kind, sentinel = codexproto.ErrKindAuthExpired, ErrAuthExpired
	}
	return fmt.Errorf("%w: %w", &codexproto.Error{Kind: kind, Err: err}, sentinel)
}

// fromSDKUsage codexsdk.UsageStatus → domain.CodexUsageSnapshot 白名单收敛
// 映射（spec 变更 3——砍四留四：approx_*/瞬时布尔/派生状态/RateLimitReached
// 一律不进契约；每块 nil → 不出字段；ResetAt Unix 秒 → time.Time；ResetAt 0
// （上游主窗口省略）→ nil 不出字段；Balance 空串 → nil 不出字段——零填充语义）。
//
// **UsedPercent 是唯一的有损收敛**：SDK 侧是 `float64`（上游会发 99.5，用 int
// 解码会让整个响应失败），domain 侧保持 `int`（改它会牵动 openapi 生成物与前端
// schema，收益只是多一位小数）。这里四舍五入到整数——精度只影响展示；冷却判决
// 走 SDK 的窗口事实（`QuotaWindow.UsedPercent` 原样 float64，见 ExhaustedUntil），
// **不用这个字段**，所以收敛不改变任何冷却决定。
func fromSDKUsage(u *codexsdk.UsageStatus) *domain.CodexUsageSnapshot {
	if u == nil {
		return nil
	}
	s := &domain.CodexUsageSnapshot{PlanType: u.PlanType}
	if rl := u.RateLimit; rl != nil && rl.PrimaryWindow != nil {
		s.RateLimit = &domain.CodexRateLimit{UsedPercent: int(math.Round(rl.PrimaryWindow.UsedPercent))}
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

// DialPrepared 用**已准备句柄**拨号（生产 WS 车道热路径）：auth 来自该句柄，与 owner
// 同版（§3.2 凭据替换）。调用方必须先 `Prepare`——未 Prepared 的入口已删除。
//
// 语义（§2 接线）：建立到上游的 Responses WebSocket 连接——Auth 是账号级长存的
// 句柄（at 缓存/单飞/rt 轮换在 Auth 内，与 HTTP 面共享；WS 连接本身 per-请求不
// 缓存）。id 是本次连接的身份（握手头 + 帧内 metadata + ts 落点）；opts 由网关侧
// 组装（codex_responses_ws.go——WithPingInterval(0) 等；SDK 侧按 codex 形状归一
// WS 帧）；端点归 SDK 官方默认（wss://chatgpt.com/backend-api/codex/responses，
// transport 观察为 https）。错误翻译（translateDialError）：
//   - *DialError → 信封包装（EnvelopeError——StatusCode()/RawJSON()/Unwrap
//     链，Refreshed 语义保留：已轮转重连一次仍失败 → 网关避免双份刷新）
//   - 裸错误（401 轮转路径 refresh 失败——client.go:391-394 透传，不包
//     DialError）→ translateError 既有双分支：fatal 类（RefreshOAuthError /
//     AccountDisabledError）→ 统一回调单次上报 + 原样透传（网关"该请求不转
//     移"，IsFatal 判定）；RefreshError/网络 → 原样透传（网关正常 failover）
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
//     身份已被管理面替换 ⇒ storeerr.ErrStaleIdentityRevision
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
		if errors.Is(err, storeerr.ErrStaleIdentityRevision) {
			// 存储围栏拒绝：这一版身份已被管理面替换（旧 Auth 在途 refresh 迟到的
			// 结果）。丢弃即可——新凭据的令牌由新条目自己维护。
			a.log.Warn("codex rotation writeback rejected by identity fence",
				logx.Int64("account_id", e.accountID), logx.Int64("source_identity_revision", srcRev))
			return
		}
		// 运维信号即时留痕（重试至多 3 次各记一条——DB 故障持续期的
		// 真实告警；fatal 上报后账号摘除，告警自停）
		a.log.Warn("codex rotation writeback failed", logx.Int64("account_id", e.accountID), logx.Error(err))
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

// asFatal 判定 SDK 错误是否为账号级终止类（fatal 四类，errors.As 穿透信封包装链
// ——EnvelopeError.Unwrap 保留链）。RefreshError 不在 fatal 集（可重试
// ——auth_errors.go:53-58 语义）。
//
// 判死码集与字段路径全在 SDK（codexsdk/upstream_error.go）：网关只消费"已经是
// 致命类型的错误值"，不持有码表副本，也就不存在两处副本分叉的可能。
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
