// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/tidwall/sjson"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/rule"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/logx"
)

// --- resp-ws relay 合一骨架（D2：relayResponsesWS/relayCodexWS 双份并发状态机
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
// = 请求帧非流式中间帧，亦为 W4 图像剥离的帧级预处理点；调用方已预处理
// stripTier 删除结果——原样转发）→ 转发首帧 → 双向事件帧 1:1 relay（流式中
// 间帧零解析零拷贝直转）→ 关闭/错误传播 → usage 记录。返回 (handled, fwMsg)：
// handled = 请求已处理完毕（成功/客户端断开/流中止已记录）；false = 首帧转
// 发失败（上游未消费请求），fwMsg 为截断错误文本，调用方按连接级错误转移
// （MarkResult + Release + 重选）。
// frameHook 可选（nil = aiclient 路径零开销——指针比较）：**读帧成功后、
// usage 嗅探（sniffResponsesCompleted）与 client.Write 之前调用**（与现状
// codex_responses_ws.go:339-341 先于 354 的调用序一致——codex 路径判死帧
// FatalAuth 钩子；客户端写失败时判死帧仍触发 FatalAuth）。
func (p *Proxy) relayWS(client *websocket.Conn, up wsRelayTransport, frameHook func([]byte), r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, firstTyp websocket.MessageType, first []byte) (handled bool, fwMsg string) {
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
	// 副作用——relayCtx 存活 = 本退出是首因；首因到达 endCh → 编排等上游
	// 读者退出 → 分类 → 取消对侧 → 等全部退出。
	//
	// I-1 竞态（评审裁决"修"）：上游关闭帧与客户端活跃写帧并发时，上游侧
	// 错误槽 upErr 有两个并发写者——up-loop 的关闭帧（CloseError）与
	// client-loop 的写失败（net.ErrClosed，库在解码关闭帧后 c.close() 所致）
	// ——首写生效下 net.ErrClosed 可能先被记录 → 健康上游误判连接级错误
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
	// relayRecover 三 goroutine 的 panic 收尾（F2 崩溃面：任一 goroutine panic
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
		defer close(upLoopDone) // 编排等本读者退出后再分类（I-1 记录可见性）
		defer relayRecover("up-loop", &clientErr) // panic 按身份入槽：本 goroutine 故障归客户端侧
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
			if frameHook != nil {
				frameHook(f) // codex 路径：判死帧 → FatalAuth（T5 §3 唯一跨边界点；判死帧照常透传客户端）
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
		defer relayRecover("heartbeat", &pingErr) // panic 按身份入槽：本 goroutine 故障归心跳错误
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
	//   ① 上游正常关闭（1000/1001）→ 成功 200 ErrNone + KindOK
	//   ② 客户端断开/关闭          → 200 ErrAbort（上游已消费请求；不 MarkResult）
	//   ③ 上游错误关闭/网络错误/心跳失联 → recordStreamAbort + 连接级分流
	//      （RuleKindOf(0) → network）
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
		p.sched.MarkResult(sel.AccountID, rule.KindOK, nil, http.StatusOK, "")
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
		p.sched.MarkResult(sel.AccountID, scheduler.RuleKindOf(0), nil, 0, endErr.Error())
		_ = client.Close(wsCloseStatus(endErr), "")
		up.CloseNow() // 上游已死/失联，免握手等待
	}
	relayCancel()
	wg.Wait()
	return true, ""
}
