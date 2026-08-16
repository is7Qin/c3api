// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
	"github.com/is7qin/c3api/pkg/logx"
)

// imageStreamGenerator 流式生图能力——sdkbridge.Codex.GenerateImageStream
// 同签名（cred → Auth 缓存 / 信封包装 / fatal 统一回调全在适配层内，网关侧只
// 消费本签名）。生产接线：codexImagesCaller stream 分支同签名直赋适配层方法
// （T3 接线提交）；单测传 fake 替身（替身不落生产代码——调用面独立验证）。
type imageStreamGenerator func(ctx context.Context, cred *domain.AccountCredential, p *domain.ImageGenParams, fn func(domain.ImageStreamEvent) error) error

// streamImageGeneration codex 类型流式生图分支（T3——caller_images 流式面）：
// 适配层 GenerateImageStream 事件流 → SSE 透传 + 流终/abort 计费。调用方
// （T2/B images 路由）负责：codex 类型判定、body → params 解析、GetImagePrice
// 预检、cred 派生（AccountExt → AccountCredential）。超时由本分支自行施加
// （UpstreamStreamTimeout 语义，镜像 responsesCaller——上游停滞超时走上游
// 错误分支，不得当作客户端断开）。
//
// SSE 透传语义（spec §5.2 + T3 spec，wire 形态 P2-1/P2-2 定死）：
//   - 收到首事件即发 SSE 响应头（CF 524 免疫——响应头一旦发出 524 即免疫；
//     keepalive 保证 120s 内必有字节流）；响应头对齐既有三件套
//     （caller_responses.go:60-62）+ WriteHeader(200)；每事件写入后 Flush
//     （keepalive 后不 Flush = 假免疫）
//   - keepalive 事件 → SSE 注释行 ": ping"（透传）
//   - image_generation.completed 事件 → SSE 帧（b64_json + usage——usage 仅
//     末事件携带；字段映射 = ImageUsage JSON tag 直透）；completed 计数
//     （call_count，每张图一个）
//   - 未知事件类型跳过（SDK 合成流不产出，防御）
//
// 错误与计费（对齐 recordStreamAbort 既有语义 + abort 双分支镜像
// caller_responses.go:92-101）：
//   - 首事件前失败（响应头未发）→ 错误原样透传（HTTP 状态可用；信封/fatal
//     文案复用 T2 提取机制）
//   - 响应头已发后失败 → HTTP 状态不可用 → SSE error 帧（event: error +
//     data {"message": "…"}）+ EOF；计费走 recordStreamAbort（已收集张数
//     落账，无 completed 则 0 张）+ MarkResult(ResultError)
//   - fn 回调错误 → 立即终止并透传（客户端断开/网关中止——写入失败即断连）
//   - 客户端断开（r.Context().Err() != nil）→ 不 MarkResult（镜像既有结构）
//   - 0 图成功边界（SDK Data 空 → 无任何事件）→ 网关自行收尾：200 + 记 0
//     张落账
//
// 计费口径（与 T2 非流式同口径——ImageCost + GetImagePrice 快照）：流终按已
// 收集张数落账；usage 取末事件（completed 携带）；image token 分量并入
// Input/OutputTokens、张数入 CallCount、TotalTokens = image tokens 之和（张数
// 不入 TotalTokens——评审 P3-6；统一计费模型 spec 2026-08-13）；text token
// 分量恒 0。
func (p *Proxy) streamImageGeneration(ctx context.Context, w http.ResponseWriter, r *http.Request, reqID string, groupID int64, start time.Time, sel *scheduler.Selection, reqModel string, cred *domain.AccountCredential, params *domain.ImageGenParams, gen imageStreamGenerator) (int, []byte, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, p.cfg.UpstreamStreamTimeout)
	defer cancel()

	var (
		count       int64
		usage       *domain.ImageUsage
		headersSent bool
	)
	writeFrame := func(frame []byte) error {
		if !headersSent {
			headersSent = true
			writeSSEHeaders(w)
			flushWriter(w)
		}
		if _, err := w.Write(frame); err != nil {
			return err
		}
		flushWriter(w)
		return nil
	}

	genErr := gen(ctx, cred, params, func(ev domain.ImageStreamEvent) error {
		switch ev.Type {
		case domain.ImageStreamEventKeepalive:
			return writeFrame([]byte(": ping\n\n"))
		case domain.ImageStreamEventCompleted:
			count++
			if ev.Usage != nil {
				usage = ev.Usage
			}
			return writeFrame(buildCompletedFrame(&ev))
		default:
			// 未知事件类型——跳过 + Warn（A-P2-10：SDK 升级改事件名 → 落账 0 张
			// 的静默面收敛——不静默吞，告警留痕；适配层已显式映射过滤，此处为
			// 分层防御）。
			if p.log != nil {
				p.log.Warn("image stream: unknown event type skipped",
					logx.String("request_id", reqID),
					logx.String("type", string(ev.Type)))
			}
			return nil
		}
	})

	var ii, io int64
	if usage != nil {
		ii, io = usage.InputImageTokens, usage.OutputImageTokens
	}
	// 已收集张数/usage 落账元组：tt = image tokens 之和（张数不入 TotalTokens）。
	u := usageTuple{ii: ii, io: io, tt: ii + io, calls: count}

	if genErr != nil {
		if !headersSent {
			// 首事件前失败：HTTP 状态可用——错误原样透传（信封/fatal 文案复用
			// T2 提取机制；骨架按 statusOf 分类：4xx 透传 / 5xx·连接级转移 /
			// 首字节前断连 499）。
			return statusOf(genErr), upstreamBody(genErr), false, genErr
		}
		// 响应头已发后失败：HTTP 状态不可用 → SSE error 帧 + EOF（写失败 = 客户端
		// 已断，best effort 忽略——abort 双分支按 r.Context() 判定）。
		_, _ = w.Write(buildErrorFrame(streamErrMessage(genErr)))
		flushWriter(w)
		// abort 双分支（镜像 caller_responses.go:92-101 既有结构）：客户端断开 →
		// 释放槽位 + 已收集张数照常计费、不 MarkResult（无法转移）；上游错误 →
		// recordStreamAbort + ResultError。
		if r.Context().Err() != nil {
			p.finish(sel.AccountID, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, sel.Format, http.StatusOK, domain.ErrAbort, u, start)))
			return 0, nil, true, nil
		}
		p.recordStreamAbort(ctx, reqID, groupID, start, sel, reqModel, u, genErr)
		p.sched.MarkResult(sel.AccountID, scheduler.ResultError, nil, statusOf(genErr), genErr.Error())
		return 0, nil, true, nil
	}

	// 成功（含 0 图边界——SDK Data 空无任何事件）：首事件前成功 → 网关自行
	// 收尾 200（空 SSE 流 + Flush）；MarkResult OK + 流终计费落账。
	if !headersSent {
		writeSSEHeaders(w)
		flushWriter(w)
	}
	p.sched.MarkResult(sel.AccountID, scheduler.ResultOK, nil, http.StatusOK, "")
	p.finish(sel.AccountID, logWithCtx(ctx, p.buildLog(reqID, groupID, sel.AccountID, reqModel, sel.Model, sel.Format, http.StatusOK, domain.ErrNone, u, start)))
	return http.StatusOK, nil, true, nil
}

// writeSSEHeaders 发 SSE 响应头三件套（对齐 caller_responses.go:60-62）：
// Content-Type: text/event-stream + Cache-Control: no-cache +
// X-Accel-Buffering: no + WriteHeader(200)。
func writeSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
}

// flushWriter 逐事件 Flush（keepalive 后不 Flush = 假免疫；httptest 记录器
// 支持 Flush 接口——测试可断言 Flushed 时序）。
func flushWriter(w http.ResponseWriter) {
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// buildCompletedFrame completed 事件 SSE 帧（wire 形态 P2-1 定死，逐帧构造不
// 整体缓冲）：
//
//	event: image_generation.completed
//	data: {"b64_json":"<base64>"[, "usage": {"input_tokens":…,"input_image_tokens":…,"output_tokens":…,"output_image_tokens":…}]
//	<空行>
//
// usage 仅末事件携带（SDK 语义）；字段映射 = ImageUsage JSON tag 直透。
func buildCompletedFrame(ev *domain.ImageStreamEvent) []byte {
	var b64len int
	if ev.B64JSON != nil {
		b64len = len(*ev.B64JSON)
	}
	// 事件名收敛为 domain.ImageStreamEventCompleted（wire 事件名四处生产字面量
	// 收敛，A-P2-10——编译期常量拼接，零运行时开销）。
	const evLine = "event: " + string(domain.ImageStreamEventCompleted) + "\ndata: "
	buf := bytes.NewBuffer(make([]byte, 0, len(evLine)+b64len+96))
	buf.WriteString(evLine)
	buf.WriteString(`{"b64_json":`)
	// base64 字符集 A-Za-z0-9+/= 不含 " 与 \ → 免 json.Marshal 转义扫描，直接
	// 手写引号零分配；nil（B64JSON 为 *string，keepalive 恒 nil）须显式写字面
	// null 保字节不变（json.Marshal 对 nil 同样产 null）。
	if ev.B64JSON != nil {
		b64 := *ev.B64JSON
		buf.WriteByte('"')
		buf.WriteString(b64)
		buf.WriteByte('"')
	} else {
		buf.WriteString("null")
	}
	if ev.Usage != nil {
		buf.WriteString(`,"usage":`)
		u, _ := json.Marshal(ev.Usage)
		buf.Write(u)
	}
	buf.WriteString("}\n\n")
	return buf.Bytes()
}

// buildErrorFrame 生成失败 SSE error 帧（P2-2 wire 形态）：
//
//	event: error
//	data: {"message": "…"}
//	<空行>
func buildErrorFrame(message string) []byte {
	buf := bytes.NewBuffer(make([]byte, 0, len("event: error\ndata: ")+len(message)+32))
	buf.WriteString("event: error\ndata: ")
	m, _ := json.Marshal(map[string]string{"message": message})
	buf.Write(m)
	buf.WriteString("\n\n")
	return buf.Bytes()
}

// streamErrMessage 错误帧 message 文案（信封/fatal 文案——T2 提取机制复用：
// 信封错误实现 RawJSON() 协议，取上游原始 body 的 message 字段——与
// upstreamErrMsg 同款提取；无 body → err.Error()；域内截断 500）。
func streamErrMessage(err error) string {
	type rawJSONer interface{ RawJSON() string }
	if rj, ok := err.(rawJSONer); ok {
		if raw := rj.RawJSON(); raw != "" {
			if s := upstreamErrMsg([]byte(raw)); s != "" {
				return domain.TruncateErrMsg(s)
			}
		}
	}
	return domain.TruncateErrMsg(err.Error())
}
