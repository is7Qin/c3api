// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"
	codexsdk "github.com/is7Qin/codex-sdk"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/sdkbridge"
)

// --- codex 独立 relay 变体（T4 §1：codex-oauth/codex-pat 类型 resp-ws 接线） ---
// 独立文件族（用户拍板文件边界：codex 相关处理不散落现有 caller/forward 文件，
// codexsdk import 仅限本文件族 + sdkbridge 扩展）。与 aiclient 路径同构的编
// 排（合一骨架 relayWS + 传输适配 codexTransport——用户裁决抽 5 方法传输接口
// wsRelayTransport，见 ws_relay.go）：双向帧透传 1:1 / usage 嗅探
// （response.completed——sniffResponsesCompleted 复用）/ 关闭分类
// （relayClassify/recordClose 复用）/ 心跳（30s Ping + 10s pong 超时同款）——
// 差异收口在本文件：传输适配层 = *codexsdk.Client 具体类型（Send/Recv/Ping/
// Close/CloseNow——SDK 具体类型经 codexTransport 适配接口）与每帧判死钩子
// frameHook（T5 §3，见 wsAttempt 调用点）；服务端 Accept 侧 = 既有
// *websocket.Conn 不动。
//
// 凭据双线分工（P2-B 定死）：relay 线 = 快照（sel.Ext → AccountExt）→
// AccountCredential 派生直供适配层（不经 credentialFor 单字符串路径——复合
// 凭据 oauth_token+refresh_token+expires_at+pat+accountID 单字符串契约表达不
// 了）。Registry 线（codex-oauth/codex-pat provider 注册 = 单令牌语义）仅供
// 既有单字符串消费面（未来 HTTP 面）——本轮**明确不注册**（无既有消费方；
// provider 无数据源可读单令牌，注册徒增死面）。

// errCodexWSNotIntegrated 501：codex 适配层未装配（SetCodex 未调用——main 装
// 配缺失的显式拒绝，不让凭据缺失路径误报 502/network；与 images 路径同款）。
var errCodexWSNotIntegrated = errors.New("codex responses unavailable (adapter not wired)")

// errCodexExtMissing codex 类型选号命中但账号快照缺 account_ext 行（配置损
// 坏——codex 账号必有 ext 行）。本地配置错误按连接级错误转移（失败文本落盘，
// 耗尽 502 语义）；不上报失效（避免 account 0 无谓上报——T2 P1-1 同款）。
var errCodexExtMissing = errors.New("codex account missing account_ext snapshot (config error)")

// codexAuthFailedMsg codex fatal 用户帧固定文案（M3 裁决：fatal 语义 = 授权
// 失败——复用 "upstream rejected request" 语义不准；不泄 SDK 内部机制串如
// "refresh 被拒绝"/重试次数）。落盘侧 ErrorMessage 仍写 dialErr 原文
// （:125-127），此路径无 Warn——落盘是唯一留痕。
const codexAuthFailedMsg = "codex authorization failed"

// dialCodexWS 组装一次 codex WS Dial（T4 §2——凭当前请求选中账号 cred）：
//   - 凭据线：sel.Ext 快照 → AccountCredential 派生（relay 线；热路径零 DB）
//   - WithBaseURL：**完整 responses 端点**（P3-1——SDK client.go:137-144 覆盖
//     值按完整端点直用，传裸根打 /v1 静默 404；复用 aiclient fullURLOf 的 URL
//     组装/缓存——ResponsesWSURL 薄封装，零新代码）
//   - 伪装四元组（W1 持久化）：WithSession（握手头 + 帧内 metadata session/
//     thread/window）+ WithCodexMeta（帧内 x-codex-installation-id——真实客户
//     端该头不进握手头，仅帧 metadata）
//   - WithPingInterval(0)：禁 SDK 内部心跳（心跳单源——编排层 30s+10s）
//   - WithPayloadFiltering(false)（P2-A 必配）：Send 默认白名单过滤会剥
//     max_output_tokens/api/user/metadata 等合法顶层键（过滤后为空整帧不入网）——
//     与双向帧透传 1:1 等价直接矛盾；关闭过滤与 client_metadata 伪装注入独立
//     （prepareFrame client.go:513-579——关闭后注入仍生效）
//   - 透传头（P3-7/P3-8）：codexWSPassthroughHeaders——session 头族 +
//     OpenAI-Beta 已剔除，其余可透传
//
// 错误经适配层翻译（DialError → 信封 + Refreshed；裸 fatal → 统一回调上报）。
func (p *Proxy) dialCodexWS(r *http.Request, sel *scheduler.Selection) (*codexsdk.Client, error) {
	if p.codex == nil {
		return nil, errCodexWSNotIntegrated
	}
	if sel.Ext == nil {
		return nil, errCodexExtMissing
	}
	cred := domain.CredentialFromExt(sel.Ext)
	full, err := p.clients.ResponsesWSURL(sel.TemplateID, sel.BaseURL)
	if err != nil {
		return nil, err
	}
	cred.BaseURL = full
	sess, meta := codexIdentityFromExt(sel.Ext)
	opts := []codexsdk.Option{
		codexsdk.WithPayloadFiltering(false), // P2-A：帧透传 1:1（白名单过滤剥合法键）
		codexsdk.WithPingInterval(0),         // 心跳单源：编排层 30s+10s 单一所有者
		codexsdk.WithSession(sess),           // 伪装：握手头 + 帧内 session/thread/window
		codexsdk.WithCodexMeta(meta),         // 伪装：帧内 x-codex-installation-id 等
		// WithBaseURL 由适配层按 cred.BaseURL 应用（与 HTTP 面 clientFor 同款）
	}
	for k, vs := range codexWSPassthroughHeaders(r.Header) {
		for _, v := range vs {
			opts = append(opts, codexsdk.WithHeader(k, v))
		}
	}
	// 拨号超时上限同款（黑洞上游不回 101）：超时 → SDK dialStatus(nil)=0 →
	// DialError{StatusCode:0} → handleCodexDialError 既有 default 分支连接级
	// 转移（零新分支；wrapped ctx 取消不向上传播，不落 499）。
	ctx, cancel := context.WithTimeout(r.Context(), wsDialTimeout)
	defer cancel()
	return p.codex.Dial(ctx, &cred, opts...)
}

// handleCodexDialError codex WS 拨号失败分类与收尾（T4 §5 错误契约适用；返回
// stop=true 请求已终止，false = 连接级/429 转移——本函数已完成分类，调用方
// MarkResult 后继续 failover 循环）：
//   - 适配层未装配 → 501 语义本地拒绝（释放槽 + recordRejected + 错误帧）
//   - 裸 fatal（Dial 401 轮转路径 refresh 失败——RefreshOAuthError/
//     AccountDisabledError）：适配层已统一回调上报（账号失效剔除 + 摘除）；
//     **该请求不转移**（P3-2 定死）——finish（code 0 ErrNetwork）+ 错误帧收尾
//   - 信封 4xx（DialError → EnvelopeError：401/403 升级拒绝）→ 确定性拒绝透
//     传不转移（与 aiclient 路径 4xx 同构：finish + 错误帧 + 记录）；
//     Refreshed=true（已轮转重连一次仍失败）→ 4xx 分支天然不再触达同账号，
//     网关避免双份刷新
//   - 信封 429 → Kind429 转移；信封 5xx / 裸 RefreshError / 网络（code 0）
//     → RuleKindOf(code) 转移（正常 failover）
func (p *Proxy) handleCodexDialError(r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, client *websocket.Conn, dialErr error) (stop bool, lastCode int, lastErrMsg string) {
	if errors.Is(dialErr, errCodexWSNotIntegrated) {
		p.sched.Release(sel.AccountID)
		p.recordRejected(r.Context(), reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIResponsesWS, http.StatusNotImplemented, domain.ErrBilling, 0, usageTuple{}, start, errCodexWSNotIntegrated.Error())
		wsWriteError(client, errCodexWSNotIntegrated.Error())
		return true, 0, ""
	}
	if sdkbridge.IsFatal(dialErr) {
		msg := domain.TruncateErrMsg(dialErr.Error())
		l := logWithCtx(r.Context(), p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, sel.Format, 0, domain.ErrNetwork, usageTuple{}, start))
		l.ErrorMessage = &msg
		p.finish(sel.AccountID, l)
		// fatal 用户帧固定文案（不泄 SDK 内部机制串）；:125-127 已落盘 dialErr
		// 原文——落盘是唯一留痕。
		wsWriteError(client, codexAuthFailedMsg)
		return true, 0, ""
	}
	code := statusOf(dialErr)
	msg := dialErr.Error()
	switch {
	case code == http.StatusTooManyRequests:
		return false, code, domain.TruncateErrMsg(msg)
	case code >= 400 && code < 500:
		em := domain.TruncateErrMsg(msg)
		l := logWithCtx(r.Context(), p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, sel.Format, code, domain.Err4xx, usageTuple{}, start))
		if em != "" {
			l.ErrorMessage = &em
		}
		p.finish(sel.AccountID, l)
		// R-1 规则驱动（与 REST 4xx 同公式）：Classify(Kind4xx) → punish 投递 → UnifiedMessage/passthrough 帧。
		// WS 无 HTTP 状态码，ResponseCode 维度自然无效，仅 CustomMessage 生效；DialError Refreshed=true 无特殊处理。
		then, punish := p.sched.Classify(rule.Event{AccountID: sel.AccountID, Kind: rule.Kind4xx, HTTPStatus: &code, Model: sel.Model, ErrorMessage: em})
		if punish {
			p.sched.MarkResult(sel.AccountID, rule.Kind4xx, nil, code, em, sel.Model)
		}
		if customMsg, isCustom := rule.UnifiedMessage(then, em); isCustom {
			wsWriteError(client, customMsg)
		} else {
			wsWriteError(client, emOr(em, "upstream rejected request"))
		}
		return true, 0, ""
	default:
		// 5xx（信封）/ 裸 RefreshError / 网络（code 0）：连接级转移。5xx 归一
		// （修复性声明）：code 原样回传（现状归 0 → 耗尽记 ErrNetwork +
		// MarkResult httpStatus 0；统一后 et=Err5xx + httpStatus 5xx——对齐
		// HTTP 路径与静态拨号分支）。
		return false, code, domain.TruncateErrMsg(msg)
	}
}

// sniffCodexWSDeath 预筛 WS 业务判死事件帧：分类知识已收拢 SDK——
// ClassifyAuthFatalFrame 与 HTTP 面 classifyAT401 共享私有 isATFatalCode 码集
// （单一真相：SDK 扩码自动双面生效），网关不再持有码集副本。本地仅保留热路
// 径预筛：bytes.Contains 零分配挡掉绝大多数帧，未命中零跨包调用。其余帧
// （含非判死错误事件——业务错误透传不判死）→ nil。
//
// 预筛针取错误事件帧标记 `"type":"error"`——JSON 键名协议固定小写（值大小写
// 与空白形态由解析层 EqualFold 兜底，判死码值任意大小写均命中）；带前导/
// 尾随空白键名（`"type" : "error"`）→ 预筛漏过 → 帧照常透传（与既有预筛同
// 形态风险，上游不产出）。
func sniffCodexWSDeath(f []byte) *codexsdk.AuthPermanentlyRevokedError {
	if !bytes.Contains(f, []byte(`"type":"error"`)) {
		return nil
	}
	if !strings.EqualFold(gjson.GetBytes(f, "type").String(), "error") {
		return nil // 非错误事件帧（业务内容误含错误帧标记）→ 不判死
	}
	return codexsdk.ClassifyAuthFatalFrame(f)
}

// codexIdentityFromExt 从账号 ext 快照组装伪装四元组（W1 数据层持久化——账号
// 存在期间稳定：InstallationID 账号级永久 / SessionID==ThreadID 会话级 /
// WindowID={thread}:0）。返回 SDK Session（握手头 + 帧内 metadata 双注入）与
// CodexMeta（帧内 x-codex-installation-id 等；优先级 CodexMeta > WithSession，
// 双选项同值无冲突）。身份 nil/缺列（codex_identity jsonb 可空——未配置/
// 旧数据异常）→ 空值（SDK 内层 omit，不注入；sdkbridge identitySig 既有
// 空身份兜底——零新增语义）；Session.ClientRequestID 留空——SDK 缺省回退
// ThreadID（client.go:349-355）。
func codexIdentityFromExt(ext *domain.AccountExt) (sess codexsdk.Session, meta codexsdk.CodexMeta) {
	if ext == nil || ext.CodexIdentity == nil {
		return sess, meta
	}
	id := ext.CodexIdentity
	meta.InstallationID = id.InstallationID
	if id.SessionID != "" {
		sess.SessionID = id.SessionID
		meta.SessionID = id.SessionID
	}
	if id.ThreadID != "" {
		sess.ThreadID = id.ThreadID
		meta.ThreadID = id.ThreadID
	}
	if id.WindowID != "" {
		sess.WindowID = id.WindowID
		meta.WindowID = id.WindowID
	}
	return sess, meta
}

// codexWSPassthroughHeaders codex 路径透传头（P3-7/P3-8 冲突面）：在
// wsPassthroughHeaders 剔除面（hop-by-hop + 网关 key Authorization）之上再剔
// 除 session 头族（session-id/thread-id/x-client-request-id/x-codex-window-id
// ——SDK WithHeader 先删后加覆盖默认头，直通会覆盖伪装身份四元组）及
// OpenAI-Beta（客户端可覆盖网关默认 beta 版本——与 aiclient 路径强制覆盖语义
// 不对称；beta 为协议面关键值，错配可致上游拒连）。其余头原样透传。
func codexWSPassthroughHeaders(h http.Header) http.Header {
	out := wsPassthroughHeaders(h)
	for _, k := range []string{
		"Session-Id", "Thread-Id", "X-Client-Request-Id", "X-Codex-Window-Id", "OpenAI-Beta",
	} {
		out.Del(k)
	}
	return out
}

// codexTransport 上游侧 *codexsdk.Client 的 wsRelayTransport 适配（codex 路
// 径）：typ 语义与现状 relayCodexWS 同款——Send 忽略 typ 恒 text（SDK Send
// 内部恒 conn.Write MessageText，client.go:499-511）、Recv 丢帧类型 → Read 恒
// 返回 MessageText（responses WS 协议全 text 帧，binary 帧降级 text 转发，风
// 险低——现状既有降级语义）；Ping/Close/CloseNow 直通 SDK（Close 幂等
// closeOnce；pong 由 relay 循环的常驻 Read 处理——SDK Client 并发语义
// client.go:273-275 前提满足）。错误原样透传（SDK Recv 透传 coder/websocket
// 错误 → errors.As *CloseError 成立，relayClassify/recordClose 直接复用）。
type codexTransport struct{ up *codexsdk.Client }

func newCodexTransport(up *codexsdk.Client) *codexTransport {
	return &codexTransport{up: up}
}

func (t *codexTransport) Write(ctx context.Context, _ websocket.MessageType, frame []byte) error {
	return t.up.Send(ctx, frame)
}

func (t *codexTransport) Read(ctx context.Context) (websocket.MessageType, []byte, error) {
	f, err := t.up.Recv(ctx)
	return websocket.MessageText, f, err
}

func (t *codexTransport) Ping(ctx context.Context) error { return t.up.Ping(ctx) }

func (t *codexTransport) Close(code websocket.StatusCode, reason string) error {
	return t.up.Close(code, reason)
}

func (t *codexTransport) CloseNow() { t.up.CloseNow() }
