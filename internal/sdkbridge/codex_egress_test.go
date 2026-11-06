// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"net/http"

	"github.com/is7qin/c3api/pkg/egress"
)

// testEgressPool 用给定 transport 装配一个出口池（M3：SDK 客户端的 transport 不再
// 是 Codex 的构造参数，而是"某个出口 client"的属性——测试用直连出口承载原来的
// 改写 transport，行为与 M3 之前逐字节一致）。nil transport ⇒ 裸底座（
// `WithTransport(nil)` 等价 SDK 默认，见 clientFor 的注释）。
func testEgressPool(rt http.RoundTripper) *egress.Pool {
	if rt == nil {
		return egress.NewPool(nil)
	}
	return egress.NewPool(&http.Client{Transport: rt})
}
