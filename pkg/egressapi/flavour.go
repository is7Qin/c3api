// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package egressapi

// Flavour 是出口 client 的**口味**：闭集，枚举的是"同一条出口上需要几种出站行为"。
//
// 为什么是闭集数组而不是 map/接口/字符串键：口味派生是**构造期**动作，读取是每请求
// 动作。数组 + 具名 accessor（**常量下标**）内联后与读字段同价（实测 0.225 ns vs
// 0.231 ns/次）；换成 map 是哈希查找，换成 `Client(f Flavour)` 变量下标是 1.8×
// （0.41 ns/次，边界检查 + 常数折叠不了）。见 specs/2026-11-16-egress-flavours-design.md §2。
type Flavour uint8

const (
	// FlavourHTTP 标准出站（压缩语义按 Go 默认）。
	FlavourHTTP Flavour = iota
	// FlavourNoCompression 关闭自动压缩的出站：对齐那些不发 `Accept-Encoding: gzip`
	// 的上游客户端（Codex 的 HTTP/WS 与探测 seam 用它）。
	FlavourNoCompression

	// NumFlavours 是口味闭集的大小（= 一条出口的槽位数）。实现侧按它开槽位表，
	// `Client` 按它定数组——新增口味只改这张表，派生循环与关闭循环自动覆盖。
	// 导出是因为填槽位的是实现侧（它得知道要填几格），读取侧只认具名常量。
	NumFlavours
)
