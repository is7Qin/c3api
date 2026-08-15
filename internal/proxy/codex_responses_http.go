// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"context"
	"net/http"
	"strings"
	"time"

	codexsdk "github.com/is7Qin/codex-sdk"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

// --- codex 类型 resp HTTP 分支（T6 §1：codex-oauth/codex-pat 类型
// openai-responses 接入——SDK 合成非流式 + Stream SSE 透传） ---
// 独立文件族（用户拍板文件边界：codex 相关处理不散落现有 caller 文件）。
// 分支插入点 = responsesCaller.Call 入口按 sel.CredentialType 分流
// （caller_responses.go）；typed 段（api_key/responses-special）零改动。与 WS
// 变体（codex_responses_ws.go）同形态编排，差异在传输面：HTTP = SDK
// HTTPClient（Responses/StreamResponses——sdkbridge T6 扩展），WS = SDK Dial。

// errCodexResponsesNotIntegrated 501：codex 适配层未装配（SetCodex 未调用——
// main 装配缺失的显式拒绝，不让凭据缺失路径误报 502/network；与 images/WS
// 路径同款）。
var errCodexResponsesNotIntegrated = &formatError{status: http.StatusNotImplemented, msg: "codex responses unavailable (adapter not wired)"}

// callCodexResponses codex-oauth/codex-pat 类型 resp 调用（T6 §1）：非流式 →
// 适配层 Responses（SDK 合成非流式——内部无条件 stream:true + SSE 事件聚合；
// 网关以非流式语义消费）；流式 → StreamResponses（SDK 载荷重帧 SSE 透传）。
//
// 预处理沿用 setModel 改写（P2-2——ModelMapping 对 codex 账号生效，不静默
// 失效）；**不 stripImageTools**（用户裁决：strip 仅针对 resp/resp-ws 的
// responses-special——codex 上游自带 image 处理）。
//
// 错误分类（骨架 statusOf/upstreamBody 零改动复用）：
//   - 信封（SDK *HTTPError 包装——403 账号无权限等）→ 4xx 透传 / 429/5xx
//     failover 既有分类（failover 循环按 code 分类）
//   - fatal（errors.As 五类）→ 适配层已统一回调上报（账号失效标记 +
//     FailAccount 快照摘除——failover 不重试同账号）；code 0 → 连接级
//     MarkResult(ResultError) + 转移其它账号（与 images 路径同语义）
//   - RefreshError/网络 → code 0 → failover 可重试
func (p *Proxy) callCodexResponses(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, body []byte, stream bool) (int, []byte, bool, error) {
	// 客户端请求模型（日志口径）：gjson 顶层提取（与 typed 流式路径同款，
	// 1 次分配；codex 分支无完整 params 解析）。
	reqModel := gjson.GetBytes(body, "model").String()
	if p.codex == nil {
		// 适配层未装配（SetCodex 未调用）：显式 501（防 nil 误走凭据缺失 502）。
		p.sched.Release(sel.AccountID)
		p.recordRejected(r.Context(), reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIResponses, http.StatusNotImplemented, domain.ErrBilling, 0, usageTuple{}, start, errCodexResponsesNotIntegrated.msg)
		writeErr(w, errCodexResponsesNotIntegrated)
		return 0, nil, true, nil
	}
	if sel.Ext == nil {
		// 配置损坏（codex 账号必有 ext 行——快照缺 account_ext 行）：本地配置
		// 错误按连接级错误转移（失败文本落盘，耗尽 502 语义）；不上报失效（避
		// 免 account 0 无谓上报——与 WS 路径 errCodexExtMissing 同语义）。
		return 0, nil, false, errCodexExtMissing
	}
	// 凭据线：快照派生直供适配层（与 WS/images 路径同款——codex 凭据为复合
	// 结构 oauth_token+refresh_token+expires_at+pat+accountID，单字符串契约表
	// 达不了；注册表未注册 codex 类型，见 codex_responses_ws.go 注释）。
	cred := domain.CredentialFromExt(sel.Ext)
	full, err := p.clients.ResponsesWSURL(sel.TemplateID, sel.BaseURL)
	if err != nil {
		return 0, nil, false, err
	}
	cred.BaseURL = full
	if stream {
		return p.streamCodexResponses(ctx, w, r, reqID, groupID, start, sel, reqModel, &cred, body)
	}
	return p.nonstreamCodexResponses(ctx, w, r, reqID, groupID, start, sel, reqModel, &cred, body)
}

// clientTurnState 客户端请求自带 x-codex-turn-state（HOST-2 透传优先——客户端
// 自管，非空覆盖网关 held 注入；空 = 未带 → 网关补"服务端签发过"的空缺）。
func clientTurnState(r *http.Request) string {
	if r == nil {
		return ""
	}
	return r.Header.Get(codexsdk.HeaderTurnState)
}

// --- turn-state 轮边界判定（HOST-2——真实代码实证钉死） ---
// 真实实例归属：轮级——ModelClientSession.new_session 每轮新建
// Arc<OnceLock<String>>（core/src/client.rs:498 "fresh turn-scoped streaming
// session"），值 = 实例生命周期内首次 set 后恒定（握手响应头
// responses_websocket.rs:538 + 流事件 :742 二次 set 被 OnceLock 忽略）；轮结束
// = 会话销毁（跨轮不回传）。真实轮结束判定（stream_events_utils.rs:318-328
// needs_follow_up + turn.rs:150-152）：响应输出项为工具调用 → 轮继续；纯
// message/reasoning → 轮结束。真实测试套件实证（core/tests/suite/turn_state.rs
// persists_within_turn_and_resets_after）：completed 响应（含工具调用）后同轮
// 后续请求仍带 ts，仅跨轮清除——**不在 response.completed 即清**。

// isCodexCallItemType 轮继续信号判定：输出项类型为工具调用型——wire type 族
// 恒以 "_call" 结尾（function_call/shell_command_call/computer_call/
// web_search_call/local_shell_call/custom_tool_call 等——对齐真实
// stream_events_utils.rs needs_follow_up 语义）。
func isCodexCallItemType(t string) bool { return strings.HasSuffix(t, "_call") }

// codexTurnEndedBody 非流式合成体轮结束判定：output 无工具调用项 → 轮结束
//（SDK 聚合器成功返回必已收到 response.completed 终态——合成体恒 completed）。
func codexTurnEndedBody(body []byte) bool {
	for _, t := range gjson.GetBytes(body, "output.#.type").Array() {
		if isCodexCallItemType(t.String()) {
			return false // 含工具调用 → 轮继续（同轮后续请求须续传）
		}
	}
	return true
}

// sniffCodexTurnCallItem 流式帧轮继续信号嗅探（热路径纪律同 sniffCodexWSDeath：
// bytes.Contains 零分配预筛——"item":{" 为 item 对象形态（消息正文 JSON 转义
// 不含该原始子串），命中才最小 gjson 解析 item.type）。
func sniffCodexTurnCallItem(f []byte) bool {
	if !bytes.Contains(f, []byte(`"item":{`)) {
		return false
	}
	return isCodexCallItemType(gjson.GetBytes(f, "item.type").String())
}

// nonstreamCodexResponses 非流式 codex resp（T6 §1）：setModel 改写（P2-2——
// 与 typed 非流式 SDK 路径 params.Model = sel.Model 等价；短路守卫零分配）→
// 适配层 Responses → 合成体原样转发（application/json）+ **顶层 usage 提取**
// （P1-1：合成体无 type 字段——顶层 usage 直接解析；typed 的 SDK 结构体解析
// 路径不适用——合成体是上游 wire 原样交付）。turn-state（HOST-2）：客户端
// 自带 → 透传优先；未带 → 网关注入 held（上游签发值——同轮回传）；合成体无
// 工具调用项（轮结束）→ ClearTurnState（跨轮不回传）。
func (p *Proxy) nonstreamCodexResponses(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, cred *domain.AccountCredential, body []byte) (int, []byte, bool, error) {
	streamBody, err := setModel(body, sel.Model)
	if err != nil {
		return 0, nil, false, err // 本地 JSON 错误（handleFormat 已过 json.Valid 硬门——防御）
	}
	// 非流式超时（B-P2-7）：HTTPClient.Timeout 不可用（流式/非流式四方法共享，
	// 覆盖整个响应体读取会切断长流式 SSE）——非流式方法各自包 ctx 超时；TCP
	// 黑洞读停滞 → 超时触发 → 连接级错误转移（并发槽/连接/goroutine 不无限占用，
	// failover 可转移）。
	ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamTimeout)
	defer cancel()
	// 伪装身份（META-2——spec 2026-08-15）：复用 WS 路径 codexIdentityFromExt
	//（account_ext 四元组 + installation——账号存在期间稳定）；HTTP 面经 SDK
	// client_metadata 注入（键集对齐真实 codex；未配置仍恒带 turn_id）。
	sess, meta := codexIdentityFromExt(sel.Ext)
	resp, err := p.codex.Responses(ctx, cred, streamBody, &sess, &meta, clientTurnState(r))
	if err != nil {
		return statusOf(err), upstreamBody(err), false, err
	}
	// 轮结束清除（HOST-2）：合成体成功返回必含 completed 终态——无工具调用项
	// → 轮结束 → 清除 held（跨轮不回传；适配层已回写本次响应签发值）。
	if codexTurnEndedBody(resp.Raw) {
		p.codex.ClearTurnState(sel.AccountID)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp.Raw)
	// 顶层 usage（非流式低频路径）：缺失/显式 null → ok=false → 恒 0（与原
	// gjson 缺失 = 0 等价）。
	var it, ot, tt, cr, cc int64
	if t, ok := responsesTopLevelUsage(resp.Raw); ok {
		it, ot, tt, cr, cc = t.it, t.ot, t.tt, t.cr, t.cc
	}
	var img int64 // resp 检测功能调用计数（spec §6 旁路；respImageDetectOn 门控）——落 CallCount
	if respImageDetectOn(sel) {
		img = respImageCountBody(resp.Raw)
	}
	p.sched.MarkResult(sel.AccountID, scheduler.ResultOK, nil, http.StatusOK, "")
	p.finish(sel.AccountID, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIResponses, http.StatusOK, domain.ErrNone, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc, calls: img}, start)))
	return http.StatusOK, nil, true, nil
}

// streamCodexResponses 流式 codex resp（T6 §1 + §4 P2-1 客户端帧规格钉死）：
// setModel 改写 → 适配层 StreamResponses（SDK 逐 data: 载荷零拷贝回调）→ 每
// 载荷重帧 `data: <payload>\n\n` + flush 直写（fn 内立即写出——SDK 回调切片
// 指向 scanner 复用缓冲，仅回调期有效，不得跨回调保留）；Stream 返回 nil
// （含 [DONE] 与 EOF 统一）后**自行补发** `data: [DONE]\n\n` + flush（客户端等
// 终止标记挂死防线）；上游错误不补发 [DONE]（信封/failover 分类）。
//
// **头延至首个 fn 调用内**（评审 P1 修复：SDK 调用前提交 200 会把首帧前 4xx
// 信封错误吞成"200 空成功流"——上游 403 → 客户端 200 + 裸错误体 + 无 [DONE]；
// 同根因致流式 failover 耗尽把 502 JSON 裸写进 SSE 体）。首帧前失败 → 头未
// 提交，HTTP 状态可用 → (code, body, false) 交 failover 循环正常分类（4xx 透
// 传 / 429/5xx 转移 / 耗尽 502——typed 分支 ResponseStreamRaw 非 200 同语义）。
//
// 断开/超时收尾镜像 caller_responses.go:92-101 双分支：客户端断开
// （r.Context().Err() != nil）→ 不 MarkResult（finish 200 ErrAbort——上游已消
// 费请求，token 取断前已收 usage 帧）；上游超时/错误（帧已写出，200 已定型）
// → recordStreamAbort 语义 + MarkResult(ResultError)。
//
// usage 嗅探（P1-1）：fn 内 gjson type 精确判定 + 顶层解析（取首个命中帧——
// completed 终态恒唯一，usage 只读一次）。
func (p *Proxy) streamCodexResponses(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, cred *domain.AccountCredential, body []byte) (int, []byte, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamStreamTimeout)
	defer cancel()
	streamBody, err := setModel(body, sel.Model)
	if err != nil {
		return 0, nil, false, err // 本地 JSON 错误（防御——同非流式）
	}
	var (
		it, ot, tt, cr, cc int64
		img                int64 // resp 检测功能调用计数（spec §6 旁路；respImageDetectOn 门控）——落 CallCount
		usageTaken         bool  // 首个 completed 帧已取（usage 只读一次）
		framesWritten      bool  // 首帧已写出（头已提交；首帧前失败 → HTTP 状态可用）
		turnCallSeen       bool  // 流中轮继续信号（工具调用输出项——HOST-2 轮边界）
		ttft               *int64
	)
	// 伪装身份同非流式（META-2——codexIdentityFromExt 复用）；turn-state 透传
	// 优先（HOST-2——客户端自带覆盖 held 注入）。
	sess, meta := codexIdentityFromExt(sel.Ext)
	err = p.codex.StreamResponses(ctx, cred, streamBody, &sess, &meta, clientTurnState(r), func(raw []byte) error {
		if !framesWritten {
			// 首事件发头（P2-1 帧规格：三件套 + WriteHeader(200) 显式——SDK 载荷
			// 直写无 sserelay 首帧隐式写头；延至此处保证首帧前失败不吞状态码）。
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(http.StatusOK)
			framesWritten = true
		}
		if !usageTaken {
			// 热路径：字节扫描 type 精确判定（防正文含子串帧冻结——P1-1）+
			// 顶层 usage 解析（单遍，零分配）；首个命中后跳过（终态事件唯一）。
			if hit, ok := sniffResponsesCompletedTop(raw); ok {
				it, ot, tt, cr, cc = hit.it, hit.ot, hit.tt, hit.cr, hit.cc
				if respImageDetectOn(sel) {
					img = respImageCountCompleted(raw)
				}
				usageTaken = true
			}
		}
		// 轮边界嗅探（HOST-2）：工具调用输出项 → 轮继续（同轮后续请求续传
		// turn-state）；命中后跳过（每响应一次判定足够）。
		if !turnCallSeen && sniffCodexTurnCallItem(raw) {
			turnCallSeen = true
		}
		if ttft == nil {
			ms := time.Since(start).Milliseconds()
			ttft = &ms
		}
		// 立即写出（回调切片仅回调期有效——不得跨回调保留）。
		return writeCodexSSEFrame(w, raw)
	})
	if err != nil {
		// 客户端断开：上游已消费请求（成功），仍须记录用量（成功请求丢日志防
		// 线——caller_responses.go:99-104 语义）；按 abort 收尾不 MarkResult。
		if r.Context().Err() != nil {
			p.finish(sel.AccountID, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIResponses, http.StatusOK, domain.ErrAbort, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc, calls: img}, start)))
			return 0, nil, true, nil
		}
		// 首帧前信封错误（4xx 透传 / 429/5xx failover——typed 分支
		// ResponseStreamRaw 非 200 同语义）：未写出任何帧，可返回 HTTP 状态由
		// failover 循环分类。
		if !framesWritten {
			return statusOf(err), upstreamBody(err), false, err
		}
		// 上游停滞/错误（流中止）：200 已写出——recordStreamAbort + ResultError
		//（caller_responses.go:106-108 同语义）。
		p.recordStreamAbort(ctx, reqID, groupID, start, sel, reqModel, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc, calls: img}, err)
		p.sched.MarkResult(sel.AccountID, scheduler.ResultError, nil, statusOf(err), err.Error())
		return 0, nil, true, nil
	}
	// 轮结束清除（HOST-2）：流正常结束且收到 completed 终态（usageTaken）且无
	// 工具调用项（!turnCallSeen）→ 轮结束 → 清除 held（跨轮不回传）。错误/断开
	// 路径不清（轮结束未知——不误清同轮续传值）。
	if usageTaken && !turnCallSeen {
		p.codex.ClearTurnState(sel.AccountID)
	}
	logCtx := ctx
	if ttft != nil {
		logCtx = context.WithValue(ctx, ctxKeyTTFT{}, ttft)
	}
	// 流正常结束（SDK 已消费 [DONE]/EOF）：补发 data: [DONE]\n\n + flush。
	// 补发失败 = 客户端断开 → 按 abort 收尾（上游已正常完成——usage 照记）。
	// 零帧防御（病态上游仅发 [DONE]——fn 从未调用、头未提交）：此刻才提交头，
	// [DONE] 是首个写出字节。
	if !framesWritten {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		framesWritten = true
	}
	if err := writeCodexSSEFrame(w, sseDonePayload); err != nil {
		p.finish(sel.AccountID, logWithCtx(logCtx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIResponses, http.StatusOK, domain.ErrAbort, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc, calls: img}, start)))
		return 0, nil, true, nil
	}
	p.sched.MarkResult(sel.AccountID, scheduler.ResultOK, nil, http.StatusOK, "")
	p.finish(sel.AccountID, logWithCtx(logCtx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAIResponses, http.StatusOK, domain.ErrNone, usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc, calls: img}, start)))
	return http.StatusOK, nil, true, nil
}

// SSE 帧常量（codex 流式分支专用——SDK 交付载荷重帧；typed 面由 sserelay 全
// 帧原样透传含 event:/[DONE]，线格式不同——P2-1 帧规格）。
var (
	sseDataPrefix  = []byte("data: ")
	sseFrameSuffix = []byte("\n\n")
	sseDonePayload = []byte("[DONE]")
)

// writeCodexSSEFrame 逐帧重帧写出（P2-1：`data: <payload>\n\n` + flush——SDK
// 交付的是载荷非完整 SSE 行，event: 行不重建）。零分配：三段直写（前缀/载荷/
// 后缀——前缀后缀为包级字节常量复用）。fn 内立即调用（SDK 回调切片仅回调期
// 有效）。
func writeCodexSSEFrame(w http.ResponseWriter, payload []byte) error {
	if _, err := w.Write(sseDataPrefix); err != nil {
		return err
	}
	if _, err := w.Write(payload); err != nil {
		return err
	}
	if _, err := w.Write(sseFrameSuffix); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}
