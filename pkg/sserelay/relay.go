// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package sserelay 提供原始字节级 SSE relay：从 io.Reader 增量读取 SSE 帧，
// 经 Mapper 变换后写入统一下行 owner Output（原样转发/自适应批量 Flush/静默
// 保活），并以写出前回调 OnEvent 暴露原始事件信息（usage/图像计数/轮次钩子/
// 续接入队；不参与转发决策）。读缓冲留 relay，写缓冲下沉 Output。
package sserelay

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"
)

// Event 是一次 SSE 事件的旁路视图。
// Raw/Event/Data 均指向 relay 内部复用的缓冲，仅在本次 OnEvent 回调期间有效；
// 消费方不得跨帧保留这些切片（下一帧会复用同一批缓冲）。
type Event struct {
	Raw   []byte // 完整原始帧（含结尾空行）
	Event []byte // event: 字段值；data-only 帧为空
	Data  []byte // 合并后的 data: payload（多行以 \n 连接）
}

// typeAnchorPrefix `{"type":"` 帧首顶层锚定（data-only 帧的 data 载荷恒为该
// 形态开头——resp/messages 事件机器生成；type 值恒为无转义 ASCII——值区间
// 直接字节切片）。锚定后嵌套 `"type":"` 先出现的帧不误判（锚定要求帧首形态）。
// 与 internal/billing/image_usage.go eventTypePrefix 同款先例（包内自足常量，
// 避免跨包耦合）。
const typeAnchorPrefix = `{"type":"`

// InferEventName 从 data-only 帧的 data 载荷推断事件名（顶层 "type" 字符串
// 值）：帧首 `{"type":"` 锚定命中 → 值区间直接切片返回（零分配——OnEvent
// 每帧调用；解码器/反序列化对字符串结果必物化分配，字节直取；照
// internal/billing/image_usage.go eventTypeIs 先例；值内 \ 转义跳过并以裸
// 字节返回——类型值恒无转义 ASCII，等价比较语义）。锚定不匹配（非首键/
// 冒号后空白/前导空白/type 非字符串/null/畸形帧）→ 回退 json.Unmarshal 全量
// 解码（兼容——行为与旧 EventName 推断一致）。无 type / type 为空串 / 非
// JSON → nil。返回切片：锚定路径指向输入字节（仅调用期间有效——调用方不得
// 跨帧保留）；回退路径为本次分配。
func InferEventName(data []byte) []byte {
	if bytes.HasPrefix(data, []byte(typeAnchorPrefix)) {
		i := len(typeAnchorPrefix)
		for ; i < len(data) && data[i] != '"'; i++ {
			if data[i] == '\\' {
				i++ // 跳过转义目标（\" 等——值恒无转义，防御性）
			}
		}
		if i > len(typeAnchorPrefix) && i < len(data) {
			return data[len(typeAnchorPrefix):i]
		}
		return nil
	}
	var t struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(data, &t) != nil || t.Type == "" {
		return nil
	}
	return []byte(t.Type)
}

// EventName 返回帧的有效事件名：event: 字段值优先；缺名（data-only）帧从
// data 的 JSON "type" 字段推断（InferEventName——resp/messages 流帧的 type
// 与事件名同值，非规范上游缺 event: 行时可用）。仍无 → 空。仅缺名帧
// 触发推断，具名帧零开销（OnEvent 每帧调用）。返回切片生命周期同 Event
// （具名帧与锚定命中推断值均指向复用缓冲，仅回调内有效；锚定未命中回退
// 全量解码的推断值为本次分配）。
func (e Event) EventName() []byte {
	if len(e.Event) > 0 {
		return e.Event
	}
	return InferEventName(e.Data)
}

// OnEvent 是**写出前**采样 seam：在帧经 Mapper 变换之后、写入 Output 写缓冲
// 之前调用；**被 Mapper drop 的帧也触发**（保真"completed 已到但下行写失败
// 时应保留的用量"）。不得阻塞 relay，不得修改待写出的字节。回调参数 Event
// 的各切片仅在回调内有效（见 Event 注释），不得跨帧保留。存在 Mapper 时
// OnEvent 始终见原始帧（转换不使用量提取失真）。
//
// 顺序（定案）：组帧 → Mapper（变换，可得 drop）→ OnEvent（原始 Event，
// 无论是否 drop 都触发）→ 写出（drop 则不写）。全部 relay 调用点（含
// converted）在此统一采集 TTFT/usage/图像计数/轮次钩子与续接入队。
type OnEvent func(Event)

type Config struct {
	FlushBytes int // 缓冲达到该值立即 flush；0 时默认 4096
	// OnEvent 唯一写出前采样回调（取代旧 post-write Observer 语义）。
	OnEvent OnEvent
	// Mapper 可选的逐帧转换器（协议转换）：nil = 原样转发（热路径零开销，
	// 单帧一次 nil 判定）。非 nil 时每帧先经 Mapper 变换再写出；OnEvent 仍见
	// 原始帧（用量提取不因转换失真）。drop=true → 帧丢弃不写出（OnEvent 仍
	// 触发）。映射帧字节生命周期仅限本帧：Mapper 返回后 relay 立即写出，调用
	// 方可复用缓冲。
	Mapper func(Event) (frame []byte, drop bool)
	// Output 由调用方构造的统一下行 owner（五路 + images）。nil 时 Relay 自建
	// 一个包装 dst 的 Output（用 Interval/Cancel），结束后自行 Release。
	Output *Output
	// Interval 保活间隔（仅在 Output==nil 自建时生效）；<=0 关闭通用定时保活。
	Interval time.Duration
	// Cancel 实际上游请求 ctx 的 cancel（仅在 Output==nil 自建时生效；调用方
	// 自建 Output 时应经 OutputOptions.Cancel 注入）。
	Cancel context.CancelFunc
}

type relay struct {
	ctx   context.Context
	w     http.ResponseWriter // 原始 dst：取消联动设写侧 deadline（方案 1）
	out   *Output             // 统一下行 owner（写缓冲/保活/提交态）
	br    *bufio.Reader
	frame *bytes.Buffer // 当前帧原始字节（池化复用；归属 relayBufio）
	cfg   Config

	stopWatch chan struct{}  // 关闭后 deadline watcher 退出
	wg        sync.WaitGroup // deadline watcher 汇合（替代 deadlineDone chan；spec 2026-08-15-gc-opt-ab）
}

// relayBufio 池化的逐流读侧缓冲组：读 bufio + 帧组装缓冲（写侧已下沉
// Output，由 Output 自带写 bufio）。读缓冲 4KB——SSE 帧 ~60B、行 ≤4KB 直读；
// >4KB 行走 ErrBufferFull 续片路径。帧缓冲随组复用（Reset 保容量）。
//
// 流结束 Reset(nil) 解除对 src 的引用后归还——watcher goroutine 在
// stopWatcher 汇合后才归还，无并发复用。
type relayBufio struct {
	br    *bufio.Reader
	frame bytes.Buffer
}

var relayBufioPool = sync.Pool{
	New: func() any {
		return &relayBufio{
			br: bufio.NewReaderSize(nil, 4096),
		}
	},
}

// Relay 把 src 的 SSE 流经 Mapper/OnEvent 转发到下行 owner。流结束 = EOF /
// 读错误 / ctx 取消。cfg.Output 非 nil 时使用调用方构造的 Output（不 Release）；
// nil 时自建一个（用 cfg.Interval/Cancel）并在结束时 Release。
func Relay(ctx context.Context, dst http.ResponseWriter, src io.Reader, cfg Config) error {
	if cfg.FlushBytes <= 0 {
		cfg.FlushBytes = 4096
	}
	rb := relayBufioPool.Get().(*relayBufio)
	rb.br.Reset(&ctxReader{ctx: ctx, r: src})
	rb.frame.Reset()

	out := cfg.Output
	owned := false
	if out == nil {
		out = NewOutput(dst, cfg.Interval, OutputOptions{Ctx: ctx, Cancel: cfg.Cancel, FlushBytes: cfg.FlushBytes})
		owned = true
	}

	r := &relay{
		ctx: ctx, cfg: cfg,
		w:         out.w,
		out:       out,
		br:        rb.br,
		frame:     &rb.frame,
		stopWatch: make(chan struct{}),
	}
	// goroutine 启动前 Add——此后 wg.Wait 恒安全（无 Add/Wait 竞态）
	r.wg.Add(1)
	r.startDeadlineWatcher()

	err := r.run()
	// 读循环退出后**不得先停 deadline watcher**：在途心跳可能阻塞在下行 Write
	// 上（bw.Flush 持 out.mu、无 ctx 感知），需 watcher 仍存活——取消/超时时它
	// 设立即过期写 deadline 才能解除阻塞、完成 StopTimer 汇合；否则汇合可永久
	// 阻塞、后续 drain 失保护。watcher 活到心跳汇合与最终写全部完成。
	out.StopTimer()
	// 取消/错误路径不额外 drain（残余业务字节直接丢弃）；正常结束（EOF）且未
	// 写失败时才 flush 残余。drain 自身失败经 failLocked 记录为 I/O 错误。
	if err == nil && !out.WriteFailed() {
		_ = out.DrainFlush()
	}
	// 心跳/写失败时优先返回已保存的 I/O 错误（不折叠为 context.Canceled）；
	// 仅在写失败确由 Output 自身取消引发时覆盖，避免把「客户端先取消」误记为
	// 写失败出口。
	if le := out.IOErr(); le != nil && out.SelfCanceled() {
		err = le
	}
	r.stopWatcher()
	if owned {
		out.Release()
	}
	// 归还池（先解除对 src 的引用，防池内残留大对象引用链）；帧缓冲
	// Reset 保容量复用，但单次超长帧（>64KB，如大 base64 图）不把池容量
	// 永久抬走——超限直接弃用该切片，池内重建小缓冲。
	rb.br.Reset(nil)
	rb.frame.Reset()
	if rb.frame.Cap() > 64<<10 {
		rb.frame = bytes.Buffer{}
	}
	relayBufioPool.Put(rb)
	return err
}

// ctxReader 在每次 Read 前检查 ctx 是否已取消，避免已取消的 ctx 下阻塞在 src 上。
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	select {
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	default:
	}
	return c.r.Read(p)
}

func (r *relay) run() error {
	br := r.br
	frame := r.frame // 池化帧缓冲（每流 Reset 复用，免每次 bytes.Buffer 增长分配）

	var (
		data   []byte // 当前帧 data payload（合并）
		event  []byte // 当前帧 event 字段
		inLine bool   // 当前行未结束（上次 ReadSlice 返回 ErrBufferFull ⟹ true；chunk 以 \n 结尾 ⟹ false）
	)
	flushFrame := func() error {
		raw := Event{Raw: frame.Bytes(), Event: event, Data: data}
		out := frame.Bytes()
		if r.cfg.Mapper != nil {
			mapped, drop := r.cfg.Mapper(raw)
			if drop {
				out = nil
			} else {
				out = mapped
			}
		}
		// 写出前采样 seam：无论是否 drop 都触发（Mapper 之后、写缓冲之前）。
		if r.cfg.OnEvent != nil {
			r.cfg.OnEvent(raw)
		}
		if out != nil {
			if _, err := r.out.WriteFrame(out); err != nil {
				return err
			}
		}
		frame.Reset()
		data = data[:0]
		event = event[:0]
		return nil
	}
	for {
		// flush-on-drain：读缓冲已空 ⟹ 下一次 ReadSlice 将因等新数据而阻塞。
		// 此刻先 flush 已写入的完整帧——同一读批的多帧合并为一次写系统调用。
		if br.Buffered() == 0 {
			if err := r.out.DrainFlush(); err != nil {
				return err
			}
		}
		// 空行 = 帧结束
		line, err := br.ReadSlice('\n')
		if len(line) > 0 {
			frame.Write(line)
			if inLine {
				// 续片（>4KB 长行）：原始 line 去尾 \n\r 直接并入 data，不经
				// splitField——续片内容不可按字段解析（可能含冒号）；>4KB
				// event 行会并入 data，真实上游 event 恒短，已知限制
				v := line
				for len(v) > 0 && (v[len(v)-1] == '\n' || v[len(v)-1] == '\r') {
					v = v[:len(v)-1]
				}
				data = append(data, v...)
			} else if len(line) == 2 && line[0] == '\r' && line[1] == '\n' {
				// 空行（CRLF）——仅行起始 chunk 才可能是真帧分隔空行
				// （续片状态下的孤立 \n 是续行终止符，归上支）
				if err := flushFrame(); err != nil {
					return err
				}
				continue
			} else if len(line) == 1 && line[0] == '\n' {
				// 空行（LF）
				if err := flushFrame(); err != nil {
					return err
				}
				continue
			} else {
				// 行起始 chunk：字段提取（注释行 splitField 返回 nil，不进 data）
				field, value := splitField(line)
				switch string(field) {
				case "event":
					event = append(event[:0], value...)
				case "data":
					if len(data) > 0 {
						data = append(data, '\n')
					}
					data = append(data, value...)
				}
			}
		}
		if err == io.EOF {
			// ReadSlice "数据+io.EOF" 双返回时末帧已累积未 flush：正常流末帧
			// 已由空行派发（此处 frame.Len()==0，行为零变化）；无末尾空行的
			// 关闭风格（第三方兼容上游）会丢最后一帧 → OnEvent 看不到
			// completed 帧 → usage 提取落空 → cost=0 落账。EOF 中途截断
			// （末行无 \n）按原样转发直写（WHATWG 视同空行派发）。
			// EOF 残帧前先停定时器，防心跳与残帧竞争；flushFrame 写错误必须
			// 传播（与正常空行 flush 分支行为一致）。
			if frame.Len() > 0 {
				r.out.StopTimer()
				if err := flushFrame(); err != nil {
					return err
				}
			}
			return nil
		}
		if err == bufio.ErrBufferFull {
			inLine = true // 行未结束：后续 chunk 为续片
			continue
		}
		if err != nil {
			return r.normalize(err)
		}
		inLine = false // chunk 以 \n 结尾：行结束复位
		if err := r.checkCancel(); err != nil {
			return err
		}
	}
}

// splitField 解析 "name: value" 行；无冒号或注释行返回 ("", nil)。
func splitField(line []byte) ([]byte, []byte) {
	i := bytes.IndexByte(line, ':')
	if i < 0 {
		return nil, nil
	}
	name := line[:i]
	if len(name) == 0 || name[0] == ':' { // 注释行
		return nil, nil
	}
	val := line[i+1:]
	if len(val) > 0 && val[0] == ' ' {
		val = val[1:]
	}
	// 去掉行尾 \n / \r\n
	for len(val) > 0 && (val[len(val)-1] == '\n' || val[len(val)-1] == '\r') {
		val = val[:len(val)-1]
	}
	return name, val
}

// normalize 错误分类：父 ctx 取消 → context.Canceled；子 ctx 超时
// （UpstreamStreamTimeout）→ context.DeadlineExceeded（r.ctx.Err() 原样返回，
// 不再折叠成 Canceled）；上游读错误原样透传。三类可区分——调用方无需再
// "查 r.Context().Err()" 补丁，标准 errors.Is(err, context.Canceled) 即可
// 判定客户端断开。
func (r *relay) normalize(err error) error {
	if err == context.Canceled {
		return context.Canceled
	}
	if r.ctx.Err() != nil {
		return r.ctx.Err()
	}
	return err
}

func (r *relay) checkCancel() error {
	select {
	case <-r.ctx.Done():
		return r.ctx.Err() // 取消/超时分类同 normalize（不折叠）
	default:
		return nil
	}
}

func (r *relay) stopWatcher() {
	close(r.stopWatch) // 唤醒阻塞在 select 上的 deadline watcher
	r.wg.Wait()        // 汇合后才允许释放 writer（close 保证 select 必然唤醒退出；退出路径唯一——select 任一分支 return 即 Done 恰好一次）
}

// startDeadlineWatcher 写侧 deadline 与 ctx.Done 联动（方案 1）：
// "取消 = 写失败 = 正常退出"——半开客户端上阻塞的写（bw.Flush 持 out.mu、
// 无 ctx 感知，全库无 SetWriteDeadline）在 deadline 处失败返回，flushFrame
// 传播 → run 正常退出；无此联动则 run + watcher 永久泄漏（每流 1 goroutine
// + 池化 bufio）。
// 本 goroutine 永不触碰 out.mu，取消必然可达。设置后即退出；流正常结束时由
// stopWatch 唤醒退出（net/http 在 handler 返回后自行复位 conn 写 deadline，
// 无残留影响 keep-alive 复用）。
func (r *relay) startDeadlineWatcher() {
	go func() {
		defer r.wg.Done()
		select {
		case <-r.ctx.Done():
			// dst 可能被中间件包装（accessLog 的 statusWriter）——
			// ResponseController 沿 Unwrap 链下探到真实 writer 才能生效
			// （无 Unwrap 的包装层 = ErrNotSupported 前置修复：
			// middleware.statusWriter.Unwrap）。
			_ = http.NewResponseController(r.w).SetWriteDeadline(time.Now())
		case <-r.stopWatch:
		}
	}()
}
