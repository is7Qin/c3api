// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package notify

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/is7qin/c3api/internal/worker"
	"github.com/is7qin/c3api/pkg/logx"
)

// Dispatcher 变更分发回调（main 装配实现；避免 notify → invalidate/service
// 依赖环——notify 只依赖本接口，invalidate 不 import notify）。
//
// 实现必须尊重 ctx 取消（Apply/FullRefresh 内部的长操作须响应 ctx.Done）：
// Close 会等待监听 goroutine 退出，不响应取消的调用会阻塞停机。
type Dispatcher interface {
	// Apply 处理一条 NOTIFY 变更：转发给 invalidate.Debouncer 的 Mark 方法
	//（本地/远端合并同窗口，天然去重）——users/templates/multipliers/groups
	// 转现有 Mark（Templates 含 clients 失效）；keys/rules 转 Keys/Rules 分支
	// （reloadAll 扩展）；settings 由装配侧同步 ReloadSettings + 注册表 scope
	// 分发（#36 时序——scope 重载必须读到新 N，实现见 cmd/server dispatcher）。
	// 无返回值：内部失败由实现独立 Warn 消化（G-P2-1——NOTIFY 是事件提示，
	// 调用方无任何可执行动作，透传只会双 Warn；周期 ticker / 60s 兜底已存在）。
	Apply(ctx context.Context, ch Change)
	// FullRefresh 连接成功（启动首连 / 断线重连）时的本地刷新：Auth Reload +
	// Balances Reload + sched InvalidateAll + settings + rules 重载（覆盖
	// 断连期间 NOTIFY 丢失；设计文档 §2.3 / R1 / R8）。E2：首连且启动首刷全
	// 成功时实现可跳过全量仅补 settings（装配侧 dispatcher 注释）。
	FullRefresh(ctx context.Context) error
}

// Conn 监听连接面：生产实现包装独立 *pgx.Conn，测试注入 fake。
type Conn interface {
	// Listen 注册 LISTEN channel。
	Listen(ctx context.Context, channel string) error
	// WaitForNotification 阻塞等待一条通知；连接断开/ctx 取消返回错误。
	WaitForNotification(ctx context.Context) (*pgconn.Notification, error)
	// Close 关闭连接（幂等）。
	Close(ctx context.Context) error
}

// pgxConn 生产连接包装：pgx.Conn → Conn。
type pgxConn struct{ conn *pgx.Conn }

func (c *pgxConn) Listen(ctx context.Context, channel string) error {
	_, err := c.conn.Exec(ctx, "LISTEN "+channel)
	return err
}
func (c *pgxConn) WaitForNotification(ctx context.Context) (*pgconn.Notification, error) {
	return c.conn.WaitForNotification(ctx)
}
func (c *pgxConn) Close(ctx context.Context) error { return c.conn.Close(ctx) }

// ConnectFunc 建立监听连接（默认 pgx.Connect；测试注入假连接）。
type ConnectFunc func(ctx context.Context, dsn string) (Conn, error)

// pgxConnect 默认连接工厂：独立 pgx 单连接（非池连接）。
//
// 不用 pgxpool.Acquire 的池连接做 LISTEN 的理由：池连接放回后 LISTEN 订阅
// 虽仍生效，但池会按 idle 超时/网络故障自动关闭并替换连接——订阅随旧连接
// 静默丢失且无任何通知；独立单连接的生命周期完全由本 worker 控制，断线检测
// （WaitForNotification 返回错误）+ 指数退避重连 + 重连全量刷新是一条自洽
// 链路，不会出现"订阅丢了但连接看着还活着"的假健康。
func pgxConnect(ctx context.Context, dsn string) (Conn, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, err
	}
	return &pgxConn{conn: conn}, nil
}

// ListenerConfig 监听器装配参数。
type ListenerConfig struct {
	DSN        string       // 独立监听连接 DSN（与业务池同 DSN）
	Src        string       // 实例 ID：跳过自播 NOTIFY（空 = 不跳过）
	Channel    string       // 空 → Channel（c3api_invalidate）
	Dispatcher Dispatcher   // 必填（nil → Start 返回错误）
	Log        *logx.Logger // 可空（nil = 不记日志）
	Connect    ConnectFunc  // nil → 默认 pgx 独立连接
	// BackoffBase/BackoffMax 断线重连退避：0 → 1s / 30s。
	BackoffBase time.Duration
	BackoffMax  time.Duration
}

// Listener NOTIFY 监听 worker（worker.Worker 契约，Name="notify"）：
//   - 独立单连接 LISTEN c3api_invalidate（重连理由见 pgxConnect 注释）；
//   - 循环消费通知：解析 Change → 自播（Src == 本实例）跳过 → Dispatcher.Apply；
//   - 连接断开 → 指数退避重连（1s→30s cap）；连接成功（启动首连或重连）立即
//     执行一次 Dispatcher.FullRefresh（覆盖断连期间 NOTIFY 丢失，R8；E2：首连
//     且启动首刷全成功 → dispatcher 跳过五路仅补 settings；60s 周期兜底是另
//     一层，见设计文档 §5 #9）；
//   - Close：取消循环 + 等 goroutine 退出（幂等；未 Start 也安全）。
type Listener struct {
	cfg      ListenerConfig
	channel  string
	connect  ConnectFunc
	newTimer func(time.Duration) <-chan time.Time // 测试注入 fake 时钟（默认 time.NewTimer）

	mu     sync.Mutex // 保护 cancel/done（Start 与 Close 并发安全）
	cancel context.CancelFunc
	done   chan struct{}
	// running 监听循环存活标志（观测面 /ops/workers：Start 置位、run 退出复位，
	// Stats 原子读零锁——不碰 mu）。
	running atomic.Bool
}

// NewListener 构造监听器。
func NewListener(cfg ListenerConfig) *Listener {
	if cfg.Channel == "" {
		cfg.Channel = Channel
	}
	if cfg.Connect == nil {
		cfg.Connect = pgxConnect
	}
	if cfg.BackoffBase <= 0 {
		cfg.BackoffBase = time.Second
	}
	if cfg.BackoffMax <= 0 {
		cfg.BackoffMax = 30 * time.Second
	}
	l := &Listener{cfg: cfg, channel: cfg.Channel, connect: cfg.Connect}
	l.newTimer = func(dur time.Duration) <-chan time.Time { return time.NewTimer(dur).C }
	return l
}

// Name 满足 worker.Worker 契约。
func (l *Listener) Name() string { return "notify" }

// Start 启动监听 goroutine（幂等：重复 Start 返回错误；worker 契约）。非阻塞：
// 首连/全量刷新在 goroutine 内异步完成。
//
// cancel/done 在 spawn 之前赋值——Close 与 Start 并发时必能看到句柄（不存在
// "goroutine 已启动但 Close 读到 nil cancel"的窗口；Manager 串行化 Start/Close，
// 先于未完成 Start 的 Close 由 Start ctx 取消兜底）。失败路径（重复 Start）经
// defer 释放本次创建的 ctx，不泄漏。
func (l *Listener) Start(ctx context.Context) error {
	if l.cfg.Dispatcher == nil {
		return fmt.Errorf("notify: dispatcher not configured")
	}
	cctx, cancel := context.WithCancel(ctx)
	started := false
	defer func() {
		if !started {
			cancel() // 失败路径释放本次创建的 ctx
		}
	}()
	l.mu.Lock()
	if l.cancel != nil {
		l.mu.Unlock()
		return fmt.Errorf("notify: already started")
	}
	l.cancel = cancel
	l.done = make(chan struct{})
	l.mu.Unlock()
	started = true
	go func() {
		l.running.Store(true) // 观测面：循环存活（退出即复位）
		defer l.running.Store(false)
		defer close(l.done)
		worker.Loop(cctx, "notify", l.cfg.Log, l.run)
	}()
	return nil
}

// Close 停止监听（幂等；未 Start 也安全）：取消循环 → 等 goroutine 退出。
// 循环的阻塞点（WaitForNotification / 退避 sleep）都响应 ctx 取消，立即返回。
func (l *Listener) Close(ctx context.Context) error {
	l.mu.Lock()
	cancel, done := l.cancel, l.done
	l.mu.Unlock()
	if cancel == nil {
		return nil // 未 Start
	}
	cancel()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run 主循环：连接 → LISTEN → 全量刷新 → 消费通知；断开 → 退避重连。
func (l *Listener) run(ctx context.Context) {
	attempt := 0
	for {
		if ctx.Err() != nil {
			return
		}
		conn, err := l.connect(ctx, l.cfg.DSN)
		if err != nil {
			attempt++
			l.warnf("connect failed", err)
			if !l.sleep(ctx, backoffDelay(attempt, l.cfg.BackoffBase, l.cfg.BackoffMax)) {
				return
			}
			continue
		}
		if err := conn.Listen(ctx, l.channel); err != nil {
			_ = conn.Close(ctx)
			attempt++
			l.warnf("listen failed", err)
			if !l.sleep(ctx, backoffDelay(attempt, l.cfg.BackoffBase, l.cfg.BackoffMax)) {
				return
			}
			continue
		}
		attempt = 0 // 连接成功重置退避
		// 启动首连 / 断线重连成功 → 本地刷新（覆盖断连期间 NOTIFY 丢失）。跳过
		// 与否由 dispatcher 裁决（E2：首连且 main 启动首刷全成功 → 跳过五路
		// ReloadAll 仅补 ReloadSettings；重连恒全量）。
		if err := l.cfg.Dispatcher.FullRefresh(ctx); err != nil && l.cfg.Log != nil {
			l.cfg.Log.Warn("notify full refresh failed", logx.Error(err))
		}
		if !l.consume(ctx, conn) {
			_ = conn.Close(ctx)
			return // consume 返回 false = ctx 已取消 → 正常退出
		}
		// consume 返回 true = 连接断开（ctx 未取消）→ 退避重连。
		_ = conn.Close(ctx)
		attempt++
		if !l.sleep(ctx, backoffDelay(attempt, l.cfg.BackoffBase, l.cfg.BackoffMax)) {
			return
		}
	}
}

// consume 阻塞消费通知直到连接断开/ctx 取消。返回 false = ctx 已取消（正常
// 退出）；true = 连接断开（需重连）。
func (l *Listener) consume(ctx context.Context, conn Conn) bool {
	for {
		n, err := conn.WaitForNotification(ctx)
		if err != nil {
			return ctx.Err() == nil
		}
		ch, err := Unmarshal([]byte(n.Payload))
		if err != nil {
			l.warnf("payload parse failed", err)
			continue
		}
		if ch.Src != "" && ch.Src == l.cfg.Src {
			continue // 自播跳过：省一次重复 reload（Src 含实例随机 nonce，判等只命中本实例，B4-1）
		}
		// 无返回值（G-P2-1）：Apply 失败由实现内部 Warn 消化——透传只会双
		// Warn 且本侧无任何可执行动作（NOTIFY 是事件提示，模块周期 ticker /
		// 60s 兜底已存在）。
		l.cfg.Dispatcher.Apply(ctx, ch)
	}
}

// sleep 退避等待（响应 ctx 取消）；返回 false = 被取消。
func (l *Listener) sleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-l.newTimer(d):
		return true
	}
}

func (l *Listener) warnf(msg string, err error) {
	if l.cfg.Log != nil {
		l.cfg.Log.Warn("notify "+msg, logx.Error(err))
	}
}

// backoffDelay 指数退避：base × 2^(attempt-1)，封顶 max
// （1s → 2s → 4s → 8s → 16s → 30s → 30s…）。
func backoffDelay(attempt int, base, max time.Duration) time.Duration {
	d := base
	for i := 1; i < attempt && d < max; i++ {
		d *= 2
	}
	if d > max {
		return max
	}
	return d
}
