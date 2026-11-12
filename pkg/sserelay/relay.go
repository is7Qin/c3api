// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package sserelay 提供原始字节级 SSE relay：从 io.Reader 增量读取 SSE 帧，
// 经 Mapper 变换后写入统一下行 owner Output（原样转发/自适应批量 Flush/静默
// 保活），并以写出前回调 OnEvent 暴露原始事件信息（usage/图像计数/轮次钩子/
// 续接入队；不参与转发决策）。读缓冲留 relay，写缓冲下沉 Output。
package sserelay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	// Terminal 可选：正常 EOF（读循环无错误结束）时调用一次，返回的帧（非空）
	// 在末次 drain 之后、deadline watcher 停止之前写出并 drain——用于 mapper
	// 无法自产的协议终止帧（如 converted 源 [DONE] 无 completed 时补发的目标
	// 终止帧）。放在此收尾生命周期内，阻塞写仍受取消/超时写 deadline 解阻保护。
	// 错误路径不调用；写失败以错误返回（与末次 drain 同语义）。不新增采样回调。
	Terminal func() []byte
	// Output 由调用方构造的统一下行 owner（五路 + images）。nil 时 Relay 自建
	// 一个包装 dst 的 Output（用 Interval/Cancel），结束后自行 Release。
	Output *Output
	// Interval 保活间隔（仅在 Output==nil 自建时生效）；<=0 关闭通用定时保活。
	Interval time.Duration
	// Cancel 实际上游请求 ctx 的 cancel（仅在 Output==nil 自建时生效；调用方
	// 自建 Output 时应经 OutputOptions.Cancel 注入）。
	Cancel context.CancelFunc
	// MaxLineBytes 单行（无换行结尾的累计字节）硬上界；<=0 时取
	// DefaultMaxLineBytes（64 MiB）。超限以 ErrLineTooLong 结束本流——目的是
	// 给「畸形/恶意上游发一条永不换行的行」一个上界（否则读缓冲会一直翻倍到
	// OOM），不是给正常帧留的余量：真实 SSE 帧在几十字节量级，最大的
	// response.completed（含整段 output）也在数 MB。
	MaxLineBytes int
	// FailOnErrorPayload 打开「流内错误载荷 = 本流失败」判定（默认 false = 原样
	// 转发，行为与旧版逐字一致）。打开后每个组好的帧在 Mapper/OnEvent/写出
	// **之前**过一遍 AsPayloadError：命中 ⇒ 该帧**不写出**、不走 Mapper、不触发
	// OnEvent，Relay 以 *PayloadError 返回（spec-inband-error-truth §3-2）。
	//
	// 默认 false 是因为语义面按调用点逐个开关：转换子车道（protoconv 协议收尾
	// 依赖该帧产出 closeOpenBlocks + event: error）与 images 车道（error 对象可能
	// 表示"某张图失败"）本轮明确不接（§6-6 / §6-9）。
	FailOnErrorPayload bool
}

type relay struct {
	ctx   context.Context
	w     http.ResponseWriter // 原始 dst：取消联动设写侧 deadline（方案 1）
	out   *Output             // 统一下行 owner（写缓冲/保活/提交态）
	rb    *readBuf
	frame *bytes.Buffer // 当前帧原始字节（池化复用；归属 relayBufio）
	cfg   Config

	stopWatch chan struct{}  // 关闭后 deadline watcher 退出
	wg        sync.WaitGroup // deadline watcher 汇合（替代 deadlineDone chan；spec 2026-08-15-gc-opt-ab）
}

// readBuf 是 relay 的读侧缓冲：自有字节缓冲 + 显式 fill，把「切出一行」与
// 「从上游读」彻底分开。bufio.ReadSlice 把两者合在一次调用里（取不到行就自己
// fill），于是调用方只能「预测」这一次调用会不会阻塞——「完整帧 + 尾部半截行」
// 这种形状下预测就会失准，已完成的帧被压到上游下一次写入才下发。分开之后
// 「即将阻塞」变成事实：flush 之后紧接着就是 fill 里的那次 Read，不需要检查
// 缓冲内容、也不需要任何记账。
//
// 不变量：buf[r:w] 是未消费字节；nl 是下次搜索换行的起点（>= r），只前进不
// 后退，同一次 fill 的结果内每个字节最多被扫一次。fill 会把残留字节搬到区首
// 并可按需扩容，所以**行切片只在下次 fill 之前有效**（各消费点都在同一轮内用
// 完：frame.Write 复制、splitField 的返回值当轮 append 复制、OnEvent 见
// Event 注释）。
type readBuf struct {
	buf   []byte
	r     int
	w     int
	nl    int
	err   error
	src   io.Reader
	max   int // 单行上界（Config.MaxLineBytes 生效值）
	empty int // 连续 (0, nil) 读次数（防退化 reader 自旋）
}

const readBufInit = 4096

// DefaultMaxLineBytes 单行默认上界（64 MiB）。见 Config.MaxLineBytes。
const DefaultMaxLineBytes = 64 << 20

// ErrLineTooLong 单行超过 Config.MaxLineBytes：上游字节流不含换行且累计超过
// 上界（畸形帧或恶意输入）。Relay 以读错误返回，调用方按上游读失败处理。
var ErrLineTooLong = errors.New("sserelay: single line too long")

func newReadBuf() readBuf {
	return readBuf{buf: make([]byte, readBufInit)}
}

// prepare 装配本流的单行上界，并把池内带来的过大缓冲收回上界之内（池缓冲
// 可能来自另一个配置：测试用小上界、生产用 64 MiB）。
func (b *readBuf) prepare(maxBytes int) {
	b.max = maxBytes
	// 只在上界比池内缓冲还小时才收缩（测试用小上界）；上界更大时保留容量，
	// 否则每条流都要从 4 KiB 重新长到工作集大小，白付几次扩容拷贝。
	if len(b.buf) > maxBytes {
		b.buf = make([]byte, min(readBufInit, maxBytes))
	}
}

// reset 清空状态并保容量（src 由 Relay 装配）。
func (b *readBuf) reset() {
	b.r, b.w, b.nl = 0, 0, 0
	b.err = nil
	b.empty = 0
}

// next 切出下一行（含行尾 '\n'）。缓冲内没有完整行 → (nil, false)：调用方此
// 时应先 flush 已写入的完整帧，再 fill——那正是「即将阻塞等上游」的时刻。
func (b *readBuf) next() ([]byte, bool) {
	if b.nl < b.r {
		b.nl = b.r
	}
	if b.nl >= b.w {
		return nil, false
	}
	if i := bytes.IndexByte(b.buf[b.nl:b.w], '\n'); i >= 0 {
		end := b.nl + i + 1
		line := b.buf[b.r:end]
		b.r, b.nl = end, end
		return line, true
	}
	b.nl = b.w // [nl:w) 已确认无换行，下轮只扫新增字节
	return nil, false
}

// fill 把残留字节搬到区首，再从 src 读一次（可能阻塞等上游）。
func (b *readBuf) fill() {
	if len(b.buf) == 0 { // 池内被丢弃过超大缓冲
		b.buf = make([]byte, min(readBufInit, b.max))
	}
	if b.r > 0 {
		b.w = copy(b.buf, b.buf[b.r:b.w])
		b.nl -= b.r
		if b.nl < 0 {
			b.nl = 0
		}
		b.r = 0
	}
	if b.w == len(b.buf) {
		// 整块缓冲都没有换行：需要扩容拼出完整行（不再有 ErrBufferFull 续片
		// 语义，超长 data 行天然全量可见）。扩容封顶在 max——越过即判上界，
		// 不允许畸形长行把内存一路抬上去。
		if len(b.buf) >= b.max {
			// 带上生效上界：运维看到这条日志就知道是「上游发了不换行的长行」，
			// 以及当前边界是多少，不必回头读代码。
			b.err = fmt.Errorf("%w (%d bytes)", ErrLineTooLong, b.max)
			return
		}
		next := len(b.buf) * 2
		if next > b.max {
			next = b.max
		}
		nb := make([]byte, next)
		copy(nb, b.buf[:b.w])
		b.buf = nb
	}
	n, err := b.src.Read(b.buf[b.w:])
	b.w += n
	b.err = err
	if n == 0 && err == nil {
		b.empty++
		if b.empty >= 100 {
			b.err = io.ErrNoProgress // 退化 reader：与 bufio 同类保护，不空转
		}
	} else {
		b.empty = 0
	}
}

// rest 返回流结束时无换行结尾的残留字节（末行），并清空读区。
func (b *readBuf) rest() []byte {
	if b.r >= b.w {
		return nil
	}
	rest := b.buf[b.r:b.w]
	b.r, b.nl = b.w, b.w
	return rest
}

// release 解除对 src 与超大缓冲的引用后归还池（watcher 已汇合，无并发复用）。
func (b *readBuf) release() {
	b.src = nil
	b.reset()
	if cap(b.buf) > 64<<10 {
		b.buf = nil // 单次超长行不把池容量永久抬走
	}
}

// relayBufio 池化的逐流读侧缓冲组：读缓冲 + 帧组装缓冲（写侧已下沉 Output，
// 由 Output 自带写 bufio）。帧缓冲随组复用（Reset 保容量）。
//
// 流结束解除对 src 的引用后归还——watcher goroutine 在 stopWatcher 汇合后才
// 归还，无并发复用。
type relayBufio struct {
	rb    readBuf
	frame bytes.Buffer
}

var relayBufioPool = sync.Pool{
	New: func() any {
		return &relayBufio{rb: newReadBuf()}
	},
}

// Relay 把 src 的 SSE 流经 Mapper/OnEvent 转发到下行 owner。流结束 = EOF /
// 读错误 / ctx 取消。cfg.Output 非 nil 时使用调用方构造的 Output（不 Release）；
// nil 时自建一个（用 cfg.Interval/Cancel）并在结束时 Release。
func Relay(ctx context.Context, dst http.ResponseWriter, src io.Reader, cfg Config) error {
	if cfg.FlushBytes <= 0 {
		cfg.FlushBytes = 4096
	}
	if cfg.MaxLineBytes <= 0 {
		cfg.MaxLineBytes = DefaultMaxLineBytes
	}
	rb := relayBufioPool.Get().(*relayBufio)
	rb.rb.prepare(cfg.MaxLineBytes)
	rb.rb.reset()
	rb.rb.src = &ctxReader{ctx: ctx, r: src}
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
		rb:        &rb.rb,
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
	// 写失败时才 flush 残余。末次 drain 的错误**必须传播**（不丢弃、不依赖是否
	// 自取消——即便 Config{} 无 Cancel 也要返回）。
	if err == nil && !out.WriteFailed() {
		if derr := out.DrainFlush(); derr != nil {
			err = derr
		}
	}
	// 正常 EOF 的协议终止帧（调用方注入）：在 deadline watcher 仍存活时写出，
	// 使阻塞写同样受取消/超时解阻（watcher 在 stopWatcher 才停止——见上）。
	if err == nil && cfg.Terminal != nil {
		if frame := cfg.Terminal(); len(frame) > 0 {
			if _, werr := out.WriteFrame(frame); werr != nil {
				err = werr
			} else if derr := out.DrainFlush(); derr != nil {
				err = derr
			}
		}
	}
	// 保存的 I/O 错优先返回（§3.8 自取消路径不折叠为 context.Canceled；正常结束
	// 但出现写/心跳失败而当前无其他错误时同样返回）。「I/O 错传播」与「是否自
	// 取消」解耦。
	if le := out.IOErr(); le != nil && (err == nil || out.SelfCanceled()) {
		err = le
	}
	r.stopWatcher()
	if owned {
		out.Release()
	}
	// 归还池（先解除对 src 的引用，防池内残留大对象引用链）；帧缓冲
	// Reset 保容量复用，但单次超长帧（>64KB，如大 base64 图）不把池容量
	// 永久抬走——超限直接弃用该切片，池内重建小缓冲。
	rb.rb.release()
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
	rb := r.rb
	frame := r.frame // 池化帧缓冲（每流 Reset 复用，免每次 bytes.Buffer 增长分配）

	var (
		data  []byte // 当前帧 data payload（合并）
		event []byte // 当前帧 event 字段
	)
	flushFrame := func() error {
		raw := Event{Raw: frame.Bytes(), Event: event, Data: data}
		// 失败出口（spec-inband-error-truth §3-2，**最顶部、Mapper 之前**）：
		// 上游在流内声明失败 ⇒ 本流失败。命中即返回：该帧不写出（未提交时由
		// pipeline 写真实状态码；已提交时既有 writeClientStreamError 补 SSE
		// error）、不走 Mapper、不触发 OnEvent。此处**不** frame.Reset()——
		// 错误路径的残余字节由 Relay 归还池时统一丢弃（既有语义）。
		if r.cfg.FailOnErrorPayload {
			if pe := AsPayloadError(raw); pe != nil {
				return pe
			}
		}
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
		// 切一行（不碰 socket）。切不出来 ⟹ 缓冲内已无完整行，而下一句就是从
		// 上游读——「即将阻塞等上游」在这里是事实而非预测，所以无条件先把已
		// 写入 Output 的完整帧 flush 出去（flush-on-drain）：同一读批的多帧仍
		// 合并为一次写系统调用，而「完整帧 + 尾部半截行」也不会再把已完整的帧
		// 压到上游下一次写入（滞后 = 上游静默时长，见
		// TestRelayFlushesCompletedFramesBeforeBlockingOnPartialTail）。
		line, ok := rb.next()
		if !ok {
			if rb.err != nil {
				if rb.err != io.EOF {
					// 读错误/取消：不 flush 残帧（与原语义一致——错误路径残余字节
					// 直接丢弃，不把半帧当完整帧下发）。
					return r.normalize(rb.err)
				}
				// 正常 EOF。中途截断（末行无 \n 结尾）按 WHATWG 视同空行派发
				// ——否则末帧不进 OnEvent → usage 提取落空 → cost=0 落账。
				if rest := rb.rest(); len(rest) > 0 {
					frame.Write(rest)
					field, value := splitField(rest)
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
				if frame.Len() > 0 {
					// 残帧前先停定时器，防心跳与残帧竞争；写错误必须传播。
					r.out.StopTimer()
					if err := flushFrame(); err != nil {
						return err
					}
				}
				return nil
			}
			if err := r.out.DrainFlush(); err != nil {
				return err
			}
			rb.fill()
			continue
		}
		frame.Write(line)
		switch {
		case len(line) == 2 && line[0] == '\r' && line[1] == '\n':
			// 空行（CRLF）= 帧结束
			if err := flushFrame(); err != nil {
				return err
			}
		case len(line) == 1 && line[0] == '\n':
			// 空行（LF）= 帧结束
			if err := flushFrame(); err != nil {
				return err
			}
		default:
			// 字段提取（注释行 splitField 返回 nil，不进 data）
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
		if err := r.checkCancel(); err != nil {
			return err
		}
	}
}

// splitField 解析 "name: value" 行；无冒号或注释行返回 ("", nil)。
func splitField(line []byte) ([]byte, []byte) {
	before, after, ok := bytes.Cut(line, []byte{':'})
	if !ok {
		return nil, nil
	}
	name := before
	if len(name) == 0 || name[0] == ':' { // 注释行
		return nil, nil
	}
	val := after
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
// + 池化读缓冲）。
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
