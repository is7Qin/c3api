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
	"sync"
	"time"

	"github.com/coder/websocket"
	codexsdk "github.com/is7Qin/codex-sdk"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/sdkbridge"
)

// --- codex 独立 relay 变体（T4 §1：codex-oauth/codex-pat 类型 resp-ws 接线） ---
// 独立文件族（用户拍板文件边界：codex 相关处理不散落现有 caller/forward 文件，
// codexsdk import 仅限本文件族 + sdkbridge 扩展）。与 aiclient 路径
// （relayResponsesWS）同构的编排：双向帧透传 1:1 / usage 嗅探
// （response.completed——sniffResponsesCompleted 复用）/ 关闭分类
// （relayClassify/recordClose 复用）/ 心跳（30s Ping + 10s pong 超时同款）——
// 差异只在传输面：上游侧 = *codexsdk.Client 具体类型（Send/Recv/Ping/Close/
// CloseNow——用户裁决不抽传输接口），服务端 Accept 侧 = 既有 *websocket.Conn
// 不动。
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
//     max_output_tokens/user/metadata 等合法顶层键（过滤后为空整帧不入网）——
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
	return p.codex.Dial(r.Context(), &cred, opts...)
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
//   - 信封 429 → Result429 转移；信封 5xx / 裸 RefreshError / 网络（code 0）
//     → ResultError 转移（正常 failover）
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
		wsWriteError(client, dialErr.Error())
		return true, 0, ""
	}
	code := statusOf(dialErr)
	msg := dialErr.Error()
	switch {
	case code == http.StatusTooManyRequests:
		return false, code, domain.TruncateErrMsg(msg)
	case code >= 400 && code < 500:
		l := logWithCtx(r.Context(), p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, sel.Format, code, domain.Err4xx, usageTuple{}, start))
		if em := domain.TruncateErrMsg(msg); em != "" {
			l.ErrorMessage = &em
		}
		p.finish(sel.AccountID, l)
		wsWriteError(client, emOr(msg, "upstream rejected request"))
		return true, 0, ""
	default:
		// 5xx（信封）/ 裸 RefreshError / 网络（code 0）：连接级转移。5xx 归一
		// （修复性声明）：code 原样回传（现状归 0 → 耗尽记 ErrNetwork +
		// MarkResult httpStatus 0；统一后 et=Err5xx + httpStatus 5xx——对齐
		// HTTP 路径与静态拨号分支）。
		return false, code, domain.TruncateErrMsg(msg)
	}
}

// sniffCodexWSDeath 预筛 WS 业务判死事件帧（T5 §3——唯一跨边界点：网关触达
// SDK 鉴权内部仅此一点；SDK 不解析业务事件帧，auth.go Fatal 注释实证）：
// 错误事件帧（type=error）的 error.code/error.type ∈ {token_invalidated,
// token_revoked}（SDK 判死码集语义 auth_errors.go:28-31 + classifyAT401 字段
// 路径，大小写不敏感——值经 EqualFold 判定）→ 构造 *AuthPermanentlyRevokedError
// （Code = 命中码，Raw = 原帧）——复用 SDK 判死码集语义，**分类逻辑不进网
// 关**（只映射既有码集，不新增判定面）。其余帧（含非判死错误事件——业务错
// 误透传不判死）→ nil。
//
// 热路径纪律（与 sniffResponsesCompleted 同款）：bytes.Contains 零分配预筛，
// 命中才最小 gjson 解析。预筛针取错误事件帧标记 `"type":"error"`——JSON 键
// 名协议固定小写（值大小写与空白形态由解析层 EqualFold 兜底——与 SDK
// classifyAT401 同语义，判死码值任意大小写均命中）；带前导/尾随空白键名
//（`"type" : "error"`）→ 预筛漏过 → 帧照常透传（与既有预筛同形态风险，上
// 游不产出）。
func sniffCodexWSDeath(f []byte) *codexsdk.AuthPermanentlyRevokedError {
	if !bytes.Contains(f, []byte(`"type":"error"`)) {
		return nil
	}
	if !strings.EqualFold(gjson.GetBytes(f, "type").String(), "error") {
		return nil // 非错误事件帧（业务内容误含错误帧标记）→ 不判死
	}
	for _, p := range []string{"error.code", "error.type"} {
		if code := gjson.GetBytes(f, p).String(); isWSDeathCode(code) {
			return &codexsdk.AuthPermanentlyRevokedError{Code: strings.ToLower(code), Raw: f}
		}
	}
	return nil
}

// isWSDeathCode 判死码判定（token_invalidated/token_revoked，大小写不敏感——
// 与 SDK isATFatalCode 同码集；SDK 私有不可复用，网关侧映射面唯一）。
func isWSDeathCode(code string) bool {
	return strings.EqualFold(code, "token_invalidated") || strings.EqualFold(code, "token_revoked")
}

// codexIdentityFromExt 从账号 ext 快照组装伪装四元组（W1 数据层持久化——账号
// 存在期间稳定：InstallationID 账号级永久 / SessionID==ThreadID 会话级 /
// WindowID={thread}:0）。返回 SDK Session（握手头 + 帧内 metadata 双注入）与
// CodexMeta（帧内 x-codex-installation-id 等；优先级 CodexMeta > WithSession，
// 双选项同值无冲突）。缺列（旧数据/异常）→ 空值（SDK 内层 omit，不注入）；
// Session.ClientRequestID 留空——SDK 缺省回退 ThreadID（client.go:349-355）。
func codexIdentityFromExt(ext *domain.AccountExt) (sess codexsdk.Session, meta codexsdk.CodexMeta) {
	if ext == nil {
		return sess, meta
	}
	meta.InstallationID = ext.InstallationID
	if ext.SessionID != nil {
		sess.SessionID = *ext.SessionID
		meta.SessionID = *ext.SessionID
	}
	if ext.ThreadID != nil {
		sess.ThreadID = *ext.ThreadID
		meta.ThreadID = *ext.ThreadID
	}
	if ext.WindowID != nil {
		sess.WindowID = *ext.WindowID
		meta.WindowID = *ext.WindowID
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

// relayCodexWS 拨号成功后的编排（T4 §1——relayResponsesWS 的 codex 变体，纯函
// 数/心跳模式/关闭分类全部复用，仅传输面换 SDK 具体类型）：首帧模型改写 →
// 转发首帧 → 双向事件帧 1:1 relay（流式中间帧零解析直转；SDK Send 关闭白名
// 单过滤后与 aiclient 直连同字节语义，client_metadata 伪装注入照常）→ 关闭/
// 错误传播 → usage 记录。返回 (handled, fwMsg) 语义与 relayResponsesWS 相同。
//
// 传输面差异（SDK 具体类型，不抽接口）：
//   - 客户端 → 上游：client.Read 帧 → up.Send（SDK 恒 MessageText——responses
//     WS 协议全 text 帧，binary 帧降级 text 转发，风险低）
//   - 上游 → 客户端：up.Recv（SDK 丢弃帧类型）→ client.Write(MessageText)
//   - 心跳：up.Ping(pc)（30s 间隔 + 10s pong 超时——pong 由本循环的常驻
//     Recv 处理，SDK Client 并发语义 client.go:273-275 前提满足）
//   - 关闭：up.Close(status, reason) / up.CloseNow()（SDK 幂等 closeOnce）
//   - 错误分类：SDK Recv 透传 coder/websocket 错误原样 → errors.As
//     *CloseError 成立，relayClassify/recordClose 直接复用
func (p *Proxy) relayCodexWS(client *websocket.Conn, up *codexsdk.Client, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, firstTyp websocket.MessageType, first []byte) (handled bool, fwMsg string) {
	frame := first
	if sel.Model != "" && sel.Model != reqModel {
		if nf, err := sjson.SetBytes(first, "model", sel.Model); err == nil {
			frame = nf
		} // 改写失败（帧非合法 JSON）→ 原样转发，上游自行校验
	}
	if err := up.Send(r.Context(), frame); err != nil {
		up.CloseNow()
		return false, domain.TruncateErrMsg(err.Error())
	}

	// --- 双向 relay：三个方向各自 goroutine，首退者触发取消 ---
	// 结构与 relayResponsesWS 同款（I-1 竞态修复语义原样：upClose 独立槽最高
	// 权威 / upLoopDone 记录可见性 / 客户端循环用 r.Context() 阻塞读——关闭
	// 帧自然解除）。
	relayCtx, relayCancel := context.WithCancel(r.Context())
	defer relayCancel()
	endCh := make(chan struct{}, 3)
	var (
		it, ot, tt, cr, cc        int64
		img                       int64 // resp 检测功能调用计数（spec §6 旁路；respImageDetectOn 门控）——落 CallCount
		ttft                      *int64
		wg                        sync.WaitGroup
		endMu                     sync.Mutex
		upClose                   *websocket.CloseError // 上游关闭帧（分类最高权威，仅 up-loop 写入）
		upErr, clientErr, pingErr error
	)
	setErr := func(dst *error, err error) {
		if err == nil || relayCtx.Err() != nil {
			return
		}
		endMu.Lock()
		if *dst == nil {
			*dst = err
		}
		endMu.Unlock()
	}
	recordClose := func(err error) {
		var ce websocket.CloseError
		if !errors.As(err, &ce) {
			return
		}
		endMu.Lock()
		if upClose == nil {
			c := ce
			upClose = &c
		}
		endMu.Unlock()
	}
	exit := func() { // 首退触发：通知编排取消对侧
		select {
		case endCh <- struct{}{}:
		default:
		}
		relayCancel()
	}

	wg.Add(1)
	go func() { // 客户端 → 上游（客户端帧透传；写失败 = 上游侧问题）
		defer wg.Done()
		for {
			_, f, err := client.Read(r.Context())
			if err != nil {
				setErr(&clientErr, err)
				exit()
				return
			}
			// SDK Send 恒 MessageText（帧类型不传递——responses WS 全 text 帧）
			if err := up.Send(relayCtx, f); err != nil {
				setErr(&upErr, err)
				exit()
				return
			}
		}
	}()

	upLoopDone := make(chan struct{})
	wg.Add(1)
	go func() { // 上游 → 客户端（热路径：预筛嗅探 response.completed 取 usage）
		defer wg.Done()
		defer close(upLoopDone) // 编排等本读者退出后再分类（I-1 记录可见性）
		for {
			f, err := up.Recv(relayCtx)
			if err != nil {
				var ce websocket.CloseError
				if errors.As(err, &ce) {
					recordClose(err)
				} else {
					setErr(&upErr, err)
				}
				exit()
				return
			}
			// T5 §3 唯一跨边界点：WS 业务判死事件帧（token_invalidated/
			// token_revoked）→ 适配层 FatalAuth（Auth.Fatal 毒化——不触发
			// OnAuthFatal——+ 统一失效回调：写 failed_at + StatusDisabled，
			// 共用 T1 处理函数；双源去重：同一 fatal 再经 errors.As 二次命
			// 中仍单次上报）。判死帧照常透传客户端（错误事件属业务流），
			// 会话随后由上游关闭帧自然收尾。
			if fatal := sniffCodexWSDeath(f); fatal != nil {
				p.codex.FatalAuth(sel.AccountID, fatal)
			}
			// 热路径纪律：bytes.Contains 零分配预筛，命中才最小 gjson 解析；
			// 流式中间帧零解析直转（与 aiclient 路径同款——SDK Recv 缓冲直写）。
			if u, ok := sniffResponsesCompleted(f); ok {
				it, ot, tt, cr, cc = u.it, u.ot, u.tt, u.cr, u.cc
				if respImageDetectOn(sel) {
					img = respImageCountCompleted(f)
				}
			}
			if ttft == nil {
				ms := time.Since(start).Milliseconds()
				ttft = &ms
			}
			if err := client.Write(relayCtx, websocket.MessageText, f); err != nil {
				setErr(&clientErr, err)
				exit()
				return
			}
		}
	}()

	wg.Add(1)
	go func() { // 心跳：向上游周期 Ping（pong 超时 = 上游失联 → 按上游错误收尾）
		defer wg.Done()
		ticker := time.NewTicker(p.wsHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-relayCtx.Done():
				return
			case <-ticker.C:
			}
			pc, pcancel := context.WithTimeout(relayCtx, responsesWSPongTimeout)
			err := up.Ping(pc)
			pcancel()
			if err != nil {
				setErr(&pingErr, err)
				exit()
				return
			}
		}
	}()

	<-endCh
	<-upLoopDone

	// 分类与关闭传播（与 relayResponsesWS 同款——relayClassify 纯函数复用）：
	// ① 上游正常关闭（1000/1001）→ 成功 ② 客户端断开 → abort ③ 上游错误/失
	// 联 → recordStreamAbort + ResultError。记录先行、关闭帧后发（记录不丢语义）。
	u := usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc, calls: img}
	logCtx := relayCtx
	if ttft != nil {
		logCtx = context.WithValue(relayCtx, ctxKeyTTFT{}, ttft)
	}
	end, endErr := relayClassify(upClose, upErr, clientErr, pingErr)
	switch end {
	case relayEndUpstreamClosed:
		_ = up.Close(websocket.StatusNormalClosure, "") // 完成关闭握手（上游已发关闭帧）
		p.sched.MarkResult(sel.AccountID, scheduler.ResultOK, nil, http.StatusOK, "")
		p.finish(sel.AccountID, logWithCtx(logCtx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, sel.Format, http.StatusOK, domain.ErrNone, u, start)))
		_ = client.Close(websocket.StatusNormalClosure, "")
	case relayEndClientAbort:
		// 客户端已死/已关闭，免握手等待
		_ = client.CloseNow()
		code := websocket.StatusGoingAway
		if isNormalWSClose(endErr) {
			code = wsCloseStatus(endErr)
		}
		p.finish(sel.AccountID, logWithCtx(logCtx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, sel.Format, http.StatusOK, domain.ErrAbort, u, start)))
		_ = up.Close(code, "") // 向上游传播客户端关闭
	case relayEndUpstreamError:
		p.recordStreamAbort(logCtx, reqID, groupID, start, sel, reqModel, u, endErr)
		p.sched.MarkResult(sel.AccountID, scheduler.ResultError, nil, 0, endErr.Error())
		_ = client.Close(wsCloseStatus(endErr), "")
		up.CloseNow() // 上游已死/失联，免握手等待
	}
	relayCancel()
	wg.Wait()
	return true, ""
}
