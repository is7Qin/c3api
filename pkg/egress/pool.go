// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package egress

import (
	"net/http"
	"net/url"
	"sync"

	"github.com/is7qin/c3api/pkg/egressapi"
)

// 值类型（Spec / Client / Flavour）与它们的全部方法都在 pkg/egressapi；本包只剩
// 别名（compat.go）与下面的实现。`Client` 的槽位表归它自己所有，所以"每条出口
// 每种口味一份连接池"这个不变量由 `egressapi.Client.CloseIdle` 收口，本包的
// `For` 只负责把派生出来的 client 填进槽位、`CloseClients` 只负责逐个转交。

// Pool 是**进程级持久**的出口 client 注册表：按 Spec.Key() 复用，不随快照回收
// （改 base_url 不该重建代理连接池），旧规格靠显式 Detach + 锁外关闭并移除。
//
// 生命周期纪律：For / Detach 只可能在快照期与管理面写后调用（低频）；热路径只
// 读快照上已解析好的 *Client 指针——0 次 map 读、0 次分支、0 分配。
type Pool struct {
	mu sync.Mutex
	// bases 是每种口味的底座 client（构造期一次定型）。同一指针出现在多个槽位 ⇒
	// 该口味无法独立派生，`For` 会让它们共用同一个克隆（保持旧的"就是同一个 client"
	// 语义）。
	bases [numFlavours]*http.Client
	// canDeriveNoCompression 构造期算好：底座 transport 是否支持派生关闭自动压缩的
	// 副本。取代上一版 `noCompressionBase != base` 的**指针比较**——那是隐式类型开关。
	canDeriveNoCompression bool
	direct                 *Client // 构造期单例（Spec 零值，各口味底座原样）
	byKey                  map[[16]byte]*Client
}

// NewPool 建池。base 是直连底座（生产由 main 注入与 aiclient Factory 同一个
// client）；nil ⇒ http.DefaultClient（仅测试路径会用到）。Codex 出站底座在这里
// 由 base 派生（签名不变：调用方不必知道 Codex 要关闭自动压缩）。
func NewPool(base *http.Client) *Pool {
	if base == nil {
		base = http.DefaultClient
	}
	noCompression := codexClientOf(base)
	p := &Pool{
		canDeriveNoCompression: noCompression != base,
		byKey:                  make(map[[16]byte]*Client),
	}
	p.bases[FlavourHTTP] = base
	p.bases[FlavourNoCompression] = noCompression
	// 直连出口：各口味用底座本身（不克隆，直连就是底座）。
	p.direct = egressapi.NewClient(Spec{}, p.bases)
	return p
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
	// 槽位表在本包派生（transport/Proxy 都在实现侧），句柄由 egressapi 装配。
	var slots [numFlavours]*http.Client
	for f := range slots {
		if slots[f] != nil {
			continue
		}
		// 每种口味一份独立连接池、同一代理；口味间的差异（如 DisableCompression）
		// 已在 bases 里定型，这里不认任何具体口味。
		slots[f] = NewClient(p.bases[f], spec)
		for g := f + 1; g < int(numFlavours); g++ {
			if p.bases[g] == p.bases[f] {
				// 同一底座 ⇒ 共用同一个克隆（旧语义：底座不能派生时两个口味就是
				// 同一个 *http.Client）。
				slots[g] = slots[f]
			}
		}
	}
	c := egressapi.NewClient(spec, slots)
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
// HTTP 与 NoCompression 是两个连接池，两份都关（同一个 client 时只关一次）。
func CloseClients(clients []*Client) {
	for _, c := range clients {
		// 遍历口味由 `Client.CloseIdle` 收口：新增口味自动被关闭（漏关是静默泄漏，
		// 所以不能靠人记得写第二段）。同一 client 出现两次是允许的——
		// CloseIdleConnections 幂等，不需要判重；nil client 也由它兜住。
		c.CloseIdle()
	}
}

// Size 返回当前注册的规格 client 数（不含直连）。装配处用它跳过"根本没配代理"
// 的 prune（零代理部署的 reload 路径因此零开销）。
func (p *Pool) Size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.byKey)
}

// proxyURL 把连接规格装配成 Transport.Proxy 认的代理 URL。
//
// **唯一实现点**：Pool.For（快照期持久 client）与 NewClient（探测这类一次性旁路
// client）共用这一处。两处各拼一次 URL 就会出现"池里的 client 与探测 client 走
// 不同认证形态"这种没人一眼看得出的偏差（例如一处漏了 url.User：生产流量认证通过、
// 探测恒 407，而两边都"能跑"）。
//
// 认证形态按 Go 原生能力装配：有用户名无密码 ⇒ url.User（URL 形态 `user@host`，
// 不臆造一个空密码段）；空用户名 ⇒ 无 userinfo（没有用户名就无从认证，此时密码
// 也不该出现在 URL 里）。
func proxyURL(spec Spec) *url.URL {
	u := &url.URL{Scheme: spec.Scheme, Host: spec.Addr()}
	if spec.Username != "" {
		if spec.Password == "" {
			u.User = url.User(spec.Username)
		} else {
			u.User = url.UserPassword(spec.Username, spec.Password)
		}
	}
	return u
}

// NewClient 由底座**按值克隆**出一个经 spec 出网的一次性 client：Transport 一律
// 克隆（不可共享，否则 Proxy 会写到别人的连接池上），Proxy 指向本规格的代理 URL；
// Timeout/CheckRedirect/Jar 沿用底座（出口不改它们的语义）。base == nil ⇒
// http.DefaultClient（与 NewPool 同款缺省，只有测试路径会走到）。
//
// **不进 Pool 注册表**——探测这类旁路动作不要用 Pool.For 代替，两个方向都会坏：
//   - 注册表只放**快照期**规格：Detach 的 active 集由"当前配置引用到的摘要"给出，
//     探测造出来的 client 没有快照期身份 ⇒ 下一次 Detach 把它当"不再引用"摘除并
//     关连接，探测可能落在一个正在被关闭的 client 上；
//   - 反过来，为了不被摘除而把它留在表里 ⇒ 永久占一个条目（泄漏），且同一规格
//     出现两个连接池，连接配额凭空翻倍。
//
// 一次性 client 由调用方收尾：用完 `CloseIdleConnections()` 后丢弃（没人替它管
// 生命周期）。零值 spec（直连）没有代理可装配，不要走这里——直连就是底座本身
// （与 Pool.Direct 同一语义）。
func NewClient(base *http.Client, spec Spec) *http.Client {
	if base == nil {
		base = http.DefaultClient
	}
	hc := *base
	hc.Transport = cloneTransport(base, http.ProxyURL(proxyURL(spec)))
	return &hc
}

// compressionDisabler 是"能派生**独立副本**并关闭自动压缩"的 transport 能力
// （仿 proxyCloner：接口声明在消费方，pkg/httpx 的 ShardedTransport 凭结构实现）。
type compressionDisabler interface {
	CloneWithCompressionDisabled() http.RoundTripper
}

// codexClientOf 由底座派生 Codex 出站 client：transport 克隆后
// `DisableCompression=true`，Timeout/CheckRedirect/Jar 按值沿用底座。
//
//   - compressionDisabler（生产的分片底座）：逐片克隆；
//   - *http.Transport（生产的单 transport 底座）：Clone 后设字段；
//   - 其他（nil Transport、测试里的改写 RoundTripper，如 proxy 包 testEgressPool
//     的重写 transport）：**不派生**，返回 base 本身——Codex 与 HTTP 是同一个
//     client。只有测试会走到这里。**不得**照搬 cloneTransport 退回
//     http.DefaultTransport 的规则：那会让测试流量绕过改写 transport 直连真实上游。
//
// 连接配额翻倍：派生出的是**独立连接池**，MaxConnsPerHost/MaxIdleConns 等配额随
// Clone 原样继承、不与 HTTP 分摊——每个出口（直连与每个代理 spec）的实际连接上限
// 是配置值的两倍（HTTP 一份 + Codex 一份）。
func codexClientOf(base *http.Client) *http.Client {
	var rt http.RoundTripper
	switch tr := base.Transport.(type) {
	case compressionDisabler:
		rt = tr.CloneWithCompressionDisabled()
	case *http.Transport:
		c := tr.Clone()
		c.DisableCompression = true
		rt = c
	}
	if rt == nil {
		return base
	}
	hc := *base
	hc.Transport = rt
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
