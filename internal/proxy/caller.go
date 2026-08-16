// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
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
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/protoconv"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/logx"
)

// forwardRoute 一次转发的路由信息（格式 + 调用器 + 请求体）：默认 = 客户端
// 格式直连（零转换）；协议转换（W5）命中时替换为模板协议路由（格式/调用器/
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
// 协议无路由）由调用方负责。
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
// 免 crypto/rand syscall——非安全用途，GC 削减 P6）。
func newReqID() string {
	var b [16]byte
	binary.LittleEndian.PutUint64(b[0:8], rand.Uint64())
	binary.LittleEndian.PutUint64(b[8:16], rand.Uint64())
	return hex.EncodeToString(b[:])
}

// UpstreamCaller 一格式一实现：完成单次上游调用（含流式写出、客户端断开判定
// 与 usage 记录）。记录职责全在 caller（finish/buildLog/recordStreamAbort/
// MarkResult 直接可用——评审 I-1）；骨架只做 code 分支（429/5xx 转移、4xx
// 透传记录）、handled 短路与耗尽 record。凭据值经 aiclient 格式方法传入
// （头名 aiclient 内组装，Phase 1 正交延续——评审 M-2）。
//
// 语义：
//   - handled == true → 请求已处理完毕（成功/客户端断开/流中止已记录；本地拒绝
//     已写出无记录），骨架直接 return（不可转移）
//   - handled == false → 上游未接受，骨架接手：
//     code 429 → MarkResult(Result429) + Release + 转移
//     code >= 500 或 code == 0（连接级/凭据错）→ MarkResult(ResultError) + Release + 转移
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

// extractTier 提取并归一化请求体 service_tier（HTTP 与 resp-ws 共用，纯函数）：
// gjson 顶层读取零分配；类型错误（非 string/null）→ error（HTTP 400 / WS 错误
// 帧语义，调用方决定写出与拒绝记录）；空/未知 → TierAuto（auto 兜底，无
// hasTier 返回——TierPolicy 只对 priority/flex/fast 生效，auto 恒透传不策略，
// hasTier 无独立信息量）。
func extractTier(body []byte) (billing.Tier, error) {
	tierVal := gjson.GetBytes(body, "service_tier")
	if tierVal.Type != gjson.String && tierVal.Type != gjson.Null {
		return billing.TierAuto, errors.New("service_tier must be a string")
	}
	return billing.NormalizeTier(tierVal.String()), nil
}

// handleFormat 通用转发骨架（从原 HandleXxx 提取，三格式共用）：鉴权 →
// quota 检查（本地预算快读；预算耗尽触发 DB 复核认领，见 gate.reclaim）→
// 两级并发门禁 acquire（user → key，失败回滚）→ 限流 →
// 读体 → stream 探测（peek unmarshal）+ model 提取（gjson，零分配）→ 选号 →
// failover 循环（每轮 credentialFor + caller.Call + code 分支）→ 耗尽记录写出。
// 门禁热路径全部内存原子（零 DB 零锁——复核仅预算耗尽的 key 触发，额度边缘
// 低频慢路径）；release 与 quota 扣减在请求结束统一完成。
func (p *Proxy) handleFormat(format domain.RequestFormat, w http.ResponseWriter, r *http.Request) {
	p.inflight.Add(1) // 优雅停机等在途归零（main waitForInflight 轮询 Inflight()）
	defer p.inflight.Add(-1)
	start := time.Now()
	reqID := newReqID()
	meta, ok := p.auth.Authenticate(r)
	if !ok {
		writeErr(w, errInvalidKey)
		// 评审 I-1：401 鉴权失败转 recordRejected（无效 key 洪水残留向量——
		// 401 也进 err_logs 错误审计，不再走 usage_logs 明细路径）。
		p.recordRejected(r.Context(), reqID, 0, 0, "", "", format, http.StatusUnauthorized, domain.ErrAuth, 0, usageTuple{}, start, errInvalidKey.msg)
		return
	}
	groupID := meta.GroupID
	// 请求元数据入 context（user_id/key_id 日志归属；不改变 Call/buildLog 签名）。
	// 单键单值 + 指针原地补 tier（GC 削减 P6：计费路径免第二次 WithValue+
	// WithContext；rm 指针只在请求 goroutine 内被读取/改写，logWithCtx 全程同
	// goroutine 同步访问——无跨 goroutine 竞态）。
	rm := &reqMeta{meta: meta}
	r = r.WithContext(context.WithValue(r.Context(), ctxKeyReqMeta{}, rm))

	// quota 检查在并发 acquire 之前（评审提醒①：失败无并发槽副作用；
	// 未设置额度 key 短路零成本；预算耗尽 → gate 内 DB 复核认领后再判定）
	if p.auth.QuotaExhausted(meta) {
		writeErr(w, errQuotaExhausted)
		p.recordRejected(r.Context(), reqID, groupID, 0, "", "", format, http.StatusTooManyRequests, domain.Err429, 0, usageTuple{}, start, errQuotaExhausted.msg)
		return
	}
	// 余额预检（Phase 5 计费；评审 I-1 无槽位问题）：快照读零 DB（滞后 ≤
	// BalanceRefreshInterval，多实例条件扣 DB 兜底）。快照缺失或 <0 → 402
	// errInsufficientBalance（不按 0 记账），但免费放行（T3.5，评审 I-1 修复）：
	// 有效倍率 0 = 免费用户/组 → 缺失/0 余额不 402（与 applyBilling 同一快照
	// 同一判定；cost 0 只记日志不扣费）。余额 0 放行——临时额度由 FEFO 扣费
	// 消化（billing_repo.go:71-76 先扣 temp）；负余额持续负债拒绝。快照缺失
	// 窗口内免费组照常放行；缺失且非免费 → 仍 402（用户不在快照 = 无余额
	// 记录，语义不变）。
	// 在 Acquire 前 → 不占用并发槽。
	if p.cfg.BillingCapture && p.bill != nil {
		bal, ok := p.bill.Balances.BalanceOf(meta.UserID)
		if (!ok || bal < 0) && p.bill.Balances.EffectiveMultiplier(meta.UserID, groupID) != 0 {
			writeErr(w, errInsufficientBalance)
			p.recordRejected(r.Context(), reqID, groupID, 0, "", "", format, http.StatusPaymentRequired, domain.ErrBilling, 0, usageTuple{}, start, errInsufficientBalance.msg)
			return
		}
	}
	// 两级并发门禁（user → key；两步回滚由 gate 内部完成；release 仅释放
	// 已 acquire 层级——defer 覆盖全部返回路径）
	acquired, ok := p.auth.Acquire(meta)
	if !ok {
		writeErr(w, errConcurrency)
		p.recordRejected(r.Context(), reqID, groupID, 0, "", "", format, http.StatusTooManyRequests, domain.Err429, 0, usageTuple{}, start, errConcurrency.msg)
		return
	}
	defer p.auth.Release(meta, acquired)

	if !p.limit.Allow(groupID, time.Now()) {
		writeErr(w, errRateLimit)
		// 架构审查 S5（用户裁决）：组限流 429 也进 err_logs（排障限流需要；
		// 与 401 同属拒绝路径——普通队列风暴采样丢弃兜底）。
		p.recordRejected(r.Context(), reqID, groupID, 0, "", "", format, http.StatusTooManyRequests, domain.Err429, 0, usageTuple{}, start, errRateLimit.msg)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, p.cfg.MaxBodySize))
	if err != nil {
		writeErr(w, errBody)
		return
	}
	// images 端点专用 body 分支（评审 P1-2）：multipart 跳过 json.Valid 硬门
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
		// model 一并在此提取（评审 I-2：不解析完整 params）；service_tier（Phase 5
		// 计费）同次提取。GC 削减 P3：json.Valid 单遍校验（零分配）保留 400 语义 +
		// gjson 顶层提取（Type 校验等价原 Unmarshal 的类型拒绝：stream 非 bool、
		// model/service_tier 非 string → 400；显式 null 与缺失等同零值语义，与
		// encoding/json 一致）。400 响应消息文案随校验方式变化（无测试断言原文；
		// 错误码/无记录/Select 前无并发槽语义逐字不变）。
		if !json.Valid(body) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: invalid JSON"}})
			return
		}
		streamVal := gjson.GetBytes(body, "stream")
		if streamVal.Type != gjson.True && streamVal.Type != gjson.False && streamVal.Type != gjson.Null {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: stream must be a boolean"}})
			return
		}
		modelVal := gjson.GetBytes(body, "model")
		if modelVal.Type != gjson.String && modelVal.Type != gjson.Null {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: model must be a string"}})
			return
		}
		tier, err := extractTier(body)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: " + err.Error()}})
			return
		}
		reqModel = modelVal.String()
		stream = streamVal.Type == gjson.True
		// service_tier 归一化 + 转发策略（计费启用才处理；auto/空/未知恒透传）：
		// strip → 转发体删该字段；reject → 直接 400（记 ErrBilling，不转发）。
		// 归一化 tier 补入已入 ctx 的 reqMeta（GC 削减 P6：免第二次 WithValue+
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

	// 路由信息：格式 + 调用器 + 请求体。默认 = 客户端格式直连（零转换）；
	// images 端点按请求路径选调用器（generations/edits 上游子路径不同）；
	// 协议转换（W5，只补差）命中时整体替换为模板协议路由。
	route := forwardRoute{format: format, caller: p.callers[format], body: body}
	if format == domain.FormatOpenAIImages {
		route.caller = p.imagesCallerFor(r)
	}

	sel, err := p.sched.Select(groupID, format, reqModel)
	if err != nil && errors.Is(err, scheduler.ErrFormatUnavailable) {
		// 补差语义：模板已支持客户端协议 → 直接转发零转换；组内无客户端协议
		// 路由（缺口）且组配置了转换方向 → 客户端协议 → 转换 → 模板协议路由。
		// off（默认）→ 上面的 errors.Is 分支零开销。ErrNoAvailable（有路由但
		// 全忙）不转换——组有客户端协议模板，按现状 429。
		if tgt, conv, ok := convertedRoute(meta.ProtocolConverts, format); ok {
			if sel2, err2 := p.sched.Select(groupID, tgt, reqModel); err2 == nil {
				cb, cerr := protoconv.ConvertRequest(body, conv)
				if cerr != nil {
					// 本地拒绝：目标 Select 已占并发槽，必须释放（与 caller 本地
					// 400 的 Release-only 语义一致），否则槽位永久泄漏。
					p.sched.Release(sel2.AccountID)
					writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: protocol conversion failed: " + cerr.Error()}})
					return
				}
				sel = sel2
				err = nil
				route = forwardRoute{format: tgt, caller: p.convCallers[conv], body: cb}
			} else {
				err = err2
			}
		}
	}
	if err != nil {
		p.handleSelectError(w, err)
		p.recordRejected(r.Context(), reqID, groupID, 0, reqModel, "", format, statusFor(err), domain.ErrNoAccount, 0, usageTuple{}, start, selectErrorMessage(err))
		return
	}

	// 注册表查找在 failover 循环外（评审 I-3）：格式固定，每轮不重查。
	caller := route.caller

	var (
		lastCode   int
		lastErrMsg string // 最后一次实际尝试的错误文本（耗尽路径 ErrorMessage 用）
		lastSel    = sel  // 最后一次实际尝试的 Selection；中途 Select 失败返回 nil 时不得解引用 sel
	)
	// 防呆（spec：failover_attempts=0 直构绕过 validate 下限）：循环零次执行时
	// 首次 Select 已占并发槽，耗尽路径按此标志补 Release——N>=1 恒 true，不双释放。
	attempted := false
	for attempt := 0; attempt < p.cfg.FailoverAttempts; attempt++ {
		lastSel = sel
		attempted = true
		// 缺价预检（评审 I-1 + P1-1 预检按格式切换）：每轮 sel 更新后、Call 前
		// 查价——计费启用时模型无价格 → 释放并发槽 + 402（不按 0 计价），零 DB
		// （快照读）。images 格式查 GetImagePrice（image_price 表；跳过 chat
		// 价预检 GetPrice——纯 image 价模型无 pricings 行，chat 预检会先行
		// 402 误杀，"ImagePrice 定生死"轮不到执行）；其余格式照旧。
		if err := p.precheckPrice(format, sel.Model); err != nil {
			p.sched.Release(sel.AccountID)
			p.recordRejected(r.Context(), reqID, groupID, sel.AccountID, reqModel, sel.Model, format, http.StatusPaymentRequired, domain.ErrBilling, 0, usageTuple{}, start, errNoPrice.msg)
			writeErr(w, errNoPrice)
			return
		}
		// codex 分流落位（T2 §2，B 的 501 骨架）：images 端点 codex-oauth/
		// codex-pat 模板选号命中 → codexImagesCaller（GenerateImage 非流式 /
		// GenerateImageStream 流式 T3 已接——caller 内 stream 分支同签名直赋）。
		// 适配层未装配（SetCodex nil）→ 501 显式拒绝，不让凭据缺失路径误报
		// 502/network。else 复位（评审 P1-1）：混合类型
		// 组 failover 跨类型换账号（codex 失败 → api_key 尝试）时复用旧
		// codexImagesCaller 会把健康 api_key 账号路由到 Ext=nil 空凭据路径
		// （502 + 错误率污染 + 无谓失效上报 account 0）——每轮按当轮
		// sel.CredentialType 双向赋值。
		if format == domain.FormatOpenAIImages {
			if isCodexCredentialType(sel.CredentialType) {
				caller = p.codexImagesFor(r)
			} else {
				caller = route.caller // 复位直连调用器（非 codex 尝试——含 codex→api_key 跨类型换账号）
			}
		}
		// 凭据每轮取（评审 I-3）：尾部 Select 后 Selection 变化，凭据随账号；
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
			code, respBody, handled, callErr = caller.Call(r.Context(), w, r, reqID, groupID, start, sel, "", route.body, stream)
		} else {
			cred, err := p.credentialFor(r.Context(), sel)
			if err != nil {
				code = 0 // 凭据错误按网络错误处理（等价现状 try* 内 false,0,nil → 耗尽 ErrNetwork）
				callErr = err
			} else {
				// err 保留（部署故障修复：错误文本落盘）：code 承载分类（0=连接级/
				// 凭据错、4xx、429、5xx），callErr 提供 err.Error() 文本——仅错误
				// 分支消费（成功路径零新增分配）。
				code, respBody, handled, callErr = caller.Call(r.Context(), w, r, reqID, groupID, start, sel, cred, route.body, stream)
			}
		}
		if handled {
			return // caller 已处理完毕（成功/客户端断开/流中止已记录；本地拒绝已写出无记录）
		}
		lastCode = code
		if code == http.StatusTooManyRequests {
			// 429：上游 body message（既有语义；域内截断 500）
			lastErrMsg = domain.TruncateErrMsg(upstreamErrMsg(respBody))
			p.sched.MarkResult(sel.AccountID, scheduler.Result429, nil, code, lastErrMsg)
		} else if code >= 500 || code == 0 {
			// 首字节前客户端断连（分类正确性，用户实证：模型思考期取消常见）：
			// r.Context() 已取消 → SDK 返回 context.Canceled（statusOf=0）。这是
			// 客户端行为，非上游错误——不 failover、不 MarkResult/冷却（否则
			// 无辜账号冷却 + failover 空转 + error_type 误记 network）；记
			// 499（nginx "client closed request" 约定）+ ErrAbort，立即返回。
			// tokens 必然 0 → cost=0 不计费；客户端已断，不写 HTTP 响应。
			// 流式路径的流中止/首字节后断连由 caller 内部分类（handled=true），
			// 到不了这里——本分支只覆盖 SDK 请求阶段（首字节前）的断连。
			if code == 0 && r.Context().Err() != nil {
				l := logWithCtx(r.Context(), p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, format, statusClientClosedRequest, domain.ErrAbort, usageTuple{}, start))
				msg := "client closed request before upstream response"
				l.ErrorMessage = &msg
				p.finish(sel.AccountID, l)
				return
			}
			// 5xx：上游 body message（既有语义）。连接级/凭据错（code==0）：
			// err.Error() 全文填 last_error 与耗尽记录（域内截断 500），并附加
			// Warn（request_id/account/model/err 全文——Warn 不截断）——根因
			// 锁定靠错误文本，两类留痕互补：Warn 全量、落盘 500 字符。
			lastErrMsg = upstreamErrMsg(respBody)
			if code == 0 && callErr != nil {
				lastErrMsg = domain.TruncateErrMsg(callErr.Error())
				if p.log != nil {
					p.log.Warn("upstream connection failure",
						logx.String("request_id", reqID),
						logx.Int64("account_id", sel.AccountID),
						logx.String("model", sel.Model),
						logx.Error(callErr))
				}
			} else if lastErrMsg != "" {
				lastErrMsg = domain.TruncateErrMsg(lastErrMsg)
			}
			p.sched.MarkResult(sel.AccountID, scheduler.ResultError, nil, code, lastErrMsg)
		} else {
			// 4xx 确定性错误：透传上游状态码与原始 body，不转移（规格 §5.3）；
			// body 不可得（连接级错误不会有 4xx 码）才回退网关文案。
			// 错误文本：上游 body 原文截断 500 落 ErrorMessage（仅错误分支构造，
			// 成功路径 ErrorMessage 恒空、零分配）。
			l := logWithCtx(r.Context(), p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, format, code, domain.Err4xx, usageTuple{}, start))
			if em := domain.TruncateErrMsg(string(respBody)); em != "" {
				l.ErrorMessage = &em
			}
			p.finish(sel.AccountID, l)
			if len(respBody) > 0 {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(code)
				_, _ = w.Write(respBody)
			} else {
				writeJSON(w, code, map[string]any{"error": map[string]any{
					"message": "upstream rejected request", "type": "upstream_error",
				}})
			}
			return
		}
		p.sched.Release(sel.AccountID)
		// 最后一轮不再为不存在的下一次尝试预选：尾部 Select 会抢占并发槽
		// （CAS 递增、仅 Release 递减、无回收），耗尽时永不释放 → 永久占槽。
		if attempt+1 >= p.cfg.FailoverAttempts {
			break
		}
		var selErr error
		sel, selErr = p.sched.Select(groupID, route.format, reqModel)
		if selErr != nil {
			break
		}
	}
	// 耗尽：请求已完成（上游消费了请求），以最后一次尝试的结果记一条用量。
	// 错误文本：最后一次尝试的 errMsg（连接级 err.Error() / 429/5xx 上游
	// message，域内截断 500）填 ErrorMessage；成功路径恒空。
	// 防呆释放：循环零次执行（failover_attempts=0 直构）时首次 Select 的槽从未
	// 释放——耗尽路径补 Release；N>=1 时 attempted 恒 true（循环尾已释放，不双释放）。
	if !attempted {
		p.sched.Release(lastSel.AccountID)
	}
	et := domain.Err5xx
	switch {
	case lastCode == http.StatusTooManyRequests:
		et = domain.Err429
	case lastCode == 0:
		et = domain.ErrNetwork
	}
	l := logWithCtx(r.Context(), p.buildLog(reqID, groupID, lastSel.AccountID, reqModel, lastSel.Model, format, lastCode, et, usageTuple{}, start))
	if lastErrMsg != "" {
		l.ErrorMessage = &lastErrMsg
	}
	p.recordLog(l)
	if lastCode == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "1")
		writeErr(w, errTooMany)
	} else {
		writeErr(w, &formatError{status: http.StatusBadGateway, msg: "all upstream attempts failed"})
	}
}

// precheckPrice 缺价预检（评审 P1-1：预检按格式切换）——images 格式查
// GetImagePrice（image_price 快照，跳过 chat 价预检 GetPrice：纯 image 价
// 模型无 pricings 行，chat 预检会先行 402 误杀，"ImagePrice 定生死"轮不到
// 执行）；其余格式照旧查 chat 价表。resp/resp-ws 共享同一 helper（P2-3
// 裁决：resp 路径保留 chat 价预检照常执行——行为不变）。零 DB（快照读）。
// 相应查找器未装配（bill 钩子 nil / 分查找器 nil）→ 不预检（等价计费全关）。
func (p *Proxy) precheckPrice(format domain.RequestFormat, model string) error {
	if format == domain.FormatOpenAIImages {
		if p.bill == nil || p.bill.ImagePrices == nil {
			return nil
		}
		_, err := p.bill.ImagePrices.GetImagePrice(model)
		return err
	}
	if p.bill == nil || p.bill.Prices == nil {
		return nil
	}
	_, err := p.bill.Prices.GetPrice(model)
	return err
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
