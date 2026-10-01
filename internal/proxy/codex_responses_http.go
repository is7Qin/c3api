// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	codexsdk "github.com/is7Qin/codex-sdk"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/protoconv"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
)

// --- codex 类型 resp HTTP 分支（codex-oauth/codex-pat 类型
// openai-responses 接入——SDK 合成非流式 + Stream SSE 透传） ---
// 独立文件族（用户拍板文件边界：codex 相关处理不散落现有 caller 文件）。
// 分支插入点 = responsesCaller.Call 入口按 sel.CredentialType 分流
// （caller_responses.go）；typed 段（api_key/responses-special）零改动。与 WS
// 变体（codex_responses_ws.go）同形态编排，差异在传输面：HTTP = SDK
// HTTPClient（Responses/StreamResponses——sdkbridge 扩展），WS = SDK Dial。

// errCodexResponsesNotIntegrated 501：codex 适配层未装配（SetCodex 未调用——
// main 装配缺失的显式拒绝，不让凭据缺失路径误报 502/network；与 images/WS
// 路径同款）。
var errCodexResponsesNotIntegrated = &formatError{status: http.StatusNotImplemented, msg: "codex responses unavailable (adapter not wired)"}

func codexDispatchBase(sel *scheduler.Selection, reqModel, reqID string, start time.Time, usage usageTuple, ttft *int64) AttemptOutcome {
	fp := "fp1"
	if sel != nil && sel.CandidateFingerprint != "" {
		fp = sel.CandidateFingerprint
	}
	mapped := ""
	accID := int64(0)
	tplID := int64(0)
	if sel != nil {
		mapped = sel.Model
		accID = sel.AccountID
		tplID = sel.TemplateID
	}
	lat := max(time.Since(start).Milliseconds(), 0)
	return AttemptOutcome{
		ID:               AttemptID(reqID),
		RouteClassID:     RouteClassID("rc1"),
		QualityClassID:   QualityClassID("qc1"),
		Fingerprint:      CandidateFingerprint(fp),
		TemplateID:       tplID,
		AccountID:        accID,
		RequestedModel:   reqModel,
		MappedModel:      mapped,
		CallerCategory:   CallerCodexHTTP,
		OperationTag:     OperationTag(domain.OpResponses),
		Ordinal:          1,
		IdentityRevision: IdentityRevision(1), // synthetic placeholder K; not part of the continuation (I,K) comparison (see AttemptOutcome.IdentityRevision)
		Lane:             LanePrimary,
		Generation:       Generation(1),
		Timing:           AttemptTiming{LatencyMS: lat, TTFTMS: ttft},
		Usage: AttemptUsage{
			InputTokens: usage.it, OutputTokens: usage.ot, CacheReadTokens: usage.cr, CacheCreationTokens: usage.cc, CallCount: usage.calls,
		},
		HardContinuation: false,
	}
}

func emitCodexOutcome(ctx context.Context, p *Proxy, sel *scheduler.Selection, o AttemptOutcome, healthKind rule.Kind, healthMsg string) {
	if sel == nil {
		return
	}
	if err := o.Validate(); err != nil {
		return
	}
	var health *AttemptHealthEvent
	if healthKind != 0 || healthMsg != "" {
		health = &AttemptHealthEvent{Kind: healthKind, ErrorMessage: healthMsg}
	}
	if o.Result == ResultClientCancel {
		health = nil
	}
	p.observeDispatchOutcome(ctx, o, health)
}

// callCodexResponses codex-oauth/codex-pat 类型 resp 调用：非流式 →
// 适配层 Responses（SDK 合成非流式——内部无条件 stream:true + SSE 事件聚合；
// 网关以非流式语义消费）；流式 → StreamResponses（SDK 载荷重帧 SSE 透传）。
//
// 预处理沿用 setModel 改写（ModelMapping 对 codex 账号生效，不静默
// 失效）；不 stripImageTools（用户裁决：strip 仅针对 resp/resp-ws 的
// responses-special——codex 上游自带 image 处理）。
//
// 错误分类（骨架 statusOf/upstreamBody 零改动复用）：
//   - 信封（SDK *HTTPError 包装——403 账号无权限等）→ 4xx 透传 / 429/5xx
//     failover 既有分类（failover 循环按 code 分类）
//   - fatal（errors.As 五类）→ 适配层已统一回调上报（账号失效标记 +
//     FailAccount 快照摘除——failover 不重试同账号）；code 0 → 连接级
//     MarkResult(RuleKindOf(0)) + 转移其它账号（与 images 路径同语义）
//   - RefreshError/网络 → code 0 → failover 可重试
func (p *Proxy) callCodexResponses(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, body []byte, stream bool) (int, []byte, bool, error) {
	// 客户端请求模型（日志口径）：gjson 顶层提取（与 typed 流式路径同款，
	// 1 次分配；codex 分支无完整 params 解析）。
	reqModel := gjson.GetBytes(body, "model").String()
	if p.codex == nil {
		// 适配层未装配（SetCodex 未调用）：显式 501（防 nil 误走凭据缺失 502）。
		o := mergeDispatchBase(ctx, codexDispatchBase(sel, reqModel, reqID, start, usageTuple{}, nil))
		o.Result = ResultFailed
		o.HTTPStatus = AttemptStatus(http.StatusNotImplemented)
		o.Commit = CommitUpstreamResponded
		o.Terminal = true
		o.BusinessFrameSent = false
		emitCodexOutcome(ctx, p, sel, o, rule.Kind5xx, errCodexResponsesNotIntegrated.msg)
		sel.Release()
		p.recordRejected(r.Context(), reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), domain.FormatOpenAIResponses, http.StatusNotImplemented, domain.ErrBilling, 0, usageTuple{}, start, errCodexResponsesNotIntegrated.msg)
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
	// Codex 端点归 SDK 官方默认，网关不再派生 BaseURL。
	cred := domain.CredentialFromExt(sel.Ext)
	if stream {
		return p.streamCodexResponses(ctx, w, r, reqID, groupID, start, sel, reqModel, &cred, body)
	}
	return p.nonstreamCodexResponses(ctx, w, r, reqID, groupID, start, sel, reqModel, &cred, body)
}

// clientTurnState 客户端请求自带 x-codex-turn-state（透传优先——客户端
// 自管，非空覆盖网关 held 注入；空 = 未带 → 网关补服务端签发过的空缺）。
func clientTurnState(r *http.Request) string {
	if r == nil {
		return ""
	}
	return r.Header.Get(codexsdk.HeaderTurnState)
}

// --- turn-state 轮边界判定（真实代码实证钉死） ---
// 真实实例归属：轮级——ModelClientSession.new_session 每轮新建
// Arc<OnceLock<String>>（core/src/client.rs:498 "fresh turn-scoped streaming
// session"），值 = 实例生命周期内首次 set 后恒定（握手响应头
// responses_websocket.rs:538 + 流事件 :742 二次 set 被 OnceLock 忽略）；轮结束
// = 会话销毁（跨轮不回传）。真实轮结束判定（stream_events_utils.rs:318-328
// needs_follow_up + turn.rs:150-152）：响应输出项为工具调用 → 轮继续；纯
// message/reasoning → 轮结束。真实测试套件实证（core/tests/suite/turn_state.rs
// persists_within_turn_and_resets_after）：completed 响应（含工具调用）后同轮
// 后续请求仍带 ts，仅跨轮清除——不在 response.completed 即清。

// isCodexCallItemType 轮继续信号判定：输出项类型为工具调用型——wire type 族
// 恒以 "_call" 结尾（function_call/shell_command_call/computer_call/
// web_search_call/local_shell_call/custom_tool_call 等——对齐真实
// stream_events_utils.rs needs_follow_up 语义）。
func isCodexCallItemType(t string) bool { return strings.HasSuffix(t, "_call") }

// codexTurnEndedBody 非流式合成体轮结束判定：output 无工具调用项 → 轮结束
// （SDK 聚合器成功返回必已收到 response.completed 终态——合成体恒 completed）。
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

// nonstreamCodexResponses 直连 codex resp 非流式（薄封装 core：注入直连 output——
// 合成体原样透传、日志口径 openai-responses）。直连路径逐字节不变。
func (p *Proxy) nonstreamCodexResponses(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, cred *domain.AccountCredential, body []byte) (int, []byte, bool, error) {
	return p.nonstreamCodexResponsesCore(ctx, r, reqID, groupID, start, sel, reqModel, cred, body, &codexDirectOutput{w: w})
}

// nonstreamCodexResponsesCore 非流式 codex resp core（渲染经注入 output——core
// 体内无转换分支）：setModel 改写（与 typed 非流式 SDK 路径 params.Model =
// sel.Model 等价；短路守卫零分配）→ 适配层 Responses → out.Body 写出（直连 =
// 合成体原样转发 application/json；转换 = responses 合成体经 protoconv 反向映射
// 回客户端协议 + 客户端模型回填）→ 合成体顶层 usage 提取（SDK 把 response.usage
// 提升到合成体顶层；无 type 字段）。turn-state：客户端自带 → 透传优先；未带 →
// 网关注入 held（上游签发值——同轮回传）；合成体无工具调用项（轮结束）→
// ClearTurnState（跨轮不回传）。日志 Format 由 out.LogFormat() 自报。
func (p *Proxy) nonstreamCodexResponsesCore(ctx context.Context, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, cred *domain.AccountCredential, body []byte, out codexRespOutput) (int, []byte, bool, error) {
	streamBody, err := setModel(body, sel.Model)
	if err != nil {
		return 0, nil, false, err
	}
	// 非流式超时：HTTPClient.Timeout 不可用（流式/非流式四方法共享，
	// 覆盖整个响应体读取会切断长流式 SSE）——非流式方法各自包 ctx 超时；TCP
	// 黑洞读停滞 → 超时触发 → 连接级错误转移（并发槽/连接/goroutine 不无限占用，
	// failover 可转移）。
	ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamTimeout)
	defer cancel()
	// 伪装身份：注入源 = 运行时槽身份（Selection.CodexIdentity 一次给全四元组，
	// installation_id 亦取自槽状态）——同账号并发各持不同槽 ⇒ 上游见多条独立会话。
	// HTTP 面经 SDK client_metadata 注入。
	sess, meta := sel.CodexIdentity()
	resp, err := p.codex.Responses(ctx, cred, streamBody, &sess, &meta, clientTurnState(r))
	if err != nil {
		if r.Context().Err() != nil {
			return 0, nil, false, r.Context().Err()
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return statusOf(err), upstreamBody(err), false, err
		}
		code := statusOf(err)
		return code, upstreamBody(err), false, err
	}
	// 写出字节：直连原样透传合成体；转换路径反向映射回客户端协议（+ 客户端模型
	// 回填）。转换失败在任何字节写出前返回，交 failover 分类（→ 500）。
	if err := out.Body(resp.Raw); err != nil {
		return http.StatusInternalServerError, nil, false, err
	}
	// 轮结束清除：合成体成功返回必含 completed 终态——无工具调用项
	// → 轮结束 → 清除 held（跨轮不回传；适配层已回写本次响应签发值）。
	if codexTurnEndedBody(resp.Raw) {
		p.codex.ClearTurnState(sel.AccountID)
	}
	// 合成体顶层 usage：缺失/显式 null → ok=false → 恒 0。
	var it, ot, tt, cr, cc int64
	if t, ok := responsesBodyUsage(resp.Raw); ok {
		it, ot, tt, cr, cc = t.it, t.ot, t.tt, t.cr, t.cc
		// 轮次推进：成功取到 usage 时 Step 一次。
		sel.AdvanceIdentity()
	}
	var img int64 // resp 检测功能调用计数（旁路；respImageDetectOn 门控）——落 CallCount
	if respImageDetectOn(sel) {
		img = respImageCountBody(resp.Raw)
	}
	ut := usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc, calls: img}
	o := mergeDispatchBase(ctx, codexDispatchBase(sel, reqModel, reqID, start, ut, nil))
	o.Result = ResultSuccess
	o.HTTPStatus = AttemptStatus(http.StatusOK)
	o.Commit = CommitResponseStarted
	o.BusinessFrameSent = true
	o.Terminal = true
	emitCodexOutcome(ctx, p, sel, o, rule.KindOK, "")
	p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), out.LogFormat(), http.StatusOK, domain.ErrNone, ut, start)))
	return http.StatusOK, nil, true, nil
}

// streamCodexResponses 直连 codex resp 流式（薄封装 core：注入直连 output——SDK
// 载荷重帧 `data: <payload>\n\n` 透传；日志口径 openai-responses）。直连路径
// 逐字节不变（帧规格 / [DONE] 补发 / outcome / log 皆同现状）。
func (p *Proxy) streamCodexResponses(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, cred *domain.AccountCredential, body []byte) (int, []byte, bool, error) {
	return p.streamCodexResponsesCore(ctx, r, reqID, groupID, start, sel, reqModel, cred, body, &codexDirectOutput{w: w})
}

// streamCodexResponsesCore 流式 codex resp core（渲染经注入 output——core 体内
// 无转换分支）：setModel 改写 → 适配层 StreamResponses（SDK 逐 data: 载荷零拷贝
// 回调）→ out.Frame 渲染写出（直连 = 重帧 `data: <payload>\n\n` + flush；转换 =
// 载荷经 protoconv.StreamMapper 映射为客户端协议完整 SSE 帧后直写 + 客户端模型
// 回填）+ out.End 流末收尾（直连补 `data: [DONE]`；转换零帧防御提交头，终止帧由
// 映射器自产：chat: [DONE] / mess: message_stop）。日志 Format 由 out.LogFormat()
// 自报。
//
// 首帧响应头由 output 自管（直连 begin() 首次 Frame/End 提交；转换仅确写帧时
// begin()——drop 帧不提交头）。首帧前失败 → 头未提交，HTTP 状态可用 →
// (code, body, false) 交 failover 循环正常分类（4xx 透传 / 429/5xx 转移 /
// 耗尽 502——typed 分支 ResponseStreamRaw 非 200 同语义）。
//
// 断开/超时收尾镜像 caller_responses.go 双分支：客户端断开
// （r.Context().Err() != nil）→ 不 MarkResult（finish 200 ErrAbort——上游已消
// 费请求，token 取断前已收 usage 帧）；上游超时/错误（帧已写出，200 已定型）
// → recordStreamAbort 语义 + 连接级/5xx 分流。
//
// usage 嗅探：fn 内精确判定 type 后读 response.usage（取首个命中帧——
// completed 终态恒唯一，usage 只读一次）。TTFT 在 out.Frame 之前记录（drop 帧
// 也记——现状）。framesWritten 由 Frame/End 回传的 wrote 复现（判据不变）。
func (p *Proxy) streamCodexResponsesCore(ctx context.Context, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, cred *domain.AccountCredential, body []byte, out codexRespOutput) (int, []byte, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamStreamTimeout)
	defer cancel()
	streamBody, err := setModel(body, sel.Model)
	if err != nil {
		return 0, nil, false, err
	}
	var (
		it, ot, tt, cr, cc int64
		img                int64 // resp 检测功能调用计数（旁路；respImageDetectOn 门控）——落 CallCount
		usageTaken         bool  // 首个 completed 帧已取（usage 只读一次）
		framesWritten      bool  // 首帧已写出（头已提交；首帧前失败 → HTTP 状态可用）
		turnCallSeen       bool  // 流中轮继续信号（工具调用输出项——轮边界）
		ttft               *int64
	)
	// 伪装身份同非流式；turn-state 透传优先（客户端自带覆盖 held 注入）。
	sess, meta := sel.CodexIdentity()
	err = p.codex.StreamResponses(ctx, cred, streamBody, &sess, &meta, clientTurnState(r), func(raw []byte) error {
		if !usageTaken {
			// 热路径：字节扫描 type 精确判定（防正文含子串帧冻结）+
			// response.usage（零分配）；首个命中后跳过（终态事件唯一）。
			if hit, ok := sniffResponsesCompletedUsage(raw); ok {
				it, ot, tt, cr, cc = hit.it, hit.ot, hit.tt, hit.cr, hit.cc
				if respImageDetectOn(sel) {
					img = respImageCountCompleted(raw)
				}
				// 轮次推进：成功取到 usage 时 Step 一次。
				sel.AdvanceIdentity()
				usageTaken = true
			}
		}
		// 轮边界嗅探：工具调用输出项 → 轮继续（同轮后续请求续传
		// turn-state）；命中后跳过（每响应一次判定足够）。
		if !turnCallSeen && sniffCodexTurnCallItem(raw) {
			turnCallSeen = true
		}
		// TTFT 先于写出记录（drop 帧也记——现状）：out.Frame 的 drop 路径
		// 不写任何字节，但 TTFT 仍按首个上游载荷计时。
		if ttft == nil {
			ms := time.Since(start).Milliseconds()
			ttft = &ms
		}
		wrote, werr := out.Frame(raw)
		framesWritten = framesWritten || wrote
		return werr
	})
	if err != nil {
		// 客户端断开：上游已消费请求（成功），仍须记录用量（成功请求丢日志防
		// 线——caller_responses.go 语义）；按 abort 收尾不 MarkResult。
		if r.Context().Err() != nil {
			if framesWritten {
				ut := usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc, calls: img}
				o := mergeDispatchBase(ctx, codexDispatchBase(sel, reqModel, reqID, start, ut, ttft))
				o.Result = ResultClientCancel
				o.HTTPStatus = 0
				o.Commit = CommitResponseStarted
				o.BusinessFrameSent = true
				o.Terminal = true
				emitCodexOutcome(ctx, p, sel, o, 0, "")
				p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), out.LogFormat(), http.StatusOK, domain.ErrAbort, ut, start)))
				return 0, nil, true, nil
			}
			return 0, nil, false, r.Context().Err()
		}
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && r.Context().Err() == nil {
			return statusOf(err), upstreamBody(err), false, err
		}
		// 首帧前信封错误（4xx 透传 / 429/5xx failover——typed 分支
		// ResponseStreamRaw 非 200 同语义）：未写出任何帧，可返回 HTTP 状态由
		// failover 循环分类。
		if !framesWritten {
			code := statusOf(err)
			return code, upstreamBody(err), false, err
		}
		// 上游停滞/错误（流中止）：200 已写出——recordStreamAbort + 连接级/5xx 分流
		// 中途失败为已发送业务帧后的网络中断，记为 ResponseStarted 的失败 outcome，保留健康观测。
		ut := usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc, calls: img}
		o := mergeDispatchBase(ctx, codexDispatchBase(sel, reqModel, reqID, start, ut, ttft))
		o.Result = ResultFailed
		o.HTTPStatus = 0
		o.Commit = CommitResponseStarted
		o.BusinessFrameSent = true
		o.Terminal = true
		emitCodexOutcome(ctx, p, sel, o, rule.KindNetwork, err.Error())
		p.recordStreamAbort(ctx, reqID, groupID, start, sel, reqModel, ut, err)
		return 0, nil, true, nil
	}
	// 轮结束清除：流正常结束且收到 completed 终态（usageTaken）且无
	// 工具调用项（!turnCallSeen）→ 轮结束 → 清除 held（跨轮不回传）。错误/断开
	// 路径不清（轮结束未知——不误清同轮续传值）。
	if usageTaken && !turnCallSeen {
		p.codex.ClearTurnState(sel.AccountID)
	}
	logCtx := ctx
	if ttft != nil {
		logCtx = context.WithValue(ctx, ctxKeyTTFT{}, ttft)
	}
	// 流末收尾（out.End）：直连补发 data: [DONE]\n\n + flush（补发失败 = 客户端
	// 断开 → 按 abort 收尾，上游已正常完成——usage 照记）；转换零帧防御（病态
	// 上游仅发 [DONE]——Frame 从未调用、头未提交）：此刻才提交头；终止帧由映射器
	// 自产（chat: [DONE] / mess: message_stop），core 不补。
	wrote, endErr := out.End()
	framesWritten = framesWritten || wrote
	if endErr != nil {
		ut := usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc, calls: img}
		o := mergeDispatchBase(ctx, codexDispatchBase(sel, reqModel, reqID, start, ut, ttft))
		o.Result = ResultClientCancel
		o.HTTPStatus = 0
		o.Commit = CommitResponseStarted
		o.BusinessFrameSent = true
		o.Terminal = true
		emitCodexOutcome(ctx, p, sel, o, 0, "")
		p.finish(sel, logWithCtx(logCtx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), out.LogFormat(), http.StatusOK, domain.ErrAbort, ut, start)))
		return 0, nil, true, nil
	}
	ut := usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc, calls: img}
	o := mergeDispatchBase(ctx, codexDispatchBase(sel, reqModel, reqID, start, ut, ttft))
	o.Result = ResultSuccess
	o.HTTPStatus = AttemptStatus(http.StatusOK)
	o.Commit = CommitClientCommitted
	o.BusinessFrameSent = true
	o.Terminal = true
	emitCodexOutcome(ctx, p, sel, o, rule.KindOK, "")
	p.finish(sel, logWithCtx(logCtx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), out.LogFormat(), http.StatusOK, domain.ErrNone, ut, start)))
	return http.StatusOK, nil, true, nil
}

// --- codex responses 输出渲染 seam（注入式 output 策略） ---
//
// codexRespOutput 渲染 codex responses 上游响应给客户端：
//   - 直连 = 合成体/载荷原样重帧透传；
//   - 协议转换 = protoconv 反向映射回客户端协议。
//
// core 只拨号/取用量/记 outcome，渲染经此注入——core 体内无任何转换分支。
// 首帧响应头由实现自管（含零帧防御）；Frame/End 回传 wrote 供 core 复现
// framesWritten 语义（分类客户端断开 / 首帧前信封错误 / 零帧防御）。
type codexRespOutput interface {
	// Frame 渲染并写出一个流式载荷（SDK 交付的 bare JSON）。
	//   提交/写出字节 → (true, err)；按映射语义丢弃（drop，未写任何字节）→ (false, nil)
	//   （供 core 判「首帧前失败」：drop 不当成已写出）。
	Frame(raw []byte) (wrote bool, err error)
	// End 流末收尾：直连补 data: [DONE]；转换空操作（终止帧由映射器自产）。
	// 仍负责零帧时的响应头提交（上游正常结束但从未回调 → 至少提交 SSE 头）。
	//   提交/写出字节 → true；写出错误 → err（直连 [DONE] 失败 = 客户端断开）。
	End() (wrote bool, err error)
	// Body 写出非流式合成体。仅「转换前置失败」（ConvertResponse 出错、字节未写出）
	// 返回 error（→ 500）；写出阶段错误按现状忽略（响应已定型）。
	Body(raw []byte) error
	// LogFormat 日志 Format 口径（直连 = openai-responses；转换 = 客户端协议）。
	LogFormat() domain.RequestFormat
}

// codexDirectOutput 直连渲染（现状默认）：合成体/载荷原样透传；started 自管头
// 提交（首次 Frame/End 时提交 SSE 头——首帧前失败不提交，HTTP 状态可用）。
type codexDirectOutput struct {
	w       http.ResponseWriter
	started bool
}

func (o *codexDirectOutput) begin() { beginSSEOnce(o.w, &o.started) }

func (o *codexDirectOutput) Frame(raw []byte) (bool, error) {
	o.begin()
	return true, writeCodexSSEFrame(o.w, raw) // begin 已提交头 ⇒ 恒 true
}

func (o *codexDirectOutput) End() (bool, error) {
	o.begin()
	return true, writeCodexSSEFrame(o.w, sseDonePayload)
}

func (o *codexDirectOutput) Body(raw []byte) error {
	o.w.Header().Set("Content-Type", "application/json")
	o.w.WriteHeader(http.StatusOK)
	// 写出阶段错误按现状忽略：WriteHeader(200) 已定型响应，状态码/头不可再改，
	// 无 failover 可分类路径（非流式无「首帧前失败」语义）。与 caller_chat /
	// caller_responses / caller_images 非流式写路径同款（见 codexRespOutput.Body 契约）。
	_, _ = o.w.Write(raw)
	return nil
}

func (o *codexDirectOutput) LogFormat() domain.RequestFormat { return domain.FormatOpenAIResponses }

// codexConvertedOutput 协议转换渲染：上游 responses 输出经 protoconv 反向映射回
// 客户端协议（mapper 懒建——非流式路径不分配）；clientModel 非空 → 逐帧/合成体
// 回填客户端模型。started 自管头提交（仅确写帧/End 时提交——drop 帧不提交头）。
type codexConvertedOutput struct {
	w           http.ResponseWriter
	dir         domain.ProtocolConvert
	clientModel string
	mapper      *protoconv.StreamMapper // 懒建：仅流式首个 Frame 建一次
	started     bool
}

func newCodexConvertedOutput(w http.ResponseWriter, dir domain.ProtocolConvert, clientModel string) *codexConvertedOutput {
	return &codexConvertedOutput{w: w, dir: dir, clientModel: clientModel}
}

func (o *codexConvertedOutput) begin() { beginSSEOnce(o.w, &o.started) }

func (o *codexConvertedOutput) Frame(raw []byte) (bool, error) {
	if o.mapper == nil {
		o.mapper = protoconv.NewStreamMapper(o.dir)
	}
	frame, drop := o.mapper.Map("", raw)
	if drop {
		return false, nil // 未写任何字节（不提交头）
	}
	if o.clientModel != "" {
		frame = rewriteConvertedFrames(frame, o.clientModel)
	}
	o.begin()
	return true, writeSSEFrameRaw(o.w, frame)
}

func (o *codexConvertedOutput) End() (bool, error) {
	o.begin()
	return true, nil
}

func (o *codexConvertedOutput) Body(raw []byte) error {
	conv, err := protoconv.ConvertResponse(raw, o.dir)
	if err != nil {
		return fmt.Errorf("protocol response conversion failed: %w", err)
	}
	if o.clientModel != "" {
		conv = rewriteResponseModelJSON(conv, o.clientModel)
	}
	o.w.Header().Set("Content-Type", "application/json")
	o.w.WriteHeader(http.StatusOK)
	// 写出阶段错误按现状忽略：响应已由 WriteHeader(200) 定型，无恢复路径（转换
	// 前置失败已在入口返回 error → 500，见 codexRespOutput.Body 契约）。
	_, _ = o.w.Write(conv)
	return nil
}

func (o *codexConvertedOutput) LogFormat() domain.RequestFormat {
	c, _ := clientAndTargetOf(o.dir)
	return c
}

// SSE 帧常量（codex 流式分支专用——SDK 交付载荷重帧；typed 面由 sserelay 全
// 帧原样透传含 event:/[DONE]，线格式不同）。
var (
	sseDataPrefix  = []byte("data: ")
	sseFrameSuffix = []byte("\n\n")
	sseDonePayload = []byte("[DONE]")
)

// beginSSE 提交 SSE 响应头：writeSSEHeaders 三件套 + 显式 WriteHeader(200)
// （SDK 载荷直写无 sserelay 首帧隐式写头，须显式下发）。提交后不可再改状态码，
// 仅在确知要写首帧时调用（首帧前失败不调用 → 头未提交，HTTP 状态可由 failover
// 循环正常分类）。
func beginSSE(w http.ResponseWriter) {
	writeSSEHeaders(w)
	w.WriteHeader(http.StatusOK)
}

// beginSSEOnce 幂等提交 SSE 头：首个确写帧/End 时调用一次，之后置 sent 短路。
// codexDirectOutput 与 codexConvertedOutput 的自管头提交共用此单一实现。
func beginSSEOnce(w http.ResponseWriter, sent *bool) {
	if !*sent {
		beginSSE(w)
		*sent = true
	}
}

// writeCodexSSEFrame 逐帧重帧写出（`data: <payload>\n\n` + flush——SDK
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

// writeSSEFrameRaw 直写完整 SSE 帧字节（映射帧已含 data:/event: 行与结尾空行）
// + flush。与 writeCodexSSEFrame 区分：后者把 SDK 载荷重包 `data: ` 前缀；映射
// 分支的帧由 protoconv 组装为完整帧，直写不得再包前缀。fn 内立即调用（映射帧
// 复用 mapper 缓冲，仅本次 Map 调用期有效）。
func writeSSEFrameRaw(w http.ResponseWriter, frame []byte) error {
	if _, err := w.Write(frame); err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}
