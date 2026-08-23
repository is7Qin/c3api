// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/logx"
)

// errCodexSearchNotIntegrated 501：codex 适配层未装配（SetCodex 未调用——main
// 装配缺失的显式拒绝，不让凭据缺失路径误报 502/network；与 images/resp 路径
// 同款）。
var errCodexSearchNotIntegrated = &formatError{status: http.StatusNotImplemented, msg: "codex search unavailable (adapter not wired)"}

// HandleSearch 转发 codex /v1/alpha/search（spec 2026-08-13 v2）：codex CLI 以
// 独立 unary POST 调 web search（模型发 web.run tool call 时触发，与主
// /responses 流并发）。**透传语义：请求体/响应体原样**（opaque results/
// encrypted_output 网关零解析——alpha 端点实验性，上游变更网关免疫）。
//
// 与主 handleFormat 的差异（search 专属语义，spec 边界声明）：
//   - 账号选择：body.model → Scheduler.Select(groupID, openai-responses, model)
//     （复用主流 resp 路由面——四类型全可达；**独立选号无会话绑定**——P2 裁
//     决：search 请求自包含，上游鉴权 = 有效 Bearer，无会话亲和机制）
//   - **不走计费预检**（余额/缺价 402 均不执行——search 无预检语义；按次价在
//     2xx 落账时结算，零余额透支扣费为产品语义，防实现期误当缺陷"修复"）
//   - **四类型分派（用户裁决 2026-08-13）**：codex-oauth/codex-pat → codex-sdk
//     Search（适配层 clientFor 缓存客户端直接复用——统一 client 形态；
//     search URL 由 SDK 方法内派生，网关零拼装；Auth 注入/刷新/fatal 生命周期
//     复用既有 SDK 面）；api_key/responses-special → 静态透传（Bearer upstream
//     key 直连上游——aiclient 既有静态 key 通道零新增机制；URL 裸根派生
//     base/v1/alpha/search）。组内混合类型路由允许（任一类型均可用——不再本地
//     拒绝）
//   - **x-codex-turn-metadata 统一不转发**（两路径均不带上游——SDK 默认头面
//     无该头；静态 rawPostCT 构造全新 Header 只设 Content-Type + Authorization，
//     与主流静态路径现状一致）
//   - **不做 ModelMapping 改写（P3-3 显式取舍）**：请求体原样 = 映射对 search
//     不生效（上游收客户端模型名）——零解析是 spec 显式约束，自洽记录
//   - 计费：2xx → usage_logs 行（format=openai-search + call_count=1 +
//     price_per_call_millis=PriceResolver call 档（codex-search 模型） + cost=按次价×整单
//     倍率，applyBilling search 分支）；非 2xx/网络错误 → 不计费（cost=0，错误
//     行走既有 err_logs 面）
//
// 复用面（评审 P3-4 点名）：guardPipeline（鉴权/配额/并发门禁/限流序列）、
// Select + handleSelectError、信封分类（statusOf/upstreamBody）、failoverLoop
// （**每轮按当轮 sel.CredentialType 重新分派**——searchAttempt 对齐 P1-1 教训：
// 跨类型换账号复用旧调用器会把健康账号路由到错误凭据路径）、
// recordRejected/finish/buildLog/MarkResult 全部既有机制零改动。
func (p *Proxy) HandleSearch(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := newReqID()
	r, rm, level, ok := p.guardPipeline(w, r, domain.FormatOpenAISearch, reqID, start, false)
	if !ok {
		return
	}
	// 门禁释放：先释并发门禁后减 inflight（与现状 defer LIFO 同序）。
	defer p.inflight.Add(-1)
	defer p.auth.Release(rm.meta, level)
	groupID := rm.meta.GroupID

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, p.cfg.MaxBodySize))
	if err != nil {
		writeErr(w, errBody)
		return
	}
	// 本地 400（同 handleFormat 语义：json.Valid 硬门零分配；search 无 stream/
	// service_tier 面——body 原样透传，不做任何改写）。
	if !json.Valid(body) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{"message": "invalid request body: invalid JSON"}})
		return
	}
	// 客户端请求模型（日志口径 + 选号）：gjson 顶层提取（1 次分配，与 resp
	// 流式路径同款）。model 缺失/非法 → 空串回落默认桶（Select 既有语义——
	// 上游契约必填 id/model，缺失由上游 4xx 兜底，网关零新增校验）。
	reqModel := gjson.GetBytes(body, "model").String()

	// 选号：复用主流 resp 路由面（openai-responses 格式——四类型全可达；
	// search 无独立路由，独立选号无会话绑定）。
	sel, err := p.sched.Select(groupID, domain.FormatOpenAIResponses, reqModel)
	if err != nil {
		p.handleSelectError(w, err)
		p.recordRejected(r.Context(), reqID, groupID, 0, reqModel, "", domain.FormatOpenAISearch, statusFor(err), domain.ErrNoAccount, 0, usageTuple{}, start, selectErrorMessage(err))
		return
	}

	// failover 循环（共享骨架，见 pipeline.go）：precheck=false（search 无缺价
	// 预检——现状语义显式关，不给 search 新增 402）；尾部 Select 走主流 resp
	// 路由面（openai-responses）；耗尽 Retry-After 分支由 httpSink 判 lastCode。
	p.failoverLoop(w, r, domain.FormatOpenAISearch, domain.FormatOpenAIResponses, reqID, groupID, start, reqModel, body, sel,
		attemptState{}, p.searchAttempt, p.httpSink, false)
}

// searchAttempt HandleSearch 的 attempt 实现（单次 codex search 上游调用，非
// 流式 unary 透传；**四类型分派**——用户裁决 2026-08-13；无状态单例——search
// 无 per-request 差异状态）。按当轮 sel.CredentialType 路由——
//   - codex-oauth/codex-pat → callCodexSearch（适配层 SDK Search——统一 client
//     形态直接复用；Auth 注入 + fatal 生命周期复用既有 SDK 面）
//   - api_key/responses-special → callStaticSearch（Bearer upstream key 直连
//     上游——aiclient 既有静态 key 通道）
//
// 分派每轮重新执行（P1-1 教训——跨类型换账号按新类型走新路径，不缓存调用
// 器）。差异段（循环不代发）：Warn 文案 "upstream search connection failure"
// ——与 chat 版保留不统一（gate Minor 2a）。
type searchAttempt struct{ p *Proxy }

func (a *searchAttempt) call(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, body []byte, st attemptState) (int, []byte, http.Header, bool, error) {
	// TODO(P22-I1): 当前 hdr 恒 nil（UpstreamCaller.Call 未回收 resp.Header），
	// 仅 fallback 1 生效；待扩展 Header 透传后替换为真实透传
	// （Global Constraints 豁免 fallback 保留）
	var (
		code     int
		respBody []byte
		handled  bool
		callErr  error
	)
	if isCodexCredentialType(sel.CredentialType) {
		code, respBody, handled, callErr = a.p.callCodexSearch(ctx, w, r, reqID, groupID, start, sel, reqModel, body)
	} else {
		code, respBody, handled, callErr = a.p.callStaticSearch(ctx, w, r, reqID, groupID, start, sel, reqModel, body)
	}
	if code == 0 && callErr != nil && ctx.Err() == nil {
		// Warn 留痕（连接级/凭据错全文——Warn 不截断；循环不代发）。ctx 已取
		// 消（499 分支）不 Warn——与现状判定顺序一致。
		if a.p.log != nil {
			a.p.log.Warn("upstream search connection failure",
				logx.String("request_id", reqID),
				logx.Int64("account_id", sel.AccountID),
				logx.String("model", sel.Model),
				logx.Error(callErr))
		}
	}
	return code, respBody, nil, handled, callErr
}

// callCodexSearch codex-oauth/codex-pat 类型 search 调用（SDK 路径）：凭据线
// 快照派生直供适配层（与 resp 路径同款——codex 凭据为复合结构，单字符串契约
// 表达不到）→ 适配层 Search（clientFor 缓存客户端 → e.client.Search——body
// 零改写、响应零解析）→ 2xx → 响应原样写出 + MarkResult + finish
// （usageTuple{calls:1} 落 CallCount——按次计费在 applyBilling 的 search 分支
// 结算）；非 2xx → 信封分类返回（4xx 透传 / 429/5xx failover，与 resp HTTP
// 分支同语义）。
//
//   - 适配层未装配（SetCodex nil）→ 501 显式拒绝（release + recordRejected +
//     writeErr，handled=true）
//   - 配置损坏（codex 账号缺 account_ext 快照）→ 连接级错误转移（handled=false
//     ——失败文本落盘，耗尽 502；不上报失效，与 resp/WS 路径 errCodexExtMissing
//     同语义）
func (p *Proxy) callCodexSearch(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, body []byte) (int, []byte, bool, error) {
	if p.codex == nil {
		// 适配层未装配（SetCodex 未调用）：显式 501（防 nil 误走凭据缺失 502）。
		p.sched.Release(sel.AccountID)
		p.recordRejected(r.Context(), reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAISearch, http.StatusNotImplemented, domain.ErrBilling, 0, usageTuple{}, start, errCodexSearchNotIntegrated.msg)
		writeErr(w, errCodexSearchNotIntegrated)
		return 0, nil, true, nil
	}
	if sel.Ext == nil {
		// 配置损坏（codex 账号必有 ext 行——快照缺 account_ext 行）：本地配置
		// 错误按连接级错误转移（失败文本落盘，耗尽 502 语义）；不上报失效（避
		// 免 account 0 无谓上报——与 resp/WS 路径 errCodexExtMissing 同语义）。
		return 0, nil, false, errCodexExtMissing
	}
	// 凭据线：快照派生直供适配层（与 resp/images 路径同款）。cred.BaseURL =
	// responses 完整端点（SDK Search 方法内按其尾段派生 /alpha/search——网关
	// 零拼装）。
	cred := domain.CredentialFromExt(sel.Ext)
	full, err := p.clients.ResponsesWSURL(sel.TemplateID, sel.BaseURL)
	if err != nil {
		return 0, nil, false, err
	}
	cred.BaseURL = full
	// 非流式超时（同 nonstreamCodexResponses 语义）：HTTPClient.Timeout 不可用
	// ——TCP 黑洞读停滞 → 超时触发 → 连接级错误转移（failover 可转移）。
	ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamTimeout)
	defer cancel()
	// 无头注入：x-codex-turn-metadata 统一不转发（SDK Search 默认头面无该头，
	// 与 resp HTTP 路径现状一致）。
	resp, err := p.codex.Search(ctx, &cred, body)
	if err != nil {
		return statusOf(err), upstreamBody(err), false, err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(resp.Raw)
	// 2xx → 按次计费落账（call_count=1；price_per_call/cost 由 applyBilling
	// search 分支按 PriceResolver call 档（codex-search）结算——无 token 分量）。
	p.sched.MarkResult(sel.AccountID, rule.KindOK, nil, http.StatusOK, "", sel.Model)
	p.finish(sel.AccountID, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAISearch, http.StatusOK, domain.ErrNone, usageTuple{calls: 1}, start)))
	return http.StatusOK, nil, true, nil
}

// callStaticSearch api_key/responses-special 类型 search 调用（静态透传路径）：
// 既有静态 key 通道（credentialFor → aiclient rawPost——Bearer upstream key
// 直连上游；URL 裸根派生 base/v1/alpha/search——与主流静态 responses 路径
// base/v1/responses 同款派生语义，尾段即 /alpha/search）。错误信封 = 原始
// HTTP 状态 + body 透传（caller_responses.go:65-68 先例——非 200 读取 body
// 交 failover 循环分类；SDK 路径的 translateError 信封不适用）。
//
// **无客户端头透传**（x-codex-turn-metadata 统一不转发——rawPostCT 构造全新
// Header 只设 Content-Type + Authorization，与主流静态路径现状一致）。
func (p *Proxy) callStaticSearch(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, body []byte) (int, []byte, bool, error) {
	cred, err := p.credentialFor(ctx, sel)
	if err != nil {
		return 0, nil, false, err // 凭据错误按连接级处理（耗尽 502，与既有路径同语义）
	}
	// 非流式超时（同 codex 路径语义）：TCP 黑洞读停滞 → 超时触发 → 连接级错误
	// 转移（failover 可转移）。
	ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamTimeout)
	defer cancel()
	resp, err := p.clients.SearchRaw(ctx, sel.TemplateID, sel.BaseURL, cred, body)
	if err != nil {
		return statusOf(err), upstreamBody(err), false, err
	}
	if resp.StatusCode != http.StatusOK {
		rb := readUpstreamBody(resp)
		resp.Body.Close()
		return resp.StatusCode, rb, false, nil
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return 0, nil, false, err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
	// 2xx → 按次计费落账（call_count=1；与 codex 路径同款——applyBilling
	// search 分支结算）。
	p.sched.MarkResult(sel.AccountID, rule.KindOK, nil, http.StatusOK, "", sel.Model)
	p.finish(sel.AccountID, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, domain.FormatOpenAISearch, http.StatusOK, domain.ErrNone, usageTuple{calls: 1}, start)))
	return http.StatusOK, nil, true, nil
}
