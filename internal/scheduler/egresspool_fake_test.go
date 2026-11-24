// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

// EgressPool 的**端口替身**（P5-3-B）。
//
// 为什么必须有它：scheduler 的 allow 矩阵里 `deny: ["pkg/egress"]` 是**硬规则**，
// 而 deny 在判据里先于 to（testFixtureAllow 只能加宽 to，豁免不了 deny）——本包
// 自己的 `_test.go` 也不能 import 实现包。于是夹具改用实现同一端口的替身。
//
// 替身**逐字对齐**用例真的断言到的那部分语义（不是随手糊的 stub）：
//   - Direct 是构造期单例（未绑定账号的出口恒是同一个指针）；
//   - For 按 Spec.Key() 复用同一 *Client（"同规格跨快照同一条线"这条断言靠它）；
//   - Size 只数规格（不含直连）；Detach 只摘不在 active 的项并返回**旧指针**。
//
// 它**不**做的事，以及为什么可以不做：transport 派生（代理 URL/关闭自动压缩/连接
// 配额）与真正的 CloseIdleConnections。那些是"出口怎么被建出来"的实现语义，覆盖
// 面在 pkg/egress（pool_test.go 的 Detach 三例、flavour_close_test.go、
// codex_transport_test.go）——替身把这条覆盖搬走这件事必须在提交正文点名。
//
// 槽位表里放空 http.Client：用例只断言 `Egress.HTTP()` 非 nil（热路径是字段读），
// 不发起真实出站。

import (
	"net/http"
	"sync"

	"github.com/is7qin/c3api/pkg/egressapi"
)

// fakeEgressPool 是 EgressPool 的实现（结构满足，调度器只认四个方法）。
type fakeEgressPool struct {
	mu     sync.Mutex
	direct *egressapi.Client
	byKey  map[[16]byte]*egressapi.Client
}

// newFakeEgressPool 建一个空替身（直连单例 + 空规格表）。
//
// 每个调度器**各拿一个**（testCfg / proxySched 都现建）：池是可变对象（For/Detach），
// 共享一个全局实例会让一个用例的 Detach 摘掉另一个用例刚建出来的 client。
func newFakeEgressPool() *fakeEgressPool {
	return &fakeEgressPool{
		direct: egressapi.NewClient(egressapi.Spec{}, [egressapi.NumFlavours]*http.Client{
			egressapi.FlavourHTTP:          {},
			egressapi.FlavourNoCompression: {},
		}),
		byKey: make(map[[16]byte]*egressapi.Client),
	}
}

func (p *fakeEgressPool) Direct() *egressapi.Client { return p.direct }

func (p *fakeEgressPool) For(spec egressapi.Spec) *egressapi.Client {
	if spec == (egressapi.Spec{}) {
		return p.Direct()
	}
	key := spec.Key()
	p.mu.Lock()
	defer p.mu.Unlock()
	if c, ok := p.byKey[key]; ok {
		return c
	}
	c := egressapi.NewClient(spec, [egressapi.NumFlavours]*http.Client{
		egressapi.FlavourHTTP:          {},
		egressapi.FlavourNoCompression: {},
	})
	p.byKey[key] = c
	return c
}

func (p *fakeEgressPool) Detach(active map[[16]byte]struct{}) []*egressapi.Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []*egressapi.Client
	for key, c := range p.byKey {
		if _, ok := active[key]; ok {
			continue
		}
		out = append(out, c)
		delete(p.byKey, key)
	}
	return out
}

func (p *fakeEgressPool) Size() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.byKey)
}

// proxySpec 是用例侧的规格字面量。pkg/egress.New 的归一化（scheme 小写、IPv6 去
// 方括号）只在实现侧，用例输入本来就是归一形态，故直接拼结构体——这也是
// "值类型在 egressapi、构造在实现包"这条边界的直接体现。
func proxySpec(scheme, host string, port int, user, pass string) egressapi.Spec {
	return egressapi.Spec{Scheme: scheme, Host: host, Port: port, Username: user, Password: pass}
}
