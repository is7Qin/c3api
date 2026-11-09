// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package httpx

import (
	"io"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
)

// ShardedTransport 把上游并发摊到 N 个**独立连接池**上（spec-upstream-h2-sharding §3）。
//
// 为什么这么做：h2 默认**非严格**——对端 MAX_CONCURRENT_STREAMS 满了会自己另开连接
// （见 `http.HTTP2Config.StrictMaxConcurrentRequests` 文档），所以"流额度互相卡"并不
// 存在。真正被共享的只有**上行 body 的连接级发送窗口**：并发落在 2..M 且已有可用连接
// 时 transport 复用同一条连接，全部请求体共享对端通告的连接窗（RFC 默认 64KiB），
// 每连接上行 ≤ 窗口/RTT。分片买到的是 N 倍上行 BDP 与 1/N 的 TCP HOL 影响面；
// **单请求自身**仍受自己的流窗限制，分片治不了（非目标）。实测数据与方案取舍见
// spec-upstream-h2-sharding（§3 设计 / E1–E9 实测）。
//
// 不承诺"1 分片 = 1 连接"：`StrictMaxConcurrentRequests=true` 在 go1.27.3 会把请求挂死
// （stdlib 自测亦被 skip），`MaxConnsPerHost=1` 在冷启动竞态下挡不住增连。两个旋钮都
// 不可用，故本类型只承诺**选路**：新请求优先给在飞最少的分片。
type ShardedTransport struct {
	shards []shard
}

type shard struct {
	tr *http.Transport
	// inflight 是该分片当前在飞请求数：从 RoundTrip 选中它开始，到响应 body 关闭
	// 为止（长流因此被正确计为"忙"）。它是**跨 origin 聚合的忙闲代理指标**——一个
	// 分片服务多个 origin 时每个 origin 各有自己的连接池，故 inflight 不等于任何
	// 一条连接的流占用，只用于比较"哪个分片更闲"。
	inflight atomic.Int64
}

// NewShardedTransport 按 base 派生 n 个独立分片；`n<=1` 或 base 为 nil 时返回 nil，
// 调用方应原样使用单 transport（不制造"单分片的分片"这种假抽象，也保证
// `http2_shards<=1` 与历史逐字节等价）。
//
// nil 只表示"未启用"：可以安全调用 ShardCount/CloseIdleConnections/Clone，**不得**喂给
// `http.Client` 当 Transport（RoundTrip 会 panic）。`NewClient` 已按 `st != nil` 分支拦掉。
//
// 连接配额按分片均分（shareConns）：不分摊的话 Σ 会是配置值的 n 倍，等于把
// max_idle_conns 悄悄改成 8192×8。
func NewShardedTransport(base *http.Transport, n int) *ShardedTransport {
	if base == nil || n <= 1 {
		return nil
	}
	s := &ShardedTransport{shards: make([]shard, n)}
	for i := range s.shards {
		tr := base.Clone() // 深拷贝：每个分片拿到自己的连接池与参数
		tr.MaxIdleConns = shareConns(base.MaxIdleConns, n, i)
		tr.MaxIdleConnsPerHost = shareConns(base.MaxIdleConnsPerHost, n, i)
		tr.MaxConnsPerHost = shareConns(base.MaxConnsPerHost, n, i)
		s.shards[i].tr = tr
	}
	return s
}

// ShardCount 返回内部的分片数量；nil 接收者返回 0。
// NewShardedTransport 返回 nil 时，调用方应使用原始 transport（0 不代表本对象能代劳）。
func (s *ShardedTransport) ShardCount() int {
	if s == nil {
		return 0
	}
	return len(s.shards)
}

// RoundTrip 选一个最闲的分片发请求，并在响应 body 关闭前持有该分片的在飞计数。
func (s *ShardedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	i := s.pick()
	resp, err := s.shards[i].tr.RoundTrip(req)
	if err != nil {
		s.shards[i].inflight.Add(-1) // 失败路径必须立即归还，否则该分片永远"最忙"
		return nil, err
	}
	// 101 Switching Protocols（WebSocket 升级）的 body 是 hijacked 连接本身，客户端
	// 库会硬断言它是 io.ReadWriteCloser（`coder/websocket` dial.go：`respBody.
	// (io.ReadWriteCloser)`，否则 "response body is not a io.ReadWriteCloser"）。
	// 包装它 = codex WS 全线握手失败，故升级响应**不包装**，立即归还计数：升级后的
	// 连接已脱离连接池，既不占 h2 流额度也不共享连接窗，不该算作分片在飞。
	if resp.StatusCode == http.StatusSwitchingProtocols {
		s.shards[i].inflight.Add(-1)
		return resp, nil
	}
	// 其余响应：http.Transport 保证 Body 非 nil（HEAD/204 给的是 http.NoBody），
	// 故不设 nil 分支；真正要防的是"减两次"，那由 countedBody.once 兜住。
	resp.Body = &countedBody{rc: resp.Body, inflight: &s.shards[i].inflight}
	return resp, nil
}

// pick 返回在飞最少的分片下标，平局取**最小下标**。
//
// 平局取最小下标（而不是轮转）是刻意的：串行请求因此恒落 0 号分片 ⇒ 低负载只建立
// 1 条连接；而真并发时每个 goroutine 的 CAS 会把它顶到不同分片 ⇒ 能铺满 N 条。
// 轮转会同时破坏前者（6 个串行请求开 6 条连接）。
//
// CAS 预留（而不是"读完就返回"）是必需的：N 个 goroutine 同时读到全 0 时会一起
// 压到 0 号分片，分片等于没做。CAS 失败只可能是别人刚占了更闲的位置，重扫一次即可。
func (s *ShardedTransport) pick() int {
	for {
		best := 0
		min := s.shards[0].inflight.Load()
		for i := 1; i < len(s.shards); i++ {
			if v := s.shards[i].inflight.Load(); v < min {
				min, best = v, i
			}
		}
		if s.shards[best].inflight.CompareAndSwap(min, min+1) {
			return best
		}
	}
}

// CloseIdleConnections 关闭全部分片的空闲连接（停机/换出口时用）。
func (s *ShardedTransport) CloseIdleConnections() {
	if s == nil {
		return
	}
	for i := range s.shards {
		s.shards[i].tr.CloseIdleConnections()
	}
}

// CloneWithProxy 派生一个独立副本，并把每片的 Proxy 覆盖成给定函数（出口池用：
// 每条代理规格一个专属连接池）。
//
// 返回 http.RoundTripper（而不是具体的 *ShardedTransport）是为了让出口池只凭
// **结构**就能认领这个能力——池子在 pkg/egress 声明窄接口，本包不必被它 import，
// 池子也不必认识具体 transport 类型（新增 transport 种类时各自实现即可）。
//
// 克隆出的分片**不与底座共享任何连接池**——否则同一条 keep-alive 连接会被跨出口
// 复用，出口配置换了而实际走的连接没换。
func (s *ShardedTransport) CloneWithProxy(proxy func(*http.Request) (*url.URL, error)) http.RoundTripper {
	if s == nil {
		return nil
	}
	out := &ShardedTransport{shards: make([]shard, len(s.shards))}
	for i := range s.shards {
		tr := s.shards[i].tr.Clone()
		tr.Proxy = proxy
		out.shards[i].tr = tr
	}
	return out
}

// countedBody 在 body 关闭时归还分片在飞计数。
//
// Close 必须**幂等**且不为负：调用方（代理层、SDK）存在读完再 Close、defer Close
// 与错误路径 Close 叠加的情况，多次减一会让计数变负、该分片永久"最闲"，分片策略
// 反而把所有请求都喂给它。
type countedBody struct {
	rc       io.ReadCloser
	inflight *atomic.Int64
	once     sync.Once
}

func (c *countedBody) Read(p []byte) (int, error) { return c.rc.Read(p) }

func (c *countedBody) Close() error {
	c.once.Do(func() { c.inflight.Add(-1) })
	return c.rc.Close()
}

// shareConns 把一份连接配额摊到 n 个分片的第 i 片上（**字段值**总和，不是有效额度）。
//
//	total<=0       → 每片字段值 0（有效语义按具体字段解释：`MaxIdleConns=0` 是不限，
//	                 `MaxIdleConnsPerHost=0` 是用 Go 默认的 2 ⇒ 有效额度随片数增长）
//	0<total<2n     → 每片 2，字段值总和 2n（本规格的容量取舍，属显式超配）
//	total>=2n      → 每片 total/n，余数 total%n 逐个补给前几片 ⇒ 总和严格等于 total
func shareConns(total, n, i int) int {
	if total <= 0 || n <= 0 {
		return 0
	}
	if total >= 2*n {
		base := total / n
		if i < total%n {
			return base + 1 // 余数不摊掉的话 Σ 会小于配置值（100/8 → 96），配额被悄悄削掉
		}
		return base
	}
	return 2
}
