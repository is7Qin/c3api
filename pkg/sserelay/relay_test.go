// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sserelay

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// recorder+flush 的 writer：带 Flusher 的记录器。
// flushed 用 atomic：测试线程与 relay 的 flush 路径并发读写，-race 下需要同步。
type flusherRecorder struct {
	*httptest.ResponseRecorder
	flushed atomic.Int32
}

func (f *flusherRecorder) Flush() { f.flushed.Add(1); f.ResponseRecorder.Flush() }

func relayStream(w http.ResponseWriter, src string, cfg Config) error {
	return Relay(context.Background(), w, strings.NewReader(src), cfg)
}

func TestRelayPreservesRawBytes(t *testing.T) {
	src := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: [DONE]\n\n"
	rec := httptest.NewRecorder()
	require.NoError(t, relayStream(rec, src, Config{}))
	require.Equal(t, src, rec.Body.String())
}

func TestRelayPreservesCRLFAndComments(t *testing.T) {
	src := ": comment line\r\nevent: ping\r\ndata: x\r\n\r\n"
	rec := httptest.NewRecorder()
	require.NoError(t, relayStream(rec, src, Config{}))
	require.Equal(t, src, rec.Body.String())
}

func TestRelayPreservesMultiLineData(t *testing.T) {
	src := "data: line1\ndata: line2\n\n"
	rec := httptest.NewRecorder()
	require.NoError(t, relayStream(rec, src, Config{}))
	require.Equal(t, src, rec.Body.String())
}

func TestRelayVeryLongFrame(t *testing.T) {
	long := strings.Repeat("x", 1<<20) // 1 MiB 单行，超过默认 4KiB bufio buffer
	src := "data: " + long + "\n\n"
	rec := httptest.NewRecorder()
	require.NoError(t, relayStream(rec, src, Config{}))
	require.Equal(t, src, rec.Body.String())
}

// TestRelayEOFFlushesFinalFrameWithoutBlankLine C-P1-1 回归：EOF 双返回
// （"数据+io.EOF"，无末尾空行的关闭风格——第三方兼容上游）时末帧必须 flush
// ——否则 Observer 看不到 completed 帧 → usage 提取落空 → cost=0 落账；
// 输出字节必须完整原样（EOF 中途截断按 WHATWG 视同空行派发直写）。
func TestRelayEOFFlushesFinalFrameWithoutBlankLine(t *testing.T) {
	src := "data: {\"type\":\"response.completed\",\"usage\":{\"total_tokens\":9}}\n"
	var got []Event
	rec := httptest.NewRecorder()
	require.NoError(t, relayStream(rec, src, Config{
		Observer: func(e Event) { got = append(got, e) },
	}))
	require.Len(t, got, 1, "EOF 无空行帧必须派发给 Observer（丢帧 = 计费为零）")
	require.Equal(t, `{"type":"response.completed","usage":{"total_tokens":9}}`, string(got[0].Data))
	require.Equal(t, src, rec.Body.String(), "输出字节必须完整原样转发")
}

// TestRelayEOFFlushesLongLineWithoutNewline C-P1-1 顺带覆盖：ErrBufferFull
// + EOF 双返回的长行（> 8KiB bufio buffer、无末尾换行）——末帧由多段累积，
// EOF 时必须整体 flush（字节完整 + Observer 可见），不可丢。
func TestRelayEOFFlushesLongLineWithoutNewline(t *testing.T) {
	long := strings.Repeat("x", 1<<20) // 1 MiB 单行
	src := "data: " + long             // 无 \n
	var got []Event
	rec := httptest.NewRecorder()
	require.NoError(t, relayStream(rec, src, Config{
		Observer: func(e Event) { got = append(got, e) },
	}))
	require.Len(t, got, 1, "ErrBufferFull+EOF 长行必须派发为末帧")
	require.Equal(t, long, string(got[0].Data), "长行 Data 必须全量命中（旧实现截断于首 chunk）")
	require.Equal(t, src, rec.Body.String(), "长行字节必须完整原样转发")
}

// TestRelayLongLineDataFull spec 2026-08-16 sserelay-lines #1：>8KB 单行 data
// （响应对象 JSON：output 数组 + 尾部 usage——真实 response.completed 帧形状，
// usage 位于 8192B 截断区外）。旧实现 Data 在首 chunk 处截断 → usage 提取落空
// → 计费归零。续片内容含冒号（"usage":{...}）不得被误判为字段行（#2 同场景）。
func TestRelayLongLineDataFull(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"type":"response.completed","response":{"id":"r","output":[`)
	item := `{"type":"function_call","name":"f","arguments":"aaaa"}`
	for i := 0; i < 400; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(item)
	}
	sb.WriteString(`],"usage":{"input_tokens":10,"output_tokens":20,"total_tokens":30}}}`)
	payload := sb.String()
	require.Greater(t, len(payload), 8192, "usage 必须位于 8192B 截断区外")
	src := "data: " + payload + "\n\n"
	var got []Event
	rec := httptest.NewRecorder()
	require.NoError(t, relayStream(rec, src, Config{
		Observer: func(e Event) { got = append(got, e) },
	}))
	require.Len(t, got, 1, "长行帧不得拆出多余空帧")
	require.Equal(t, payload, string(got[0].Data), "Data 必须全量命中（截断 = usage 提取落空 = 计费归零）")
	require.Equal(t, "response.completed", string(got[0].EventName()), "data-only 长行帧 type 推断不受续片影响")
	require.Equal(t, src, rec.Body.String(), "原始字节转发零变化")
}

// TestRelayLongLineColonInContinuation spec #2：续片 chunk 含冒号不得被当作
// 字段行丢弃——内容判据（按冒号/字段名解析续片）会把 "aaaa:bbb…" chunk 当
// 新字段行；state-based 判据下续片恒归 data 行。
func TestRelayLongLineColonInContinuation(t *testing.T) {
	payload := strings.Repeat("a", 8190) + ":" + strings.Repeat("b", 5000)
	src := "data: " + payload + "\n\n"
	var got []Event
	rec := httptest.NewRecorder()
	require.NoError(t, relayStream(rec, src, Config{
		Observer: func(e Event) { got = append(got, e) },
	}))
	require.Len(t, got, 1)
	require.Equal(t, payload, string(got[0].Data), "续片含冒号必须原样并入 data")
	require.Equal(t, src, rec.Body.String())
}

// TestRelayCommentLinesNotInData spec #3：注释行回归——": c" 行（非续片状态）
// 不得出现在 Data 中（splitField 注释行返回 nil；state-based 判据不得改变）。
func TestRelayCommentLinesNotInData(t *testing.T) {
	src := ": c\n: not data\ndata: x\n\n"
	var got []Event
	rec := httptest.NewRecorder()
	require.NoError(t, relayStream(rec, src, Config{
		Observer: func(e Event) { got = append(got, e) },
	}))
	require.Len(t, got, 1)
	require.Equal(t, "x", string(got[0].Data), "注释行不得进入 Data")
	require.Equal(t, src, rec.Body.String())
}

// TestRelayLongLineForkPoint8186 spec #4：精确分叉点——payload=8186B 时行总长
// 8193B（"data: " 6B + 8186B + "\n"）：chunk1 = 8192B（ErrBufferFull、无 \n），
// chunk2 = 孤立 "\n" 尾 chunk。孤立 \n 是续行终止符而非帧分隔空行——空行
// flush 必须 gating 于 !inLine，否则尾 chunk 触发一次空帧 flush
// （flushes=2+空帧）。断言单帧 flush、Data 全量、无多余空帧。
func TestRelayLongLineForkPoint8186(t *testing.T) {
	payload := strings.Repeat("x", 8186)
	src := "data: " + payload + "\n\n"
	var got []Event
	rec := httptest.NewRecorder()
	require.NoError(t, relayStream(rec, src, Config{
		Observer: func(e Event) { got = append(got, e) },
	}))
	require.Len(t, got, 1, "孤立 \n 尾 chunk 不得触发空帧 flush（gating 失效 = flushes=2+空帧）")
	require.Equal(t, payload, string(got[0].Data))
	require.Equal(t, src, rec.Body.String())
}

// TestRelayLongLineCRLF spec #5：CRLF 长行——续片含 \r\n 尾部剥离（Data 不含
// 行终止符），CRLF 空行正常分帧。
func TestRelayLongLineCRLF(t *testing.T) {
	payload := strings.Repeat("x", 10000)
	src := "data: " + payload + "\r\n\r\n"
	var got []Event
	rec := httptest.NewRecorder()
	require.NoError(t, relayStream(rec, src, Config{
		Observer: func(e Event) { got = append(got, e) },
	}))
	require.Len(t, got, 1)
	require.Equal(t, payload, string(got[0].Data), "CRLF 行终止符必须剥离")
	require.Equal(t, src, rec.Body.String())
}

// TestRelayMultiDataAndLongLineMixed spec #6：多 data 行 + 长行混合——\n 合并
// 语义逐位不变（"a\n" + 长行 + "\nb"），续片不得插入多余 \n 或丢失边界。
func TestRelayMultiDataAndLongLineMixed(t *testing.T) {
	long := strings.Repeat("y", 10000)
	src := "data: a\n" + "data: " + long + "\n" + "data: b\n\n"
	var got []Event
	rec := httptest.NewRecorder()
	require.NoError(t, relayStream(rec, src, Config{
		Observer: func(e Event) { got = append(got, e) },
	}))
	require.Len(t, got, 1)
	require.Equal(t, "a\n"+long+"\nb", string(got[0].Data), "\n 合并语义不得被续片破坏")
	require.Equal(t, src, rec.Body.String())
}

func TestRelayObserverReceivesTypedEvent(t *testing.T) {
	var got []Event
	rec := httptest.NewRecorder()
	require.NoError(t, relayStream(rec, "event: message_delta\ndata: {\"usage\":{\"output_tokens\":5}}\n\n", Config{
		Observer: func(e Event) { got = append(got, e) },
	}))
	require.Len(t, got, 1)
	require.Equal(t, "message_delta", string(got[0].Event))
	require.Equal(t, `{"usage":{"output_tokens":5}}`, string(got[0].Data))
	require.Contains(t, string(got[0].Raw), "event: message_delta")
}

func TestRelayObserverReceivesDone(t *testing.T) {
	var got []Event
	rec := httptest.NewRecorder()
	require.NoError(t, relayStream(rec, "data: [DONE]\n\n", Config{
		Observer: func(e Event) { got = append(got, e) },
	}))
	require.Len(t, got, 1)
	require.Equal(t, "", string(got[0].Event))
	require.Equal(t, "[DONE]", string(got[0].Data))
}

func TestRelayMultiLineDataMergedInObserver(t *testing.T) {
	var got []Event
	rec := httptest.NewRecorder()
	require.NoError(t, relayStream(rec, "data: a\ndata: b\n\n", Config{
		Observer: func(e Event) { got = append(got, e) },
	}))
	require.Len(t, got, 1)
	require.Equal(t, "a\nb", string(got[0].Data)) // SSE 规范：多行 data 以 \n 连接
}

func TestEventNamePrefersEventField(t *testing.T) {
	e := Event{Event: []byte("response.completed"), Data: []byte(`{"type":"ignored"}`)}
	require.Equal(t, "response.completed", string(e.EventName()), "event: 字段优先，不依赖 data")
}

func TestEventNameInfersFromDataType(t *testing.T) {
	// 缺 event: 名的 data-only 帧（非规范上游，P3）：data JSON 的 type 与事件名同值
	e := Event{Data: []byte(`{"type":"response.output_text.delta","delta":"x"}`)}
	require.Equal(t, "response.output_text.delta", string(e.EventName()))
}

func TestEventNameEmptyWhenUninferrable(t *testing.T) {
	require.Empty(t, Event{Data: []byte("[DONE]")}.EventName(), "非 JSON 载荷")
	require.Empty(t, Event{Data: []byte(`{"no_type":1}`)}.EventName(), "JSON 但无 type 字段")
	require.Empty(t, Event{Data: nil}.EventName(), "空载荷")
}

// TestEventNameAnchorPath 字节锚定路径（spec 2026-08-16-single-pass-parse-design
// E2）：帧首 `{"type":"` 形态 → 值区间直切片（零拷贝零分配）。
func TestEventNameAnchorPath(t *testing.T) {
	e := Event{Data: []byte(`{"type":"response.output_text.delta","delta":"x"}`)}
	got := e.EventName()
	require.Equal(t, "response.output_text.delta", string(got))
	// 锚定命中：返回切片指向输入字节（零拷贝——与旧全量解码本次分配区分）
	require.Same(t, &e.Data[len(typeAnchorPrefix)], &got[0], "锚定路径零拷贝直切片")
}

// TestEventNameAnchorEmptyType 空串 type（{"type":""}）→ 空名（与旧
// t.Type=="" → nil 语义一致）。
func TestEventNameAnchorEmptyType(t *testing.T) {
	require.Empty(t, Event{Data: []byte(`{"type":""}`)}.EventName())
}

// TestEventNameAnchorFallback 锚定不匹配形态 → 回退全量解码，行为逐字节不变：
// 非首键 type / 冒号后空白 / 前导空白 / type 非字符串 / type 显式 null。
func TestEventNameAnchorFallback(t *testing.T) {
	cases := []struct {
		name string
		data string
		want string
	}{
		{"非首键type", `{"a":1,"type":"x"}`, "x"},
		{"冒号后空白", `{"type" : "x"}`, "x"},
		{"前导空白", ` {"type":"x"}`, "x"},
		{"type非字符串", `{"type":123}`, ""},
		{"type显式null", `{"type":null}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, string(Event{Data: []byte(tc.data)}.EventName()), "data=%s", tc.data)
		})
	}
}

// TestEventNameAnchorEscapedValueRawRetention 值含转义引号：锚定仍命中（\ 跳
// 过转义目标）→ 返回裸字节（转义保留）——与 eventTypeIs 逐字节比较语义一致；
// 类型值恒无转义的机器生成形态下等价（比较目标为 ASCII 常量，转义形态必
// ≠ 目标，宁漏勿错方向）。
func TestEventNameAnchorEscapedValueRawRetention(t *testing.T) {
	got := Event{Data: []byte(`{"type":"a\"b"}`)}.EventName()
	require.Equal(t, `a\"b`, string(got), "锚定命中返回裸字节（\\\" 不物化解码）")
}

// TestEventNameAnchorZeroAlloc 锚定命中路径零分配（热路径红线；回退路径允许
// 分配——AllocsPerRun 只断言锚定形态）。
func TestEventNameAnchorZeroAlloc(t *testing.T) {
	data := []byte(`{"type":"response.output_text.delta","delta":"x"}`)
	alloc := testing.AllocsPerRun(100, func() {
		_ = Event{Data: data}.EventName()
	})
	require.Zero(t, alloc, "锚定路径必须零分配（每帧调用——热路径红线）")
}

func TestRelayFirstEventFlushesImmediately(t *testing.T) {
	fr := &flusherRecorder{ResponseRecorder: httptest.NewRecorder()}
	require.NoError(t, relayStream(fr, "data: {\"a\":1}\n\n", Config{FlushBytes: 4096, FlushInterval: time.Hour}))
	require.True(t, fr.Flushed, "首个事件必须触发 Flush")
	require.Equal(t, int32(1), fr.flushed.Load())
}

func TestRelayBatchesUntilThreshold(t *testing.T) {
	fr := &flusherRecorder{ResponseRecorder: httptest.NewRecorder()}
	// 3 个各 2056B 的事件，阈值 4096：首事件立即 flush（不重置累计，2056 仍计入阈值），
	// 第 2 个使累计 4112 >= 4096 触发 flush 并归零，第 3 个未达阈值，由流结束残余 flush
	var src strings.Builder
	for i := 0; i < 3; i++ {
		src.WriteString("data: ")
		src.WriteString(strings.Repeat("y", 2048))
		src.WriteString("\n\n")
	}
	require.NoError(t, relayStream(fr, src.String(), Config{FlushBytes: 4096, FlushInterval: time.Hour}))
	// 首事件 1 次 + 第 2 事件攒满阈值 1 次 + 流结束残余 1 次
	require.Equal(t, int32(3), fr.flushed.Load())
}

// stagedReader 依次返回 chunks，之后阻塞在 block 上：用于构造"流仍存活、
// 停在 Read 上、没有下一帧"的场景，让 timer 成为唯一可能的 flush 来源。
type stagedReader struct {
	chunks [][]byte
	block  chan struct{}
	idx    int
}

func (s *stagedReader) Read(p []byte) (int, error) {
	if s.idx < len(s.chunks) {
		n := copy(p, s.chunks[s.idx])
		s.idx++
		return n, nil
	}
	<-s.block
	return 0, io.EOF
}

func TestRelayTimerFlushesWithoutNextEvent(t *testing.T) {
	fr := &flusherRecorder{ResponseRecorder: httptest.NewRecorder()}
	src := &stagedReader{chunks: [][]byte{[]byte("data: x\n\n")}, block: make(chan struct{})}
	errCh := make(chan error, 1)
	go func() {
		errCh <- Relay(context.Background(), fr, src, Config{FlushBytes: 4096, FlushInterval: 20 * time.Millisecond})
	}()
	// 首事件立即 flush
	require.Eventually(t, func() bool { return fr.flushed.Load() >= 1 }, time.Second, time.Millisecond)
	// 源仍阻塞在 Read、relay 未退出：期间没有下一帧，2nd flush 只能来自 timer
	select {
	case err := <-errCh:
		t.Fatalf("relay 在源阻塞期间提前退出: %v", err)
	default:
	}
	require.Eventually(t, func() bool { return fr.flushed.Load() >= 2 }, 200*time.Millisecond, 5*time.Millisecond)
	// 解除阻塞，流正常结束
	close(src.block)
	require.NoError(t, <-errCh)
}

func TestRelayReaderErrorPropagates(t *testing.T) {
	rec := httptest.NewRecorder()
	rd := &errorReader{err: errors.New("boom")}
	err := Relay(context.Background(), rec, rd, Config{})
	require.Error(t, err)
}

type errorReader struct{ err error }

func (r *errorReader) Read([]byte) (int, error) { return 0, r.err }

func TestRelayWriterErrorPropagates(t *testing.T) {
	wr := &errorWriter{}
	err := Relay(context.Background(), wr, strings.NewReader("data: x\n\n"), Config{})
	require.Error(t, err)
}

type errorWriter struct{}

func (w *errorWriter) Header() http.Header         { return http.Header{} }
func (w *errorWriter) Write(p []byte) (int, error) { return 0, errors.New("client gone") }
func (w *errorWriter) WriteHeader(int)             {}

func TestRelayContextCancelStops(t *testing.T) {
	rec := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 已取消
	rd := &blockingReader{ch: make(chan struct{})}
	errCh := make(chan error, 1)
	go func() { errCh <- Relay(ctx, rec, rd, Config{}) }()
	select {
	case err := <-errCh:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("ctx cancel 未停止 relay")
	}
}

type blockingReader struct{ ch chan struct{} }

func (r *blockingReader) Read(p []byte) (int, error) { <-r.ch; return 0, io.EOF }

// ctxBlockingReader 阻塞直到 ctx 结束或 release 关闭（ctxReader 只在本层
// Read 入口查 ctx，无法中断已阻塞的下层 Read——模拟"上游停滞"必须由本层
// 感知 ctx 取消）。
type ctxBlockingReader struct {
	ctx context.Context
	ch  chan struct{}
}

func (r *ctxBlockingReader) Read(p []byte) (int, error) {
	select {
	case <-r.ctx.Done():
		return 0, r.ctx.Err()
	case <-r.ch:
		return 0, io.EOF
	}
}

// TestRelayTimeoutClassifiesAsDeadlineExceeded C-P2-2 回归：子 ctx 超时
// （UpstreamStreamTimeout）必须分类为 context.DeadlineExceeded 而非被折叠
// 成 context.Canceled——调用方据此区分"客户端断开"与"上游停滞超时"
// （5 个 caller 的 r.Context().Err() 补丁已删，靠 normalize 的三类可区分）。
func TestRelayTimeoutClassifiesAsDeadlineExceeded(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	rd := &ctxBlockingReader{ctx: ctx, ch: make(chan struct{})}
	err := Relay(ctx, httptest.NewRecorder(), rd, Config{})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotErrorIs(t, err, context.Canceled, "超时不得折叠为 Canceled（否则被记成 200+ErrAbort）")
}

// TestRelayCancelClassifiesAsCanceled C-P2-2 对称断言：父 ctx 取消（客户端
// 断开）必须分类为 context.Canceled——与超时区分开（errors.Is 断言）。
func TestRelayCancelClassifiesAsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rd := &ctxBlockingReader{ctx: ctx, ch: make(chan struct{})}
	errCh := make(chan error, 1)
	go func() { errCh <- Relay(ctx, httptest.NewRecorder(), rd, Config{}) }()
	time.Sleep(10 * time.Millisecond) // 确保已阻塞在 Read 后再取消（读侧取消路径）
	cancel()
	select {
	case err := <-errCh:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(time.Second):
		t.Fatal("取消未停止 relay")
	}
}

// deadClientWriter 模拟半开客户端（进程死亡、无 FIN/RST 收包）：Write 永久
// 阻塞，直到 SetWriteDeadline 被调用（ctx 取消联动）才失败返回——"取消 =
// 写失败 = 正常退出"的唯一路径。实现退化（deadline 无独立 watcher，永不被
// 设置）时 Write 永不返回 → Relay 挂死 → 测试超时失败（兜底抓住方案 2 类
// 失效：timer goroutine 阻塞在 r.mu 上时 ctx.Done 无法唤醒锁等待者）。
type deadClientWriter struct {
	entered    chan struct{} // Write 已进入阻塞（测试等它再取消）
	deadlineCh chan struct{} // SetWriteDeadline 已调用 → 放行 Write
	once       sync.Once
}

func (w *deadClientWriter) Header() http.Header { return http.Header{} }
func (w *deadClientWriter) WriteHeader(int)     {}

func (w *deadClientWriter) Write(p []byte) (int, error) {
	select {
	case w.entered <- struct{}{}:
	default:
	}
	<-w.deadlineCh
	return 0, errors.New("simulated write deadline: client not reading")
}

func (w *deadClientWriter) SetWriteDeadline(time.Time) error {
	w.once.Do(func() { close(w.deadlineCh) })
	return nil
}

// TestRelayCancelUnblocksDeadClientWrite C-P2-1 回归：半开客户端（写阻塞）
// → ctx 取消后必须写失败退出且无 goroutine 泄漏（run + timer + watcher
// 全汇合——Relay 返回即 stopFlushTimer 已 join 全部内部 goroutine）。
func TestRelayCancelUnblocksDeadClientWrite(t *testing.T) {
	wr := &deadClientWriter{entered: make(chan struct{}, 1), deadlineCh: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	src := &stagedReader{chunks: [][]byte{[]byte("data: x\n\n")}, block: make(chan struct{})}
	errCh := make(chan error, 1)
	go func() { errCh <- Relay(ctx, wr, src, Config{}) }()
	select {
	case <-wr.entered:
	case <-time.After(time.Second):
		t.Fatal("首事件 flush 未进入底层写阻塞（测试前置失败）")
	}
	cancel()
	select {
	case err := <-errCh:
		require.Error(t, err, "取消后阻塞写必须以写错误失败退出")
	case <-time.After(2 * time.Second):
		t.Fatal("取消后写阻塞未解除：relay 未退出（deadline 联动失效——双 goroutine 泄漏）")
	}
	close(src.block)
}

// plainWriter 只实现 http.ResponseWriter 的 Header/Write/WriteHeader，
// 刻意不提供 Flush 方法，用于覆盖 dst 无 Flusher 的路径。
type plainWriter struct{ buf bytes.Buffer }

func (w *plainWriter) Header() http.Header         { return http.Header{} }
func (w *plainWriter) Write(p []byte) (int, error) { return w.buf.Write(p) }
func (w *plainWriter) WriteHeader(int)             {}

func TestRelayNoFlusherStillWrites(t *testing.T) {
	wr := &plainWriter{} // 无 Flusher：flush 路径跳过 fl.Flush，但字节仍完整写出
	require.NoError(t, relayStream(wr, "data: x\n\n", Config{}))
	require.Equal(t, "data: x\n\n", wr.buf.String())
}

func TestRelayTimerGoroutineExits(t *testing.T) {
	// 大量短流：每流 timer 必须退出，-race 下无泄漏/竞争
	for i := 0; i < 200; i++ {
		rec := httptest.NewRecorder()
		require.NoError(t, relayStream(rec, "data: x\n\n", Config{FlushInterval: time.Millisecond}))
	}
}

func TestRelayConcurrentStreams(t *testing.T) {
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec := httptest.NewRecorder()
			require.NoError(t, relayStream(rec, "data: x\n\ndata: y\n\n", Config{FlushBytes: 1024, FlushInterval: time.Millisecond}))
		}()
	}
	wg.Wait()
}
