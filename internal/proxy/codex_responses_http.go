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
	"time"

	codexsdk "github.com/is7Qin/codex-sdk"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/protoconv"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/internal/sdkbridge"
	"github.com/is7qin/c3api/pkg/sserelay"
)

// --- codex 类型 resp HTTP 分支（codex-oauth/codex-pat 类型
// openai-responses 接入——SDK 合成非流式 + Stream SSE 透传） ---
// 独立文件族（用户拍板文件边界：codex 相关处理不散落现有 caller 文件）。
// 分支插入点 = responsesCaller.Call 入口按 sel.CredentialType 分流
// （caller_responses.go）；typed 段（api_key/responses-special）零改动。与 WS
// 变体（codex_responses_ws.go）同形态编排，差异在传输面：HTTP = SDK
// HTTPClient（Responses/StreamBody——sdkbridge 扩展），WS = SDK Dial。

// errCodexResponsesNotIntegrated 501：codex 适配层未装配（SetCodex 未调用——
// main 装配缺失的显式拒绝，不让凭据缺失路径误报 502/network；与 images/WS
// 路径同款）。
var errCodexResponsesNotIntegrated = &formatError{status: http.StatusNotImplemented, msg: "codex responses unavailable (adapter not wired)"}

// headerUpstreamTransport 生效传输头（spec §6）：原生 codex responses 车道的
// 每个响应都带"网关选了什么腿"，值 = transport.Name()（http|ws）。它不是契约、
// 不进 openapi/web，是实验仪器（A/B harness 的 effective 判据）。
const headerUpstreamTransport = "x-c3api-upstream-transport"

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

func emitCodexOutcome(ctx context.Context, p *Proxy, sel *scheduler.Selection, o AttemptOutcome, health *AttemptHealthEvent) {
	if sel == nil {
		return
	}
	if err := o.Validate(); err != nil {
		return
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
// 失效）；不 stripImageTools（strip 仅针对 resp/resp-ws 的
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
	// 生效传输头（spec §6）：每个响应都带"网关选了什么腿"。写在这里是**唯一**
	// 写点——它同时覆盖下面两条早退（p.codex == nil 的 501、sel.Ext == nil 的
	// errCodexExtMissing）与流式/非流式两个入口；每次 attempt 重设，WriteHeader
	// 之后不再变 ⇒ 语义正好是"产生首字节那条腿的传输"。转换车道绕过本函数
	// （直接调 core），因此天然不写头。
	w.Header().Set(headerUpstreamTransport, sel.CodexResponsesTransport.Name())
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
		emitCodexOutcome(ctx, p, sel, o, &AttemptHealthEvent{Kind: rule.Kind5xx, ErrorMessage: errCodexResponsesNotIntegrated.msg})
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
	cred := sel.Credential()
	if stream {
		return p.streamCodexResponses(ctx, w, r, reqID, groupID, start, sel, reqModel, &cred, body)
	}
	return p.nonstreamCodexResponses(ctx, w, r, reqID, groupID, start, sel, reqModel, &cred, body)
}

// codexTurnStateReplay 是 turn-state 重放策略的**单点常量**（§5.1）：
//   - false：本请求既不带 ts 也不捕获（Identity 的 ts 恒空、Sink = nil）；
//   - true：本请求带会话当前 ts，上游签发新值即覆盖（"更新即跟随、跟随即复用"）。
//
// 重放**不违反**真实客户端契约——恰恰是它的默认行为（codex-rs
// `core/src/client.rs`：`x-codex-turn-state` 自述 "sticky routing token captured
// earlier in the turn"；同一条连接上每个后续请求都把捕获值塞进
// `client_metadata`（:2311-2312），HTTP 车道则每个请求都带同名请求头
// （:1638-1642 → :2687-2703）；只有换账号/换 auth owner 才 reset（:1884-1886）。
// WS 握手头不带它（:1558-1566 与 :1498 传 None）——"只在帧里带"只对握手成立）。
//
// 因此 gate 不是"契约是否允许"，而是**收益未测**：开 true 后上游是否真的按 token
// 命中同一分片（prompt cache 局部性 → TTFT/成本），需要真实账号多轮 A/B 才能定量。
// 在那之前生产保持 false（错开不亏，开错可能把命中率变化记成"波动"）。
const codexTurnStateReplay = false

// codexRequestPrep 请求准备两步合一（§3.3）：取得条目句柄 → 对齐 owner（可能清 ts）
// → 构造 Identity。错误原样上抛，由调用方按各自车道既有的错误映射收口（HTTP /
// stream / ws 的状态面不同，不在这里归一）。replay 是本车道的 ts 重放策略（WS 车道
// 恒 false：身份随连接传入，客户端帧自带 ts，网关不二次注入）。
//
// 顺序是契约：`IdentityFor` 必须拿**本次请求准备好的条目句柄**的 owner（
// `Prepared.Owner`，条目构造时定型的稳定字段），不能拿请求里的凭据快照重新散列
// （条目尚未重建时会给出"新 owner + 旧 auth"），也不读池上的计数器（池整体替换会
// 丢）。先构造 Identity 再清 ts 更不行：换凭据后的第一个请求会带着旧 owner 的 ts
// 出去，事后追不回来。所以重放/清空是 `IdentityFor` 的**参数**，不是后置步骤。
func (p *Proxy) codexRequestPrep(sel *scheduler.Selection, cred *domain.AccountCredential, replay bool) (sdkbridge.Prepared, codexsdk.Identity, error) {
	prep, err := p.codex.Prepare(cred)
	if err != nil {
		return sdkbridge.Prepared{}, codexsdk.Identity{}, err
	}
	return prep, sel.IdentityFor(prep.Owner(), replay), nil
}

// nonstreamCodexResponses 直连 codex resp 非流式（薄封装 core：注入直连 output——
// 合成体原样透传、日志口径 openai-responses）。直连路径逐字节不变。
func (p *Proxy) nonstreamCodexResponses(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, cred *domain.AccountCredential, body []byte) (int, []byte, bool, error) {
	return p.nonstreamCodexResponsesCore(ctx, r, reqID, groupID, start, sel, reqModel, cred, body, &codexDirectOutput{w: w}, sel.CodexResponsesTransport)
}

// nonstreamCodexResponsesCore 非流式 codex resp core（渲染经注入 output——core
// 体内无转换分支）：setModel 改写（与 typed 非流式 SDK 路径 params.Model =
// sel.Model 等价；短路守卫零分配）→ 适配层 Responses → out.Body 写出（直连 =
// 合成体原样转发 application/json；转换 = responses 合成体经 protoconv 反向映射
// 回客户端协议 + 客户端模型回填）→ 合成体顶层 usage 提取（SDK 把 response.usage
// 提升到合成体顶层；无 type 字段）。turn-state：属**会话**（ts 只经 Identity 发送、
// 只经 Sink 落回会话，§3.1）——本层不读也不回写；跨轮是否重放由 codexTurnStateReplay
// 单点策略决定。
// 日志 Format 由 out.LogFormat() 自报。
// transport 为出站传输实参（原生车道 = Selection 原值；转换车道钉 http）。
func (p *Proxy) nonstreamCodexResponsesCore(ctx context.Context, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, cred *domain.AccountCredential, body []byte, out codexRespBody, transport domain.CodexResponsesTransport) (int, []byte, bool, error) {
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
	// 请求级身份（§3.1）：**先准备条目句柄**（owner 与 auth/client 同版），
	// 再对齐 owner，最后由会话给出 Session/Meta/ts/Sink——同账号并发各持不同会话 ⇒
	// 上游见多条独立线程。身份经 SDK 每次调用传入，不再是连接/客户端的构造期配置。
	prep, id, err := p.codexRequestPrep(sel, cred, p.turnStateReplay)
	if err != nil {
		if r.Context().Err() != nil {
			return 0, nil, false, r.Context().Err()
		}
		return statusOf(err), upstreamBody(err), false, err
	}
	// 出站传输开关由调用方给值（原生车道 = Selection 原值；转换车道钉 http，
	// 见 caller_converted.go）。sdkbridge 侧按它分 client 条目——构造期参数，
	// 改模板 ⇒ 换条目。**同一个 prep 句柄**继续取 client/auth（不查第二次）。
	resp, err := p.codex.ResponsesPrepared(ctx, prep, sel.Egress, streamBody, id, transport)
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
	// 合成体顶层 usage：缺失/显式 null → ok=false → 恒 0。
	var it, ot, tt, cr, cc int64
	if t, ok := responsesBodyUsage(resp.Raw); ok {
		it, ot, tt, cr, cc = t.it, t.ot, t.tt, t.cr, t.cc
		// 推进（§3.3）：SDK 的聚合臂在返回前已 Close（连接同步归还进 idle 表），
		// 所以此刻推进/退休看得见那条连接。仅"成功取到 usage"才推进。
		sel.StepIdentity()
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
	emitCodexOutcome(ctx, p, sel, o, &AttemptHealthEvent{Kind: rule.KindOK})
	p.finish(sel, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), out.LogFormat(), http.StatusOK, domain.ErrNone, ut, start)))
	return http.StatusOK, nil, true, nil
}

// streamCodexResponses 直连 codex resp 流式（薄封装 core：注入直连 plan——上游
// 原始 SSE 字节 verbatim 透传；日志口径 openai-responses）。
func (p *Proxy) streamCodexResponses(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, cred *domain.AccountCredential, body []byte) (int, []byte, bool, error) {
	return p.streamCodexResponsesCore(ctx, w, r, reqID, groupID, start, sel, reqModel, cred, body, codexDirectStreamPlan(), sel.CodexResponsesTransport)
}

// streamCodexResponsesCore 流式 codex resp core（渲染经注入 plan——core 体内无
// 转换分支）：setModel 改写 → 适配层 StreamBody 取上游 raw body → 复用 native
// 字节级 relay（sserelay.Relay + Output：完整帧转发 / 自适应批量 flush / 静默
// 保活）：直连 verbatim（原始 SSE 字节含 event:/[DONE] 原样透传）；converted 经
// plan.mapper（protoconv.StreamMapper 适配 sserelay seam，先过滤源协议 [DONE]）
// 映射为客户端协议帧 + 客户端模型回填，流末按「目标终止帧尚未产生」补发
// （plan.finish）。日志 Format 由 plan.logFormat 自报。
// transport 为出站传输实参（原生车道 = Selection 原值；转换车道钉 http）。
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
func (p *Proxy) streamCodexResponsesCore(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, cred *domain.AccountCredential, body []byte, plan codexStreamPlan, transport domain.CodexResponsesTransport) (int, []byte, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamStreamTimeout)
	defer cancel()
	streamBody, err := setModel(body, sel.Model)
	if err != nil {
		return 0, nil, false, err
	}
	// 请求级身份同非流式（先准备条目句柄，再对齐 owner——同样必须先于身份快照）。
	prep, id, err := p.codexRequestPrep(sel, cred, p.turnStateReplay)
	if err != nil {
		if r.Context().Err() != nil {
			return 0, nil, false, r.Context().Err()
		}
		return streamUpstreamStatus(statusOf(err)), upstreamBody(err), false, err
	}
	// 出站传输开关同非流式（原生车道 = Selection 原值；转换车道钉 http）。
	// 同一个 prep 句柄继续取 client/auth（不查第二次）。
	resp, err := p.codex.StreamBodyPrepared(ctx, prep, sel.Egress, streamBody, id, transport)
	if err != nil {
		// 客户端断开（上游响应前）优先归因。
		if r.Context().Err() != nil {
			return 0, nil, false, r.Context().Err()
		}
		// 首帧前信封错误（4xx 透传 / 429/5xx failover / fatal）：未写出任何
		// 字节，HTTP 状态可用，交 failover 循环分类。SDK StreamBody 对所有非
		// 200 返 *HTTPError（含 201/204/3xx）——上游 2xx-非-200 归一 502
		// （streamUpstreamStatus），不可直接 statusOf 当 4xx 处理。
		return streamUpstreamStatus(statusOf(err)), upstreamBody(err), false, err
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
		ttft               *int64
		contEnqueued       bool // 首个有效可续接 id 已入队（每流一次）
	)
	err = sserelay.Relay(ctx, w, resp.Body, sserelay.Config{
		Output:   out,
		Mapper:   plan.mapper,
		Terminal: plan.finish,
		// 流内错误载荷 = 本流失败（spec-inband-error-truth §3-3）：**仅直连**
		// （plan.mapper == nil）。转换子车道（mapper != nil）的协议收尾依赖该帧
		// 产出 closeOpenBlocks + event: error，吞帧会让客户端停在半截内容块
		// （§6-9 本轮不接）。
		FailOnErrorPayload: plan.mapper == nil,
		OnEvent: func(ev sserelay.Event) {
			// 唯一写出前采样 seam（含 drop 帧）：TTFT 仅对**真实 payload** 计时
			// （旧 SDK 只对真实 data 行回调——纯注释/控制帧、仅 [DONE] 不计；
			// 保真「输出前、drop 帧也记」）。
			claim := bytes.TrimSpace(ev.Data)
			if ttft == nil && len(claim) != 0 && !bytes.Equal(claim, []byte("[DONE]")) {
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
					usageTaken = true
				}
			}
			// 续接入队：与 native 同一 seam（首个有效响应 id 写出前快照入队
			// 一次）。仅**直连**（客户端 Responses）入队——converted 客户端为
			// Chat/Messages，无法消费 Responses 续接 id（native 同款限定）。
			if !contEnqueued && plan.contEligible && p.contBindWired() {
				if id := contFrameID(ev.Data); id != "" {
					p.contEnqueue(ctx, contProtocolREST, id, groupID)
					contEnqueued = true
				}
			}
		},
	})
	_ = resp.Body.Close()
	// 推进/退休（§3.3 顺序契约）：relay 读到终态 → Close（连接同步归还进 idle 表）
	// → Step（退休此刻才看得见那条连接）→ finish。提前 Close / 取消 / 读取失败
	// **不推进不退休**；failover 重试因此不可能"先推进再失败"。
	if err == nil && usageTaken {
		sel.StepIdentity()
	}
	if ttft != nil {
		ctx = context.WithValue(ctx, ctxKeyTTFT{}, ttft)
	}
	ut := usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc, calls: img}
	if err != nil {
		// 统一出口判定（§3.7）：取消/写失败不补写；未提交交 pipeline；已提交写 SSE error。
		exit := classifyStreamExit(ctx, out, err, AttemptUsage{InputTokens: it, OutputTokens: ot, CacheReadTokens: cr, CacheCreationTokens: cc, CallCount: img}, ttft)
		switch exit {
		case streamExitClientCancel:
			// 0a 客户端断开：上游已消费请求（成功），仍须记录用量（成功请求丢日志
			// 防线）；按 abort 收尾不 MarkResult、不投递健康事件。
			// 交付事实快照：本分支不补写 error 帧，仍按统一时点先取事实。
			framesSent, writeFailed := out.BusinessFrameSent(), out.WriteFailed()
			o := mergeDispatchBase(ctx, codexDispatchBase(sel, reqModel, reqID, start, ut, ttft))
			o.Result = ResultClientCancel
			o.HTTPStatus = 0
			o.Commit = CommitResponseStarted
			o.BusinessFrameSent = true
			o.Terminal = true
			emitCodexOutcome(ctx, p, sel, o, nil)
			p.finish(sel, withCommit(logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), plan.logFormat, http.StatusOK, domain.ErrAbort, ut, start)), commitStateFor(framesSent, writeFailed, true)))
			return 0, nil, true, nil
		case streamExitUncommitted:
			// 未提交读取失败 → pipeline（handled=false，可 failover/写 JSON）；缓冲残余丢弃。
			// body 带上游原文（SDK *HTTPError.RawJSON）：否则 429 行的 ErrorMessage
			// 恒空（pipeline.go:355-358 从 body 取 message），"网关侧看不到为什么"的
			// 原始症状会以"看到 429 但不知道原因"的形式残留。
			return statusOf(err), upstreamBody(err), false, err
		}
		// 已提交 / 写失败：终态归因只在这里算一次（§3 序 0b–0c + 1–5）。
		f := streamFailureOf(ctx, exit, err)
		// 交付事实快照（必须在补写 SSE error 帧之前）：补写失败会把 WriteFailed
		// 由 false 翻成 true，晚读会让同一次故障的取值漂移。
		framesSent, writeFailed := out.BusinessFrameSent(), out.WriteFailed()
		if f.ET == domain.ErrAbort {
			// 0c 客户端侧结束（下行写不进去）：abort 行 + 不投递健康事件。
			// clientCancel 由归因派生（序 0c ⇒ abort）。
			p.recordStreamAbort(ctx, reqID, groupID, start, sel, reqModel, ut, commitStateFor(framesSent, writeFailed, clientEnded(exit, f)), err)
			return 0, nil, true, nil
		}
		if out.Committed() {
			writeClientStreamError(out, plan.logFormat, err)
		}
		o := mergeDispatchBase(ctx, codexDispatchBase(sel, reqModel, reqID, start, ut, ttft))
		o.Result = ResultFailed
		o.HTTPStatus = 0
		// 类型层占位编码统一收口（typeLayerTerminalFailure）：Validate 对
		// 「failed + status 0 + terminal」只接受 sent_ambiguous/response_started/
		// client_committed 且都要求 BusinessFrameSent=true。真实交付事实由
		// commitStateFor 从 out 的写出事实另算，持久化列不读本字段——二者不得
		// 互相喂值（见 helper 注释）。
		o.Commit, o.BusinessFrameSent = typeLayerTerminalFailure()
		o.Terminal = true
		emitCodexOutcome(ctx, p, sel, o, streamFailureHealth(f))
		l := logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), plan.logFormat, f.Status, f.ET, ut, start))
		l.ErrorMessage = f.message()
		p.finish(sel, withCommit(l, commitStateFor(framesSent, writeFailed, clientEnded(exit, f))))
		return 0, nil, true, nil
	}
	o := mergeDispatchBase(ctx, codexDispatchBase(sel, reqModel, reqID, start, ut, ttft))
	o.Result = ResultSuccess
	o.HTTPStatus = AttemptStatus(http.StatusOK)
	o.Commit = CommitClientCommitted
	o.BusinessFrameSent = true
	o.Terminal = true
	emitCodexOutcome(ctx, p, sel, o, &AttemptHealthEvent{Kind: rule.KindOK})
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
	// contEligible 是否允许异步续接入队：仅直连（客户端 Responses，id 可被
	// 客户端消费）为 true；converted（客户端 Chat/Messages）为 false。仅此
	// 开关联合 `contFrameID`——不回退到映射帧 id。
	contEligible bool
	// logFormat 日志 Format 口径。
	logFormat domain.RequestFormat
}

// codexDirectStreamPlan 直连 verbatim plan：无 mapper/finish（上游原始字节含
// event:/[DONE]），日志口径 openai-responses；直连客户端为 Responses → 允许续接。
func codexDirectStreamPlan() codexStreamPlan {
	return codexStreamPlan{contEligible: true, logFormat: domain.FormatOpenAIResponses}
}

// codexConvertedStreamPlan 协议转换 plan：经 protoconv.StreamMapper 适配
// sserelay seam（源协议 [DONE] 由适配层过滤），流末按目标终止帧条件补发。
// 客户端为 Chat/Messages → 不允许续接入队。
func codexConvertedStreamPlan(dir domain.ProtocolConvert, clientModel string) codexStreamPlan {
	a := &codexConvertedStreamMapper{
		mapper:      protoconv.NewStreamMapper(dir),
		clientModel: clientModel,
	}
	client, _ := clientAndTargetOf(dir)
	return codexStreamPlan{
		mapper:    a.mapEvent,
		finish:    a.finish,
		logFormat: client,
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
