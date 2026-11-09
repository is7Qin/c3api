// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/egress"
	"github.com/is7qin/c3api/pkg/logx"
)

// Codex 是 codex SDK 适配层（§1——SDK 调用集中于此，codexsdk import 仅限
// 本文件族）：cred → Auth 账号级缓存 + GenerateImage 包装 + 信封包装 +
// fatal → 统一回调（双源去重）+ 轮转回写。新增能力（流式 / Dial /
// resp）同形态扩展本文件。
type Codex struct {
	mu      sync.Mutex
	entries map[int64]*codexEntry // accountID → 客户端缓存（同账号复用）
	failure FailureHandler        // 统一失效回调；nil = no-op（测试/未装配）
	rotate  RotationStore         // 轮转回写落库面；nil = 不落库（测试/未装配）
	inval   func(accountID int64) // 回写后失效账号快照条目（下个会话重载新凭据）；nil = 不失效
	// clearTurnStates 凭据替换（条目重建且 credSig 变）时清该账号全部会话的 ts
	// （scheduler.ClearTurnStates 实现；nil = 不接）。
	clearTurnStates func(accountID int64)
	log             *logx.Logger // 回写/失效错误日志；nil = 不记
	// egress 出口池（构造期注入；M3 取代旧的单个 http.RoundTripper 字段）。
	// SDK 客户端的 transport 是**构造期**参数，所以出口只能按账号在 clientFor
	// 构造时注入（`eg.HTTP.Transport`——与 HTTP 面共享同一连接池）；同一账号
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
type RotationStore interface {
	WriteOAuthRotation(ctx context.Context, accountID int64, at, rt string, expiresAt *time.Time) error
}

// RotationDeps 轮转回写依赖（§1——main 装配：repository.AccountExts +
// scheduler）。
type RotationDeps struct {
	Store RotationStore
	// InvalidateSnapshot 回写成功后失效调度器 AccountExt 内存快照对应条目
	// （下个会话重载新凭据）；nil = 不失效（测试/未装配）。
	InvalidateSnapshot func(accountID int64)
	// ClearTurnStates 凭据**替换**（外部 credSig 变化 ⇒ 条目重建）时让该账号全部
	// 会话的 turn-state 失效（scheduler.ClearTurnStates 实现；nil = 不接）。
	//
	// owner 判定就是「条目被替换」这件事本身——token 轮转回调不重建条目，因此
	// 不在这里清（§3.2 凭据替换）。
	ClearTurnStates func(accountID int64)
	// Log 回写/失效错误日志（旋转低频事件，错误恒 Warn 记一条）；nil = 不记。
	Log *logx.Logger
}

// (SetRotationDeps setter deleted by hygiene: same reason — see NewCodex.)

// codexWSConnCacheCapacity 是每个 HTTPClient 的空闲 WS 连接**总数上限**（仅上限、
// 不预分配；SDK 的 connCache 按需 append）。
//
// 取 2 × 会话池容量上界（scheduler.maxIdentityPoolSlots = 1024）：一条 client 上
// 每个 thread 至多两条（流式 64MiB / 聚合 16MiB 各一），而容量是构造期参数、K 随
// reload 变化——按当前 K 配会让「改大 K」之后仍按旧上限淘汰。
//
// **必须显式注入**：SDK 默认 0 = 不复用（每请求一条新连接）——不接这一行，"跨轮
// 复用"整条链就是零收益（spec GAP-7）。
const codexWSConnCacheCapacity = 2 * 1024

// codexEntry 单账号缓存条目：Auth（HTTP/WS 双面共享——at 缓存/单飞/rt 轮换
// 在 SDK Auth 内）+ HTTPClient 多条目缓存（按身份签名，nil = 未构造）+ 重建
// 判定签名 + fatal 已上报标记（双源去重——回调路径与 errors.As 路径共享同一 CAS）。
// expiresAt 为构造时凭据携带的旧过期时刻（轮转回调无 expiry，回写保旧
// 用；外部凭据变更 → 重建刷新）。
type codexEntry struct {
	accountID int64
	auth      codexsdk.Auth
	sig       credKey // 凭据签名（外部凭据变更 → 重建）
	// clients HTTP 面客户端缓存：键 = **纯传输维度**（出口 × responses 传输）——
	// 两者都是 SDK 的构造期参数。身份不在键里：身份是请求数据（§3.1），所以同一条
	// client 服务该账号全部会话，条目数有界（出口数 × 2 条传输），不需要 LRU。
	//
	// turn-state 也不在条目上：存储属会话（scheduler.codexSession），经 Identity.Sink
	// 写入、经 Identity.TurnState 读出。
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
}

// NewCodex 构造 codex 适配层。failure 为统一失效回调（适配层构造注册
// WithOnAuthFatal → 回调；nil = 上报 no-op——测试替身形态）；egressPool 为出口池
// （构造期注入，生产 = main 里与调度器 Config.Egress 同一个池；客户端 transport
// 就从这个池的 client 上取，spec-proxy-pool §4.6）；rotation 为轮转回写面
// （零值 = 不落库/不失效/不记，测试与未装配形态）。
// 三者皆为构造期依赖，一次给齐——构造后不存在“半装配”的 Codex。
func NewCodex(failure FailureHandler, egressPool *egress.Pool, rotation RotationDeps) *Codex {
	return &Codex{
		failure:         failure,
		egress:          egressPool,
		rotate:          rotation.Store,
		inval:           rotation.InvalidateSnapshot,
		clearTurnStates: rotation.ClearTurnStates,
		log:             rotation.Log,
		entries:         make(map[int64]*codexEntry),
		usageSem:        make(chan struct{}, usageFetchConcurrency),
		usageTTL:        defaultUsageSnapshotTTL,
		usageCooldown:   defaultUsageCooldown,
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
		return nil, a.translateError(e, err)
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
		// 与 GenerateImage 同款（评审修复）：SDK *HTTPError（字段裸类型，无
		// StatusCode()/RawJSON() 方法）→ EnvelopeError 包装——网关 statusOf/
		// upstreamBody/streamErrMessage 的协议才能消费（4xx 状态 + 原始 body
		// 透传、SSE error 帧 message 取上游文案）；fatal 五类统一回调单次上报
		// + 原样透传。fn 回调错误（网关写入失败/客户端断开）非 SDK 错误 →
		// translateError 原样透传（不过滤）。
		return a.translateError(e, err)
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
	e, client, err := a.clientFor(cred, eg, transport)
	if err != nil {
		return nil, err
	}
	resp, err := client.Responses(ctx, payload, id)
	if err != nil {
		return nil, a.translateError(e, err)
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
		return nil, a.translateError(e, err)
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
	e, client, err := a.clientFor(cred, eg, transport)
	if err != nil {
		return nil, err
	}
	resp, err := client.StreamBody(ctx, payload, id)
	if err != nil {
		return nil, a.translateError(e, err)
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
	sig := credSig(cred)
	a.mu.Lock()
	old := a.entries[cred.AccountID]
	if old != nil && old.sig == sig {
		a.mu.Unlock()
		return old, nil
	}
	e := &codexEntry{accountID: cred.AccountID, sig: sig}
	auth, err := a.buildAuth(cred, e)
	// 锁内摘除、锁外关闭（G4）：覆盖旧条目时它名下的 HTTPClient 全部弃用，其中
	// 的 WS 空闲连接必须主动关（否则旧 socket 一直挂在上游，直到 TTL 到期）。
	var dropped []*codexsdk.HTTPClient
	replaced := false
	if err == nil {
		e.auth = auth
		if old != nil {
			dropped = old.takeClients()
			replaced = old.sig != sig
		}
		a.entries[cred.AccountID] = e
	}
	a.mu.Unlock()
	closeWSIdleConns(dropped)
	if replaced && a.clearTurnStates != nil {
		// 凭据**替换**（sig 变）⇒ 该账号全部会话的 ts 作废：旧 ts 属于旧 auth owner，
		// 带进新 owner 的请求就是串台（§3.2）。token 轮转不重建条目，所以不在此列。
		a.clearTurnStates(cred.AccountID)
	}
	if err != nil {
		// 构造失败（空 rt 等）：锁外上报（reportFatal → evict 需取
		// a.mu——锁内调用即重入死锁；sync.Mutex 不可重入）。旧条目仍在表里
		// （构建失败不覆盖），所以上面也没摘它的 client。
		a.reportFatal(e, err)
		return nil, err
	}
	return e, nil
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
	// proxy 出口维（spec-proxy-pool §4.6.3）：= 本次出口的 Spec 摘要，直连（零值
	// Spec）为全零。
	proxy [16]byte
	// transport responses 出站传输（spec §5）；取值面 = CodexResponsesTransport.Key()，
	// 默认面取零值 ⇒ 只走 HTTP 的部署键形状与改动前一致。
	transport string
}

// egressDigest 出口摘要（clientKey 的 proxy 维）：直连（零值 Spec）→ 全零，
// 代理 → `Spec.Key()`。不直接用 `Spec.Key()` 的原因是它连零值也给一个非零哈希，
// 那会让"零代理部署的缓存键与 M3 之前逐字节一致"这条零回归承诺失效。
func egressDigest(eg *egress.Client) [16]byte {
	if eg == nil || eg.Spec == (egress.Spec{}) {
		return [16]byte{}
	}
	return eg.Spec.Key()
}

// clientFor entryFor + HTTPClient 懒构造（GenerateImage/Stream/Responses/Search/
// GetUsage 面——键 = clientKey，同传输组合复用连接池）。
//
// 身份与 turn-state 都不是构造期参数（§3.1）：本次请求带什么由 Identity 决定，
// 签发值由 Sink 落到会话——所以"ts 更新"不构成重建理由，同一条 client 跨轮存活。
// 这也是 S1（连接身份去 ts）在网关侧成立的前提：client 跨轮存活，连接才可能跨轮
// 存活。
//
// NewHTTPClient 为纯构造（无 I/O 无 error）；构造/读取全程持 a.mu。返回选中的
// 客户端供调用面直用。
//
// transport 为 responses 出站传输（spec §5）：SDK 侧同样是**构造期**参数，不入
// 键就会"Selection 说 ws、请求仍走 HTTP POST"。
func (a *Codex) clientFor(cred *domain.AccountCredential, eg *egress.Client, transport domain.CodexResponsesTransport) (*codexEntry, *codexsdk.HTTPClient, error) {
	eg, err := a.resolveEgress(eg)
	if err != nil {
		return nil, nil, err
	}
	e, err := a.entryFor(cred)
	if err != nil {
		return nil, nil, err
	}
	key := clientKey{proxy: egressDigest(eg), transport: transport.Key()}
	a.mu.Lock()
	if e.clients == nil {
		e.clients = make(map[clientKey]*codexsdk.HTTPClient)
	}
	c := e.clients[key]
	if c == nil {
		c = codexsdk.NewHTTPClient(e.auth,
			// 出口 transport（spec §4.6）：eg 恒非 nil（resolveEgress 保证），直连时
			// 就是底座 transport、与 HTTP 面共享同一连接池。底座是裸 client
			// （`Transport == nil`）时 WithTransport(nil) 等价于"不加 Transport"——
			// 所以这里恒注入、不写分支（测试路径也走这一条，与生产同构）。
			codexsdk.WithTransport(eg.HTTP.Transport),
			// responses 出站传输（spec §5）：同样恒注入。WithResponsesTransport
			// (TransportHTTP) 与"不注入"等价，而不写 if 分支的原因正是要让"注入点
			// 被删掉"这种变异在组合用例里现红（删了它，ws 臂会静默跑 HTTP）。
			codexsdk.WithResponsesTransport(codexTransport(transport)),
			// G6：**必须**开 WS 连接复用。SDK 默认 0 = 每请求一条新连接；不接这一行，
			// S1（连接身份去 ts）带来的"可复用"就只是潜力，实际上零复用。
			codexsdk.WithWSConnCache(codexWSConnCacheCapacity),
		)
		e.clients[key] = c
	}
	a.mu.Unlock()
	return e, c, nil
}

// takeClients 摘除并返回条目名下全部 HTTPClient（G4）。**a.mu 下调用**：摘除
// 必须与表操作原子，而关闭（terminate 可能等写闸）必须留在锁外——调用方拿到返回值
// 后在锁外逐个 closeWSIdleConns。
func (e *codexEntry) takeClients() []*codexsdk.HTTPClient {
	if len(e.clients) == 0 {
		return nil
	}
	out := make([]*codexsdk.HTTPClient, 0, len(e.clients))
	for k, c := range e.clients {
		out = append(out, c)
		delete(e.clients, k)
	}
	return out
}

// RetireThread 退役账号某线程的空闲 WS 连接（线程退休 / 重载丢槽的唯一出口）。
//
// 前置条件由会话池保证：调用时该 threadKey 不可能有在飞 exchange（会话独占租借，
// 退休发生在该请求 `Close` 归还之后），所以只需摘 idle。摘表与 terminate 都在
// `RetireThread` 内部完成，本方法只负责在**锁外**把整表快照出来再逐条调用。
func (a *Codex) RetireThread(accountID int64, threadKey string) {
	if a == nil || threadKey == "" {
		return
	}
	a.mu.Lock()
	e := a.entries[accountID]
	var clients []*codexsdk.HTTPClient
	if e != nil {
		clients = make([]*codexsdk.HTTPClient, 0, len(e.clients))
		for _, c := range e.clients {
			clients = append(clients, c)
		}
	}
	a.mu.Unlock()
	for _, c := range clients {
		c.RetireThread(threadKey)
	}
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
// 分支 reportFatal → evict 删 entry，冷却随 entry 消失）：IsFatal（fatal 五类
// 穿透信封链——SDK 说凭据死才是凭据死，鉴权面真相唯一来源）→ ErrAuthExpired；
// 其余（*HTTPError/网络/RefreshError，含非致命 401——无判死标记的拒绝属上游
// 面，不从状态码反推鉴权结论）保守归 ErrUpstream。usage 是查询面不是会话面——凭据失效
// 由会话路径上报（防循环），entry 恒保留（fatal 后后续调用仍命中冷却）。
// 命中路径零分配：先按 accountID 查 entry.usage（map 查 + 锁 + 时间
// 比较——不计算 credSig/不重建条目），命中直接返回；未命中才走
// clientFor（entryFor 签名比对/重建）。
//
// 出口：本面**没有 Selection**（额度快照是节点本地的后台拉取，不是某次请求的
// 出栈），故走池的直连出口（`DirectEgress()`）。想让额度拉取也随账号的代理走，
// 需要把"绑定 → 出口"解析搬到调用面并一起传进来——那是独立的一步，不在 M3。
func (a *Codex) GetUsageSnapshot(ctx context.Context, cred *domain.AccountCredential) (*domain.CodexUsageSnapshot, error) {
	if s, ok := a.snapshotCachedFor(cred.AccountID); ok {
		return s, nil
	}
	e, client, err := a.clientFor(cred, a.DirectEgress(), domain.CodexResponsesTransportDefault)
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
	a.mu.Lock()
	e.usage = s
	e.usageAt = time.Now()
	// 成功清冷却态（死状态不留：哨兵仅在 usageErrAt 新鲜时被读取，但
	// 状态机卫生——检查顺序调整不误服旧哨兵）。
	e.usageErrAt = time.Time{}
	e.usageErr = nil
	a.mu.Unlock()
	return s, nil
}

// snapshotCachedFor 命中路径零分配快查：按 accountID 查 entry + TTL
// 判定——命中直接返回缓存实例，不经过 clientFor/credSig/
// entryFor（重建判定）。未命中/无 entry → nil（走完整路径）。
func (a *Codex) snapshotCachedFor(accountID int64) (*domain.CodexUsageSnapshot, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.entries[accountID]; e != nil && e.usage != nil && time.Since(e.usageAt) < a.usageTTL {
		return e.usage, true
	}
	return nil, false
}

// snapshotCached 快照 TTL 命中判定（5min 慢变量；e.usage nil = 未拉取过）。
func (a *Codex) snapshotCached(e *codexEntry) (*domain.CodexUsageSnapshot, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e.usage != nil && time.Since(e.usageAt) < a.usageTTL {
		return e.usage, true
	}
	return nil, false
}

// snapshotCooldown 失败冷却判定（60s）：冷却中 → 返回分类哨兵（
// ErrAuthExpired/ErrUpstream——非错误体，冷却后重试拉取新错误）；冷却外 →
// nil（可拉取）。
func (a *Codex) snapshotCooldown(e *codexEntry) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !e.usageErrAt.IsZero() && time.Since(e.usageErrAt) < a.usageCooldown {
		return e.usageErr
	}
	return nil
}

// recordUsageFailure 失败冷却起点（gate Major 2）：写 usageErrAt + 分类哨兵
// （不缓存错误体）。
func (a *Codex) recordUsageFailure(e *codexEntry, err error) {
	a.mu.Lock()
	e.usageErrAt = time.Now()
	e.usageErr = classifyUsageErr(err)
	a.mu.Unlock()
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
	e, err := a.entryFor(cred)
	if err != nil {
		return nil, err
	}
	eg, err = a.resolveEgress(eg)
	if err != nil {
		return nil, err
	}
	// 出口 transport 与 HTTP 面同一注入点（spec §4.6.4）：ws 握手走
	// `HTTPClient.Do`，transport 上的 Proxy 是代理唯一的注入面。eg 恒非 nil；
	// `Transport == nil`（裸底座）时 WithTransport(nil) 等价 SDK 默认。
	opts = append(opts, codexsdk.WithTransport(eg.HTTP.Transport))
	c, err := codexsdk.Dial(ctx, e.auth, id, opts...)
	if err != nil {
		return nil, a.translateDialError(e, err)
	}
	return c, nil
}

// translateDialError Dial 错误翻译（§5 错误契约）：*DialError → 信封
// （Refreshed 保留）；其余（refresh 失败裸错误）复用 translateError 双分支
// （fatal → 统一回调 + 原样；RefreshError/网络 → 原样 → 网关 failover 分类）。
func (a *Codex) translateDialError(e *codexEntry, err error) error {
	if de, ok := errors.AsType[*codexsdk.DialError](err); ok {
		env := NewEnvelopeError(de.StatusCode, "", de)
		env.Refreshed = de.Refreshed
		return env
	}
	return a.translateError(e, err)
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
	if cred.PATKey != "" {
		return codexsdk.PAT(cred.PATKey,
			// 统一回调装配（与 OAuth 同源）：SDK 判死（PAT 401 致命体分类）→
			// reportFatal 上报禁用（双源去重单次）
			codexsdk.WithPATOnAuthFatal(func(fatal error) { a.reportFatal(e, fatal) }),
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
		codexsdk.WithOnAuthFatal(func(fatal error) { a.reportFatal(e, fatal) }),
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

// credKey 是外部凭据签名（重建判定）的**可比较值键**，取代原先的 \x00 拼串：
// 逐字段 `==` 比较，不依赖「OAuth token/PAT 为 base64url 字符集」的脆弱约定。
type credKey struct {
	oauthToken     string
	oauthRefresh   string
	patKey         string
	codexAccountID string
}

// credSig 凭据签名（重建判定）：外部凭据变更（管理面导入/更新——token/rt/pat/
// account id 任一变化）→ 重建。过期时刻不参与签名（构造时的初始 at 预置决策已
// 经生效；过期 at 由 SDK 401 自愈轮转，无需重建）。
func credSig(c *domain.AccountCredential) credKey {
	return credKey{
		oauthToken:     c.OAuthToken,
		oauthRefresh:   c.OAuthRefreshToken,
		patKey:         c.PATKey,
		codexAccountID: c.CodexAccountID,
	}
}

// report 单次上报核心（双源去重——CAS 胜者上报，败者并发调用/补报路径跳过）；
// evict=true 上报后失效剔除条目（HTTP 路径——账号已判死，缓存条目随弃，管理
// 面恢复后重建）。WS 帧路径（FatalAuth）用 evict=false——毒化 Auth 保留。
func (a *Codex) report(e *codexEntry, fatal error, evict bool) {
	if !e.reported.CompareAndSwap(false, true) {
		return
	}
	if a.failure != nil {
		a.failure(e.accountID, fatal)
	}
	if evict {
		a.evict(e.accountID)
	}
}

// reportFatal fatal 统一上报（双源去重核心）：rotationAuth 路径同一 fatal 既
// 触发 WithOnAuthFatal 又随返回错误 errors.As 命中——**以回调为准去重、单次
// 上报**（CAS 胜者上报；败者并发调用/errors.As 补报路径跳过）。上报后失效
// 剔除（联动——账号已判死，缓存条目随弃，管理面恢复后重建）。
func (a *Codex) reportFatal(e *codexEntry, fatal error) {
	a.report(e, fatal, true)
}

// FatalAuth 显式终止 + 单次上报（§3——WS 业务判死事件帧接线，relay 解析
// 帧后调用；唯一跨边界点）：
//   - e.auth.Fatal(fatal)：SDK 显式终止——**不触发 OnAuthFatal**（实证
//     auth_oauth.go:187-195），仅毒化 Auth（后续 Authorization 恒返回该错误）
//   - 上报走 report(e, fatal, false)：与 errors.As 路径共享 CAS 双源去重
//     （帧判死后同一 fatal 再经 errors.As 二次命中 → 仍单次上报）；
//     **不剔除**——毒化 Auth 保留至外部凭据变更（管理面重新导入 → sig 变化
//     重建；与"不重建缓存"裁决一致——剔除会丢毒化态，凭据未变重建后仍走
//     旧 token）
//
// 条目不存在（未构造/并发 fatal 已上报剔除）→ no-op（无 Auth 可毒化；上报
// 已由并发胜者完成，账号已走失效链）。
func (a *Codex) FatalAuth(accountID int64, fatal error) {
	if fatal == nil {
		return // 防御：无错误不上报
	}
	a.mu.Lock()
	e := a.entries[accountID]
	a.mu.Unlock()
	if e == nil {
		return // 并发 fatal 已上报剔除 / 未构造：无 Auth 可毒化，上报已由胜者完成
	}
	e.auth.Fatal(fatal)
	a.report(e, fatal, false)
}

// rotateWriteback 轮转回写（§1——SDK OnTokenRotated 回调；在 SDK 单飞内
// 串行执行——同账号并发轮转天然单飞，无需额外互斥）：
//   - account_ext 部分更新 upsert（codex_oauth_token + codex_oauth_refresh_token +
//     codex_oauth_expires_at 保旧——携带 e.expiresAt 构造时旧值）
//   - 失败 → panic（SDK 契约：回调失败 = 令牌持久化中断信号——callRotate
//     recover 后记 pending 下次 refresh 前重试，连续达阈值 →
//     CallbackDeliveryError fatal → 统一回调摘除；fail-closed）
//   - 成功后失效调度器 AccountExt 内存快照条目（下个会话重载新凭据；
//     失效失败仅 Warn 不阻断——令牌已落库，适配层 Auth 内存新 at 自愈）
//   - **不重建缓存**：回调写回的是本 Auth 内部已更新的状态（at 缓存/rt 轮换
//     已在 SDK 内生效），重建丢 at 缓存破坏轮转连续性；仅外部凭据变更重建
//     （机制——sig 比对）
//   - **pending 竞态（接受）**：适配层重建缓存后旧 Auth 在途 401 →
//     deliverPendingRotate 可能写回旧轮转结果——旧 rt 已吊销则 refresh 判死
//     正确摘除，基本自愈；低概率，不额外防护
//
// 回调在 SDK 单飞内阻塞并发等待者——必须快速返回（本地 upsert 毫秒级）；
// 用固定短超时兜底 PG 故障（超时 → 重试链接管）。
func (a *Codex) rotateWriteback(e *codexEntry, at, rt string) {
	if a.rotate == nil {
		return // 未装配（测试形态）：no-op
	}
	if at == "" || rt == "" {
		return // 盲写防御：SDK 已保证非空（响应缺 refresh 时回调收旧 rt），双保险
	}
	ctx, cancel := context.WithTimeout(context.Background(), rotateWritebackTimeout)
	defer cancel()
	if err := a.rotate.WriteOAuthRotation(ctx, e.accountID, at, rt, e.expiresAt); err != nil {
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

// evict 失效剔除（缓存条目摘除 + 其名下 WS 空闲连接关闭；不存在 = no-op——
// 并发/重复上报安全）。
//
// 关闭必须在**锁外**（G4）：条目一旦从表里消失就再没人会复用它，但它的 WS
// 连接还挂在上游——不主动关就白占一个 socket 直到 TTL/maxAge 到期。terminate
// 可能在写闸上阻塞，锁内调用会把整张 client 表卡住。
func (a *Codex) evict(accountID int64) {
	a.mu.Lock()
	e := a.entries[accountID]
	delete(a.entries, accountID)
	var dropped []*codexsdk.HTTPClient
	if e != nil {
		dropped = e.takeClients()
	}
	a.mu.Unlock()
	closeWSIdleConns(dropped)
}

// translateError SDK 错误 → 网关侧错误翻译（错误契约）：
//   - fatal 五类（errors.As——RefreshOAuthError / AuthPermanentlyRevokedError /
//     AccountDisabledError / CallbackDeliveryError）→ 双源去重单次上报（回调
//     路径 CAS 已胜出则此处跳过；PAT/无回调路径此处补报）——原样透传不包装
//     （SDK 已保证 errors.As 可命中）
//   - RefreshError → 不上报（可重试——对齐 SDK 语义 auth_errors.go:53-58，
//     网关按既有 failover 分类处理）
//   - *HTTPError → 信封包装（EnvelopeError：StatusCode()/RawJSON()/Unwrap 链）
//   - 其余（网络/解析等）原样透传（code 0 连接级分类）
//
// 背压（codexsdk.ErrWSBackpressure）是**传输层**错误而非协议终态：上游没坏、
// 下游消费太慢。sentinel 换 sentinel（不按字符串匹配），并保留原始错误链——
// `errors.Is(mapped, domain.ErrUpstreamBackpressure)` 与
// `errors.Is(mapped, codexsdk.ErrWSBackpressure)` 同时为真，调用方既能按网关
// 语义分类（不误判成网络失败/鉴权失败去重试与冷却账号），又能回溯 SDK 原始面。
// 包装风格与 repository/usage_repo.go:86 的 ErrPartitionUnavailable 一致。
func (a *Codex) translateError(e *codexEntry, err error) error {
	if errors.Is(err, codexsdk.ErrWSBackpressure) {
		return fmt.Errorf("%w: %w", domain.ErrUpstreamBackpressure, err)
	}
	if f := asFatal(err); f != nil {
		// 双源去重（与 reportFatal 同语义，直接复用）：
		// CAS 在回调路径已胜出则此处跳过（单次上报）；PAT/无回调路径此处补报
		a.reportFatal(e, f)
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
