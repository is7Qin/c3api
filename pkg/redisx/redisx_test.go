// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package redisx

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

// TestOpenSuccess miniredis 下构造 + Ping 通过：客户端非 nil 且可用（foundation
// spec §5——pkg/redisx 两例门禁之一）。
func TestOpenSuccess(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := Open(Options{Addr: mr.Addr()})
	require.NoError(t, err)
	require.NotNil(t, c)
	t.Cleanup(func() { _ = Close(c) })
	require.NoError(t, c.Ping(t.Context()).Err(), "交付的客户端可继续 Ping")
}

// TestOpenPingFailure 对端不可达 → error（含 addr，不含密码明文）且客户端已回收。
func TestOpenPingFailure(t *testing.T) {
	mr := miniredis.RunT(t)
	addr := mr.Addr()
	mr.Close() // 先关服务再构造 → Ping 必败（v2.38 Close 无返回值）

	c, err := Open(Options{Addr: addr, Password: "super-secret"})
	require.Error(t, err)
	require.Nil(t, c)
	require.Contains(t, err.Error(), addr, "错误链含 addr 可归因")
	require.NotContains(t, err.Error(), "super-secret", "密码不入错误链（foundation spec §2.2 纪律 3）")
}

// TestOpenEnablesContextTimeout 钉死硬超时契约（spec 2026-10-09 §2.2b，裁决 A）：
// Open 产物必须显式启用 ContextTimeoutEnabled，并以 defaultIOTimeout 作为无 ctx
// deadline 命令的后备 socket 上界。这是"每批 Redis 2s / contOpTimeout 2s 真正生效"
// 的根因——去掉它，命令级 ctx deadline 会被 go-redis 静默忽略。
func TestOpenEnablesContextTimeout(t *testing.T) {
	mr := miniredis.RunT(t)
	c, err := Open(Options{Addr: mr.Addr()})
	require.NoError(t, err)
	t.Cleanup(func() { _ = Close(c) })

	opt := c.Options()
	require.True(t, opt.ContextTimeoutEnabled, "命令级 ctx deadline 必须生效")
	require.Equal(t, defaultIOTimeout, opt.ReadTimeout, "后备读上界须为明示的 defaultIOTimeout")
	require.Equal(t, defaultIOTimeout, opt.WriteTimeout, "后备写上界须为明示的 defaultIOTimeout")
	require.GreaterOrEqual(t, opt.ReadTimeout, pingTimeout,
		"后备上界不得低于最大 ctx 预算（否则截短携带 ctx 的启动 Ping）")
}

// TestOpenHonorsContextDeadlineOnStalledServer 核心用例（B7）：连接**存活**但对端
// **响应停滞**时，一条带短 ctx 的命令必须在 ctx deadline 附近返回——而非退化为
// ReadTimeout（默认 5s）×重试。若 ContextTimeoutEnabled 未生效，本用例耗时约 5s。
func TestOpenHonorsContextDeadlineOnStalledServer(t *testing.T) {
	s := newStalledRedis(t)
	// 构造期假服务处于响应态 → Open 的 Ping 成功，连接入池。
	c, err := Open(Options{Addr: s.addr})
	require.NoError(t, err)
	t.Cleanup(func() { _ = Close(c) })

	// 此后服务端停滞：读到命令但永不响应（连接仍存活）。
	s.stall.Store(true)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err = c.Ping(ctx).Err()
	elapsed := time.Since(start)

	require.Error(t, err, "停滞服务器：命令必须因 ctx deadline 失败")
	require.GreaterOrEqual(t, elapsed, 150*time.Millisecond, "不得早于 ctx deadline 返回")
	require.Less(t, elapsed, 2*time.Second,
		"须在 ctx deadline 附近返回，而非 ReadTimeout(5s) 后备")
}

// TestContextTimeoutDisabledIgnoresContextDeadline 对照：ContextTimeoutEnabled=false
// （go-redis 默认）时 ctx deadline 被忽略，真实上界是 ReadTimeout。用短 ReadTimeout +
// 关闭重试，使对照自终止且快（~600ms），并证明与启用态的时序差异。
func TestContextTimeoutDisabledIgnoresContextDeadline(t *testing.T) {
	s := newStalledRedis(t)
	c := redis.NewClient(&redis.Options{
		Addr:                  s.addr,
		ReadTimeout:           600 * time.Millisecond,
		WriteTimeout:          600 * time.Millisecond,
		MaxRetries:            -1, // 关闭重试：单次 ReadTimeout 即为上界
		ContextTimeoutEnabled: false,
	})
	t.Cleanup(func() { _ = c.Close() })

	// 响应态握手 + 预热连接（否则首次 dial 握手同样受 ReadTimeout 制约）。
	require.NoError(t, c.Ping(context.Background()).Err())
	s.stall.Store(true)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := c.Ping(ctx).Err()
	elapsed := time.Since(start)

	require.Error(t, err)
	require.GreaterOrEqual(t, elapsed, 450*time.Millisecond,
		"ctx 被忽略：耗时须逼近 ReadTimeout 而非 200ms")
	require.Less(t, elapsed, 2*time.Second)
}

// --- 停滞 Redis 假服务 ---

// stalledRedis 最小 RESP2 端点：响应握手命令（HELLO 拒绝→回落 RESP2、CLIENT
// SETINFO→OK、PING→PONG），使 redisx.Open 的 Ping 成功；随后可切到"停滞"态——
// 读到命令但永不响应（连接保持存活）。自终止：路径由测试清理关闭监听/连接。
type stalledRedis struct {
	addr  string
	stall atomic.Bool
	ln    net.Listener
	done  chan struct{}
	mu    sync.Mutex
	conns []net.Conn
}

func newStalledRedis(t *testing.T) *stalledRedis {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	s := &stalledRedis{addr: ln.Addr().String(), ln: ln, done: make(chan struct{})}
	go s.serve()
	t.Cleanup(s.stop)
	return s
}

func (s *stalledRedis) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns = append(s.conns, c)
		s.mu.Unlock()
		go s.handle(c)
	}
}

func (s *stalledRedis) handle(c net.Conn) {
	defer func() { _ = c.Close() }()
	r := bufio.NewReader(c)
	w := bufio.NewWriter(c)
	for {
		args, err := readRESPCommand(r)
		if err != nil {
			return
		}
		if s.stall.Load() {
			<-s.done // 停滞：读到命令后永不响应，直到测试清理
			return
		}
		if err := writeRESPReply(w, args); err != nil {
			return
		}
		if err := w.Flush(); err != nil {
			return
		}
	}
}

// stop 幂等清理：唤醒停滞的 handler、关闭监听与全部连接（阻塞读随之解围）。
func (s *stalledRedis) stop() {
	close(s.done)
	_ = s.ln.Close()
	s.mu.Lock()
	for _, c := range s.conns {
		_ = c.Close()
	}
	s.mu.Unlock()
}

// readRESPCommand 解析一个 RESP 数组（命令 = bulk string 列表）。
func readRESPCommand(r *bufio.Reader) ([]string, error) {
	header, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	header = strings.TrimRight(header, "\r\n")
	if len(header) == 0 || header[0] != '*' {
		return nil, fmt.Errorf("stalled redis: bad array header %q", header)
	}
	n, err := strconv.Atoi(header[1:])
	if err != nil {
		return nil, err
	}
	args := make([]string, 0, n)
	for i := 0; i < n; i++ {
		bulk, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		bulk = strings.TrimRight(bulk, "\r\n")
		if len(bulk) == 0 || bulk[0] != '$' {
			return nil, fmt.Errorf("stalled redis: bad bulk header %q", bulk)
		}
		l, err := strconv.Atoi(bulk[1:])
		if err != nil {
			return nil, err
		}
		buf := make([]byte, l+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args = append(args, string(buf[:l]))
	}
	return args, nil
}

// writeRESPReply 对握手命令给出最小合法响应。
func writeRESPReply(w *bufio.Writer, args []string) error {
	if len(args) == 0 {
		return nil
	}
	switch strings.ToUpper(args[0]) {
	case "HELLO":
		// 拒绝 RESP3 握手 → go-redis 回落 RESP2（无 AUTH/DB 时 init 无其他命令）。
		_, err := w.WriteString("-ERR unknown command 'HELLO'\r\n")
		return err
	case "PING":
		_, err := w.WriteString("+PONG\r\n")
		return err
	default:
		_, err := w.WriteString("+OK\r\n")
		return err
	}
}
