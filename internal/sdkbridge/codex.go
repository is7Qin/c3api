// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	codexsdk "github.com/is7Qin/codex-sdk"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// Codex 是 codex SDK 适配层（T2 §1——SDK 调用集中于此，codexsdk import 仅限
// 本文件族）：cred → Auth 账号级缓存 + GenerateImage 包装 + 信封包装 +
// fatal → 统一回调（双源去重）+ 轮转回写（T5）。新增能力（T3 流式 / T4 Dial /
// T6 resp）同形态扩展本文件。
type Codex struct {
	mu      sync.Mutex
	entries map[int64]*codexEntry // accountID → 客户端缓存（同账号复用）
	failure FailureHandler        // T1 统一失效回调；nil = no-op（测试/未装配）
	rotate  RotationStore         // T5 轮转回写落库面；nil = 不落库（测试/未装配）
	inval   func(accountID int64) // T5 P3-3 回写后失效账号快照条目（下个会话重载新凭据）；nil = 不失效
	log     *logx.Logger          // T5 回写/失效错误日志；nil = 不记
	// transport SDK HTTPClient 上游 transport（resp HTTP 面连接池形态；nil =
	// SDK 默认——MaxIdleConnsPerHost=2，补压测连接风暴根因）。装配点见
	// SetTransport（main 注入 httpx 网关同形态 transport）。
	transport http.RoundTripper
}

// SetTransport 装配 SDK HTTPClient 的上游 transport（resp 补压测修复——SDK
// 默认 transport MaxIdleConnsPerHost=2，压测 profile ~12% CPU 连接风暴；main
// 装配 httpx.NewTransport(网关同形态连接池参数)。构造期一次（冷面），热路径
// 零影响；nil = SDK 默认（测试形态）。httpx 默认 Proxy=nil 直连（C2-1 防劫持
// ——环境代理不静默改道 SDK 上游请求，main 装配传 nil 同网关既有 client）。
func (a *Codex) SetTransport(rt http.RoundTripper) {
	a.transport = rt
}

// RotationStore 轮转回写落库面（repository.AccountExtRepo 满足；接口化供测试
// 注入与装配侧解耦）。部分更新 upsert——仅 oauth_token/oauth_refresh_token/
// oauth_expires_at（expiresAt 为调用方携带的旧值，保旧语义），其余列不动。
type RotationStore interface {
	WriteOAuthRotation(ctx context.Context, accountID int64, at, rt string, expiresAt *time.Time) error
}

// RotationDeps 轮转回写依赖（T5 §1——main 装配：repository.AccountExts +
// scheduler）。
type RotationDeps struct {
	Store RotationStore
	// InvalidateSnapshot 回写成功后失效调度器 AccountExt 内存快照对应条目
	// （P3-3——下个会话重载新凭据）；nil = 不失效（测试/未装配）。
	InvalidateSnapshot func(accountID int64)
	// Log 回写/失效错误日志（旋转低频事件，错误恒 Warn 记一条）；nil = 不记。
	Log *logx.Logger
}

// SetRotationDeps 装配轮转回写面（T5 §1；Store nil = 回调不落库——测试形态）。
// 与 failure 回调（构造时注册）分离：回写面冷面低频，main 装配点独立。
func (a *Codex) SetRotationDeps(deps RotationDeps) {
	a.rotate = deps.Store
	a.inval = deps.InvalidateSnapshot
	a.log = deps.Log
}

// codexEntry 单账号缓存条目：Auth（HTTP/WS 双面共享——at 缓存/单飞/rt 轮换
// 在 SDK Auth 内）+ HTTPClient（HTTP 面懒构造，nil = 未构造）+ 重建判定签名
// + fatal 已上报标记（双源去重——回调路径与 errors.As 路径共享同一 CAS）。
// expiresAt 为构造时凭据携带的旧过期时刻（T5——轮转回调无 expiry，回写保旧
// 用；外部凭据变更 → 重建刷新）。
type codexEntry struct {
	accountID int64
	auth      codexsdk.Auth
	client    *codexsdk.HTTPClient
	sig       string // 凭据签名（外部凭据变更 → 重建）
	// idSig 伪装身份签名（META-2：identity 变化 → 重建 HTTPClient——与 cred
	// sig 同语义：账号配置变更 → 重建；WS 面每请求新鲜 identity，HTTP 面缓存
	// 客户端以 sig 比对等价对齐——同 identity 复用连接池，变化才重建）。
	idSig string
	// turnState HTTP 面 turn-state 持有（HOST-2——spec 2026-08-15 评审 PASS）：
	// 上游响应签发值（HTTPResponse.TurnState / SDK 池级捕获值回读），后续请求
	// 注入 x-codex-turn-state 头（同轮回传对齐真实 codex client.rs:1202——
	// 轮级实例实证：ModelClientSession.new_session 每轮新建 OnceLock，
	// responses_websocket.rs:538 握手响应头 + :742 流事件二次 set 一次定值）。
	// 粒度 = clientFor 缓存一致（账号级——HTTP 面 sig 缓存客户端）；轮结束由
	// 网关 ClearTurnState 清除（跨轮不回传）。
	turnState string
	// appliedTurnState 当前客户端构造期已应用的 turn-state（重建判定：变化 →
	// 重建 HTTPClient——与 idSig 同语义；生产路径共享 transport 承载连接池，
	// 重建不重置连接池）。
	appliedTurnState string
	expiresAt         *time.Time
	reported          atomic.Bool
	// usage 额度快照缓存（Task 3——GetUsageSnapshot 5min TTL）+ 失败冷却起点
	// （60s——gate Major 2）：重建（sig 变化）随新条目一并清除；usageErr 只存
	// 分类哨兵（ErrAuthExpired/ErrUpstream），不缓存错误体。
	usage      *domain.CodexUsageSnapshot
	usageAt    time.Time
	usageErrAt time.Time
	usageErr   error
}

// NewCodex 构造 codex 适配层。failure 为 T1 统一失效回调（适配层构造注册
// WithOnAuthFatal → 回调；nil = 上报 no-op——测试替身形态）。
func NewCodex(failure FailureHandler) *Codex {
	return &Codex{failure: failure, entries: make(map[int64]*codexEntry)}
}

// GenerateImage 非流式生图包装（T2 §1）：cred → 缓存取 HTTPClient →
// c.GenerateImage(ctx, toSDKParams(p))；domain↔codexsdk 双向转换集中本文件。
// 错误翻译（translateError）：SDK *HTTPError → 网关侧信封错误（EnvelopeError——
// StatusCode()/RawJSON()/Unwrap 链，网关 statusOf/upstreamErrMsg 零改动复用）；
// fatal 五类（errors.As）→ 统一回调单次上报（双源去重）+ 原样透传（SDK 已
// 不包装，errors.As 可命中）；RefreshError 可重试不上报。cred.BaseURL = 模板
// base 派生完整 generations 端点（空 → SDK 内置 DefaultImagesURL）。
func (a *Codex) GenerateImage(ctx context.Context, cred *domain.AccountCredential, p *domain.ImageGenParams) (*domain.ImageResponse, error) {
	e, err := a.clientFor(cred, nil, nil, "")
	if err != nil {
		return nil, err
	}
	resp, err := e.client.GenerateImage(ctx, toSDKParams(p))
	if err != nil {
		return nil, a.translateError(e, err)
	}
	return fromSDKResponse(resp), nil
}

// GenerateImageStream 流式生图包装（T3 生产接线——合成流式：SDK 内部非流式
// 调 + 等待期 keepalive 合成 + completed 逐张合成，网关零合成逻辑）：cred →
// 缓存取 HTTPClient → c.GenerateImageStream(ctx, toSDKParams(p), fn)；事件翻
// 译 codexsdk.ImageStreamEvent → domain.ImageStreamEvent（Type/B64JSON/Usage
// 逐字段映射——usage 平铺直透，网关 completed 帧 JSON tag 直透同一口径）；
// 错误翻译同 GenerateImage（translateError——信封/fatal 统一回调/refresh 分
// 类复用；fn 回调错误经 translateError 原样透传——非 SDK 错误不过滤）。
func (a *Codex) GenerateImageStream(ctx context.Context, cred *domain.AccountCredential, p *domain.ImageGenParams, fn func(domain.ImageStreamEvent) error) error {
	e, err := a.clientFor(cred, nil, nil, "")
	if err != nil {
		return err
	}
	err = e.client.GenerateImageStream(ctx, toSDKParams(p), func(ev codexsdk.ImageStreamEvent) error {
		// 事件类型显式映射（A-P2-10）：SDK 升级改事件名 → 未知 Warn + 跳过
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
		// 与 GenerateImage 同款（评审 P1-1 修复）：SDK *HTTPError（字段裸类型，无
		// StatusCode()/RawJSON() 方法）→ EnvelopeError 包装——网关 statusOf/
		// upstreamBody/streamErrMessage 的协议才能消费（4xx 状态 + 原始 body
		// 透传、SSE error 帧 message 取上游文案）；fatal 五类统一回调单次上报
		// + 原样透传。fn 回调错误（网关写入失败/客户端断开）非 SDK 错误 →
		// translateError 原样透传（不过滤）。
		return a.translateError(e, err)
	}
	return nil
}

// Responses 非流式 responses 合成调用（T6 §1）：cred → 缓存取 HTTPClient
// （clientFor——T2 机制复用）→ c.Responses(ctx, payload)（SDK 合成非流式——
// 内部无条件 stream:true + SSE 事件聚合重组完整响应体；网关以非流式语义消费，
// 原样转发 + 顶层 usage 提取）。sess/meta 为 HTTP 面伪装身份（META-2——
// client_metadata 注入键集对齐真实 codex；nil = 未配置——SDK 仍恒带 turn_id，
// spec META-1）。clientTurnState 为客户端请求自带 x-codex-turn-state（HOST-2
// 透传优先——客户端自管，非空覆盖 held 注入）；未带 → 注入 held（上游签发值
// ——同轮回传）。成功响应签发值 → held 回写（非空才覆盖）。错误翻译同
// GenerateImage（translateError——SDK *HTTPError → 信封；fatal 五类统一回调
// 双源去重；RefreshError/网络原样）。
func (a *Codex) Responses(ctx context.Context, cred *domain.AccountCredential, payload []byte, sess *codexsdk.Session, meta *codexsdk.CodexMeta, clientTurnState string) (*codexsdk.HTTPResponse, error) {
	ts := clientTurnState
	if ts == "" {
		ts = a.turnStateOf(cred.AccountID)
	}
	e, err := a.clientFor(cred, sess, meta, ts)
	if err != nil {
		return nil, err
	}
	resp, err := e.client.Responses(ctx, payload)
	if err != nil {
		return nil, a.translateError(e, err)
	}
	a.captureTurnState(e, resp.TurnState)
	return resp, nil
}

// Search codex search 端点透传调用（spec 2026-08-13）：cred → 缓存取
// HTTPClient（clientFor——统一 client 形态直接复用，无独立实例问题）→
// e.client.Search(ctx, payload)（URL 方法内派生：baseURL（responses 完整端
// 点）尾段 /responses → /alpha/search——网关零拼装；请求/响应体 opaque 零解
// 析——alpha 端点实验性，上游变更网关免疫）。**无头注入**（x-codex-turn-
// metadata 统一不转发——与 resp HTTP 路径现状一致；SDK Search 默认头面无该
// 头）。错误翻译同 Responses（translateError——信封/fatal 统一回调双源去重/
// RefreshError 分类复用）。
func (a *Codex) Search(ctx context.Context, cred *domain.AccountCredential, payload []byte) (*codexsdk.HTTPResponse, error) {
	e, err := a.clientFor(cred, nil, nil, "")
	if err != nil {
		return nil, err
	}
	resp, err := e.client.Search(ctx, payload)
	if err != nil {
		return nil, a.translateError(e, err)
	}
	return resp, nil
}

// StreamResponses 流式 responses SSE 透传（T6 §1）：cred → 缓存取 HTTPClient →
// c.Stream(ctx, payload, fn)（SSE data: 行逐帧交付零拷贝——SDK 回调 raw 指向
// scanner 复用缓冲，**仅回调执行期间有效**：fn 必须立即消费，不得跨回调保留
// 切片）。sess/meta 同 Responses（META-2 伪装身份；nil = 未配置——SDK 仍恒带
// turn_id）。clientTurnState 同 Responses（HOST-2 透传优先）。流式无
// HTTPResponse 返回面——签发值回读 SDK 池级捕获（Stream 内部已
// captureTurnState，http.go:144——2xx 响应头非空才覆盖）→ held 回写。错误翻译
// 同 Responses。fn 返回错误 → SDK 终止读取并原样透传（网关写出失败/客户端断
// 开路径——translateError 对非 SDK 错误不过滤）。
func (a *Codex) StreamResponses(ctx context.Context, cred *domain.AccountCredential, payload []byte, sess *codexsdk.Session, meta *codexsdk.CodexMeta, clientTurnState string, fn func(raw []byte) error) error {
	ts := clientTurnState
	if ts == "" {
		ts = a.turnStateOf(cred.AccountID)
	}
	e, err := a.clientFor(cred, sess, meta, ts)
	if err != nil {
		return err
	}
	if err := e.client.Stream(ctx, payload, fn); err != nil {
		return a.translateError(e, err)
	}
	a.captureTurnState(e, e.client.TurnState())
	return nil
}

// entryFor cred → 账号级缓存条目（构造冷面——每账号首次/凭据变更后；互斥锁
// + 签名比对，同账号并发请求单飞构造——对齐 SDK OAuth 单飞 refresh 语义）：
//   - 同账号复用（Auth 内 at 缓存/轮转状态保持；sig 相同直接返回）
//   - 仅外部凭据变更（管理面导入/更新——token/rt/pat/base URL 任一变化 → sig
//     不同）后重建；**轮转回调写回不重建**（回调写回的是本 Auth 内部已更新
//     的状态，重建丢 at 缓存破坏轮转连续性——写回走 T5 管理面通道，不经缓存）
//   - 失效剔除（T1 联动）：fatal 上报后 evict，恢复后重建
//   - 空 rt 防护（P2-3）：codex-oauth 缺 refresh_token → 按失效上报（账号凭据
//     不完整）不 panic（OAuthWithRotation 空 rt 构造 panic）；PAT 走 PAT(key)
//     无此面
//   - 重建 = 新条目构造——usage/usageAt/usageErrAt/usageErr 一并清除（对齐
//     auth 重建；Task 3：凭据变更后快照重拉）
//
// 条目承载 Auth（HTTP 面 GenerateImage/Stream 与 WS 面 Dial 共享——连接
// per-请求不缓存，Auth 账号级状态跨面复用；HTTPClient 由 clientFor 懒构造）。
func (a *Codex) entryFor(cred *domain.AccountCredential) (*codexEntry, error) {
	sig := credSig(cred)
	a.mu.Lock()
	if e := a.entries[cred.AccountID]; e != nil && e.sig == sig {
		a.mu.Unlock()
		return e, nil
	}
	e := &codexEntry{accountID: cred.AccountID, sig: sig}
	auth, err := a.buildAuth(cred, e)
	if err == nil {
		e.auth = auth
		a.entries[cred.AccountID] = e
	}
	a.mu.Unlock()
	if err != nil {
		// 构造失败（空 rt 等——P2-3）：锁外上报（reportFatal → evict 需取
		// a.mu——锁内调用即重入死锁；sync.Mutex 不可重入）。
		a.reportFatal(e, err)
		return nil, err
	}
	return e, nil
}

// clientFor entryFor + HTTPClient 懒构造（GenerateImage/Stream 面——非 nil
// 后同账号复用连接池；sig 变更 → entryFor 重建条目）。sess/meta 为 HTTP 面伪
// 装身份（META-2——SDK 注入点读构造期 opts（http.go injectResponsesClient-
// Metadata），须随构造下发；nil = 未配置）。turnState 为本次请求生效的
// turn-state（HOST-2——客户端自带透传优先值或 held 签发值；非空 → 构造期
// WithHeader 注入 x-codex-turn-state——SDK HTTPClient 无 per-request 头面，
// 与 idSig 同语义：值变化 → 重建客户端；生产路径共享 transport 承载连接池，
// 重建不重置连接池（补压测 4e08fbd 连接风暴防护保持））。NewHTTPClient 为纯
// 构造（无 I/O 无 error）；构造/读取全程持 a.mu——entryFor 每次调用已取锁，
// 无新增竞争（补压测 -race 实证修复：原双检锁锁外读 e.client vs 锁内写，数
// 据竞争；去掉锁外快路径即消除）。idSig 比对（identitySig）——identity 变化
// → 重建客户端（与 cred sig 同语义：账号配置变更 → 重建；同 identity 复用连
// 接池）。
func (a *Codex) clientFor(cred *domain.AccountCredential, sess *codexsdk.Session, meta *codexsdk.CodexMeta, turnState string) (*codexEntry, error) {
	e, err := a.entryFor(cred)
	if err != nil {
		return nil, err
	}
	var opts []codexsdk.Option
	if cred.BaseURL != "" {
		opts = append(opts, codexsdk.WithBaseURL(cred.BaseURL))
	}
	if a.transport != nil {
		opts = append(opts, codexsdk.WithTransport(a.transport))
	}
	if turnState != "" {
		opts = append(opts, codexsdk.WithHeader(codexsdk.HeaderTurnState, turnState))
	}
	opts = append(opts, identityOpts(sess, meta)...)
	sig := identitySig(sess, meta)
	a.mu.Lock()
	if e.client == nil || e.idSig != sig || e.appliedTurnState != turnState {
		e.client = codexsdk.NewHTTPClient(e.auth, opts...)
		e.idSig = sig
		e.appliedTurnState = turnState
	}
	a.mu.Unlock()
	return e, nil
}

// turnStateOf 读账号 held turn-state（HOST-2 生效值判定——客户端未自带时注入
// 面；a.mu 保护与 entryFor 同锁，热路径每请求一次，锁开销可忽略）。
func (a *Codex) turnStateOf(accountID int64) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.entries[accountID]; e != nil {
		return e.turnState
	}
	return ""
}

// captureTurnState 响应签发值 → held 回写（非空才覆盖——对齐 SDK
// captureTurnState 语义；HTTPResponse.TurnState / 池级捕获值为最近签发值——
// 响应未再签发时保持旧值，重复回写幂等）。
func (a *Codex) captureTurnState(e *codexEntry, ts string) {
	if ts == "" {
		return
	}
	a.mu.Lock()
	e.turnState = ts
	a.mu.Unlock()
}

// ClearTurnState 轮结束清除（HOST-2——网关在响应含轮结束信号时调用：跨轮不得
// 回传）。条目不存在 = no-op（未构造/失效剔除）。清除后下次请求生效值 ""
// → 客户端重建（无头）——SDK 池级旧值随重建丢弃，不残留。
func (a *Codex) ClearTurnState(accountID int64) {
	a.mu.Lock()
	if e := a.entries[accountID]; e != nil {
		e.turnState = ""
	}
	a.mu.Unlock()
}

// —— Task 3：codex 额度快照（GetUsageSnapshot——TTL 缓存 + 有界并发 + 失败冷却） ——

// ErrAuthExpired 凭据失效分类错误（GetUsageSnapshot 错误面——IsFatal 纯判定
// 零副作用：usage 查询面不重复上报不摘除——凭据失效由会话路径（Dial/
// Responses）经 FatalAuth 上报，防循环；凭据变更后 entry sig 重建自然恢复。
// task 2 upstream_error 映射输入）。
var ErrAuthExpired = errors.New("codex: usage auth expired")

// ErrUpstream 上游错误分类错误（GetUsageSnapshot 错误面——*HTTPError/网络/
// RefreshError 等一律保守归本类；task 2 upstream_error 映射输入）。
var ErrUpstream = errors.New("codex: usage upstream error")

// usageFetchSem 快照拉取包级 semaphore（容量 8——所有调用面共享节流：管理面
// 几百账号懒加载 + 未来路由面共用同一上限，防 OpenAI 429；只限并发不限速率
// ——速率上限由失败冷却封顶）。
// 测试勿 t.Parallel（包级状态串扰——测试可调包级 var/共享 semaphore）。
var usageFetchSem = make(chan struct{}, usageFetchConcurrency)

const usageFetchConcurrency = 8

// usageSnapshotTTL 快照 TTL（5min 慢变量——上游分钟级更新；滚动查看零上游）。
// var（非 const）——测试注入可调值。测试勿 t.Parallel（包级状态串扰）。
var usageSnapshotTTL = 5 * time.Minute

// usageCooldown 失败冷却（60s——冷却内直接返回分类错误零上游，封顶重试率
// ≤1 次/账号/分钟；不缓存错误体，冷却后重试）。测试勿 t.Parallel（包级状态
// 串扰）。
var usageCooldown = 60 * time.Second

// GetUsageSnapshot 账号 codex 额度快照（ChatGPT 面 wham/usage）：cred →
// 缓存条目（entryFor——sig 比对/重建，重建时 usage/usageAt/usageErrAt 随新
// 条目一并清除）→ 5min TTL 命中直接返回（零上游）；未命中 → 包级 semaphore
// （容量 8，有界并发）→ SDK GetUsage（e.client 复用——BaseURL 已带，usage
// 端点 SDK 内部派生，网关零拼装）→ 白名单收敛映射（fromSDKUsage）→ 写
// e.usage + e.usageAt。失败 → 写 e.usageErrAt（60s 冷却）。
//
// 错误分类**纯判定零副作用**（gate Major 3——不复用 translateError：其 fatal
// 分支 reportFatal → evict 删 entry，冷却随 entry 消失）：IsFatal（fatal 五类
// 穿透信封链）→ ErrAuthExpired；HTTPError 401（PAT 死 token——SDK 对 PAT 不
// 判死不轮转原样返回，语义 = 凭据失效）→ ErrAuthExpired；其余（*HTTPError/
// 网络/RefreshError）保守归 ErrUpstream。usage 是查询面不是会话面——凭据失效
// 由会话路径上报（防循环），entry 恒保留（fatal 后后续调用仍命中冷却）。
// 命中路径零分配（T3-4）：先按 accountID 查 entry.usage（map 查 + 锁 + 时间
// 比较——不计算 credSig 字符串拼接/不重建条目），命中直接返回；未命中才走
// clientFor（entryFor 签名比对/重建）。
func (a *Codex) GetUsageSnapshot(ctx context.Context, cred *domain.AccountCredential) (*domain.CodexUsageSnapshot, error) {
	if s, ok := a.snapshotCachedFor(cred.AccountID); ok {
		return s, nil
	}
	e, err := a.clientFor(cred, nil, nil, "")
	if err != nil {
		// 入口错误分类（N2）：oauth 缺 rt 凭据不完整（errCredentialIncomplete）
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
	case usageFetchSem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-usageFetchSem }()
	// 双检：semaphore 等待期间并发请求可能已拉取成功/失败（TTL/冷却语义保持
	// ——不重复拉取、不重复报错）。
	if s, ok := a.snapshotCached(e); ok {
		return s, nil
	}
	if err := a.snapshotCooldown(e); err != nil {
		return nil, err
	}
	resp, err := e.client.GetUsage(ctx)
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
	// 成功清冷却态（T3-2——死状态不留：哨兵仅在 usageErrAt 新鲜时被读取，但
	// 状态机卫生——检查顺序调整不误服旧哨兵）。
	e.usageErrAt = time.Time{}
	e.usageErr = nil
	a.mu.Unlock()
	return s, nil
}

// snapshotCachedFor 命中路径零分配快查（T3-4）：按 accountID 查 entry + TTL
// 判定——命中直接返回缓存实例，不经过 clientFor/credSig（字符串拼接）/
// entryFor（重建判定）。未命中/无 entry → nil（走完整路径）。
func (a *Codex) snapshotCachedFor(accountID int64) (*domain.CodexUsageSnapshot, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e := a.entries[accountID]; e != nil && e.usage != nil && time.Since(e.usageAt) < usageSnapshotTTL {
		return e.usage, true
	}
	return nil, false
}

// snapshotCached 快照 TTL 命中判定（5min 慢变量；e.usage nil = 未拉取过）。
func (a *Codex) snapshotCached(e *codexEntry) (*domain.CodexUsageSnapshot, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if e.usage != nil && time.Since(e.usageAt) < usageSnapshotTTL {
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
	if !e.usageErrAt.IsZero() && time.Since(e.usageErrAt) < usageCooldown {
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

// classifyUsageErr 错误分类纯判定（gate Major 3——零副作用不上报不摘除）：
// IsFatal（fatal 五类穿透信封链——RefreshOAuthError/AuthPermanentlyRevoked-
// Error/AccountDisabledError/CallbackDeliveryError）→ ErrAuthExpired；
// *HTTPError 401 特判（T3-5——PAT 死 token：SDK 对 PAT 不判死不轮转原样返回
// HTTPError，语义 = 凭据失效非上游故障）→ ErrAuthExpired；其余（*HTTPError/
// 网络/RefreshError 等）保守归 ErrUpstream。
func classifyUsageErr(err error) error {
	if IsFatal(err) {
		return ErrAuthExpired
	}
	var he *codexsdk.HTTPError
	if errors.As(err, &he) && he.StatusCode == http.StatusUnauthorized {
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

// identityOpts 伪装身份 → SDK Option（META-2：WithSession/WithCodexMeta——
// SDK HTTP 面注入键集对齐真实 codex client_metadata()；nil = 未配置 = 现状
// 零注入面，SDK 仍恒带 turn_id——spec META-1）。
func identityOpts(sess *codexsdk.Session, meta *codexsdk.CodexMeta) []codexsdk.Option {
	var opts []codexsdk.Option
	if sess != nil {
		opts = append(opts, codexsdk.WithSession(*sess))
	}
	if meta != nil {
		opts = append(opts, codexsdk.WithCodexMeta(*meta))
	}
	return opts
}

// identitySig 伪装身份签名（客户端重建判定——HTTPClient 构造期 opts 承载，
// 变化必须重建才能生效；与 credSig 同约定：\x00 分隔——身份值为 UUID/URI 字
// 符集，不含控制字符）。nil 与全空等价（均不注入 → ""）——proxy 恒传
// codexIdentityFromExt 产物（缺列 = 全空），与测试/未配置路径（nil）同签名，
// 不引发无谓重建。
func identitySig(sess *codexsdk.Session, meta *codexsdk.CodexMeta) string {
	if (meta == nil || *meta == (codexsdk.CodexMeta{})) &&
		(sess == nil || *sess == (codexsdk.Session{})) {
		return ""
	}
	var b strings.Builder
	if meta != nil {
		b.WriteString(meta.InstallationID)
		b.WriteByte(0)
		b.WriteString(meta.SessionID)
		b.WriteByte(0)
		b.WriteString(meta.ThreadID)
		b.WriteByte(0)
		b.WriteString(meta.WindowID)
		b.WriteByte(0)
		b.WriteString(meta.Subagent)
		b.WriteByte(0)
		b.WriteString(meta.ParentThreadID)
		b.WriteByte(0)
		b.WriteString(meta.ParentTurnID)
		b.WriteByte(0)
		b.WriteString(meta.TurnMetadata)
	}
	b.WriteByte(0x1e) // meta/session 段分隔
	if sess != nil {
		b.WriteString(sess.SessionID)
		b.WriteByte(0)
		b.WriteString(sess.ThreadID)
		b.WriteByte(0)
		b.WriteString(sess.WindowID)
		b.WriteByte(0)
		b.WriteString(sess.ClientRequestID)
	}
	return b.String()
}

// Dial 建立到上游的 Responses WebSocket 连接（T4 §2 接线）：cred → Auth 缓存
// 取（entryFor——账号级长存：at 缓存/单飞/rt 轮换在 Auth 内，与 HTTP 面共享；
// WS 连接本身 per-请求不缓存）→ codexsdk.Dial(ctx, auth, opts...)。opts 由
// 网关侧组装（codex_responses_ws.go——伪装四元组 / WithPingInterval(0) /
// WithPayloadFiltering(false) / 透传头）；cred.BaseURL 由本方法应用（完整
// responses 端点——P3-1，与 clientFor 同款）。错误翻译（translateDialError）：
//   - *DialError → 信封包装（EnvelopeError——StatusCode()/RawJSON()/Unwrap
//     链，Refreshed 语义保留：已轮转重连一次仍失败 → 网关避免双份刷新）
//   - 裸错误（Dial 401 轮转路径 refresh 失败——client.go:391-394 透传，不包
//     DialError）→ translateError 既有双分支：fatal 类（RefreshOAuthError /
//     AccountDisabledError）→ 统一回调单次上报 + 原样透传（网关"该请求不转
//     移"，IsFatal 判定）；RefreshError/网络 → 原样透传（网关正常 failover）
func (a *Codex) Dial(ctx context.Context, cred *domain.AccountCredential, opts ...codexsdk.Option) (*codexsdk.Client, error) {
	e, err := a.entryFor(cred)
	if err != nil {
		return nil, err
	}
	// cred.BaseURL 由调用方路由填充（T4：aiclient fullURLOf 派生完整 responses
	// 端点——P3-1 完整端点语义；空 → SDK 内置 DefaultResponsesURL）。与
	// clientFor（HTTP 面）同款：适配层统一应用，调用方不再重复传 WithBaseURL。
	if cred.BaseURL != "" {
		opts = append(opts, codexsdk.WithBaseURL(cred.BaseURL))
	}
	c, err := codexsdk.Dial(ctx, e.auth, opts...)
	if err != nil {
		return nil, a.translateDialError(e, err)
	}
	return c, nil
}

// translateDialError Dial 错误翻译（T4 §5 错误契约）：*DialError → 信封
// （Refreshed 保留）；其余（refresh 失败裸错误）复用 translateError 双分支
// （fatal → 统一回调 + 原样；RefreshError/网络 → 原样 → 网关 failover 分类）。
func (a *Codex) translateDialError(e *codexEntry, err error) error {
	var de *codexsdk.DialError
	if errors.As(err, &de) {
		env := NewEnvelopeError(de.StatusCode, "", de)
		env.Refreshed = de.Refreshed
		return env
	}
	return a.translateError(e, err)
}

// buildAuth 按 cred 构造 SDK Auth（构造前校验——P2-3）：
//   - codex-oauth：OAuthWithRotation(rt, WithOnAuthFatal(统一回调) [,
//     WithInitialAccessToken(at)])——过期判定在网关侧构造前：OAuthExpiresAt 已
//     过期 → 不传 WithInitialAccessToken（SDK 走初始 at 缺省路径，首请求前用
//     rt 换取——auth_oauth.go:106-109 只判非空不判过期，401 自愈）；未过期/
//     未知（nil）→ 预置单参 at 避免首调用强制 refresh
//   - WithOnTokenRotated（T5 §1）：每次 refresh 成功产出新 at+rt → account_ext
//     部分更新回写（幂等；回调在 SDK 单飞内串行——同账号并发轮转不重复回写）
//   - codex-pat：PAT(key)
//   - 空 rt（oauth 缺 refresh_token）→ 上报失效（凭据不完整）并返回错误，不
//     panic
func (a *Codex) buildAuth(cred *domain.AccountCredential, e *codexEntry) (codexsdk.Auth, error) {
	if cred.PATKey != "" {
		return codexsdk.PAT(cred.PATKey), nil
	}
	if cred.OAuthRefreshToken == "" {
		return nil, errCredentialIncomplete // 上报在 clientFor 锁外执行（见 clientFor）
	}
	e.expiresAt = cred.OAuthExpiresAt // T5 回写保旧（SDK 回调无 expiry）
	opts := []codexsdk.OAuthOption{
		// 统一回调装配（T2 §3）：SDK 判死（RT 判死码 / token 端点 401 / 账号
		// 禁用 / AT 401 判死 / 回调连续失败）→ 双源去重单次上报
		codexsdk.WithOnAuthFatal(func(fatal error) { a.reportFatal(e, fatal) }),
		codexsdk.WithOnTokenRotated(func(at, rt string) { a.rotateWriteback(e, at, rt) }),
	}
	if atUsable(cred) {
		opts = append(opts, codexsdk.WithInitialAccessToken(cred.OAuthToken))
	}
	return codexsdk.OAuthWithRotation(cred.OAuthRefreshToken, opts...), nil
}

// errCredentialIncomplete 凭据不完整（P2-3 构造前校验——oauth 类型缺
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

// credSig 凭据签名（重建判定）：外部凭据变更（管理面导入/更新——token/rt/pat/
// base URL 任一变化）→ 重建。过期时刻不参与签名（构造时的初始 at 预置决策已
// 经生效；过期 at 由 SDK 401 自愈轮转，无需重建）。
//
// 分隔符用 \x00（评审 P3-3）："|" 在理论上可被 token 内容携带（碰撞误重建——
// 仅多构造一次，无害但脏）；\x00 为 Go 字符串中不可现字符（OAuth token/PAT
// base64url 字符集、base URL 经 url.Parse 拒绝控制字符——构造前校验）。
func credSig(c *domain.AccountCredential) string {
	return c.OAuthToken + "\x00" + c.OAuthRefreshToken + "\x00" + c.PATKey + "\x00" + c.BaseURL
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
// 剔除（T1 联动——账号已判死，缓存条目随弃，管理面恢复后重建）。
func (a *Codex) reportFatal(e *codexEntry, fatal error) {
	a.report(e, fatal, true)
}

// FatalAuth 显式终止 + 单次上报（T5 §3——WS 业务判死事件帧接线，relay 解析
// 帧后调用；唯一跨边界点）：
//   - e.auth.Fatal(fatal)：SDK 显式终止——**不触发 OnAuthFatal**（实证
//     auth_oauth.go:187-195），仅毒化 Auth（后续 Authorization 恒返回该错误）
//   - 上报走 report(e, fatal, false)：与 errors.As 路径共享 CAS 双源去重
//     （帧判死后同一 fatal 再经 errors.As 二次命中 → 仍单次上报——P3-4）；
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

// rotateWriteback 轮转回写（T5 §1——SDK OnTokenRotated 回调；在 SDK 单飞内
// 串行执行——同账号并发轮转天然单飞，无需额外互斥）：
//   - account_ext 部分更新 upsert（oauth_token + oauth_refresh_token +
//     oauth_expires_at 保旧——携带 e.expiresAt 构造时旧值）
//   - 失败 → panic（SDK D4 契约：回调失败 = 令牌持久化中断信号——callRotate
//     recover 后记 pending 下次 refresh 前重试，连续达阈值 →
//     CallbackDeliveryError fatal → 统一回调摘除；fail-closed）
//   - 成功后失效调度器 AccountExt 内存快照条目（P3-3——下个会话重载新凭据；
//     失效失败仅 Warn 不阻断——令牌已落库，适配层 Auth 内存新 at 自愈）
//   - **不重建缓存**：回调写回的是本 Auth 内部已更新的状态（at 缓存/rt 轮换
//     已在 SDK 内生效），重建丢 at 缓存破坏轮转连续性；仅外部凭据变更重建
//     （T2 机制——sig 比对）
//   - **D4 pending 竞态（P3-5，接受）**：适配层重建缓存后旧 Auth 在途 401 →
//     deliverPendingRotate 可能写回旧轮转结果——旧 rt 已吊销则 refresh 判死
//     正确摘除，基本自愈；低概率，不额外防护
//
// 回调在 SDK 单飞内阻塞并发等待者——必须快速返回（本地 upsert 毫秒级）；
// 用固定短超时兜底 PG 故障（超时 → D4 重试链接管）。
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
			// 运维信号即时留痕（D4 重试至多 3 次各记一条——DB 故障持续期的
			// 真实告警；fatal 上报后账号摘除，告警自停）
			a.log.Warn("codex rotation writeback failed", logx.Int64("account_id", e.accountID), logx.Error(err))
		}
		// D4 契约：回调失败 → panic → SDK recover → pending 重试 → 达阈值
		// CallbackDeliveryError fatal（令牌无法持久化 = 账号失效信号）
		panic(err)
	}
	if a.inval != nil {
		a.inval(e.accountID)
	}
}

// rotateWritebackTimeout 轮转回写固定超时（回调在 SDK 单飞内阻塞并发等待
// 者——本地 upsert 毫秒级，3s 兜底 PG 故障；超时 → D4 重试链接管）。
const rotateWritebackTimeout = 3 * time.Second

// evict 失效剔除（缓存条目摘除；不存在 = no-op——并发/重复上报安全）。
func (a *Codex) evict(accountID int64) {
	a.mu.Lock()
	delete(a.entries, accountID)
	a.mu.Unlock()
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
func (a *Codex) translateError(e *codexEntry, err error) error {
	if f := asFatal(err); f != nil {
		// 双源去重（评审 P3-2——与 reportFatal 同语义，直接复用）：
		// CAS 在回调路径已胜出则此处跳过（单次上报）；PAT/无回调路径此处补报
		a.reportFatal(e, f)
		return err
	}
	var he *codexsdk.HTTPError
	if errors.As(err, &he) {
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

// IsFatal 网关侧 fatal 判定（T4 §5：Dial 裸错误 fatal 类 → 该请求不转移，由
// handleCodexDialError 调用）：与 asFatal 同构导出（errors.As 穿透信封链）。
func IsFatal(err error) bool { return asFatal(err) != nil }

// --- domain ↔ codexsdk 双向转换（集中本文件 + 转换单测防漂移） ---

// mapStreamEventType codexsdk 流式事件类型 → domain 类型化常量（A-P2-10 显式
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
