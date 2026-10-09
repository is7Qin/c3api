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
	"github.com/is7qin/c3api/pkg/sserelay"
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
// 网关以非流式语义消费）；流式 → StreamBody（SDK 返回原始 body）→ 复用 native
// 字节级 relay（sserelay.Relay + Output）透传/转换。
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
func (p *Proxy) nonstreamCodexResponsesCore(ctx context.Context, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, cred *domain.AccountCredential, body []byte, out codexRespBody) (int, []byte, bool, error) {
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

// streamCodexResponses 直连 codex resp 流式（薄封装 core：注入直连 plan——上游
// 原始 SSE 字节 verbatim 透传；日志口径 openai-responses）。
func (p *Proxy) streamCodexResponses(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, cred *domain.AccountCredential, body []byte) (int, []byte, bool, error) {
	return p.streamCodexResponsesCore(ctx, w, r, reqID, groupID, start, sel, reqModel, cred, body, codexDirectStreamPlan())
}

// streamCodexResponsesCore 流式 codex resp core（渲染经注入 plan——core 体内无
// 转换分支）：setModel 改写 → 适配层 StreamBody 取上游 raw body → 复用 native
// 字节级 relay（sserelay.Relay + Output：完整帧转发 / 自适应批量 flush / 静默
// 保活）：直连 verbatim（原始 SSE 字节含 event:/[DONE] 原样透传）；converted 经
// plan.mapper（protoconv.StreamMapper 适配 sserelay seam，先过滤源协议 [DONE]）
// 映射为客户端协议帧 + 客户端模型回填，流末按「目标终止帧尚未产生」补发
// （plan.finish）。日志 Format 由 plan.logFormat 自报。
//
// 采集统一在**唯一写出前 seam**（sserelay.Config.OnEvent，与 native 五路同一回调
// 实例）：TTFT（保真「输出前、drop 帧也记」）/usage/图像计数/轮次 hooks/续接入队
// 全在此采样，取消写后二次采样。
//
// 首帧响应头由 Output 自管（仅确写帧/Commit 时提交）。首帧前失败 → 头未提交，
// HTTP 状态可用 → (code, body, false) 交 failover 循环正常分类（4xx 透传 /
// 429/5xx 转移 / 耗尽 502）。
//
// 断开/超时/已提交错误收尾走统一出口判定 classifyStreamExit（§3.7）：客户端取消
// → 不 MarkResult（200 ErrAbort——上游已消费请求，token 取断前已收 usage）；
// 未提交读取失败 → pipeline；已提交 → 客户端协议 SSE error + recordStreamAbort。
func (p *Proxy) streamCodexResponsesCore(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, cred *domain.AccountCredential, body []byte, plan codexStreamPlan) (int, []byte, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamStreamTimeout)
	defer cancel()
	streamBody, err := setModel(body, sel.Model)
	if err != nil {
		return 0, nil, false, err
	}
	// 伪装身份同非流式；turn-state 透传优先（客户端自带覆盖 held 注入）。
	sess, meta := sel.CodexIdentity()
	resp, err := p.codex.StreamBody(ctx, cred, streamBody, &sess, &meta, clientTurnState(r))
	if err != nil {
		// 客户端断开（上游响应前）优先归因。
		if r.Context().Err() != nil {
			return 0, nil, false, r.Context().Err()
		}
		// 首帧前信封错误（4xx 透传 / 429/5xx failover / fatal）：未写出任何
		// 字节，HTTP 状态可用，交 failover 循环分类。
		return statusOf(err), upstreamBody(err), false, err
	}
	// SDK StreamBody 仅 200 放行；防御性再校验（上游 2xx-非-200 归一 502）。
	if resp.StatusCode != http.StatusOK {
		rb := readUpstreamBody(resp)
		_ = resp.Body.Close()
		return streamUpstreamStatus(resp.StatusCode), rb, false, nil
	}
	writeSSEHeaders(w)
	out := sserelay.NewOutput(w, p.cfg.StreamKeepaliveInterval, sserelay.OutputOptions{Ctx: ctx, Cancel: cancel})
	defer out.Release()
	var (
		it, ot, tt, cr, cc int64
		img                int64 // resp 检测功能调用计数（旁路；respImageDetectOn 门控）——落 CallCount
		usageTaken         bool  // 首个 completed 帧已取（usage 只读一次）
		turnCallSeen       bool  // 流中轮继续信号（工具调用输出项——轮边界）
		ttft               *int64
		contEnqueued       bool // 首个有效可续接 id 已入队（每流一次）
	)
	err = sserelay.Relay(ctx, w, resp.Body, sserelay.Config{
		Output: out,
		Mapper: plan.mapper,
		OnEvent: func(ev sserelay.Event) {
			// 唯一写出前采样 seam（含 drop 帧）：TTFT 先于写出记录（保真
			// 「输出前、drop 帧也记」——首个上游载荷即计时）。
			if ttft == nil {
				ms := time.Since(start).Milliseconds()
				ttft = &ms
			}
			// 热路径：字节扫描 type 精确判定（防正文含子串帧冻结）+
			// response.usage（零分配）；首个命中后跳过（终态事件唯一）。
			if !usageTaken {
				if hit, ok := sniffResponsesCompletedUsage(ev.Data); ok {
					it, ot, tt, cr, cc = hit.it, hit.ot, hit.tt, hit.cr, hit.cc
					if respImageDetectOn(sel) {
						img = respImageCountCompleted(ev.Data)
					}
					// 轮次推进：成功取到 usage 时 Step 一次。
					sel.AdvanceIdentity()
					usageTaken = true
				}
			}
			// 轮边界嗅探：工具调用输出项 → 轮继续（同轮后续请求续传
			// turn-state）；命中后跳过（每响应一次判定足够）。
			if !turnCallSeen && sniffCodexTurnCallItem(ev.Data) {
				turnCallSeen = true
			}
			// 续接入队：与 native 同一 seam（首个有效响应 id 写出前快照入队
			// 一次）。direct 取上游帧响应 id；converted 取 mapper 显式返回的
			// 客户端可续接 id（不从映射帧机械解析）。
			if !contEnqueued && p.contBindWired() {
				id := ""
				if plan.bindableID != nil {
					id = plan.bindableID()
				} else {
					id = contFrameID(ev.Data)
				}
				if id != "" {
					p.contEnqueue(ctx, contProtocolREST, id, groupID)
					contEnqueued = true
				}
			}
		},
	})
	_ = resp.Body.Close()
	// converted 终止帧补发（三情形恰一个目标终止帧）：仅正常结束（err==nil）且
	// 目标终止帧尚未产生时补发（直连 verbatim 不补——上游原始字节含 [DONE]）。
	// 补发帧在 relay 返回后写入，须显式 DrainFlush（relay 的流末 drain 不覆盖
	// 此帧；首帧后小帧仅入 Output 写缓冲，不 drain 会被 Release 丢弃）。
	if err == nil && plan.finish != nil {
		if f := plan.finish(); f != nil {
			if _, werr := out.WriteFrame(f); werr != nil {
				err = werr
			} else if derr := out.DrainFlush(); derr != nil {
				err = derr
			}
		}
	}
	// 轮结束清除：正常结束且收到 completed 终态（usageTaken）且无工具调用项
	// （!turnCallSeen）→ 轮结束 → 清除 held（跨轮不回传）。错误/断开路径不清
	// （轮结束未知——不误清同轮续传值）。
	if err == nil && usageTaken && !turnCallSeen {
		p.codex.ClearTurnState(sel.AccountID)
	}
	if ttft != nil {
		ctx = context.WithValue(ctx, ctxKeyTTFT{}, ttft)
	}
	ut := usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc, calls: img}
	if err != nil {
		// 统一出口判定（§3.7）：取消/写失败不补写；未提交交 pipeline；已提交写 SSE error。
		switch classifyStreamExit(ctx, out, err, AttemptUsage{InputTokens: it, OutputTokens: ot, CacheReadTokens: cr, CacheCreationTokens: cc, CallCount: img}, ttft) {
		case streamExitClientCancel:
			// 客户端断开：上游已消费请求（成功），仍须记录用量（成功请求丢日志
			// 防线）；按 abort 收尾不 MarkResult。
			o := mergeDispatchBase(ctx, codexDispatchBase(sel, reqModel, reqID, start, ut, ttft))
			o.Result = ResultClientCancel
			o.HTTPStatus = 0
			o.Commit = CommitResponseStarted
			o.BusinessFrameSent = true
			o.Terminal = true
			emitCodexOutcome(ctx, p, sel, o, 0, "")
			p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), plan.logFormat, http.StatusOK, domain.ErrAbort, ut, start)))
			return 0, nil, true, nil
		case streamExitUncommitted:
			// 未提交读取失败 → pipeline（handled=false，可 failover/写 JSON）；缓冲残余丢弃。
			return statusOf(err), nil, false, err
		default: // 写失败 / 已提交
			if out.Committed() {
				writeClientStreamError(out, plan.logFormat, err)
			}
			// 上游停滞/错误（流中止）：200 已写出——recordStreamAbort + 连接级/5xx
			// 分流，保留健康观测。
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
	}
	o := mergeDispatchBase(ctx, codexDispatchBase(sel, reqModel, reqID, start, ut, ttft))
	o.Result = ResultSuccess
	o.HTTPStatus = AttemptStatus(http.StatusOK)
	o.Commit = CommitClientCommitted
	o.BusinessFrameSent = true
	o.Terminal = true
	emitCodexOutcome(ctx, p, sel, o, rule.KindOK, "")
	p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), plan.logFormat, http.StatusOK, domain.ErrNone, ut, start)))
	return http.StatusOK, nil, true, nil
}

// --- codex responses 输出渲染 seam ---
//
// codexRespBody 渲染 codex responses 上游**非流式**响应给客户端（Body/LogFormat
// 两方法；流式已改走 sserelay raw relay，不再经此 seam 逐帧重帧）：
//   - 直连 = 合成体原样透传；
//   - 协议转换 = protoconv 反向映射回客户端协议。
//
// core 只拨号/取用量/记 outcome，渲染经此注入——core 体内无任何转换分支。
type codexRespBody interface {
	// Body 写出非流式合成体。仅「转换前置失败」（ConvertResponse 出错、字节未写出）
	// 返回 error（→ 500）；写出阶段错误按现状忽略（响应已定型）。
	Body(raw []byte) error
	// LogFormat 日志 Format 口径（直连 = openai-responses；转换 = 客户端协议）。
	LogFormat() domain.RequestFormat
}

// codexDirectOutput 直连非流式渲染：合成体原样透传。
type codexDirectOutput struct {
	w http.ResponseWriter
}

func (o *codexDirectOutput) Body(raw []byte) error {
	o.w.Header().Set("Content-Type", "application/json")
	o.w.WriteHeader(http.StatusOK)
	// 写出阶段错误按现状忽略：WriteHeader(200) 已定型响应，状态码/头不可再改，
	// 无 failover 可分类路径（非流式无「首帧前失败」语义）。与 caller_chat /
	// caller_responses / caller_images 非流式写路径同款（见 codexRespBody.Body 契约）。
	_, _ = o.w.Write(raw)
	return nil
}

func (o *codexDirectOutput) LogFormat() domain.RequestFormat { return domain.FormatOpenAIResponses }

// codexConvertedOutput 协议转换非流式渲染：上游 responses 合成体经 protoconv 反向
// 映射回客户端协议；clientModel 非空 → 回填客户端模型。
type codexConvertedOutput struct {
	w           http.ResponseWriter
	dir         domain.ProtocolConvert
	clientModel string
}

func newCodexConvertedOutput(w http.ResponseWriter, dir domain.ProtocolConvert, clientModel string) *codexConvertedOutput {
	return &codexConvertedOutput{w: w, dir: dir, clientModel: clientModel}
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
	// 前置失败已在入口返回 error → 500，见 codexRespBody.Body 契约）。
	_, _ = o.w.Write(conv)
	return nil
}

func (o *codexConvertedOutput) LogFormat() domain.RequestFormat {
	c, _ := clientAndTargetOf(o.dir)
	return c
}

// --- codex responses 流式下行 plan ---
//
// codexStreamPlan 描述 codex responses 流式下行：直连 verbatim 或经协议转换适配
// 后写入 sserelay 统一 relay（完整帧转发/保活/采样）。
type codexStreamPlan struct {
	// mapper sserelay 逐帧转换器；nil = 直连 verbatim（上游原始 SSE 字节含
	// event:/[DONE] 原样透传）。
	mapper func(sserelay.Event) ([]byte, bool)
	// finish 流末按「目标终止帧尚未产生」条件补发（converted 专用；direct = nil）。
	finish func() []byte
	// bindableID converted 续接 id（direct = nil——direct 走 contFrameID 解析上游帧）。
	bindableID func() string
	// logFormat 日志 Format 口径。
	logFormat domain.RequestFormat
}

// codexDirectStreamPlan 直连 verbatim plan：无 mapper/finish（上游原始字节含
// event:/[DONE]），日志口径 openai-responses。
func codexDirectStreamPlan() codexStreamPlan {
	return codexStreamPlan{logFormat: domain.FormatOpenAIResponses}
}

// codexConvertedStreamPlan 协议转换 plan：经 protoconv.StreamMapper 适配
// sserelay seam（源协议 [DONE] 由适配层过滤），流末按目标终止帧条件补发。
func codexConvertedStreamPlan(dir domain.ProtocolConvert, clientModel string) codexStreamPlan {
	a := &codexConvertedStreamMapper{
		mapper:      protoconv.NewStreamMapper(dir),
		clientModel: clientModel,
	}
	client, _ := clientAndTargetOf(dir)
	return codexStreamPlan{
		mapper:     a.mapEvent,
		finish:     a.finish,
		bindableID: a.mapper.BindableID,
		logFormat:  client,
	}
}

// codexConvertedStreamMapper 适配 codex converted 流式：先消费/过滤源协议
// [DONE]，再经 protoconv.StreamMapper 映射，客户端模型回填；流末按「目标终止
// 帧尚未产生」条件补发目标终止帧。过滤不搬进 StreamMapper.Map（它须保持纯
// name→frame，被非 codex native converted 复用——本 wrapper 仅覆盖 codex）。
type codexConvertedStreamMapper struct {
	mapper      *protoconv.StreamMapper
	clientModel string
}

// mapEvent sserelay.Mapper：源协议 [DONE] 丢弃（目标终止帧归目标 mapper / 适配层
// 流末补发）；其余经 StreamMapper 映射 + 客户端模型回填。
func (a *codexConvertedStreamMapper) mapEvent(ev sserelay.Event) ([]byte, bool) {
	if isSourceSSEDone(ev) {
		return nil, true
	}
	frame, drop := a.mapper.Map(string(ev.Event), ev.Data)
	if drop {
		return nil, true
	}
	if a.clientModel != "" {
		frame = rewriteConvertedFrames(frame, a.clientModel)
	}
	return frame, false
}

// finish 流末补发（目标终止帧尚未产生时）：镜像目标 mapper 自产终止帧。
func (a *codexConvertedStreamMapper) finish() []byte {
	if a.mapper.Done() {
		return nil
	}
	return a.mapper.Finish()
}

// isSourceSSEDone 判定帧是否为源协议 [DONE]（data 载荷命中；event 名无关）。
func isSourceSSEDone(ev sserelay.Event) bool {
	return bytes.Equal(ev.Data, []byte("[DONE]"))
}
