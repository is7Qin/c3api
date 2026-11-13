// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
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
	"github.com/is7qin/c3api/pkg/sserelay"
)

// --- resp-ws relay 合一骨架（relayWS：双传输面共用的并发状态机，传输差异抽
// 到 5 方法 wsRelayTransport 接口 + 一个可选 frameHook） ---
// 双传输面差异只在传输层（上游具体类型/typ 语义/每帧判死钩子），状态机段（首帧
// 改写 → 三向 goroutine relay → 分类 → 关闭传播 → 记录）逐字同款——骨架只合一
// 逐字同款段，差异收口在 wsRelayTransport 5 方法 + 一个可选 frameHook。

// wsRelayTransport 双向 relay 传输面抽象（传输接口，5 方法）：
// 语义与各实现逐方法对应；codex 实现（SDK 具体类型）对 typ 恒 MessageText
// （responses WS 协议全 text 帧——既有降级语义）。
// 方法签名即 relayWS 骨架调用点逐一对位；热路径每帧零新增分配（无参数装箱、
// 无回调注册——frameHook 只是可选函数指针）。
type wsRelayTransport interface {
	Write(ctx context.Context, typ websocket.MessageType, frame []byte) error // 客户端 → 上游
	Read(ctx context.Context) (websocket.MessageType, []byte, error)          // 上游 → 客户端
	Ping(ctx context.Context) error
	Close(code websocket.StatusCode, reason string) error
	CloseNow()
}

// relayWS 合一骨架：首帧由调用方预处理（模型改写见 wsFirstFrameForSel——
// ModelMapping 语义，与 setModel 同构；非 Codex 另有 stripTier）后原样转发
// → 转发首帧 → 双向事件帧 1:1 relay（流式中间帧零解析零拷贝直转）→ 关闭/错误传播
// → usage 记录。返回 (handled, fwErr)：handled = 请求已处理完毕（成功/客户端断开/
// 流中止已记录）；false = 首帧转发失败（上游未消费，记 not-sent 可重试），fwErr
// 为原始错误（保留链），调用方截断成文本后按连接级错误转移。业务帧未送达前可重试，送达后不再迁移。
// frameHook 可选（nil = aiclient 路径零开销——指针比较）：**读帧成功后、
// usage 嗅探（sniffResponsesCompletedUsage）与 client.Write 之前调用**——codex
// 路径的判死帧 FatalAuth 钩子即挂此处（客户端写失败时判死帧仍触发 FatalAuth）。
func (p *Proxy) relayWS(client *websocket.Conn, up wsRelayTransport, frameHook func([]byte), r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, firstTyp websocket.MessageType, first []byte) (handled bool, fwErr error) {
	if err := relayWSFirstFrame(up, r, firstTyp, first); err != nil {
		return false, err
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
	// 零变化零开销。Codex 不绑定续接：contTag 为空使下方两处 contBind
	// （W6 首个 id、W7 后续新 id）对 Codex 不可达。
	contTag := ""
	if p.cont != nil && !sel.CredentialType.IsCodex() {
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

	wg.Go(func() { // 客户端 → 上游（客户端帧中继；codex 面由传输适配层按 codex 形状归一；写失败 = 上游侧问题）
		defer relayRecover("client-loop", &upErr) // panic 按身份入槽：本 goroutine 故障归上游侧
		for {
			typ, f, err := client.Read(r.Context())
			if err != nil {
				setErr(&clientErr, err)
				exit()
				return
			}
			if err := up.Write(relayCtx, typ, f); err != nil {
				// SDK 本地拒绝（§5.4，仅 Codex 传输会返回）：该帧未出网，连接完好——
				// 回客户端一帧错误、丢弃该帧、会话继续；不写错误槽、不改结束分类。
				if isCodexInvalidBody(err) {
					convLocalRejects.Add(1)
					_ = writeCodexInvalidBodyFrame(relayCtx, client)
					continue
				}
				setErr(&upErr, err)
				exit()
				return
			}
		}
	})

	upLoopDone := make(chan struct{})
	// 入站 WS 的流内错误载荷记账（spec-inband-error-truth §3-3 第 5 行）：帧必须
	// 照旧 1:1 转发（错误即退役的连接策略不改），所以判据只置位、不阻断。
	// 无需加锁：只在 up-loop 内写，编排读它之前先 `<-upLoopDone`（close 提供
	// happens-before）。
	var upPayloadErr *sserelay.PayloadError
	// businessN 是真实下行业务帧计数（spec-stream-failure-truth §5.3）：计费资格
	// （commit_state）的事实源。旧实现里 wsOutcomeFor{ClientAbort,UpstreamError}
	// 的 BusinessFrameSent=true 是构造常量而非事实。只统计「上游帧成功写客户端」
	// 的次数——网关自己生成的错误帧（contFail 收尾 / wsWriteError）、关闭帧、心跳
	// ping 都不计业务交付。同样只在 up-loop 内写，编排在 `<-upLoopDone` 之后读。
	var businessN int
	// downstreamWriteFailed 是真实下行写失败事实（client.Write 返回错误）：
	// commitStateFor 的 writeFailed 入参。只在 up-loop 与主 goroutine 的收尾 flushing
	// 写，读取前分别由 upLoopDone/wg 建立 happens-before。
	var downstreamWriteFailed bool
	// roundCompleted 是「本轮是否已读到协议终态帧」这一**分类输入**（spec §3 归因表
	// 的 completed 列）：只在本 goroutine 内写，读取在 `<-upLoopDone` 之后（close 提供
	// happens-before）。它不落库（I3）——只进 relayClassify 的入参。
	// 终态帧即本轮末帧：之后还有帧 ⇒ 已是下一轮，清零（长连接可承载多轮；轮状态
	// 机另属后续提交，本处只保留「当前轮」这一个事实）。
	var roundCompleted bool
	wg.Go(func() { // 上游 → 客户端（热路径：预筛嗅探 response.completed 取 usage）
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
				if _, ok := errors.AsType[websocket.CloseError](err); ok {
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
			// 流内错误载荷记账（spec-inband-error-truth §3-3）：每读到上行帧就调
			// **同一个**判据；命中 ⇒ 置位（帧照旧 1:1 转发，不阻断——入站 WS 已升级，
			// 没有换号机会，只改记账与账号健康）。**轮级清零**：长连接可承载多轮，
			// 只在 response.completed 处清（与 usage 的"最后一帧覆盖"记账同向）。
			// 位置必须在续接闸门之前：闸门命中会把帧塞进 contPending 后 continue，
			// 放在闸门之后会漏掉被缓冲的错误帧。
			// frameErr 同时作为业务计数排除条件：上游错误帧是失败信号，不是业务交付。
			frameErr := sserelay.AsPayloadError(sserelay.Event{Data: f})
			if frameErr != nil {
				upPayloadErr = frameErr
			}
			if upPayloadErr != nil && isResponsesCompleted(f) {
				upPayloadErr = nil
			}
			// 本轮终态帧事实（§3 归因表 completed）：子串预筛（memmem、零分配、
			// 不解析 JSON）命中才做精确 type 判定（isResponsesCompleted 的
			// scanKeyValue）。终态帧即本轮末帧——之后再有帧就是下一轮 ⇒ 清零。
			// 只在 up-loop 内写，编排在 `<-upLoopDone` 之后读（close 建
			// happens-before），与 upPayloadErr/businessN 同款纪律。
			roundCompleted = bytes.Contains(f, completedTypeBytes) && isResponsesCompleted(f)
			// 热路径纪律：只嗅探 response.completed 帧（精确 type 判定，
			// usage_extract.go sniffResponsesCompletedUsage scanKeyValue 单遍扫描
			// 零分配）；流式中间帧零解析直转——网关层零分配，库层每帧 io.ReadAll
			// 物化 + flate 属库内账目。
			if u, ok := sniffResponsesCompletedUsage(f); ok {
				it, ot, tt, cr, cc = u.it, u.ot, u.tt, u.cr, u.cc
				// codex 会话轮次推进（成功取到 usage 时 Step 一次；非 codex
				// Selection 无会话 → no-op）。**多轮语义**：入站 WS 长连接一次
				// 订阅可承载多轮，每轮一帧 response.completed → 每帧各推进一次
				// （多次观测，语义正确）。与 HTTP 流式分支的 usageTaken「仅取
				// 首个」守卫有意不同：后者单次响应只可能有一个终态 completed
				// 帧，此处则每轮各一个——非缺陷，勿据文档统一。
				sel.StepIdentity()
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
						contPending = append(contPending, wsContFrame{typ: typ, frame: out, errFrame: frameErr != nil})
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
							downstreamWriteFailed = true
							setErr(&clientErr, err)
							exit()
							return
						}
						if !pf.errFrame {
							businessN++
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
				downstreamWriteFailed = true
				setErr(&clientErr, err)
				exit()
				return
			}
			// 业务交付计数：网关生成的错误帧 / 关闭帧 / ping 不计（上方），
			// 上游错误载荷帧是失败信号而非模型产出，同样不计。
			if frameErr == nil {
				businessN++
			}
		}
	})

	hbDone := make(chan struct{})
	wg.Go(func() { // 心跳：向上游周期 Ping，pong 超时视为上游失联按网络错误收尾
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
	})

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
		p.relayWSContFail(client, up, &wg, logCtx, contFail, contAcked, businessN, sel, reqID, groupID, reqModel, start, u, ttft)
		return true, nil
	}
	if contTag != "" && !contAcked && len(contPending) > 0 {
		// 流结束而无 id 帧（无可续接身份）：缓冲字节安全放出（与 REST 闸门
		// EOF 放行同轨——pending 恒无 id，放出零绑定泄漏）。
		fctx, fcancel := context.WithTimeout(r.Context(), responsesWSCloseTimeout)
		for _, pf := range contPending {
			if err := client.Write(fctx, pf.typ, pf.frame); err != nil {
				downstreamWriteFailed = true
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
	end, endErr := relayClassify(uc, ue, ce, pe, roundCompleted)
	// 收尾改判（spec-inband-error-truth §3-3）：上游"正常关闭"但本连接上见过
	// 未清零的错误帧 ⇒ 失败。**两个都换**——正常关闭时 relayClassify 的 endErr 是
	// nil，只换 end 会让 wsCloseStatus(nil) 返 1011、客户端拿到虚构的内部错误码，
	// 且 errors.AsType[*PayloadError](nil) 永不成立、"关闭码保持 1000"写不出来。
	if end == relayEndUpstreamClosed && upPayloadErr != nil {
		end, endErr = relayEndUpstreamError, upPayloadErr
	}
	p.relayWSPropagate(client, up, logCtx, sel, reqID, groupID, reqModel, start, u, ttft, businessN, downstreamWriteFailed, end, endErr)
	wg.Wait() // client-loop 的阻塞 Read 已被上方关闭传播/直拆解除
	return true, nil
}

// isResponsesCompleted 判定一帧是否是 response.completed（spec-inband-error-truth
// 轮级清零用，6 行、零分配）。
//
// 不复用 sniffResponsesCompletedUsage：它的 ok 是"是 completed 帧**且**解析到
// usage"（usage_extract.go:111-126）——无 usage 的 completed 会返 false，挂它
// 等于把清零条件收窄成"带 usage 的 completed"。这里照抄它 :117-119 的两道守卫
// （ok/越界/首字节必须是引号），否则非字符串值时 data[start+1:end-1] 会越界。
// 只在 upPayloadErr != nil 时才调（常态零成本，不新增每帧全扫）。
func isResponsesCompleted(data []byte) bool {
	start, end, ok := scanKeyValue(data, typeKeyBytes)
	if !ok || start >= len(data) || data[start] != '"' {
		return false
	}
	return bytes.Equal(data[start+1:end-1], completedTypeBytes)
}

// wsFirstFrameForSel 按本次尝试的 sel 改写首帧模型：sel.Model 非空且与 reqModel
// 不同 → sjson 改写 model（改写失败/帧非合法 JSON → 原样返回，上游或 SDK 自行
// 校验）。由 wsAttempt 在拨号前调用（每次尝试一次，故障转移换号按当次 sel 重算），
// 结果原样交给 relayWS 写出；Codex 分支还把同一份字节交给拨号（WithFirstFrame）。
func wsFirstFrameForSel(first []byte, sel *scheduler.Selection, reqModel string) []byte {
	if sel.Model != "" && sel.Model != reqModel {
		if nf, err := sjson.SetBytes(first, "model", sel.Model); err == nil {
			return nf
		}
	}
	return first
}

// relayWSFirstFrame 首帧转发（relayWS 提取）：first 已由调用方预处理（模型改写
// 见 wsFirstFrameForSel，非 Codex 分支另有 stripTier），这里原样写出；写失败 →
// CloseNow 并返回原始错误（调用方按连接级错误转移，保留可重试语义；Codex 分支先
// 识别 SDK 本地拒绝）。
//
// 重复键：首帧的重复 model/stream 在**更上游**就被拒了——caller_responses_ws.go
// 读帧之后、选号与拨号之前用 duplicateTopLevelKey 校验并回错误帧（与 HTTP 入口
// caller.go 同一条规则）。
//
// ⚠ 范围限定：该守卫只覆盖**首帧**（response.create）。后续客户端帧按字节原样
// 转发（本文件上方 client-loop），它们的 model 不参与路由、也不做重复键校验；
// 因此不要把首帧守卫描述成「整个 WS 会话的模型身份已强制」。
func relayWSFirstFrame(up wsRelayTransport, r *http.Request, firstTyp websocket.MessageType, first []byte) error {
	// 首帧未送达视为上游未消费，不可记业务帧已见，保留可重试语义。错误原样返回
	// （保留错误链：Codex 分支要按 errors.Is 识别 SDK 本地拒绝）。
	if err := up.Write(r.Context(), firstTyp, first); err != nil {
		up.CloseNow()
		return err
	}
	return nil
}

// relayWSContFail 硬续接 fail-closed 收尾（relayWS 提取）：绑定权威不可用/冲突
// ——缓冲帧弃置（未绑定 id 永不达客户端），错误帧可达后收尾；已消耗用量保留计费；
// 网关侧失败不冷却账号（health=nil）。ACK 后新 id 绑定失败 = 帧已可见，走
// sent_ambiguous 终态（与流中止同轨）。错误帧写出后 CloseNow 直拆（免库内 5s
// 握手超时），wg.Wait 全 join。记录/字段/时机逐字一致。
func (p *Proxy) relayWSContFail(client *websocket.Conn, up wsRelayTransport, wg *sync.WaitGroup, logCtx context.Context, contFail *formatError, contAcked bool, businessN int, sel *scheduler.Selection, reqID string, groupID int64, reqModel string, start time.Time, u usageTuple, ttft *int64) {
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
	// 归因（I9：relayWSContFail 属归因输入域，各出口不得自建第二套）：
	//   - ACK 前（帧未可见）：网关侧 fail-closed，状态 = 网关给出的 contFail.status
	//     （这是网关自己的终态码，不是上游状态，故不走 classifyStreamFailure）；
	//   - ACK 后（帧已可见）：与流中止同轨的派生归因——**不得再默认 abort**。
	var f streamFailure
	if contAcked {
		out = wsOutcomeForUpstreamError(base, u, ttft)
		f = classifyStreamFailure(contFail)
	} else {
		out.Result = ResultFailed
		out.HTTPStatus = AttemptStatus(contFail.status)
		out.Commit = CommitUpstreamResponded
		out.Terminal = true
		out.Usage = wsUsageFromTuple(u)
		out.Timing.TTFTMS = ttft
		f = streamFailure{
			Status:  contFail.status,
			ET:      domain.Err5xx,
			Kind:    scheduler.RuleKindOf(contFail.status),
			Message: failureMessage(contFail),
		}
	}
	// 网关侧失败不冷却账号（health=nil）：续接权威不可用是网关自身的问题。
	p.observeDispatchOutcome(logCtx, out, nil)
	// 交付事实（唯一折算入口）：framesSent = 真实业务帧计数 businessN>0，在写
	// 终态错误帧**之前**就已冻结（businessN 只由 up-loop 写，这里读的是终值）。
	// contFail 是网关 fail-closed 收尾（绑定/缓冲/ACK 失败），不是客户端侧结束，
	// 也没有业务帧写失败——writeFailed/clientCancel 恒 false；后续写 wsErrorFrame
	// 的成败不改变交付事实（错误帧不是业务帧）。
	commit := commitStateFor(businessN > 0, false, false)
	l := withCommit(logWithCtx(logCtx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.LogMappedModel(reqModel), domain.FormatOpenAIResponsesWS, f.Status, f.ET, u, start)), commit)
	// 与其余中止/失败出口同口径：contFail 的文案就是网关给出的失败原因
	// （续接权威不可用/绑定冲突），落盘后 err_logs 才有可读文本。
	l.ErrorMessage = f.message()
	p.finish(sel, l)
	up.CloseNow()
	wg.Wait()
}

// relayWSPropagate 关闭传播阶段（relayWS 提取）：按分类结果（relayEnd/endErr）
// 完成关闭握手/直拆 + reportWSOutcome 落盘（成功 / 客户端 abort / 上游错误三分支）。
// 关闭帧优先级与「先记录后关」的时机逐字一致。
func (p *Proxy) relayWSPropagate(client *websocket.Conn, up wsRelayTransport, logCtx context.Context, sel *scheduler.Selection, reqID string, groupID int64, reqModel string, start time.Time, u usageTuple, ttft *int64, businessN int, writeFailed bool, end relayEnd, endErr error) {
	base := mergeDispatchBase(logCtx, wsDispatchedBase(sel, reqModel, start))
	switch end {
	case relayEndUpstreamClosed:
		_ = up.Close(websocket.StatusNormalClosure, "") // 上游已发关闭帧，完成握手
		// 先记录后关：业务帧已见，成功落盘后再向客户端发关闭帧
		_ = p.reportWSOutcome(logCtx, wsOutcomeForSuccess(base, ttft, u), sel, reqID, groupID, reqModel, start, u, ttft, true, false, nil)
		_ = client.Close(websocket.StatusNormalClosure, "")
	case relayEndClientAbort:
		_ = client.CloseNow() // 客户端已断开，免握手等待
		code := websocket.StatusGoingAway
		if isNormalWSClose(endErr) {
			code = wsCloseStatus(endErr)
		}
		// 客户端 abort：业务帧已见但不计冷却，向上游传播关闭
		_ = p.reportWSOutcome(logCtx, wsOutcomeForClientAbort(base, businessN > 0, u, ttft), sel, reqID, groupID, reqModel, start, u, ttft, businessN > 0, writeFailed, endErr)
		_ = up.Close(code, "")
	case relayEndUpstreamError:
		// 上游错误/心跳超时：业务帧已见但连接异常，需冷却。sent/writeFailed 都是
		// relayWS 在**补写终态帧之前**快照的真实事实（businessN 计数 / 下行写
		// 失败）；reportWSOutcome 先落盘，随后才写关闭帧，故取值不随补写漂移。
		// 真实计数只进持久化（reportWSOutcome 的 sent 形参），不进 outcome 的
		// 类型层编码（见 typeLayerTerminalFailure）。
		_ = p.reportWSOutcome(logCtx, wsOutcomeForUpstreamError(base, u, ttft), sel, reqID, groupID, reqModel, start, u, ttft, businessN > 0, writeFailed, endErr)
		code := wsCloseStatus(endErr)
		// 流内错误载荷（spec-inband-error-truth §3-3）：上游**确实**是正常关闭
		// （发完错误帧后 1000），客户端观感不得变——关闭码保持 1000，不虚构 1011。
		if _, ok := errors.AsType[*sserelay.PayloadError](endErr); ok {
			code = websocket.StatusNormalClosure
		}
		_ = client.Close(code, "")
		up.CloseNow() // 上游已失联，免握手等待
	}
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
