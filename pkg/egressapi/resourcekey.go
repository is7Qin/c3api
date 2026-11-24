// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package egressapi

// ResourceKey 一条出口 client 的**资源键**（传输资源表 `EgressResourceKey` 的取值
// 面，类型转换由消费方做）：直连（零值 Spec，含 nil client）→ **零值**，代理 →
// Spec.Key()。
//
// 为什么不直接用 Spec.Key()：它连零值也给一个非零哈希，而"直连"必须与
// "某个真实规格"在键空间里可分辨——配置权威要能显式说"直连这条出口保留"，
// 表据此决定保留哪个桶，而不是在表里写一条"零摘要恒保留"的特例。
func ResourceKey(c *Client) (k [16]byte) {
	if c == nil || c.Spec == (Spec{}) {
		return k
	}
	return c.Spec.Key()
}
