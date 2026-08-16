// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/is7qin/c3api/internal/billing"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/aiclient"
)

// --- resp-ws 通用编排（openai-responses-ws 格式） ---
// 独立文件（用户拍板文件边界：codex 相关处理不散落现有 caller/forward 文件）。
// resp-ws = 通用能力：任何模板 supported_formats 含 resp-ws 即可用，1:1 透传。
// 编排语义与 SSE caller 同构：鉴权/门禁/选号/failover/记录复用骨架组件，差异
// 只在传输面（WS 升级 + 双向事件帧转发 + usage 嗅探 + 心跳）。
//
// 热路径纪律（架构定稿 §5）：流式中间帧零解析直转——账目分层：网关层零解析
// 零分配（bytes.Contains 子串预筛，命中才字节扫描取 usage——usage_extract.go
// A-1 scanKeyValue 单遍扫描）；库层（coder/websocket）每帧 io.ReadAll 物化 +
// permessage-deflate 往返属库内账目，非网关责任。只嗅探 response.completed
// 帧（预筛命中才最小字节扫描）；首帧（response.create = 请求帧）才做模型改写
// （ModelMapping 语义，与 setModel 同构）——也是 W4 图像剥离的帧级预处理点。

const (
	// responsesWSFirstFrameTimeout 升级后首个请求帧（response.create）等待上限：
	// 客户端连接后不发帧即断/挂死 → 记 abort 收尾。选号在前帧读之后，挂死
	// 不占账号并发槽。真实客户端（codex）连接即发帧。可配置化留待模板 ext。
	responsesWSFirstFrameTimeout = 60 * time.Second
	// responsesWSHeartbeatInterval 网关作为 WS 客户端向上游的心跳间隔（长连接
	// 保活 + 上游失联探测；服务端侧客户端 ping 由库自动回 pong，无编排）。
	// SSE 无心跳 → 依赖 UpstreamStreamTimeout 兜底；WS 有心跳 → 无整体超时。
	responsesWSHeartbeatInterval = 30 * time.Second
	// responsesWSPongTimeout 心跳 pong 等待上限：超时 = 上游失联/停滞 →
	// 按上游错误收尾（与 SSE 上游停滞同语义）。
	responsesWSPongTimeout = 10 * time.Second
	// responsesWSReadLimit 单帧读取上限（16MB）：库默认 32KB 放不下
	// response.completed 全量响应帧；读侧上界防恶意超大帧拖垮内存。
	responsesWSReadLimit = 16 << 20
	// responsesWSCloseTimeout 错误帧写出/关闭传播超时（对侧不读不回应时防挂死）。
	responsesWSCloseTimeout = 5 * time.Second
)

// HandleResponsesWS 处理 resp-ws 升级请求（/v1/responses 带 upgrade 头——
// 真实客户端无 /ws 后缀，WS 与 POST /v1/responses 同路径，按协议分流）。
// 与 handleFormat 同构：guardPipeline（鉴权 → 额度/余额预检 → 两级并发门禁
// → 限流，见 pipeline.go）→ 升级 → 首帧（= 请求体）→ 模型提取 → 选号 →
// failoverLoop（wsAttempt + wsSink，precheck=true）→ 双向 relay（usage 嗅探）
// → 记录。差异段留本文件：
//   - 无 HTTP body：请求体 = 升级后首个 WS 帧（response.create）
//   - 选号在首帧之后（模型来自首帧；挂死不占账号槽）
//   - 本地拒绝在升级后无 HTTP 状态码 → 错误事件帧承载（wsWriteError）
//   - 门禁覆盖整个长会话：guard 释放 defer 到 relay 结束（同现状语义）
func (p *Proxy) HandleResponsesWS(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := newReqID()
	r, rm, level, ok := p.guardPipeline(w, r, domain.FormatOpenAIResponsesWS, reqID, start, true)
	if !ok {
		return
	}
	// 门禁释放：先释并发门禁后减 inflight（与现状 defer LIFO 同序）；defer 到
	// relay 会话结束——门禁覆盖整个长会话（同现状语义）。
	defer p.inflight.Add(-1)
	defer p.auth.Release(rm.meta, level)
	groupID := rm.meta.GroupID

	if !isWebSocketUpgrade(r) {
		// 本地拒绝（无记录，同 invalid JSON 语义）
		writeErr(w, errUpgradeRequired)
		return
	}
	client, err := aiclient.AcceptResponsesWS(w, r)
	if err != nil {
		return // Accept 已写出 4xx（非升级请求已在上方拦截，此处罕见）
	}
	// 兜底关闭：正常路径已显式 Close（关闭握手）；CloseNow 免握手等待
	// （对侧已死/异常时 defer 不拖 5s 关闭握手超时）。
	defer client.CloseNow()
	client.SetReadLimit(responsesWSReadLimit)

	// 首个请求帧：升级后客户端发 response.create（模型/输入都在这帧）。
	// 读帧超时防挂死；超时/断开 → 记 499 abort（无上游接触、无账号槽占用）。
	firstCtx, firstCancel := context.WithTimeout(r.Context(), responsesWSFirstFrameTimeout)
	firstTyp, first, err := client.Read(firstCtx)
	firstCancel()
	if err != nil {
		p.record(r.Context(), reqID, groupID, 0, "", "", domain.FormatOpenAIResponsesWS, statusClientClosedRequest, domain.ErrAbort, 0, usageTuple{}, start)
		return
	}
	reqModel := gjson.GetBytes(first, "model").String()

	// service_tier 归一化 + 转发策略（首帧读后、Select 前——与 HTTP
	// handleFormat 的 strip/reject 同位置，均先于选号；codex 分流在后，首帧
	// 字节在共享点不被改写——strip 标记于此处、执行于 relayResponsesWS 帧改写
	// 点，codex 路径零变化）。类型错误 → 400 错误帧无记录（同 HTTP，ErrBilling
	// 只用于 reject）；reject → 错误帧 + ErrBilling 记录，不升级。归一化 tier
	// 补入已入 ctx 的 reqMeta（HTTP 同机制——logWithCtx 消费链四 buildLog 调用
	// 点 + 首帧失败路径全自动覆盖）→ BillingTier 恒非空（无显式 tier 落
	// "auto"）；非计费路径 hasTier=false → 恒空。
	var stripTier bool
	if p.bill != nil {
		tier, err := extractTier(first)
		if err != nil {
			wsWriteError(client, "invalid request body: "+err.Error())
			return
		}
		rm.tier = tier
		rm.hasTier = true
		if (tier == billing.TierPriority || tier == billing.TierFlex || tier == billing.TierFast) && p.bill.TierPolicy != nil {
			switch p.bill.TierPolicy(tier) {
			case billing.TierPolicyStrip:
				stripTier = true // 标记：共享帧改写点（relayResponsesWS）删除该字段
			case billing.TierPolicyReject:
				wsWriteError(client, errServiceTierRejected.msg)
				p.recordRejected(r.Context(), reqID, groupID, 0, reqModel, "", domain.FormatOpenAIResponsesWS, http.StatusBadRequest, domain.ErrBilling, 0, usageTuple{}, start, errServiceTierRejected.msg)
				return
			}
		}
	}

	// 选号（含账号并发槽抢占）：格式硬过滤由调度器路由承担（模板
	// SupportedFormats 含 resp-ws 才建路由）。挂死客户端不占槽（槽在首帧后取）。
	sel, err := p.sched.Select(groupID, domain.FormatOpenAIResponsesWS, reqModel)
	if err != nil {
		wsWriteError(client, selectErrorMessage(err))
		p.recordRejected(r.Context(), reqID, groupID, 0, reqModel, "", domain.FormatOpenAIResponsesWS, statusFor(err), domain.ErrNoAccount, 0, usageTuple{}, start, selectErrorMessage(err))
		return
	}

	// failover 循环（共享骨架，见 pipeline.go）：precheck=true（resp-ws 保留
	// chat 价预检照常执行——评审 P2-3 裁决：纯 image 价模型经 resp 出图会被
	// 既有预检 402，接受，职责边界清晰；共享 helper 只对 images 格式切换）；
	// 4xx 透传统一走循环分类（finish + wsSink 错误帧，emOr 语义保持）；耗尽
	// 固定文案由 wsSink 承载（WS 无 Retry-After/429 语义）。codex 拨号分类
	//（handleCodexDialError 的 stop 分支——501/fatal/4xx 已收尾）留在
	// wsAttempt 内（不统一 codex 4xx 收尾差异：分类代码位置 + 错误文本来源）。
	p.failoverLoop(w, r, domain.FormatOpenAIResponsesWS, domain.FormatOpenAIResponsesWS, reqID, groupID, start, reqModel, nil, sel,
		attemptState{client: client, firstTyp: firstTyp, first: first, stripTier: stripTier},
		p.wsAttempt, p.wsSink, true)
}

// wsAttempt HandleResponsesWS 的 attempt 实现（struct 方法 + 接口引用——不用
// 闭包；无状态单例，per-request 差异（client/首帧/strip 标记）经 attemptState
// 流入，热路径零新增分配）。差异段：codex 拨号分流（dialCodexWS +
// handleCodexDialError）与静态拨号（credentialFor + ResponsesWSDial）、relay
//（relayCodexWS/relayResponsesWS——首帧模型改写 + 双向帧透传 + usage 嗅探）。
// 错误文本统一经 respBody 回传（msg 归一：上游 body message → dialErr 文本回
// 退——循环的 upstreamErrMsg gjson 提取吃空时直取原文防丢失）；code==0 恒
// callErr=nil（WS 不新增 Warn——gate Minor 2b）。
type wsAttempt struct{ p *Proxy }

func (a *wsAttempt) call(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, body []byte, st attemptState) (int, []byte, bool, error) {
	p := a.p
	if isCodexCredentialType(sel.CredentialType) {
		// codex 独立 relay 变体（T4 §1）：SDK Dial 路径——快照派生 cred 直供
		// 适配层（不经 credentialFor 单字符串路径：codex 凭据为复合结构
		// oauth_token+refresh_token+expires_at+pat+accountID，单字符串契约表
		// 达不了——注册表未注册 codex 类型，见 dialCodexWS 注释），伪装四元
		// 组 + 头部族剥离 + 心跳单源全部在 dialCodexWS/relayCodexWS
		//（codex_responses_ws.go）。
		up, dialErr := p.dialCodexWS(r, sel)
		if dialErr == nil {
			handled, fwMsg := p.relayCodexWS(st.client, up, r, reqID, groupID, start, sel, reqModel, st.firstTyp, st.first)
			if handled {
				return 0, nil, true, nil
			}
			// 首帧转发失败 = 上游未消费请求 → 连接级错误转移（同拨号失败）
			return 0, []byte(fwMsg), false, nil
		}
		if stop, code, msg := p.handleCodexDialError(r, reqID, groupID, start, sel, reqModel, st.client, dialErr); stop {
			// 501/fatal/4xx 已收尾（错误帧 + 记录）——请求终止不转移
			return 0, nil, true, nil
		} else {
			// 429 → Result429 转移；5xx（归一 lastCode 原样）/RefreshError/
			// 网络（code 0）→ ResultError 转移——分类由循环统一完成。
			return code, []byte(msg), false, nil
		}
	}
	cred, err := p.credentialFor(ctx, sel)
	if err != nil {
		// 凭据错误按网络错误处理（等价 handleFormat 的 code==0 语义）
		return 0, []byte(domain.TruncateErrMsg(err.Error())), false, nil
	}
	up, resp, dialErr := p.clients.ResponsesWSDial(ctx, sel.TemplateID, sel.BaseURL, cred, wsPassthroughHeaders(r.Header))
	if dialErr == nil {
		handled, fwMsg := p.relayResponsesWS(st.client, up, r, reqID, groupID, start, sel, reqModel, st.firstTyp, st.first, st.stripTier)
		if handled {
			return 0, nil, true, nil
		}
		// 首帧转发失败 = 上游未消费请求 → 连接级错误转移（同拨号失败）
		return 0, []byte(fwMsg), false, nil
	}
	// 拨号失败分类（与 handleFormat 的 code 分支同构）：msg 归一 = 上游 body
	// message，空 → dialErr 文本回退。code 原样回传——5xx 归一（修复性声明：
	// 现状 default 分支归 0 → 耗尽记 ErrNetwork + MarkResult httpStatus 0；
	// 统一为 code 原样 → et=Err5xx + httpStatus 5xx，对齐 codex 分支与 HTTP
	// 路径，规则 when http_status 匹配面恢复真实值）；非 429/4xx/5xx 的异常
	// 状态（2xx/3xx 未升级拒绝）按现状归连接级 0。
	code := 0
	var msg string
	if resp != nil {
		code = resp.StatusCode
		msg = upstreamErrMsg(readUpstreamBody(resp))
		_ = resp.Body.Close()
	}
	if msg == "" {
		msg = dialErr.Error()
	}
	if code < 400 || code >= 600 {
		code = 0 // 连接级/非标准拒绝（含 2xx/3xx 未升级）→ 现状 default 分支语义
	}
	return code, []byte(msg), false, nil
}

// relayResponsesWS 拨号成功后的编排：首帧模型改写（ModelMapping 语义，与
// setModel 同构；首帧 = 请求帧非流式中间帧，亦为 W4 图像剥离的帧级预处理点）
// + strip 策略的 service_tier 删除（共享帧改写点）→ 转发首帧 → 双向事件帧
// 1:1 relay（流式中间帧零解析零拷贝直转）→ 关闭/错误传播 → usage 记录。
// stripTier 为 HandleResponsesWS 首帧 tier 策略标记（仅本共享点消费——codex
// 变体 relayCodexWS 不经此函数，codex 路径帧零静默变化）。返回 (handled,
// fwMsg)：handled = 请求已处理完毕（成功/客户端断开/流中止已记录）；false =
// 首帧转发失败（上游未消费请求），fwMsg 为截断错误文本，调用方按连接级错误
// 转移（MarkResult + Release + 重选）。
func (p *Proxy) relayResponsesWS(client, up *websocket.Conn, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, firstTyp websocket.MessageType, first []byte, stripTier bool) (handled bool, fwMsg string) {
	up.SetReadLimit(responsesWSReadLimit)
	frame := first
	if sel.Model != "" && sel.Model != reqModel {
		if nf, err := sjson.SetBytes(first, "model", sel.Model); err == nil {
			frame = nf
		} // 改写失败（帧非合法 JSON）→ 原样转发，上游自行校验
	}
	if stripTier {
		// strip 策略：sjson 同库同风格字节级删除（非 map 往返）；字段存在性由
		// extractTier 非 auto 档保证——缺失/类型错已在 Select 前拒绝。
		if nf, err := sjson.DeleteBytes(frame, "service_tier"); err == nil {
			frame = nf
		}
	}
	if err := up.Write(r.Context(), firstTyp, frame); err != nil {
		_ = up.CloseNow()
		return false, domain.TruncateErrMsg(err.Error())
	}

	// --- 双向 relay：三个方向各自 goroutine，首退者触发取消 ---
	// 每个 goroutine 退出时把"本侧真实错误"记录到共享变量（仅当退出非取消
	// 副作用——relayCtx 存活 = 本退出是首因；首因到达 endCh → 编排等上游
	// 读者退出 → 分类 → 取消对侧 → 等全部退出。
	//
	// I-1 竞态（评审裁决"修"）：上游关闭帧与客户端活跃写帧并发时，上游侧
	// 错误槽 upErr 有两个并发写者——up-loop 的关闭帧（CloseError）与
	// client-loop 的写失败（net.ErrClosed，库在解码关闭帧后 c.close() 所致）
	// ——首写生效下 net.ErrClosed 可能先被记录 → 健康上游误判 ResultError
	// 冷却。修复：关闭帧记录到独立槽 upClose（仅 up-loop 写入、无取消守卫
	// ——真实帧永不丢），分类时正常关闭帧优先于一切；写失败只归因网络错误
	// 槽（无关闭帧时才判错）。upLoopDone 保证 upClose 先于分类读取可见
	// （记录 happens-before 退出 happens-before close(upLoopDone)）。
	//
	// 关键细节：客户端循环的阻塞 Read 用 r.Context()（非 relayCtx）——库对
	// 取消中的阻塞 Read 会直接拆连接（客户端拿不到正常关闭帧）；客户端循环
	// 的退出由编排的分类关闭帧（Close 握手）自然解除：对端回关闭帧 → Read
	// 返回关闭错误 → 退出。取消仅用于上游侧（上游已结束/失联，直拆无害）。
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
	// setErr 记录单侧退出错误（首写生效；取消副作用不记录）。dst 指针即
	// 变量地址，单 goroutine 之外只有并发写 upErr 的可能——同语义（上游侧），
	// 首写即可。
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
	// recordClose 记录上游关闭帧（仅 CloseError）。不设 relayCtx 守卫：取消
	// 副作用（ctx.Canceled）本就不是 CloseError，真实关闭帧永远优先于并发写
	// 失败症状（net.ErrClosed 只进 upErr，首写覆盖不了关闭帧）。
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
			typ, f, err := client.Read(r.Context())
			if err != nil {
				setErr(&clientErr, err)
				exit()
				return
			}
			if err := up.Write(relayCtx, typ, f); err != nil {
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
			typ, f, err := up.Read(relayCtx)
			if err != nil {
				// 关闭帧 → 独立槽（最高权威）；其余（EOF/网络）→ upErr。
				// 本 goroutine 是唯一解码者：关闭帧 decode+record 与客户端
				// 循环的写失败天然并发，槽位分离使两者各归其位、互不覆盖。
				var ce websocket.CloseError
				if errors.As(err, &ce) {
					recordClose(err)
				} else {
					setErr(&upErr, err)
				}
				exit()
				return
			}
			// 热路径纪律：bytes.Contains 零分配预筛，命中才最小字节扫描取 usage
			// （usage_extract.go A-1 scanKeyValue 单遍扫描零分配）；流式中间帧
			// 零解析直转——网关层零分配，库层每帧 io.ReadAll 物化 + flate 属库
			// 内账目。
			if u, ok := sniffResponsesCompleted(f); ok {
				it, ot, tt, cr, cc = u.it, u.ot, u.tt, u.cr, u.cc
				// 响应检测旁路（spec §6）：completed 帧恒在流末——最终计数由其
				// 覆盖（最后帧语义）；门控关闭（api_key/strip 开）→ 零额外解析。
				if respImageDetectOn(sel) {
					img = respImageCountCompleted(f)
				}
			}
			if ttft == nil {
				ms := time.Since(start).Milliseconds()
				ttft = &ms
			}
			if err := client.Write(relayCtx, typ, f); err != nil {
				setErr(&clientErr, err)
				exit()
				return
			}
		}
	}()

	wg.Add(1)
	go func() { // 心跳：向上游周期 Ping（pong 超时 = 上游失联 → 按上游错误收尾）
		defer wg.Done()
		ticker := time.NewTicker(p.wsHeartbeatInterval) // seam：测试缩短验证节奏（T4）
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
	// I-1：等上游读者退出再分类——关闭帧的 decode+record 与客户端循环的
	// 写失败记录并发，关闭帧可能晚于首退到达；upLoopDone 保证 upClose（或
	// 上游侧真实错误）先于分类读取可见。该等待各路径都快速收敛：关闭帧
	// 送达 / 首退已 relayCancel（阻塞 Read 立即返回）/ 连接死亡。
	<-upLoopDone

	// 分类与关闭传播（与 SSE caller 同构；relayClassify 纯函数可单测）：
	//   ① 上游正常关闭（1000/1001）→ 成功 200 ErrNone + ResultOK
	//   ② 客户端断开/关闭          → 200 ErrAbort（上游已消费请求；不 MarkResult）
	//   ③ 上游错误关闭/网络错误/心跳失联 → recordStreamAbort + ResultError
	// 关闭传播在取消之前：client.Close 握手本身解除客户端循环的阻塞 Read
	// （对端回关闭帧 → Read 自然返回退出），客户端拿到正常关闭帧。
	u := usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc, calls: img}
	logCtx := relayCtx
	if ttft != nil {
		logCtx = context.WithValue(relayCtx, ctxKeyTTFT{}, ttft)
	}
	// 记录全部先行、关闭帧后发：客户端"感知会话结束"与"用量记录入队"之间有
	// 竞态窗口——若先发关闭帧，对侧读到后立刻断开/网关停机收尾（rec.Close），
	// finish 的 Record 落在 Close 之后即丢（无消费者）。先入队再关，任何时序下
	// 记录不丢（优雅停机"等在途归零"语义）。
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
		_ = up.CloseNow() // 上游已死/失联，免握手等待
	}
	relayCancel()
	wg.Wait()
	return true, ""
}

// isNormalWSClose 正常结束的关闭帧（1000 正常 / 1001 离开——上游完成流后关闭）。
func isNormalWSClose(err error) bool {
	var ce websocket.CloseError
	return errors.As(err, &ce) && isNormalWSCloseCode(ce.Code)
}

// isNormalWSCloseCode 正常结束的关闭码（1000 正常 / 1001 离开）。
func isNormalWSCloseCode(code websocket.StatusCode) bool {
	return code == websocket.StatusNormalClosure || code == websocket.StatusGoingAway
}

// relayEnd 关闭路径分类结果（relayClassify 纯函数输出，编排据此收尾）。
type relayEnd int

const (
	// relayEndUpstreamClosed 上游正常关闭（1000/1001）——流已完成，成功收尾。
	relayEndUpstreamClosed relayEnd = iota
	// relayEndClientAbort 客户端断开/关闭——上游已消费请求，记录但不计冷却。
	relayEndClientAbort
	// relayEndUpstreamError 上游错误关闭/网络错误/心跳失联——计冷却。
	relayEndUpstreamError
)

// relayClassify 错误分类（I-1 修复核心）：上游关闭帧（upClose）优先于一切——
// 客户端循环并发写失败（net.ErrClosed）只归因网络错误槽，绝不覆盖关闭帧
// （健康上游 + 正常关闭帧 → 恒成功）；无关闭帧时：客户端断开 → abort，
// 其余（上游错误/失联）→ 错误。返回结束类型与收尾依据错误（abort/error
// 分支的关闭码与记录来源）。约定：至少一个槽非 nil（首退者必记录）。
func relayClassify(upClose *websocket.CloseError, upErr, clientErr, pingErr error) (relayEnd, error) {
	switch {
	case upClose != nil && isNormalWSCloseCode(upClose.Code):
		return relayEndUpstreamClosed, nil
	case clientErr != nil:
		return relayEndClientAbort, clientErr
	default:
		abortErr := upErr
		if abortErr == nil {
			abortErr = pingErr
		}
		if abortErr == nil {
			abortErr = upClose // 上游错误关闭帧（1011 等，无网络错误记录时）
		}
		return relayEndUpstreamError, abortErr
	}
}

// wsCloseStatus 从错误提取对端关闭码（非关闭帧错误 → 内部错误 1011，传播语义）。
func wsCloseStatus(err error) websocket.StatusCode {
	var ce websocket.CloseError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return websocket.StatusInternalError
}

// sniffResponsesCompleted 热路径预筛：bytes.Contains 零分配子串预筛
// response.completed 帧（命中才最小字节扫描取 usage——response.usage 前缀
// 五计数：input/output/total + cache_read/cache_creation 明细）；流式中间帧
// 零解析。ok 语义 = usage 存在（随 responsesCompletedUsage 签名改写，非"子串
// 命中"）：误命中帧（内容文本含该子串）与 error 终态（{"type":
// "response.completed","response":{...,"error":...}} 无 usage）→ ok=false 不
// 更新——completed 终态唯一、元组仅此处写入（此前值恒 0），与旧行为（覆盖 0）
// 实际等价，勿误判为行为回归。
func sniffResponsesCompleted(frame []byte) (usageTuple, bool) {
	if !bytes.Contains(frame, []byte(`"type":"response.completed"`)) {
		return usageTuple{}, false
	}
	return responsesCompletedUsage(frame)
}

// isWebSocketUpgrade 升级请求判定（coder/websocket 未导出该检查，与库内
// Accept 的校验同构：Connection 头含 Upgrade token 且 Upgrade 头含
// websocket token，均大小写不敏感、逗号分隔）。路由与编排共用。
func isWebSocketUpgrade(r *http.Request) bool {
	return headerHasToken(r.Header, "Connection", "Upgrade") &&
		headerHasToken(r.Header, "Upgrade", "websocket")
}

// headerHasToken 头值按逗号拆 token 匹配（RFC 7230 列表语义；case-insensitive）。
func headerHasToken(h http.Header, key, token string) bool {
	for _, v := range h.Values(key) {
		for _, part := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(part), token) {
				return true
			}
		}
	}
	return false
}

// wsPassthroughHeaders 客户端头透传（WS 握手面，显式契约）：透传是 WS 面
// 有意行为——上游依赖客户端特征头（User-Agent/Origin/自定义头）做校验与
// 路由，SDK 伪装研究完成前此面不动。剔除面 = 连接级 hop-by-hop 头
// （Connection/Upgrade/Sec-WebSocket-*/Host/Content-Length，其中
// Sec-WebSocket-Protocol 透传会让上游协商网关不支持的子协议 → 握手失败）
// + 网关凭据（Authorization/x-api-key 同载网关 key——auth.go 任一非空即鉴权，
// 不得直通上游；账号鉴权由 aiclient 注入）。codex 面 codexWSPassthroughHeaders
// 委托本函数（再剔 session 头族 + OpenAI-Beta），本表变更双面自动覆盖。
// HTTP 面零透传（rawPostCT 只设 Content-Type + 账号鉴权头，客户端原始头不达
// 上游）同属有意契约——与 WS 面不对称是裁决非遗漏，互引见
// pkg/aiclient/aiclient.go rawPostCT。其余头原样透传。
func wsPassthroughHeaders(h http.Header) http.Header {
	out := make(http.Header, len(h))
	for k, v := range h {
		switch http.CanonicalHeaderKey(k) {
		case "Connection", "Upgrade",
			"Sec-Websocket-Key", "Sec-Websocket-Version",
			"Sec-Websocket-Protocol", "Sec-Websocket-Extensions",
			"Host", "Content-Length", "Authorization", "X-Api-Key":
			continue
		}
		out[k] = v
	}
	return out
}

// wsWriteError 向已升级客户端发送 error 事件帧后关闭（WS 无 HTTP 状态码，
// 拒绝语义经事件帧承载；客户端按 Responses WS 协议渲染）。写/关超时防挂死
// （responsesWSCloseTimeout 写超时 + 库内 5s 关闭握手超时）。
func wsWriteError(client *websocket.Conn, msg string) {
	b, err := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"message": msg},
	})
	if err != nil {
		b = []byte(`{"type":"error","error":{"message":"gateway error"}}`)
	}
	ctx, cancel := context.WithTimeout(context.Background(), responsesWSCloseTimeout)
	defer cancel()
	_ = client.Write(ctx, websocket.MessageText, b)
	_ = client.Close(websocket.StatusNormalClosure, "")
}

// emOr 取非空错误文本（4xx 透传：上游 body message 优先，缺省回退网关文案）。
func emOr(msg, fallback string) string {
	if msg == "" {
		return fallback
	}
	return msg
}
