// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

// --- 转发管线骨架（D3：三份内联管线合一） ---
// handleFormat / HandleSearch / HandleResponsesWS 三份内联管线（鉴权 → reqMeta
// ctx 注入 → quota → 余额预检 → 两级门禁 → 限流 → 选号 → failover 循环 → 耗尽
// 记录）同构复制，骨架收敛**逐字一致段**（guard 六段 + 循环分类 + 耗尽记录）；
// 差异点收口成两个窄接口：
//   - upstreamAttempt：单次上游尝试（chat/search/WS 各自实现——分流/拨号/
//     relay/错误文本来源/Warn 文案等差异留在各自文件，不强行统一）
//   - pipelineSink：循环收尾写出（httpSink: HTTP 信封；wsSink: WS 错误事件帧）
// 不造阶段表/配置驱动（过度抽象否决——评审 b06："抽共享阶段函数"）。

// guardPipeline 鉴权 → reqMeta ctx 注入 → quota → 余额预检(可选) → 两级并发
// 门禁 → 限流；任一失败已写响应并记录，返回 ok=false（失败路径 inflight 减量
// 由内部完成——等价现状各失败分支 return 时 defer 已生效；401/429/402 无并发
// 槽占用，限流失败已回滚门禁）。precheckBalance=false 跳过余额预检（search
// 无 402 语义）。成功返回 (r, rm, level, true)：
//   - r 已注入 reqMeta ctx（user_id/key_id 日志归属；rm 指针直接返回——chat/WS
//     后续 tier 注入用，免从 ctx 重取）
//   - level 为已 acquire 门禁层级，调用方须注册释放（两条 defer 即合并释放的
//     展开形态，**不用 release 闭包**——用户红线：热路径零新增分配，闭包捕获
//     必逃逸）：
//     defer p.inflight.Add(-1)
//     defer p.auth.Release(rm.meta, level)
//     先释门禁后减 inflight——与现状 defer LIFO 同序；WS 场景释放延到 relay
//     会话结束——门禁覆盖整个长会话（同现状语义）。
func (p *Proxy) guardPipeline(w http.ResponseWriter, r *http.Request, format domain.RequestFormat, reqID string, start time.Time, precheckBalance bool) (*http.Request, *reqMeta, int, bool) {
	p.inflight.Add(1) // 优雅停机等在途归零（main waitForInflight 轮询 Inflight()）
	// reqMeta 创建 + ctx 注入整体提前到鉴权前（gate M1 方案）：401 及全部拒绝
	// 路径（401/429/402/限流）ctx 统一带 rm → recordRejected 行自动带 client_ip
	// （不变量：拒绝行恒有 client_ip）。rm 初始化只填 clientIP（clientIP 提取
	// 只读 RemoteAddr/请求头，鉴权前安全执行）；鉴权成功后原地补 meta。
	// Authenticate(r) 只读 Header 不碰 ctx，WithValue 不改 header 无干扰；成功
	// 路径分配不变（原本就有一个 rm），错误路径多一个 reqMeta 堆分配（非热
	// 路径，可接受）。
	rm := &reqMeta{clientIP: clientIP(r, p.cfg.BehindCDN)}
	r = r.WithContext(context.WithValue(r.Context(), ctxKeyReqMeta{}, rm))
	meta, ok := p.auth.Authenticate(r)
	if !ok {
		p.inflight.Add(-1)
		writeErr(w, errInvalidKey)
		// 评审 I-1：401 鉴权失败转 recordRejected（无效 key 洪水残留向量——
		// 401 也进 err_logs 错误审计，不再走 usage_logs 明细路径）。
		p.recordRejected(r.Context(), reqID, 0, 0, "", "", format, http.StatusUnauthorized, domain.ErrAuth, 0, usageTuple{}, start, errInvalidKey.msg)
		return nil, nil, 0, false
	}
	groupID := meta.GroupID
	// 请求元数据入 context（user_id/key_id 日志归属；不改变 Call/buildLog 签名）。
	// 单键单值 + 指针原地补 tier（GC 削减 P6：计费路径免第二次 WithValue+
	// WithContext；rm 指针只在请求 goroutine 内被读取/改写，logWithCtx 全程同
	// goroutine 同步访问——无跨 goroutine 竞态）。
	rm.meta = meta

	// quota 检查在并发 acquire 之前（评审提醒①：失败无并发槽副作用；
	// 未设置额度 key 短路零成本；预算耗尽 → gate 内 DB 复核认领后再判定）
	if p.auth.QuotaExhausted(meta) {
		p.inflight.Add(-1)
		writeErr(w, errQuotaExhausted)
		p.recordRejected(r.Context(), reqID, groupID, 0, "", "", format, http.StatusTooManyRequests, domain.Err429, 0, usageTuple{}, start, errQuotaExhausted.msg)
		return nil, nil, 0, false
	}
	// 余额预检（Phase 5 计费；评审 I-1 无槽位问题）：快照读零 DB（滞后 ≤
	// BalanceRefreshInterval，多实例条件扣 DB 兜底）。快照缺失或 <0 → 402
	// errInsufficientBalance（不按 0 记账），但免费放行（T3.5，评审 I-1 修复）：
	// 有效倍率 0 = 免费用户/组 → 缺失/0 余额不 402（与 applyBilling 同一快照
	// 同一判定；cost 0 只记日志不扣费）。余额 0 放行——临时额度由 FEFO 扣费
	// 消化（billing_repo.go:71-76 先扣 temp）；负余额持续负债拒绝。快照缺失
	// 窗口内免费组照常放行；缺失且非免费 → 仍 402（用户不在快照 = 无余额
	// 记录，语义不变）。在 Acquire 前 → 不占用并发槽。
	if precheckBalance && p.cfg.BillingCapture && p.bill != nil {
		bal, ok := p.bill.Balances.BalanceOf(meta.UserID)
		if (!ok || bal < 0) && p.bill.Balances.EffectiveMultiplier(meta.UserID, groupID) != 0 {
			p.inflight.Add(-1)
			writeErr(w, errInsufficientBalance)
			p.recordRejected(r.Context(), reqID, groupID, 0, "", "", format, http.StatusPaymentRequired, domain.ErrBilling, 0, usageTuple{}, start, errInsufficientBalance.msg)
			return nil, nil, 0, false
		}
	}
	// 两级并发门禁（user → key；两步回滚由 gate 内部完成；level 仅含已
	// acquire 层级——defer 覆盖全部返回路径）
	level, ok := p.auth.Acquire(meta)
	if !ok {
		p.inflight.Add(-1)
		writeErr(w, errConcurrency)
		p.recordRejected(r.Context(), reqID, groupID, 0, "", "", format, http.StatusTooManyRequests, domain.Err429, 0, usageTuple{}, start, errConcurrency.msg)
		return nil, nil, 0, false
	}
	if !p.limit.Allow(groupID, time.Now()) {
		p.inflight.Add(-1)
		p.auth.Release(meta, level)
		writeErr(w, errRateLimit)
		// 架构审查 S5（用户裁决）：组限流 429 也进 err_logs（排障限流需要；
		// 与 401 同属拒绝路径——普通队列风暴采样丢弃兜底）。
		p.recordRejected(r.Context(), reqID, groupID, 0, "", "", format, http.StatusTooManyRequests, domain.Err429, 0, usageTuple{}, start, errRateLimit.msg)
		return nil, nil, 0, false
	}
	return r, rm, level, true
}

// attemptState 单次尝试的格式差异输入（各格式只用自己字段，未用字段零值）。
// **按值传递**（栈拷贝零分配）——attempt/sink 均为 New 构造的无状态单例
// （同 callers map 惯例），per-request 差异状态全部经本结构流入：per-request
// 构造 attempt 结构体会逃逸堆（failoverLoop 体量超内联预算，接口转换必逃逸
// ——用户红线：热路径零新增分配）。
type attemptState struct {
	// chat（handleFormat）：
	format      domain.RequestFormat // 客户端格式（streaming-required 400 记录用）
	routeFormat domain.RequestFormat // 路由格式（images codex 分流判定）
	caller      UpstreamCaller       // 直连调用器（codex 分流复位基准）
	stream      bool                 // 流式标志（读请求阶段提取，恒定）
	// resp-ws（HandleResponsesWS）：
	client    *websocket.Conn
	firstTyp  websocket.MessageType
	first     []byte
	stripTier bool
}

// upstreamAttempt 一次上游尝试（三格式各自实现；语义与现状循环内分类输入一
// 致）：
//   - code：0=连接级/凭据错、4xx、429、5xx（WS 拨号 5xx 归原样——修复性声明）
//   - handled=true → 请求已处理完毕（成功/客户端断开/流中止已记录；本地拒绝
//     已写出无记录）——骨架直接返回（不可转移）；false → respBody/callErr 供
//     骨架分类
//   - respBody：4xx 透传原文（chat/search 原始 body；WS 归一错误文本——上游
//     body message，无则空——B1 分通道）；code==0 时亦携带错误文本（WS 纯文本
//     经骨架"直取原文"回退落盘，防 gjson 提取吃空）
//   - callErr：连接级/凭据错（code==0）或 WS 拨号 4xx（dialErr 全文——B1 分
//     通道：SDK 文本不进 respBody，帧面与落盘面解耦）时非 nil；Warn 由
//     attempt 内部代发（Warn 文案两版本保留不统一——循环不代发；WS code==0
//     恒 callErr=nil 不新增 Warn）
type upstreamAttempt interface {
	call(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, body []byte, st attemptState) (code int, respBody []byte, handled bool, callErr error)
}

// pipelineSink 循环收尾写出（HTTP 信封 vs WS 错误事件帧）。
type pipelineSink interface {
	writeUpstreamRejection(w http.ResponseWriter, st attemptState, code int, body []byte) // 4xx 确定性拒绝透传（http 原文；ws 错误帧 emOr 语义）
	writePrecheckRejected(w http.ResponseWriter, st attemptState)                          // 缺价 402 本地拒绝（骨架 precheck 失败收尾；http writeErr / ws 错误帧）
	writeExhausted(w http.ResponseWriter, st attemptState, lastCode int, msg string)       // 耗尽收尾（http: Retry-After 429 分支；ws: 固定文案忽略入参）
}

// failoverLoop 共享 failover 骨架：precheckPrice(开关) → attempt.call → 分类
// （429 MarkResult / 5xx、0 MarkResult / 4xx finish+透传）→ Release → attempted
// 防呆 → 尾部 Select（最后一轮不预选——防并发槽泄漏）→ 耗尽记录（et 分类 +
// recordLog + 防呆释放）→ sink.writeExhausted。任一终态（handled/4xx/499/预检
// 失败）已收尾，返回即请求结束。format 用于记录（客户端协议——WS 现状
// sel.Format 与 format 恒等，统一用 format）；selectFormat 用于尾部 Select
// （chat 协议转换命中时为模板协议路由——convertedRoute 转换补差不抽象）。
// precheck=false 跳过缺价预检（search——现状无预检语义）。
// 记录（recordRejected/buildLog/finish/recordLog/MarkResult）参数逐字段与三份
// 原内联管线一致（行为契约：状态码/错误帧/Retry-After/固定文案逐字节不变）。
func (p *Proxy) failoverLoop(w http.ResponseWriter, r *http.Request, format, selectFormat domain.RequestFormat, reqID string, groupID int64, start time.Time, reqModel string, body []byte, sel *scheduler.Selection, st attemptState, attempt upstreamAttempt, sink pipelineSink, precheck bool) {
	lastSel := sel
	var (
		lastCode   int
		lastErrMsg string // 最后一次实际尝试的错误文本（耗尽路径 ErrorMessage 用）
	)
	// 防呆（spec：failover_attempts=0 直构绕过 validate 下限）：循环零次执行时
	// 首次 Select 已占并发槽，耗尽路径按此标志补 Release——N>=1 恒 true，不双释放。
	attempted := false
	for i := 0; i < p.cfg.FailoverAttempts; i++ {
		lastSel = sel
		attempted = true
		// 缺价预检（评审 I-1 + P1-1 预检按格式切换）：每轮 sel 更新后、Call 前
		// 查价——计费启用时模型无价格 → 释放并发槽 + 402（不按 0 计价），零 DB
		// （快照读）。images 格式查 GetImagePrice（image_price 表；跳过 chat
		// 价预检 GetPrice——纯 image 价模型无 pricings 行，chat 预检会先行
		// 402 误杀，"ImagePrice 定生死"轮不到执行）；其余格式照旧。
		if precheck {
			if err := p.precheckPrice(format, sel.Model); err != nil {
				p.sched.Release(sel.AccountID)
				p.recordRejected(r.Context(), reqID, groupID, sel.AccountID, reqModel, sel.Model, format, http.StatusPaymentRequired, domain.ErrBilling, 0, usageTuple{}, start, errNoPrice.msg)
				sink.writePrecheckRejected(w, st)
				return
			}
		}
		code, respBody, handled, callErr := attempt.call(r.Context(), w, r, reqID, groupID, start, sel, reqModel, body, st)
		if handled {
			return // attempt 已处理完毕（成功/客户端断开/流中止已记录；本地拒绝已写出无记录）
		}
		lastCode = code
		if code == http.StatusTooManyRequests {
			// 429：上游 body message（既有语义；域内截断 500）。WS 拨号 429 的
			// 错误文本经 respBody 传递（可能为 SDK 纯文本）——提取为空时直取
			// 原文（与 code==0 分支同款防丢失；chat/search 上游错误体恒 JSON
			// message 键，该回退零影响）。
			lastErrMsg = domain.TruncateErrMsg(upstreamErrMsg(respBody))
			if lastErrMsg == "" && len(respBody) > 0 {
				lastErrMsg = domain.TruncateErrMsg(string(respBody))
			}
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
			// WS 修复性声明（gate Minor 2c）：WS 拨号/首帧转发阶段断连同样归
			// 此分支（现状记 ResultError 冷却无辜账号——统一 499 语义不冷却）。
			if code == 0 && r.Context().Err() != nil {
				l := logWithCtx(r.Context(), p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, format, statusClientClosedRequest, domain.ErrAbort, usageTuple{}, start))
				msg := "client closed request before upstream response"
				l.ErrorMessage = &msg
				p.finish(sel.AccountID, l)
				return
			}
			// 5xx：上游 body message（既有语义）。连接级/凭据错（code==0）：
			// err.Error() 全文填 last_error 与耗尽记录（域内截断 500）——Warn
			// 由 attempt 内部代发（文案两版本保留不统一，循环不代发）。
			// code==0 且 callErr==nil（WS 拨号/首帧转发失败）：错误文本经
			// respBody 传递（可能为纯文本——upstreamErrMsg 只认 JSON 键，
			// gjson 提取吃空）→ 直取原文（复审实证；chat/search 该分支不可达
			// ——code==0 恒带 callErr，零影响）。
			lastErrMsg = upstreamErrMsg(respBody)
			if code == 0 && callErr != nil {
				lastErrMsg = domain.TruncateErrMsg(callErr.Error())
			} else if lastErrMsg != "" {
				lastErrMsg = domain.TruncateErrMsg(lastErrMsg)
			} else if len(respBody) > 0 {
				lastErrMsg = domain.TruncateErrMsg(string(respBody))
			}
			p.sched.MarkResult(sel.AccountID, scheduler.ResultError, nil, code, lastErrMsg)
		} else {
			// 4xx 确定性错误：透传上游状态码与原始 body，不转移（规格 §5.3）；
			// body 不可得（连接级错误不会有 4xx 码）才回退网关文案。
			// 错误文本：上游 body 原文截断 500 落 ErrorMessage（仅错误分支构造，
			// 成功路径 ErrorMessage 恒空、零分配）；respBody 空且 callErr 非空 →
			// 以 callErr 全文回退落盘（B1 分通道的落盘面：WS 拨号 4xx 的 dialErr
			// 不再顶替进 respBody，改经 err 通道到此处落盘——4xx 空 body 边缘落盘
			// 增益 B1' 裁决接受、不加守卫；chat/search 4xx 恒有上游 body 且
			// callErr=nil，该回退不可达，落盘语义零变化）。WS 静态拨号 4xx 也归
			// 此统一段（finish + 错误帧——emOr 语义由 wsSink 保持）。
			l := logWithCtx(r.Context(), p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, format, code, domain.Err4xx, usageTuple{}, start))
			em := domain.TruncateErrMsg(string(respBody))
			if em == "" && callErr != nil {
				em = domain.TruncateErrMsg(callErr.Error())
			}
			if em != "" {
				l.ErrorMessage = &em
			}
			p.finish(sel.AccountID, l)
			sink.writeUpstreamRejection(w, st, code, respBody)
			return
		}
		p.sched.Release(sel.AccountID)
		// 最后一轮不再为不存在的下一次尝试预选：尾部 Select 会抢占并发槽
		// （CAS 递增、仅 Release 递减、无回收），耗尽时永不释放 → 永久占槽。
		if i+1 >= p.cfg.FailoverAttempts {
			break
		}
		var selErr error
		sel, selErr = p.sched.Select(groupID, selectFormat, reqModel)
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
	sink.writeExhausted(w, st, lastCode, lastErrMsg)
}

// httpSink HTTP 信封收尾（chat/search 共用；无状态——w 经方法参数流入）。
type httpSink struct{}

func (s *httpSink) writeUpstreamRejection(w http.ResponseWriter, _ attemptState, code int, body []byte) {
	if len(body) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_, _ = w.Write(body)
	} else {
		writeJSON(w, code, map[string]any{"error": map[string]any{
			"message": "upstream rejected request", "type": "upstream_error",
		}})
	}
}

func (s *httpSink) writePrecheckRejected(w http.ResponseWriter, _ attemptState) { writeErr(w, errNoPrice) }

func (s *httpSink) writeExhausted(w http.ResponseWriter, _ attemptState, lastCode int, _ string) {
	if lastCode == http.StatusTooManyRequests {
		// search 耗尽 Retry-After 分支保持（httpSink 判 lastCode==429）
		w.Header().Set("Retry-After", "1")
		writeErr(w, errTooMany)
	} else {
		writeErr(w, &formatError{status: http.StatusBadGateway, msg: "all upstream attempts failed"})
	}
}

// wsSink WS 错误事件帧收尾（WS 无 HTTP 状态码，拒绝语义经事件帧承载；无状态
// ——client 经方法参数 attemptState 流入）。
type wsSink struct{}

func (s *wsSink) writeUpstreamRejection(_ http.ResponseWriter, st attemptState, code int, body []byte) {
	wsWriteError(st.client, emOr(string(body), "upstream rejected request"))
}

func (s *wsSink) writePrecheckRejected(_ http.ResponseWriter, st attemptState) { wsWriteError(st.client, errNoPrice.msg) }

func (s *wsSink) writeExhausted(_ http.ResponseWriter, st attemptState, _ int, _ string) { wsWriteError(st.client, "all upstream attempts failed") }
