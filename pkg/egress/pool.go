// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package egress

import (
	"net/http"
	"net/url"
	"sync"
)

// Client 是一条出口：连接规格 + 专属 *http.Client。
//
// 专属的是 **Transport**（= 专属连接池）：换出口必须换连接池，否则同一条 upstream
// keep-alive 连接会被跨出口复用——出口配置换了，实际走的连接没换。底座 client
// 的其余行为（Timeout/CheckRedirect/Jar）按值克隆，出口不改它们的语义。
//
// Spec 是**值**且零值 = 直连：直连与代理走同一个类型，出栈处没有"有没有代理"
// 的第二个分支（spec-proxy-pool §4.2 的核心）。
type Client struct {
	Spec Spec // 零值 = 直连
	HTTP *http.Client
}

// Pool 是**进程级持久**的出口 client 注册表：按 Spec.Key() 复用，不随快照回收
// （改 base_url 不该重建代理连接池），旧规格靠显式 Detach + 锁外关闭并移除。
//
// 生命周期纪律：For / Detach 只可能在快照期与管理面写后调用（低频）；热路径只
// 读快照上已解析好的 *Client 指针——0 次 map 读、0 次分支、0 分配。
type Pool struct {
	mu     sync.Mutex
	base   *http.Client // 克隆底座（与 aiclient Factory 共用同一个 client）
	direct *Client      // 构造期单例（Spec 零值，base 原样）
	byKey  map[[16]byte]*Client
}

// NewPool 建池。base 是直连底座（生产由 main 注入与 aiclient Factory 同一个
// client）；nil ⇒ http.DefaultClient（仅测试路径会用到）。
func NewPool(base *http.Client) *Pool {
	if base == nil {
		base = http.DefaultClient
	}
	return &Pool{
		base:   base,
		direct: &Client{HTTP: base},
		byKey:  make(map[[16]byte]*Client),
	}
}

// Direct 返回直连出口（Spec 零值，构造期单例——未绑定账号的出口恒是这一个）。
func (p *Pool) Direct() *Client { return p.direct }

// For 按规格取/建出口 client：同键复用同一 *Client 指针（连接池因此跨快照重建
// 保留）。零值 spec ⇒ Direct()。
//
// **仅快照期调用**（buildSnapshots / 管理面写后）；热路径不得调用（否则每次出栈
// 多一次锁与 map 读，正是本设计要消灭的开销）。
func (p *Pool) For(spec Spec) *Client {
	if spec == (Spec{}) {
		return p.Direct()
	}
	key := spec.Key()
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.byKey[key]; ok {
		return c
	}
	c := &Client{Spec: spec, HTTP: p.newClient(spec)}
	p.byKey[key] = c
	return c
}

// Detach 摘除并**返回**不在 active 的规格 client（幂等）。active 由调用方给出当前
// 配置引用到的摘要集。
//
//   - 锁内只做 map 摘除（O(被摘项)）；**关闭动作由调用方在锁外做**——CloseIdleConnections
//     会等在途连接收尾，把它留在池锁里会连带拖住持锁的配置临界区（scheduler 还是在
//     publisher.mu 里调用这里，关闭必须晚于那次解锁）。
//   - 返回的是**精确的旧 client 指针**：调用方不得在锁外再按 key 查一次最新注册表
//     （两次查之间可能已经建出同规格的新 client，关错对象）。
//   - 不靠 GC 猜、不靠 finalizer：返回了就由调用方负责关闭。
func (p *Pool) Detach(active map[[16]byte]struct{}) []*Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*Client
	for key, c := range p.byKey {
		if _, ok := active[key]; ok {
			continue
		}
		out = append(out, c)
		delete(p.byKey, key)
	}
	return out
}

// CloseClients 关闭一批已从注册表摘除的出口 client 的空闲连接（锁外调用；nil 安全）。
func CloseClients(clients []*Client) {
	for _, c := range clients {
		if c != nil && c.HTTP != nil {
			c.HTTP.CloseIdleConnections()
		}
	}
}

// Size 返回当前注册的规格 client 数（不含直连）。装配处用它跳过"根本没配代理"
// 的 prune（零代理部署的 reload 路径因此零开销）。
func (p *Pool) Size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.byKey)
}

// newClient 由底座克隆出**专属连接池**的 client：Transport 一律克隆（不可共享，
// 否则 Proxy 会写到别人的连接池上），Proxy 指向本规格的代理 URL。
//
// schema 白名单校验在写面（§3.5）；这里按 Go 原生能力装配：socks5 / socks5h /
// http / https 四种都经 Transport.Proxy（https 代理需 Go ≥1.24）。
func (p *Pool) newClient(spec Spec) *http.Client {
	u := &url.URL{Scheme: spec.Scheme, Host: spec.Addr()}
	if spec.Username != "" {
		// 有用户名无密码 ⇒ url.User（URL 形态 `user@host`，不臆造一个空密码段）。
		if spec.Password == "" {
			u.User = url.User(spec.Username)
		} else {
			u.User = url.UserPassword(spec.Username, spec.Password)
		}
	}
	hc := *p.base // 按值克隆：Timeout/CheckRedirect/Jar 沿用底座
	hc.Transport = cloneTransport(p.base, http.ProxyURL(u))
	return &hc
}

// proxyCloner 是"能派生**独立副本**并把 Proxy 换成给定函数"的 transport 能力。
//
// 接口声明在消费方（本包），实现方无需 import 本包：池子因此只依赖能力、不依赖
// 具体 transport 类型——分片分片、重试层、指标层各自实现即可，加一种不用改这里，
// 也不会把 pkg/httpx 这类装配层包拉进 pkg/egress 的依赖里。
type proxyCloner interface {
	CloneWithProxy(proxy func(*http.Request) (*url.URL, error)) http.RoundTripper
}

// cloneTransport 克隆底座 transport 并覆盖 Proxy。
//
// 优先认 proxyCloner（分片底座走这条，否则"经代理"的交易会退回单连接，等于把上行
// BDP 分摊在绑了代理的账号上整体丢掉，spec-upstream-h2-sharding G5）；其次是
// *http.Transport 的常规克隆；底座没给 transport（nil，或给了个不懂 Proxy 的包装
// RoundTripper）时退回 http.DefaultTransport 的克隆——代理装配是"加一层 Proxy"，
// 不该因底座类型未知而放弃。
func cloneTransport(base *http.Client, proxy func(*http.Request) (*url.URL, error)) http.RoundTripper {
	if c, ok := base.Transport.(proxyCloner); ok {
		return c.CloneWithProxy(proxy)
	}
	tr, ok := base.Transport.(*http.Transport)
	if !ok {
		// 底座 Transport 若自带 Proxy 是本规格要覆盖的（main 显式直连）：克隆后
		// 覆盖，克隆本身只是为了拿到独立的连接池与参数。
		tr = http.DefaultTransport.(*http.Transport)
	}
	c := tr.Clone()
	c.Proxy = proxy
	return c
}
