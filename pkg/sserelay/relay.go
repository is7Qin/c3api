// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package sserelay 提供原始字节级 SSE relay：从 io.Reader 增量读取 SSE 帧，
// 原样转发给 http.ResponseWriter，自适应批量 Flush，并以 Observer 旁路暴露
// 事件信息（仅用于 usage 提取，不参与转发决策）。
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
// Raw/Event/Data 均指向 relay 内部复用的缓冲，仅在本次 Observer 回调期间有效；
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
// 值）：帧首 `{"type":"` 锚定命中 → 值区间直接切片返回（零分配——Observer
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
// 与事件名同值，非规范上游缺 event: 行时可用，P3）。仍无 → 空。仅缺名帧
// 触发推断，具名帧零开销（Observer 每帧调用）。返回切片生命周期同 Event
// （具名帧与锚定命中推断值均指向复用缓冲，仅回调内有效；锚定未命中回退
// 全量解码的推断值为本次分配）。
func (e Event) EventName() []byte {
	if len(e.Event) > 0 {
		return e.Event
	}
	return InferEventName(e.Data)
}

// Observer 在帧（原样或经 Mapper 变换后）写出后调用；不得阻塞 relay，不得
// 修改已写出的字节。回调参数 Event 的各切片仅在回调内有效（见 Event 注释），
// 不得跨帧保留。Mapper 存在时 Observer 始终见原始帧（转换不使用量提取失真）。
type Observer func(Event)

type Config struct {
	FlushBytes int // 缓冲达到该值立即 flush；0 时默认 4096
	Observer   Observer
	// Mapper 可选的逐帧转换器（协议转换 W5）：nil = 原样转发（热路径零开销，
	// 单帧一次 nil 判定）。非 nil 时每帧先经 Mapper 变换再写出；Observer 仍见
	// 原始帧（用量提取不因转换失真）。drop=true → 帧丢弃不写出。映射帧字节
	// 生命周期仅限本帧：Mapper 返回后 relay 立即写出，调用方可复用缓冲。
	Mapper func(Event) (frame []byte, drop bool)
}

type relay struct {
	ctx   context.Context
	w     http.ResponseWriter // 原始 dst：取消联动设写侧 deadline（C-P2-1 方案 1）
	bw    *bufio.Writer
	br    *bufio.Reader
	frame *bytes.Buffer // 当前帧原始字节（池化复用；归属 relayBufio）
	fl    http.Flusher
	cfg   Config

	mu       sync.Mutex // 保护 bw/pending
	pending  int        // 累计写入字节；阈值/drain/结束残余 flush 后归零（首事件 latency flush 不归零，其字节继续计入阈值）
	lastTick time.Time

	stopWatch chan struct{}  // 关闭后 deadline watcher 退出
	wg        sync.WaitGroup // deadline watcher 汇合（替代 deadlineDone chan；spec 2026-08-15-gc-opt-ab B-1）
}

// relayBufio 池化的逐流缓冲组：读/写 bufio + 帧组装缓冲。尺寸按语义水位取，
// 不按"越大越快"直觉取：
//   - 写缓冲 4KB = FlushBytes 默认水位——drain-flush 后写缓冲最多盛一个读批
//     （通常一帧）就被冲掉，8KB 容量 99% 是浪费；≥4KB 的单帧走 bufio 直写
//     旁路，不受缓冲大小影响；
//   - 读缓冲 4KB——SSE 帧 ~60B、行 ≤4KB 直读；>4KB 行走 ErrBufferFull 续片
//     路径（与 8KB 时同一状态机，只是分段更多）；
//   - 帧缓冲随组复用（Reset 保容量），消除每流一次 bytes.Buffer 增长分配。
//
// 流结束 Reset(nil) 解除对 dst/src 的引用后归还——watcher goroutine 在
// stopWatcher 汇合后才归还，无并发复用。
type relayBufio struct {
	bw    *bufio.Writer
	br    *bufio.Reader
	frame bytes.Buffer
}

var relayBufioPool = sync.Pool{
	New: func() any {
		return &relayBufio{
			bw: bufio.NewWriterSize(nil, 4096),
			br: bufio.NewReaderSize(nil, 4096),
		}
	},
}

// Relay 把 src 的 SSE 流原样转发到 dst。流结束 = EOF / 读错误 / ctx 取消。
func Relay(ctx context.Context, dst http.ResponseWriter, src io.Reader, cfg Config) error {
	if cfg.FlushBytes <= 0 {
		cfg.FlushBytes = 4096
	}
	rb := relayBufioPool.Get().(*relayBufio)
	rb.bw.Reset(dst)
	rb.br.Reset(&ctxReader{ctx: ctx, r: src})
	rb.frame.Reset()
	r := &relay{
		ctx: ctx, cfg: cfg,
		w:         dst,
		bw:        rb.bw,
		br:        rb.br,
		frame:     &rb.frame,
		stopWatch: make(chan struct{}),
	}
	r.fl, _ = dst.(http.Flusher)
	// goroutine 启动前 Add——此后 wg.Wait 恒安全（无 Add/Wait 竞态）
	r.wg.Add(1)
	r.startDeadlineWatcher()

	err := r.run()
	r.stopWatcher()
	// 读循环退出（watcher 已汇合）后再 flush 残余并归还 writer；
	// 仅实际仍有缓冲字节时才 flush（首事件已 flush 后无残余，不产生多余 Flush）
	r.mu.Lock()
	if r.bw.Buffered() > 0 {
		_ = r.flushLocked()
	}
	r.mu.Unlock()
	// 归还池（先解除对 dst/src 的引用，防池内残留大对象引用链）；帧缓冲
	// Reset 保容量复用，但单次超长帧（>64KB，如大 base64 图）不把池容量
	// 永久抬走——超限直接弃用该切片，池内重建小缓冲。
	rb.bw.Reset(nil)
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
		out := frame.Bytes()
		if r.cfg.Mapper != nil {
			mapped, drop := r.cfg.Mapper(Event{Raw: frame.Bytes(), Event: event, Data: data})
			if drop {
				out = nil
			} else {
				out = mapped
			}
		}
		if out != nil {
			if err := r.write(out); err != nil {
				return err
			}
		}
		if r.cfg.Observer != nil {
			r.cfg.Observer(Event{Raw: frame.Bytes(), Event: event, Data: data})
		}
		frame.Reset()
		data = data[:0]
		event = event[:0]
		return nil
	}
	for {
		// flush-on-drain：读缓冲已空 ⟹ 下一次 ReadSlice 将因等新数据而阻塞。
		// 此刻先 flush 已写入的完整帧——同一读批的多帧合并为一次写系统调用。
		// 这是稳态唯一 flush 触发点（另有阈值与结束残余两条）；旧逐帧 1ms
		// timer 机制已整体删除（每流一次 timer 火 + goroutine 唤醒）。
		if br.Buffered() == 0 {
			if err := r.drainFlush(); err != nil {
				return err
			}
		}
		// 空行 = 帧结束
		line, err := br.ReadSlice('\n')
		if len(line) > 0 {
			frame.Write(line)
			if inLine {
				// 续片（>8KB 长行）：原始 line 去尾 \n\r 直接并入 data，不经
				// splitField——续片内容不可按字段解析（可能含冒号）；>8KB
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
			// 关闭风格（第三方兼容上游）会丢最后一帧 → Observer 看不到
			// completed 帧 → usage 提取落空 → cost=0 落账。EOF 中途截断
			// （末行无 \n）按原样转发直写（WHATWG 视同空行派发）。
			// flushFrame 写错误必须传播（与正常空行 flush 分支行为一致）。
			if frame.Len() > 0 {
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

// normalize 错误分类（C-P2-2）：父 ctx 取消 → context.Canceled；子 ctx 超时
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

func (r *relay) write(p []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.bw.Write(p); err != nil {
		return err
	}
	r.pending += len(p)
	first := r.lastTick.IsZero()
	r.lastTick = time.Now()
	if first {
		// 首事件立即 flush，保证首字节延迟；不重置 pending——首事件字节仍计入
		// 阈值，后续小事件可叠加触发一次批量 flush（区分于阈值 flush 的归零语义）
		if err := r.flushNoResetLocked(); err != nil {
			return err
		}
	}
	if r.pending >= r.cfg.FlushBytes {
		return r.flushLocked()
	}
	// 无定时器：flush 只由三处触发——阈值（上）、run() 顶部的 drainFlush
	// （读缓冲耗尽、即将阻塞等新数据时同步 flush 完整帧）、结束残余 flush。
	// 每次 write 后 run() 必回到 drain 检查或结束路径，故不存在"写后无人
	// flush"的窗口；逐帧 1ms timer 机制（每流一次 timer 火 + goroutine 唤醒，
	// 50k 并发实测 timers.run 26% + timer 锁 16%）随 flush-on-drain 一并删除。
	return nil
}

func (r *relay) stopWatcher() {
	close(r.stopWatch) // 唤醒阻塞在 select 上的 deadline watcher
	r.wg.Wait()        // 汇合后才允许释放 writer（close 保证 select 必然唤醒退出；退出路径唯一——select 任一分支 return 即 Done 恰好一次）
}

// startDeadlineWatcher 写侧 deadline 与 ctx.Done 联动（C-P2-1 方案 1）：
// "取消 = 写失败 = 正常退出"——半开客户端上阻塞的写（bw.Flush 持 r.mu、
// 无 ctx 感知，全库无 SetWriteDeadline）在 deadline 处失败返回，flushFrame
// 传播 → run 正常退出；无此联动则 run + watcher 永久泄漏（每流 2 goroutine
// + 2×8KB 池化 bufio）。
// 本 goroutine 永不触碰 r.mu，取消必然可达。设置后即退出；流正常结束时由
// stopWatch 唤醒退出（net/http 在 handler 返回后自行复位 conn 写 deadline，
// 无残留影响 keep-alive 复用）。
func (r *relay) startDeadlineWatcher() {
	go func() {
		defer r.wg.Done()
		select {
		case <-r.ctx.Done():
			// dst 可能被中间件包装（accessLog 的 statusWriter）——
			// ResponseController 沿 Unwrap 链下探到真实 writer 才能生效
			// （无 Unwrap 的包装层 = ErrNotSupported，C-P2-1 前置修复：
			// middleware.statusWriter.Unwrap）。
			_ = http.NewResponseController(r.w).SetWriteDeadline(time.Now())
		case <-r.stopWatch:
		}
	}()
}

// flushLocked 批量 flush（阈值 / drain / 结束残余触发）：pending > 0 时执行
// bw.Flush + fl.Flush，并把 pending 归零，使事件重新累积批量（spec 规则 2/3）。
// 不检查 bw.Buffered()：>= 8192B 的帧走 bufio 直写路径时缓冲为空但确实有数据
// 待 flush，bw.Flush 对空缓冲是廉价 no-op，随后仍需 fl.Flush 把数据推给对端。
// 错误返回给写路径上报；drain/退出路径忽略（客户端断开不可恢复）。
func (r *relay) flushLocked() error {
	if r.pending <= 0 {
		return nil
	}
	if err := r.bw.Flush(); err != nil {
		return err
	}
	if r.fl != nil {
		r.fl.Flush()
	}
	r.pending = 0
	return nil
}

// flushNoResetLocked 只 flush 不重置 pending：首事件 latency flush 专用。
func (r *relay) flushNoResetLocked() error {
	if err := r.bw.Flush(); err != nil {
		return err
	}
	if r.fl != nil {
		r.fl.Flush()
	}
	return nil
}

// drainFlush flush-on-drain：读缓冲耗尽、即将阻塞等新数据前调用，flush 已
// 写入的完整帧（同一读批多帧 = 一次写系统调用）。这是稳态下的唯一 flush
// 触发点（另有阈值与结束残余两条）；旧逐帧 1ms timer 机制已随此前置语义
// 一并删除——drain 比 timer 更早、更强。空 pending 为 no-op。
func (r *relay) drainFlush() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.pending <= 0 {
		return nil
	}
	if r.bw.Buffered() == 0 {
		// 字节已由首帧即时 flush 写出（flushNoResetLocked 不清零 pending，
		// 只是账面）：仅清账，不再触发空 Flush。
		r.pending = 0
		return nil
	}
	return r.flushLocked()
}
