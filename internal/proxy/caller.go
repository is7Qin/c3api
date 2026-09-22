// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"

	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/continuation"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/protoconv"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/logx"
)

var convertRequest = protoconv.ConvertRequest

// forwardRoute 一次转发的路由信息（格式 + 调用器 + 请求体）：默认 = 客户端
// 格式直连（零转换）；协议转换命中时替换为模板协议路由（格式/调用器/
// 已转换请求体）。failover 循环按 route 重选号（模板协议），日志仍按客户端
// 协议记录（buildLog format 参数不变）。
type forwardRoute struct {
	format domain.RequestFormat
	caller UpstreamCaller
	body   []byte
}

// convertedRoute 组协议转换方向集合 → （模板协议格式, 转换方向）：仅当配置方向
// 的客户端协议与本次请求格式一致才返回转换（其余请求格式不受影响）；空集合
// （off）→ 无。多方向按客户端格式命中第一个匹配方向——同客户端格式多方向已被
// 创建/更新校验拒绝（service），至多命中一个。只补差语义的缺口判定（客户端
// 协议无路由或无可用账号）由调用方负责。
func convertedRoute(converts []domain.ProtocolConvert, client domain.RequestFormat) (domain.RequestFormat, domain.ProtocolConvert, bool) {
	for _, pc := range converts {
		switch pc {
		case domain.ProtocolConvertChatToResp:
			if client == domain.FormatOpenAIChat {
				return domain.FormatOpenAIResponses, pc, true
			}
		case domain.ProtocolConvertMessToResp:
			if client == domain.FormatAnthropic {
				return domain.FormatOpenAIResponses, pc, true
			}
		case domain.ProtocolConvertRespToMess:
			if client == domain.FormatOpenAIResponses {
				return domain.FormatAnthropic, pc, true
			}
		case domain.ProtocolConvertChatToMess:
			if client == domain.FormatOpenAIChat {
				return domain.FormatAnthropic, pc, true
			}
		}
	}
	return "", "", false
}

// newReqID 生成 32 位 hex 请求 ID（仅日志关联键，DB 无格式约束；math/rand/v2
// 免 crypto/rand syscall——非安全用途，GC 削减）。
func newReqID() string {
	var b [16]byte
	binary.LittleEndian.PutUint64(b[0:8], rand.Uint64())
	binary.LittleEndian.PutUint64(b[8:16], rand.Uint64())
	return hex.EncodeToString(b[:])
}

// UpstreamCaller 一格式一实现：完成单次上游调用（含流式写出、客户端断开判定
// 与 usage 记录）。记录职责全在 caller（finish/buildLog/recordStreamAbort/
// MarkResult 直接可用）；骨架只做 code 分支（429/5xx 转移、4xx
// 透传记录）、handled 短路与耗尽 record。凭据值经 aiclient 格式方法传入
// （头名 aiclient 内组装，正交延续）。
//
// 语义：
//   - handled == true → 请求已处理完毕（成功/客户端断开/流中止已记录；本地拒绝
//     已写出无记录），骨架直接 return（不可转移）
//   - handled == false → 上游未接受，骨架接手：
//     code 429 → MarkResult(Kind429) + Release + 转移
//     code >= 500 或 code == 0（连接级/凭据错）→ MarkResult(RuleKindOf(code)) + Release + 转移
//     code 4xx（err == nil）→ 骨架 finish(buildLog(Err4xx)) + 透传 respBody
//     （空 → 网关文案 "upstream rejected request"）
//   - err 非 nil 仅在错误路径返回（分类由 code 承载）；骨架用它提取错误文本
//     （部署故障修复）：code==0 → err.Error() 落 ErrorMessage/last_error +
//     Warn（err 全文），4xx → respBody 原文落 ErrorMessage。成功路径 err 恒
//     nil（零新增分配）。
//   - 例外（首字节前客户端断连，分类正确性）：code==0 且 r.Context().Err()!=nil
//     （客户端已断开）→ 记 499+ErrAbort 立即返回——不 failover、不 MarkResult、
//     不冷却（否则连接级误分类把无辜账号冷却 + failover 空转）。
type UpstreamCaller interface {
	Call(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64,
		start time.Time, sel *scheduler.Selection, cred string, body []byte, stream bool) (code int, respBody []byte, handled bool, err error)
}

// extractTier 提取并归一化请求体 service_tier（resp-ws 专用，纯函数；HTTP 路径
// 由 handleFormat 的 scanKeys 单遍提取直接判定——同语义不同入口）：gjson 顶层
// 读取零分配；类型错误（非 string/null）→ error（WS 错误帧语义，调用方决定
// 写出与拒绝记录）；空/未知 → TierAuto（auto 兜底，无 hasTier 返回——
// TierPolicy 只对 priority/flex/fast 生效，auto 恒透传不策略，hasTier 无独立
// 信息量）。
func extractTier(body []byte) (billing.Tier, error) {
	tierVal := gjson.GetBytes(body, "service_tier")
	if tierVal.Type != gjson.String && tierVal.Type != gjson.Null {
		return billing.TierAuto, errors.New("service_tier must be a string")
	}
	return billing.NormalizeTier(tierVal.String()), nil
}

// handleFormat 通用转发入口（openai-chat/openai-responses/anthropic/openai-
// images 四格式共用——从原 HandleXxx 提取）：guardPipeline（鉴权 → reqMeta ctx
// 注入 → quota → 余额预检 → 两级并发门禁 → 限流，见 pipeline.go）→ 读体 →
// json.Valid + scanKeys 单遍顶层提取（stream/model/service_tier）→ 选号 →
// failoverLoop（chatAttempt + httpSink，precheck=true）→ 耗尽记录写出。差异段
// 留本文件：读请求阶段（multipart 分支/scanKeys 三键/tier 策略）与 convertedRoute
// 转换补差（chat 专属，不抽象）。门禁热路径全部内存原子（零 DB 零锁——复核仅
// 预算耗尽的 key 触发，额度边缘低频慢路径）；release 与 quota 扣减在请求结束
// 统一完成。
func (p *Proxy) handleFormat(format domain.RequestFormat, w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := newReqID()
	r, rm, level, ok := p.guardPipeline(w, r, format, reqID, start, true)
	if !ok {
		return
	}
	// 门禁释放：先释并发门禁后减 inflight（与现状 defer LIFO 同序——release
	// 合并两者的展开形态，见 guardPipeline 注释）。
	defer p.inflight.Add(-1)
	defer p.auth.Release(rm.meta, level)
	groupID := rm.meta.GroupID

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, p.cfg.MaxBodySize))
	if err != nil {
		writeErr(w, errBody)
		return
	}
	// images 端点专用 body 分支：multipart 跳过 json.Valid 硬门
	// 与 gjson 顶层提取（下述 JSON 校验/stream 探测/body 重写对 multipart
	// 全部失效——multipart 字节对 json.Valid 必然 false，撞门即误杀）；model
	// 从 form 字段取；图片文件原样透传（不解析内容）；不做
	// setModel/setStreamAndModel JSON 重写（form model 字段原样透传，spec
	// §5.1 声明）。JSON 形态照常：model/stream 顶层提取 + service_tier 归一化。
	var reqModel string
	var stream bool
	if format == domain.FormatOpenAIImages && isMultipartForm(r.Header.Get("Content-Type")) {
		reqModel = imagesMultipartModel(body, r.Header.Get("Content-Type"))
		// stream 恒 false：multipart 无流式形态（stream 探测仅 JSON 路径）
	} else {
		// SDK v1.x 参数里没有 Stream 字段（流式由 NewStreaming 在请求选项层注入
		// "stream": true），故从原始请求体探测 stream 标志决定走流式还是非流式。
		// model 一并在此提取（不解析完整 params）；service_tier（
		// 计费）同次提取。GC 削减：json.Valid 单遍校验（零分配）保留 400 语义 +
		// scanKeys 单遍顶层提取三键（spec 2026-08-16-single-pass-parse-design：
		// 每 JSON 请求 4 遍全文档扫描 → 2 遍——gjson.ParseBytes 方案经证伪弃用）。
		// 值判定与现状 gjson Type 校验语义精确等价：stream 非 bool/null、model/
		// service_tier 非 string/null → 400（显式 null 与缺失等同零值语义，与
		// encoding/json 一致；字符串形态经 decodeUnicodeEscapes 取值）。400 响应
		// 消息文案随校验方式变化（无测试断言原文；错误码/无记录/Select 前无
		// 并发槽语义逐字不变）。
		if !json.Valid(body) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: invalid JSON"}})
			return
		}
		var vals [6][]byte
		scanKeys(body, requestAffinityKeys, vals[:])
		streamRaw, modelRaw, tierRaw := vals[0], vals[1], vals[2]
		affinityHash, hasAffinity := affinityIdentity(vals[3], vals[4], vals[5])
		rm.AffinityHash = affinityHash
		rm.HasAffinity = hasAffinity
		switch {
		case streamRaw == nil || bytes.Equal(streamRaw, falseBytes) || bytes.Equal(streamRaw, nullBytes):
			stream = false
		case bytes.Equal(streamRaw, trueBytes):
			stream = true
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: stream must be a boolean"}})
			return
		}
		modelVal, ok := parseStringValue(modelRaw)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: model must be a string"}})
			return
		}
		reqModel = string(modelVal)
		tierVal, ok := parseStringValue(tierRaw)
		if !ok {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: service_tier must be a string"}})
			return
		}
		tier := billing.NormalizeTier(string(tierVal))
		// service_tier 归一化 + 转发策略（计费启用才处理；auto/空/未知恒透传）：
		// strip → 转发体删该字段；reject → 直接 400（记 ErrBilling，不转发）。
		// 归一化 tier 补入已入 ctx 的 reqMeta（GC 削减 免第二次 WithValue+
		// WithContext；非计费路径 hasTier=false → BillingTier 恒空）。
		if p.bill != nil {
			rm.tier = tier
			rm.hasTier = true
			if (tier == billing.TierPriority || tier == billing.TierFlex || tier == billing.TierFast) && p.bill.TierPolicy != nil {
				switch p.bill.TierPolicy(tier) {
				case billing.TierPolicyStrip:
					if body, err = stripServiceTier(body); err != nil {
						writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: " + err.Error()}})
						return
					}
				case billing.TierPolicyReject:
					writeErr(w, errServiceTierRejected)
					p.recordRejected(r.Context(), reqID, groupID, 0, reqModel, "", format, http.StatusBadRequest, domain.ErrBilling, 0, usageTuple{}, start, errServiceTierRejected.msg)
					return
				}
			}
		}
	}

	// 硬续接（仅 Responses）：previous_response_id 把派生钉死在产出该响应的
	// 账号上。普通请求（无续接键、其余全部格式）零 Redis。解析在计划就绪后
	// 执行（route class 取计划规范身份，与 create 侧绑定键同源），失败一律
	// fail-closed（missing/expired→410、Redis 不可用→503），零上游拨号。
	contPrevID := ""
	if format == domain.FormatOpenAIResponses {
		contPrevID = gjson.GetBytes(body, "previous_response_id").String()
	}

	// 路由信息：格式 + 调用器 + 请求体。默认 = 客户端格式直连（零转换）；
	// images 端点按请求路径选调用器（generations/edits 上游子路径不同）；
	// 协议转换（只补差）命中时整体替换为模板协议路由。
	route := forwardRoute{format: format, caller: p.callers[format], body: body}
	if format == domain.FormatOpenAIImages {
		route.caller = p.imagesCallerFor(r)
	}

	identity := scheduler.AttemptPlanIdentity{RequestID: reqID, UserID: rm.meta.UserID, ApplyModelMapping: true, AffinityHash: rm.AffinityHash, HasAffinity: rm.HasAffinity}
	var sel *scheduler.Selection
	var plan scheduler.AttemptPlan
	var attempt scheduler.Attempt
	if format == domain.FormatOpenAIImages {
		if ic, ok := route.caller.(*imagesCaller); ok {
			rr := scheduler.RouteRefForOp(groupID, string(format), reqModel, ic.operationTag())
			sel, plan, attempt, err = p.selectWithPlanForRoute(rr, groupID, format, reqModel, identity)
		} else {
			sel, plan, attempt, err = p.selectWithPlan(groupID, format, reqModel, identity)
		}
	} else {
		sel, plan, attempt, err = p.selectWithPlan(groupID, format, reqModel, identity)
	}
	// converted route uses target identity when fallback
	if err != nil && (errors.Is(err, scheduler.ErrFormatUnavailable) || errors.Is(err, scheduler.ErrNoAvailable) || errors.Is(err, scheduler.ErrAttemptsExhausted)) {
		if tgt, conv, ok := convertedRoute(rm.meta.ProtocolConverts, format); ok {
			// Target identity uses same request identity but target RouteClassID
			targetIdentity := scheduler.AttemptPlanIdentity{RequestID: reqID, UserID: rm.meta.UserID, ApplyModelMapping: true, AffinityHash: rm.AffinityHash, HasAffinity: rm.HasAffinity}
			if sel2, plan2, attempt2, err2 := p.selectWithPlan(groupID, tgt, reqModel, targetIdentity); err2 == nil {
				sel2GuardActive := true
				defer func() {
					if sel2GuardActive {
						if r := recover(); r != nil {
							sel2.Release()
							panic(r)
						}
					}
				}()
				cb, cerr := convertRequest(body, conv)
				if cerr != nil {
					sel2.Release()
					sel2GuardActive = false
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: protocol conversion failed: " + cerr.Error()}})
					return
				}
				sel2GuardActive = false
				sel, plan, attempt = sel2, plan2, attempt2
				err = nil
				route = forwardRoute{format: tgt, caller: p.convCallers[conv], body: cb}
			} else {
				// Preserve target error (distinguish ErrNoAvailable vs AttemptsExhausted)
				err = err2
			}
		}
	}
	// Plan-only contract: selection succeeded only with a compiled plan and a
	// canonical reserved attempt; any absent/invalid compiled plan lands here
	// as a typed error and fails closed (no legacy selection lane exists).
	if err != nil {
		p.handleSelectError(w, err)
		p.recordRejected(r.Context(), reqID, groupID, 0, reqModel, "", format, statusFor(err), domain.ErrNoAccount, 0, usageTuple{}, start, selectErrorMessage(err))
		return
	}
	// 续接解析：计划规范 route class 下查绑定；missing/expired/Redis 不可用
	// fail-closed（释放已占并发槽，零上游拨号）。
	var contBinding *continuation.Binding
	if contPrevID != "" {
		b, ferr := p.contResolve(r.Context(), rm.meta.UserID, groupID, contProtocolREST, contPrevID, attempt)
		if ferr != nil {
			sel.Release()
			p.recordRejected(r.Context(), reqID, groupID, 0, reqModel, "", format, ferr.status, domain.Err4xx, 0, usageTuple{}, start, ferr.msg)
			writeErr(w, ferr)
			return
		}
		contBinding = b
	}
	// 续接钉选：计划推进到绑定账号（其余候选释放跳过）；绑定账号身份漂移或
	// 不可派生 → fail-closed，绝不迁移到其他账号。
	if contBinding != nil {
		pinned, pinnedAttempt, ferr := p.contPin(&plan, sel, attempt, contBinding)
		if ferr != nil {
			p.recordRejected(r.Context(), reqID, groupID, contBinding.AccountID, reqModel, "", format, ferr.status, domain.Err4xx, 0, usageTuple{}, start, ferr.msg)
			writeErr(w, ferr)
			return
		}
		sel, attempt = pinned, pinnedAttempt
	}
	defer leaseGuard(sel)

	// failover 循环（共享骨架，见 pipeline.go）：precheck=true（chat/resp/
	// anthropic/images 走缺价预检）；尾部推进走入口编译计划（选号时已按
	// route.format 绑定路由，协议转换命中即目标路由）；记录仍按客户端
	// format（buildLog 参数不变）。差异状态按值传入 attemptState（零分配
	// ——attempt/sink 为 New 构造单例）。
	p.failoverLoopWithPlan(w, r, format, reqID, groupID, start, reqModel, route.body, sel, plan, attempt,
		attemptState{format: format, routeFormat: route.format, caller: route.caller, stream: stream, hardContinuation: contBinding != nil},
		p.chatAttempt, p.httpSink, true)
}

// chatAttempt handleFormat 的 attempt 实现（覆盖 chat/responses/anthropic/
// images 四格式——共用同一循环骨架；无状态单例，per-request 差异经
// attemptState 流入）。差异段：codex images 分流（按当轮 sel.CredentialType）、
// credentialFor、caller.Call、Warn 文案（"upstream connection failure"——两版
// 本保留不统一，循环不代发）与 SDK 校验错误识别（"streaming is required"
// 本地拒绝——chat 专属）。
type chatAttempt struct{ p *Proxy }

func (a *chatAttempt) call(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, body []byte, st attemptState) (int, []byte, http.Header, bool, error) {
	// TODO(P22-I1): 当前 hdr 恒 nil（UpstreamCaller.Call 未回收 resp.Header），
	// 仅 fallback 1 生效；待扩展 Header 透传后替换为真实透传
	// （Global Constraints 豁免 fallback 保留）
	// codex 分流落位（§2，B 的 501 骨架）：images 端点 codex-oauth/
	// codex-pat 模板选号命中 → codexImagesCaller（GenerateImage 非流式 /
	// GenerateImageStream 流式 已接——caller 内 stream 分支同签名直赋）。
	// 适配层未装配（SetCodex nil）→ 501 显式拒绝，不让凭据缺失路径误报
	// 502/network。caller 每轮自 st.caller 起算 = 天然复位：
	// 混合类型组 failover 跨类型换账号（codex 失败 → api_key 尝试）时复用旧
	// codexImagesCaller 会把健康 api_key 账号路由到 Ext=nil 空凭据路径
	// （502 + 错误率污染 + 无谓失效上报 account 0）。
	caller := st.caller
	if st.routeFormat == domain.FormatOpenAIImages && isCodexCredentialType(sel.CredentialType) {
		caller = a.p.codexImagesFor(r)
	}
	// 凭据每轮取：尾部 Select 后 Selection 变化，凭据随账号；
	// 循环外取一次会把旧账号 key 发给新账号上游。codex 类型跳过单字符串
	// credentialFor（注册表无 codex provider——单字符串契约表达不了复合
	// 凭据；codexImagesCaller 按 sel.Ext 派生 AccountCredential 直供适配层）。
	var (
		code     int
		respBody []byte
		handled  bool
		callErr  error
	)
	if isCodexCredentialType(sel.CredentialType) {
		code, respBody, handled, callErr = caller.Call(ctx, w, r, reqID, groupID, start, sel, "", body, st.stream)
	} else {
		cred, err := a.p.credentialFor(ctx, sel)
		if err != nil {
			return 0, nil, nil, false, err // 凭据错误按网络错误处理（等价现状 try* 内 false,0,nil → 耗尽 ErrNetwork）
		}
		// err 保留（部署故障修复：错误文本落盘）：code 承载分类（0=连接级/
		// 凭据错、4xx、429、5xx），callErr 提供 err.Error() 文本——仅错误
		// 分支消费（成功路径零新增分配）。
		code, respBody, handled, callErr = caller.Call(ctx, w, r, reqID, groupID, start, sel, cred, body, st.stream)
	}
	// code==0 && callErr != nil 的网络错误分类（Warn + 文本提取）在循环内完成；
	// 本 attempt 只代发 Warn（文案 chat 版）与 SDK 校验错误识别（提前收尾）。
	// ctx.Err()==nil 判定与循环 499 分支同序（客户端断连不 Warn、不识别）。
	if code == 0 && callErr != nil && ctx.Err() == nil {
		sdkErr := callErr.Error()
		// Warn 留痕：两种子路径（SDK 识别归 4xx / 网络错误）同款——错误
		// 文本全量、request_id/account_id/model 字段，识别错误不因识别而丢留痕。
		if a.p.log != nil {
			a.p.log.Warn("upstream connection failure",
				logx.String("request_id", reqID),
				logx.Int64("account_id", sel.AccountID),
				logx.String("model", sel.Model),
				logx.Error(callErr))
		}
		// SDK 校验错误识别（spec，检测前置）：anthropic-sdk-go
		// v1.62.0 client.go:316（CalculateNonStreamingTimeout）对
		// max_tokens 大 + 非流式请求本地拒绝——无网络请求、无状态码
		// （code=0），此前误归 network（err_logs 记 network + 502 无
		// 原因文案，可观测性差）。固定文本匹配 strings.Contains 零分配；
		// 文本为 SDK 硬编码——升级 SDK 若改文案则识别静默失效，须同步
		// 更新本处与版本标注。
		if strings.Contains(sdkErr, "streaming is required") {
			// 确定性错误（重试必同结果，§5.3）：不 failover、不
			// MarkResult（与 4xx 分支现状一致），记录后提前 return。
			l := logWithCtx(ctx, a.p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), st.format, http.StatusBadRequest, domain.Err4xx, usageTuple{}, start))
			em := domain.TruncateErrMsg(sdkErr)
			l.ErrorMessage = &em
			a.p.finish(sel, l)
			// message 用 SDK 原文不加前缀（"streaming is required..." 已
			// 自明，避免措辞重复）；type 与 4xx 分支通用回退同款（可选）。
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
				"message": sdkErr, "type": "upstream_error",
			}})
			return 0, nil, nil, true, nil
		}
	}
	return code, respBody, nil, handled, callErr
}

// precheckPrice 缺价预检（统一 PriceEntry resolver，零 DB）。
func (p *Proxy) precheckPrice(format domain.RequestFormat, model string) error {
	if p.bill == nil || p.bill.Resolver == nil {
		return nil
	}
	rp, ok := p.bill.Resolver.ResolvePrices(model, 0, "", time.Now())
	if !ok {
		return errNoPrice
	}
	if format == domain.FormatOpenAIImages {
		if rp.ImgInTokPerM == nil && rp.ImgOutTokPerM == nil && rp.PricePerImage == nil {
			return errNoPrice
		}
	}
	return nil
}

// imagesCallerFor 按端点路径选 images 调用器（generations/edits 上游子路径
// 不同；两端点同一格式 openai-images——路径后缀区分，New 构造的调用器复用，
// per-request 零分配）。
func (p *Proxy) imagesCallerFor(r *http.Request) UpstreamCaller {
	if strings.HasSuffix(r.URL.Path, "/edits") {
		return p.imageEdits
	}
	return p.imageGenerations
}
