// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package egressapi

import "net/http"

// Client 是一条出口：连接规格 + 每种口味一个专属 *http.Client。
//
// 专属的是 **Transport**（= 专属连接池）：换出口必须换连接池，否则同一条 upstream
// keep-alive 连接会被跨出口复用——出口配置换了，实际走的连接没换。底座 client
// 的其余行为（Timeout/CheckRedirect/Jar）按值克隆，出口不改它们的语义。
//
// Spec 是**值**且零值 = 直连：直连与代理走同一个类型，出栈处没有"有没有代理"的第二
// 个分支（spec-proxy-pool §4.2 的核心）。
//
// 口味存在数组里（而不是按名字摊成字段）：**新增一种口味只改常量表与底座表**，
// 实现侧的派生循环与本类型的关闭循环（CloseIdle）自动覆盖——"加了口味忘了关连接池"
// 这个静默泄漏因此写不出来。
//
// 本包只**持有**槽位表，不派生它：怎么由底座克隆出"经这条出口出网"的 client 是实现
// 侧的事（代理 URL、transport 派生、连接配额都在 pkg/egress），本包不知道它们存在。
type Client struct {
	Spec    Spec // 零值 = 直连
	clients [NumFlavours]*http.Client
}

// NewClient 组装一条出口：规格 + 每种口味一个 client（槽位表按值拷入，nil 槽位原样
// 保留）。**唯一构造点**：槽位表归本类型所有，"每个口味各一份连接池、关的时候全关"
// 因此只有一个改动面。
//
// 归属：生产侧只该由实现包的池（pkg/egress.Pool 的 NewPool/For）调它——槽位表里装
// 什么是"出口怎么被建出来"的结论，本包只负责装起来与读出来。测试可直接写
// `egress.Client{Spec: spec}`（槽位全 nil ⇒ HTTP()/NoCompression() 返回 nil），
// 与类型搬来之前逐字同义。
func NewClient(spec Spec, clients [NumFlavours]*http.Client) *Client {
	return &Client{Spec: spec, clients: clients}
}

// HTTP 标准出站 client。
func (c *Client) HTTP() *http.Client { return c.clients[FlavourHTTP] }

// NoCompression 关闭自动压缩的出站 client。
//
// 与 HTTP 同 Spec、同代理、同 Timeout/CheckRedirect/Jar，差别只在
// `DisableCompression=true` 与它自己的连接池（独立连接池 ⇒ 每个出口的实际连接上限
// 是配置值的倍数：每种口味各一份）。
func (c *Client) NoCompression() *http.Client { return c.clients[FlavourNoCompression] }

// CloseIdle 关闭本出口**每个口味**的空闲连接（nil client 安全）。
//
// 为什么是方法而不是把槽位表交给调用方去遍历：槽位表是闭集（NumFlavours），循环由
// 类型的持有者维护，新增口味时自动覆盖——"加了口位忘了关"写不出来。同一 client 出现
// 在两个槽位是允许的：CloseIdleConnections 幂等，不需要判重。
func (c *Client) CloseIdle() {
	if c == nil {
		return
	}
	for _, hc := range c.clients {
		if hc != nil {
			hc.CloseIdleConnections()
		}
	}
}
