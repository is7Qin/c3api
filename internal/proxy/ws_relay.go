// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/tidwall/sjson"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/logx"
)

// --- resp-ws relay 合一骨架（relayResponsesWS/relayCodexWS 双份并发状态机
// 合一，用户裁决抽 5 方法传输接口） ---
// 双份差异只在传输面（上游具体类型/typ 语义/每帧判死钩子），状态机段（首帧改
// 写 → 三向 goroutine relay → 分类 → 关闭传播 → 记录）逐字同款——骨架只合一
// 逐字同款段，差异收口在 wsRelayTransport 5 方法 + 一个可选 frameHook。

// wsRelayTransport 双向 relay 传输面抽象（传输接口，5 方法——用户裁决形状）：
// 语义与现状逐方法对应；codex 实现（SDK 具体类型）对 typ 恒 MessageText
// （responses WS 协议全 text 帧——现状 relayCodexWS 的既有降级语义）。
// 方法签名即 relayWS 骨架调用点逐一对位；热路径每帧零新增分配（无参数装箱、
// 无回调注册——frameHook 只是可选函数指针）。
type wsRelayTransport interface {
	Write(ctx context.Context, typ websocket.MessageType, frame []byte) error // 客户端 → 上游
	Read(ctx context.Context) (websocket.MessageType, []byte, error)          // 上游 → 客户端
	Ping(ctx context.Context) error
	Close(code websocket.StatusCode, reason string) error
	CloseNow()
}

// relayWS 合一骨架：首帧模型改写（ModelMapping 语义，与 setModel 同构；首帧
// = 请求帧非流式中间帧，图像剥离预处理点；调用方已预处理 stripTier——原样转发）
// → 转发首帧 → 双向事件帧 1:1 relay（流式中间帧零解析零拷贝直转）→ 关闭/错误传播
// → usage 记录。返回 (handled, fwMsg)：handled = 请求已处理完毕（成功/客户端断开/
// 流中止已记录）；false = 首帧转发失败（上游未消费，记 not-sent 可重试），fwMsg
// 为截断错误文本，调用方按连接级错误转移。业务帧未送达前可重试，送达后不再迁移。
// frameHook 可选（nil = aiclient 路径零开销——指针比较）：**读帧成功后、
// usage 嗅探（sniffResponsesCompleted）与 client.Write 之前调用**（与现状
// codex_responses_ws.go:339-341 先于 354 的调用序一致——codex 路径判死帧
// FatalAuth 钩子；客户端写失败时判死帧仍触发 FatalAuth）。
func (p *Proxy) relayWS(client *websocket.Conn, up wsRelayTransport, frameHook func([]byte), r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, firstTyp websocket.MessageType, first []byte) (handled bool, fwMsg string) {
	// 首帧未送达视为上游未消费，不可记业务帧已见，保留可重试语义。
	frame := first
	if sel.Model != "" && sel.Model != reqModel {
		if nf, err := sjson.SetBytes(first, "model", sel.Model); err == nil {
			frame = nf
		} // 改写失败（帧非合法 JSON）→ 原样转发，上游自行校验
	}
	if err := up.Write(r.Context(), firstTyp, frame); err != nil {
		up.CloseNow()
		return false, domain.TruncateErrMsg(err.Error())
	}

	// --- 双向 relay：三个方向各自 goroutine，首退者触发取消 ---
	// 每个 goroutine 退出时把"本侧真实错误"记录到共享变量（仅当退出非取消
	// 副作用——relayCtx 存活 = 本退出是首因；首因到达 endCh → 编排取消全侧
	// → 等上游读者与心跳退出 → endMu 快照分类 → 关闭传播解除 client-loop
	// 的阻塞 Read → 等全部退出。
	//
	// 上游关闭帧与客户端活跃写帧并发竞态：上游侧
	// 错误槽 upErr 有两个并发写者——up-loop 的关闭帧（CloseError）与
	// client-loop 的写失败（net.ErrClosed，库在解码关闭帧后 c.close() 所致）
	// ——首写生效下 net.ErrClosed 可能先被记录 → 健康上游误判连接级错误
	// 冷却。修复：关闭帧记录到独立槽 upClose（仅 up-loop 写入、无取消守卫
	// ——真实帧永不丢），分类时正常关闭帧优先于一切；写失败只归因网络错误
	// 槽（无关闭帧时才判错）。upLoopDone/hbDone 保证 upClose/pingErr 先于
	// 分类读取可见（记录 happens-before 退出 happens-before close(done)）；
	// 分类读取在 endMu 下快照——client-loop 唯一可能晚于快照的写者（其 Read
	// 由分类关闭帧解除），写与快照读取同锁互斥，无数据竞争；relayCancel 先
	// 行使这类迟到写全部落入取消守卫（合成取消错误不改判）。
	//
	// 关键细节：客户端循环的阻塞 Read 用 r.Context()（非 relayCtx）——库对
	// 取消中的阻塞 Read 会直接拆连接（客户端拿不到正常关闭帧）；客户端循环
	// 的退出由编排的分类关闭帧（Close 握手）自然解除：对端回关闭帧 → Read
	// 返回关闭错误 → 退出。取消仅用于上游侧（上游已结束/失联，直拆无害）；
	// contFail 收尾无正常关闭帧可等，改写错误帧后 CloseNow 直拆解除。
	// implicit 响应身份：非空时重写上游→客户端 text 帧的模型字段（显式/无映射保持直通零扫描）
	respModel := sel.ClientResponseModel(reqModel)
	// 硬续接（WS create 面）：store 装配且非 codex 传输时，每个 response id
	// 在 Redis ACK 前不得转发客户端（ACK-before-visible）；codex/未装配 =
	// 零变化零开销。
	contTag := ""
	if p.cont != nil && !isCodexCredentialType(sel.CredentialType) {
		contTag = contProtocolWS
	}
	var (
		contAcked    bool
		contLastID   string
		contPending  []wsContFrame
		contPendingN int
		contFail     *formatError
	)
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
	// recordClose 记录上游关闭帧（仅 CloseError，仅上游读循环写入）。关闭帧优先级最高，
	// 不设取消守卫，确保真实关闭帧不被并发写失败覆盖。
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
	// relayRecover 三 goroutine 的 panic 收尾（崩溃面：任一 goroutine panic
	// 未 recover → 杀整个进程）。记录**先于 exit()**（与 setErr→exit 正常路径
	// 同序，spec 变更 2"错误槽 setErr + 取消对侧"）：编排在 <-upLoopDone 后读
	// 槽——up-loop 的退出由本 goroutine exit 的取消触发（relayCtx 取消链：
	// 记录 → exit → 取消 → up-loop 读返回 → close(upLoopDone) → 编排读取），
	// 记录须 happens-before 取消，否则 client-loop/heartbeat 的槽写入与编排
	// 读取无同步边（数据竞争）。记录不走 setErr——其 relayCtx 守卫在取消后
	// 吞掉记录；且 panic 若恰在 setErr/recordClose 临界区内（理论情形——临界
	// 区无用户代码，仅指针写），recover 后再取 endMu 即重入死锁，故直接 endMu
	// 首写（首写生效，与 setErr 同语义）。defer 注册序（LIFO）：本函数最后
	// 注册、最先执行——记录 happens-before 后续的 close(upLoopDone)/wg.Done。
	relayRecover := func(who string, dst *error) {
		if rec := recover(); rec != nil {
			err := fmt.Errorf("ws relay panic: %v", rec)
			endMu.Lock()
			if *dst == nil {
				*dst = err
			}
			endMu.Unlock()
			if p.log != nil {
				p.log.Error("ws relay panic",
					logx.String("request_id", reqID),
					logx.Int64("account_id", sel.AccountID),
					logx.String("goroutine", who),
					logx.Any("panic", rec),
				)
			}
			exit()
		}
	}

	wg.Add(1)
	go func() { // 客户端 → 上游（客户端帧透传；写失败 = 上游侧问题）
		defer wg.Done()
		defer relayRecover("client-loop", &upErr) // panic 按身份入槽：本 goroutine 故障归上游侧
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
		defer close(upLoopDone)                   // 编排等本读者退出后再分类，保证记录可见性
		defer relayRecover("up-loop", &clientErr) // panic 按身份入槽：本 goroutine 故障归客户端侧
		for {
			// ACK 前读上限：停滞上游不得无限扣住未绑定帧（无 ACKed 绑定时
			// 生效；ACK 后恢复无总体流超时的既有设计）。
			readCtx := relayCtx
			var readCancel context.CancelFunc
			if contTag != "" && !contAcked {
				readCtx, readCancel = context.WithTimeout(relayCtx, contWSAckTimeout)
			}
			typ, f, err := up.Read(readCtx)
			if readCancel != nil {
				readCancel()
			}
			if err != nil {
				// 读失败分类：关闭帧进独立槽优先级最高，其余网络错误进 upErr，槽位分离避免覆盖
				var ce websocket.CloseError
				if errors.As(err, &ce) {
					recordClose(err)
				} else {
					setErr(&upErr, err)
					if contTag != "" && !contAcked && relayCtx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
						// ACK 期限到：id 帧未现，缓冲帧弃置 fail-closed
						contFail = errContUnavailable
					}
				}
				exit()
				return
			}
			if frameHook != nil {
				frameHook(f) // codex 路径：判死帧 → FatalAuth（唯一跨边界点，判死帧照常透传客户端）
			}
			// 热路径纪律：bytes.Contains 零分配预筛，命中才最小字节扫描取 usage
			// （usage_extract.go scanKeyValue 单遍扫描零分配）；流式中间帧
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
			out := f
			if typ == websocket.MessageText && respModel != "" {
				out = rewriteResponseModelJSON(f, respModel)
			}
			// 硬续接闸门（仅 responses 非 codex 传输）：id 帧在 Redis ACK 前
			// 不得达客户端；ACK 后新 id（多轮 response.create）同样先绑后转。
			if contTag != "" {
				id := contFrameID(out)
				if !contAcked {
					if id == "" {
						contPendingN += len(out)
						if contPendingN > contMaxBuffer {
							contFail = errContUnavailable
							exit()
							return
						}
						contPending = append(contPending, wsContFrame{typ: typ, frame: out})
						continue
					}
					if ferr := p.contBind(r.Context(), contTag, id, groupID); ferr != nil {
						contFail = ferr
						exit()
						return
					}
					contAcked = true
					contLastID = id
					for _, pf := range contPending {
						if err := client.Write(relayCtx, pf.typ, pf.frame); err != nil {
							setErr(&clientErr, err)
							exit()
							return
						}
					}
					contPending, contPendingN = nil, 0
				} else if id != "" && id != contLastID {
					if ferr := p.contBind(r.Context(), contTag, id, groupID); ferr != nil {
						contFail = ferr
						exit()
						return
					}
					contLastID = id
				}
			}
			if err := client.Write(relayCtx, typ, out); err != nil {
				setErr(&clientErr, err)
				exit()
				return
			}
		}
	}()

	hbDone := make(chan struct{})
	wg.Add(1)
	go func() { // 心跳：向上游周期 Ping，pong 超时视为上游失联按网络错误收尾
		defer wg.Done()
		defer close(hbDone)                             // 编排等本侧退出后再快照——pingErr 写入 happens-before 本关闭
		defer relayRecover("heartbeat", &pingErr)       // panic 按身份入槽：心跳失败归 pingErr
		ticker := time.NewTicker(p.wsHeartbeatInterval) // 可注入缩短以便测试
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
	// 幂等取消先行：exit 的 relayCancel 滞后于 endCh 信号——先取消，此后所有
	// 解除路径（心跳 Ping/上游读、client-loop 的 up.Write）的退出错误都落入
	// setErr 取消守卫，合成取消错误不再改判。
	relayCancel()
	// 等上游读者与心跳退出再分类：关闭帧解码与客户端写失败并发，
	// upLoopDone/hbDone 保证 upClose/pingErr 先于快照可见。
	<-upLoopDone
	<-hbDone

	// 分类与关闭传播（纯函数可单测）：
	// ① 上游正常关闭（1000/1001）→ 成功，已见业务帧；② 客户端断开 → abort，已见业务帧不计冷却；
	// ③ 上游错误/网络错误/心跳超时 → 失败，已见业务帧但需冷却。关闭帧优先级高于读写错误。
	// 先记录后发关闭帧：避免关闭帧先达使对端断开导致记录丢失，优雅停机需等在途归零。
	// 槽读取在 endMu 下快照：client-loop 是唯一可能未退出的写者（其 Read 由
	// 下方关闭传播解除），同锁互斥即无数据竞争。
	u := usageTuple{it: it, ot: ot, tt: tt, cr: cr, cc: cc, calls: img}
	logCtx := relayCtx
	if ttft != nil {
		logCtx = context.WithValue(relayCtx, ctxKeyTTFT{}, ttft)
	}
	if contFail != nil {
		// 硬续接 fail-closed（WS create 面）：绑定权威不可用/冲突——缓冲帧
		// 弃置（未绑定 id 永不达客户端），错误帧可达后收尾；已消耗用量保留
		// 计费；网关侧失败不冷却账号（health=nil）。ACK 后新 id 绑定失败 =
		// 帧已可见，走 sent_ambiguous 终态（与流中止同轨）。
		// 错误帧写出后 CloseNow 直拆：空闲客户端不回关闭握手，优雅 Close 会把
		// 收尾挂在库内握手超时上（5s）——relay 退出不得等客户端响应，主动
		// 拆连接解除 client-loop 的阻塞 Read 后 wg.Wait 全 join。
		ectx, ecancel := context.WithTimeout(context.Background(), responsesWSCloseTimeout)
		if err := client.Write(ectx, websocket.MessageText, wsErrorFrame(contFail.msg)); err != nil && p.log != nil {
			p.log.Warn("continuation error frame write failed", logx.String("request_id", reqID), logx.Error(err))
		}
		ecancel()
		_ = client.CloseNow()
		base := mergeDispatchBase(logCtx, wsDispatchedBase(sel, reqModel, start))
		out := base
		logStatus, logET := contFail.status, domain.Err5xx
		if contAcked {
			out = wsOutcomeForUpstreamError(base, u, ttft)
			logStatus, logET = http.StatusOK, domain.ErrAbort
		} else {
			out.Result = ResultFailed
			out.HTTPStatus = AttemptStatus(contFail.status)
			out.Commit = CommitUpstreamResponded
			out.Terminal = true
			out.Usage = wsUsageFromTuple(u)
			out.Timing.TTFTMS = ttft
		}
		p.observeDispatchOutcome(logCtx, out, nil)
		l := logWithCtx(logCtx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), domain.FormatOpenAIResponsesWS, logStatus, logET, u, start))
		p.finish(sel, l)
		up.CloseNow()
		wg.Wait()
		return true, ""
	}
	if contTag != "" && !contAcked && len(contPending) > 0 {
		// 流结束而无 id 帧（无可续接身份）：缓冲字节安全放出（与 REST 闸门
		// EOF 放行同轨——pending 恒无 id，放出零绑定泄漏）。
		fctx, fcancel := context.WithTimeout(r.Context(), responsesWSCloseTimeout)
		for _, pf := range contPending {
			if err := client.Write(fctx, pf.typ, pf.frame); err != nil {
				endMu.Lock()
				clientErr = err
				endMu.Unlock()
				break
			}
		}
		fcancel()
		contPending = nil
	}
	endMu.Lock()
	uc, ue, ce, pe := upClose, upErr, clientErr, pingErr
	endMu.Unlock()
	end, endErr := relayClassify(uc, ue, ce, pe)
	base := mergeDispatchBase(logCtx, wsDispatchedBase(sel, reqModel, start))
	switch end {
	case relayEndUpstreamClosed:
		_ = up.Close(websocket.StatusNormalClosure, "") // 上游已发关闭帧，完成握手
		// 先记录后关：业务帧已见，成功落盘后再向客户端发关闭帧
		_ = p.reportWSOutcome(logCtx, wsOutcomeForSuccess(base, ttft, u), sel, reqID, groupID, reqModel, start, u, ttft)
		_ = client.Close(websocket.StatusNormalClosure, "")
	case relayEndClientAbort:
		_ = client.CloseNow() // 客户端已断开，免握手等待
		code := websocket.StatusGoingAway
		if isNormalWSClose(endErr) {
			code = wsCloseStatus(endErr)
		}
		// 客户端 abort：业务帧已见但不计冷却，向上游传播关闭
		_ = p.reportWSOutcome(logCtx, wsOutcomeForClientAbort(base, u, ttft), sel, reqID, groupID, reqModel, start, u, ttft)
		_ = up.Close(code, "")
	case relayEndUpstreamError:
		// 上游错误/心跳超时：业务帧已见但连接异常，需冷却
		_ = p.reportWSOutcome(logCtx, wsOutcomeForUpstreamError(base, u, ttft), sel, reqID, groupID, reqModel, start, u, ttft)
		_ = client.Close(wsCloseStatus(endErr), "")
		up.CloseNow() // 上游已失联，免握手等待
	}
	wg.Wait() // client-loop 的阻塞 Read 已被上方关闭传播/直拆解除
	return true, ""
}

// wsErrorFrame 网关终态错误帧（与 wsWriteError 的帧形一致）。contFail 收尾
// 自写帧后 CloseNow 直拆——不能复用 wsWriteError 的优雅 Close：空闲客户端
// 不回关闭握手，收尾会挂在库内 5s 握手超时上。
func wsErrorFrame(msg string) []byte {
	b, err := json.Marshal(map[string]any{
		"type":  "error",
		"error": map[string]any{"message": msg},
	})
	if err != nil {
		return []byte(`{"type":"error","error":{"message":"gateway error"}}`)
	}
	return b
}
