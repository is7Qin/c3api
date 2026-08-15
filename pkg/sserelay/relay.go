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

// EventName 返回帧的有效事件名：event: 字段值优先；缺名（data-only）帧从
// data 的 JSON "type" 字段推断——resp/messages 流帧的 type 与事件名同值
// （非规范上游缺 event: 行时可用，P3）。仍无 → 空。仅缺名帧触发 JSON
// 扫描，具名帧零开销（Observer 每帧调用）。返回切片生命周期同 Event
// （具名帧 = 复用缓冲仅回调内有效；推断值 = 本次分配）。
func (e Event) EventName() []byte {
	if len(e.Event) > 0 {
		return e.Event
	}
	var t struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(e.Data, &t) != nil || t.Type == "" {
		return nil
	}
	return []byte(t.Type)
}

// Observer 在帧（原样或经 Mapper 变换后）写出后调用；不得阻塞 relay，不得
// 修改已写出的字节。回调参数 Event 的各切片仅在回调内有效（见 Event 注释），
// 不得跨帧保留。Mapper 存在时 Observer 始终见原始帧（转换不使用量提取失真）。
type Observer func(Event)

type Config struct {
	FlushBytes    int           // 缓冲达到该值立即 flush；0 时默认 4096
	FlushInterval time.Duration // 从 relay 启动起以固定间隔触发 timer flush（仅 pending > 0 时实际 flush）；0 时默认 1ms
	Observer      Observer
	// Mapper 可选的逐帧转换器（协议转换 W5）：nil = 原样转发（热路径零开销，
	// 单帧一次 nil 判定）。非 nil 时每帧先经 Mapper 变换再写出；Observer 仍见
	// 原始帧（用量提取不因转换失真）。drop=true → 帧丢弃不写出。映射帧字节
	// 生命周期仅限本帧：Mapper 返回后 relay 立即写出，调用方可复用缓冲。
	Mapper func(Event) (frame []byte, drop bool)
}

type relay struct {
	ctx context.Context
	w   http.ResponseWriter // 原始 dst：取消联动设写侧 deadline（C-P2-1 方案 1）
	bw  *bufio.Writer
	br  *bufio.Reader
	fl  http.Flusher
	cfg Config

	mu       sync.Mutex // 保护 bw/pending/timer
	pending  int        // 累计写入字节；阈值/timer/结束残余 flush 后归零（首事件 latency flush 不归零，其字节继续计入阈值）
	lastTick time.Time

	timer      *time.Timer
	timerArmed bool           // 按需武装：仅当存在待 flush 数据时 timer 才在跑（瞬时短流零 timer 开销）
	stopFlush  chan struct{}  // 关闭后 timer goroutine 与 deadline watcher 退出
	wg         sync.WaitGroup // timer/deadline 两 goroutine 汇合（替代 timerDone/deadlineDone chan——纯退出汇合语义，WaitGroup 等价且免每流 2 个 makechan；spec 2026-08-15-gc-opt-ab B-1）
}

// relayBufio 池化的 bufio 读写器（每流各 8KB；GC 削减 P6：免每流 2×8KB 新建
// + 直接压 sizeclass；流结束 Reset(nil) 解除对 dst/src 的引用后归还——timer
// goroutine 在 stopFlushTimer 汇合后才归还，无并发复用）。
type relayBufio struct {
	bw *bufio.Writer
	br *bufio.Reader
}

var relayBufioPool = sync.Pool{
	New: func() any {
		return &relayBufio{
			bw: bufio.NewWriterSize(nil, 8192),
			br: bufio.NewReaderSize(nil, 8192),
		}
	},
}

// Relay 把 src 的 SSE 流原样转发到 dst。流结束 = EOF / 读错误 / ctx 取消。
func Relay(ctx context.Context, dst http.ResponseWriter, src io.Reader, cfg Config) error {
	if cfg.FlushBytes <= 0 {
		cfg.FlushBytes = 4096
	}
	if cfg.FlushInterval <= 0 {
		cfg.FlushInterval = time.Millisecond
	}
	rb := relayBufioPool.Get().(*relayBufio)
	rb.bw.Reset(dst)
	rb.br.Reset(&ctxReader{ctx: ctx, r: src})
	r := &relay{
		ctx: ctx, cfg: cfg,
		w:         dst,
		bw:        rb.bw,
		br:        rb.br,
		stopFlush: make(chan struct{}),
	}
	r.fl, _ = dst.(http.Flusher)
	// 两 goroutine 启动前 Add——此后 wg.Wait 恒安全（无 Add/Wait 竞态）
	r.wg.Add(2)
	r.startFlushTimer()
	r.startDeadlineWatcher()

	err := r.run()
	r.stopFlushTimer()
	// 读循环退出后（timer 已停、goroutine 已退出）再 flush 残余并归还 writer；
	// 仅实际仍有缓冲字节时才 flush（首事件已 flush 后无残余，不产生多余 Flush）
	r.mu.Lock()
	if r.bw.Buffered() > 0 {
		_ = r.flushLocked()
	}
	r.mu.Unlock()
	// 归还池（先解除对 dst/src 的引用，防池内残留大对象引用链）
	rb.bw.Reset(nil)
	rb.br.Reset(nil)
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
	var (
		frame  bytes.Buffer // 当前帧原始字节
		data   []byte       // 当前帧 data payload（合并）
		event  []byte       // 当前帧 event 字段
		inLine bool         // 当前行未结束（上次 ReadSlice 返回 ErrBufferFull ⟹ true；chunk 以 \n 结尾 ⟹ false）
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
	// 按需武装 flush timer：仅当有待 flush 数据时 timer 才运行。**瞬时短流
	// （上游秒回、首事件已 flush）绝无 timer 开销**——否则每流一个 1ms 周期
	// timer + goroutine，万级并发流 = 每秒千万次 timer 唤醒，runtime 计时器
	// 堆 + 锁打满 CPU（50k 并发上机实测：timers.run 26% + timer 锁 16%，
	// 吞吐从 11.9k/s 崩到 3k/s 的死亡螺旋；短流 ~1ms 结束所以旧行为不炸）。
	r.armFlushTimerLocked()
	return nil
}

// armFlushTimerLocked 武装 flush timer（一次写入只武装一次；触发后由 timer
// goroutine 置回未武装，下次写入按需重新武装）。
func (r *relay) armFlushTimerLocked() {
	if !r.timerArmed {
		r.timerArmed = true
		r.timer.Reset(r.cfg.FlushInterval)
	}
}

func (r *relay) startFlushTimer() {
	r.timer = time.NewTimer(time.Hour)
	r.timer.Stop() // 初始未武装（见 armFlushTimerLocked 注释）
	go func() {
		defer r.wg.Done()
		for {
			select {
			case <-r.ctx.Done():
				return
			case <-r.stopFlush:
				return
			case <-r.timer.C:
				r.mu.Lock()
				_ = r.flushLocked() // 触发时必有待 flush 数据（武装前提），pending 归零
				r.timerArmed = false
				r.mu.Unlock()
			}
		}
	}()
}

func (r *relay) stopFlushTimer() {
	r.timer.Stop()
	close(r.stopFlush) // 唤醒阻塞在 select 上的 timer goroutine 与 deadline watcher
	r.wg.Wait()        // 两 goroutine 全汇合后才允许释放 writer（close 保证两 select 必然唤醒退出；各 goroutine 退出路径唯一——select 任一分支 return 即 Done 恰好一次）
}

// startDeadlineWatcher 写侧 deadline 与 ctx.Done 联动（C-P2-1 方案 1）：
// "取消 = 写失败 = 正常退出"——半开客户端上阻塞的写（bw.Flush 持 r.mu、
// 无 ctx 感知，全库无 SetWriteDeadline）在 deadline 处失败返回，flushFrame
// 传播 → run 正常退出；无此联动则 run + timer 双 goroutine 永久泄漏
// （每流 2 goroutine + 2×8KB 池化 bufio）。
// 独立 goroutine 而非挂 timer goroutine（方案 2 失效窗口：timer 已消费 C
// 后阻塞在 r.mu.Lock/flush 时，ctx.Done 无法唤醒锁等待者）；本 goroutine
// 永不触碰 r.mu，取消必然可达。设置后即退出；流正常结束时由 stopFlush
// 唤醒退出（net/http 在 handler 返回后自行复位 conn 写 deadline，无残留
// 影响 keep-alive 复用）。
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
		case <-r.stopFlush:
		}
	}()
}

// flushLocked 批量 flush（阈值 / timer / 结束残余触发）：pending > 0 时执行
// bw.Flush + fl.Flush，并把 pending 归零，使事件重新累积批量（spec 规则 2/3）。
// 不检查 bw.Buffered()：>= 8192B 的帧走 bufio 直写路径时缓冲为空但确实有数据
// 待 flush，bw.Flush 对空缓冲是廉价 no-op，随后仍需 fl.Flush 把数据推给对端。
// 错误返回给写路径上报；timer goroutine 与退出路径忽略（客户端断开不可恢复）。
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
