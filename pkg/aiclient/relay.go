// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 本文件承载全仓唯一一份上游请求头剔除清单与透传规则（spec v7）。
//
// 根规则：网关不做「客户端头 → 上游头」的翻译或映射；它只删除那些会因为自己的
// 存在而撒谎的头——连接级（描述这条入站连接，不是出站连接）、实体级（描述这个
// body，而 body 可能被我们改写）、寻址与凭据级（描述这个网关，不是这个账号）。
// 其余头对网关不透明，原样送达（default-allow，无白名单、无语义级判断）。
//
// 清单是数据不是重复字面量：raw / typed / WS 三个面都消费 relayDeny，任何扩容
// 都必须过同级评审，且不得被配置穿透。
package aiclient

import "net/http"

// relayDeny 是全仓唯一的剔除清单（21 键，5 类：连接级 12 + 实体级 4 + 寻址 1
// + 凭据/多租户 3 + 传输协商 1）。
//
// 键必须是 http.CanonicalHeaderKey 的规范形：本表是普通 map 查表（不像
// Header.Get/Del 会规范化实参），含缩写的键写错大小写能编译、但静默永不命中。
// R1（relay_test.go）遍历断言每一项都是规范形，是这条的唯一机器防线。
//
// 有意不进本表（客户端值允许覆盖网关/SDK 默认）：Anthropic-Version、
// OpenAI-Beta、User-Agent、Originator 及一切自定义头。Accept-Encoding 必须进
// 表：透传后上游回 gzip 裸流，setModel/usage 抽取/model 重写会静默拿到压缩
// 字节 = 漏计费。
var relayDeny = map[string]struct{}{
	// 连接级（RFC 9110 §7.6.1 hop-by-hop）
	"Connection":               {},
	"Upgrade":                  {},
	"Te":                       {},
	"Trailer":                  {},
	"Keep-Alive":               {},
	"Proxy-Connection":         {},
	"Proxy-Authorization":      {},
	"Proxy-Authenticate":       {},
	"Sec-Websocket-Key":        {},
	"Sec-Websocket-Version":    {},
	"Sec-Websocket-Protocol":   {},
	"Sec-Websocket-Extensions": {},
	// 实体级（body 可能被网关就地重写：setModel / 协议转换 / images 双协议）
	"Content-Length":    {},
	"Content-Type":      {},
	"Content-Encoding":  {},
	"Transfer-Encoding": {},
	// 寻址（上游是另一个 host；Host 实测被 Go 提升为 r.Host 不在 map 里，列出为文档性防御）
	"Host": {},
	// 凭据/多租户（网关 key 绝不达上游；Cookie = 跨租户态泄漏）
	"Authorization": {},
	"X-Api-Key":     {},
	"Cookie":        {},
	// 传输协商
	"Accept-Encoding": {},
}

// RelayHeaders 把入站客户端头收敛为出栈上游头：剔除 relayDeny 与脏值，其余
// 原样送达。返回值为每次新建的 map（nil 入参 → 非 nil 空 map，调用方可直接
// Set），键一律是 http.CanonicalHeaderKey 规范形。
//
// 调用方契约（不变量，改动前必读）：
//   - 干净分支 out[ck] = v 共享入站 slice、不深拷（这是「值拷贝 0」的前提）。
//     出栈侧只允许 Set/Del（走 textproto.MIMEHeader 的整槽替换语义），
//     禁止就地改 v 的元素，也禁止 Add：Add = append(dst, value) 而 dst 就是
//     入站那条 slice，cap>1 时会把值写进入站底层数组的下一个槽位（入站 len
//     不变⇒当下看不见，但数组已被污染）。多值上行只在 typed 面需要，那里用
//     SDK 的 WithHeaderAdd 作用于 SDK 自建 Header。
//   - 网关自身声明项（Content-Type、账号鉴权头）必须在 relay 之后写——后写覆盖赢。
//   - 输出键取规范形是硬要求：直接 map 赋值不经 Set，键名不会被顺手规范化，
//     手工构造的 "OpenAI-Beta" 这类字面键会既躲过 Del（只规范化实参）又与规范
//     形那份在同一请求线上写成两行。
func RelayHeaders(in http.Header) http.Header {
	out := make(http.Header, len(in))
	for k, v := range in {
		ck := http.CanonicalHeaderKey(k) // 每次迭代只算一次，匹配与输出键共用
		if _, drop := relayDeny[ck]; drop {
			continue
		}
		clean := true
		for _, s := range v {
			if !valueClean(s) {
				clean = false
				break
			}
		}
		if clean {
			out[ck] = v // 全净 ⇒ 直接共享入站 slice（见函数注释的调用方契约）
			continue
		}
		keep := v[:0:0] // 有脏值 ⇒ len 0 cap 0 的新 slice（绝不可写 v[:0]：会就地回写入站底层数组）
		for _, s := range v {
			if valueClean(s) {
				keep = append(keep, s)
			}
		}
		if len(keep) == 0 {
			continue // 整键值全脏 ⇒ 该键不出现
		}
		out[ck] = keep
	}
	return out
}

// valueClean 头值卫生判据：含任一 <0x20 且非 TAB 的字节，或 0x7f（DEL）即不合
// 法（单个脏值会被整个请求 Do 掉，一个脏头拖死一次调用）。入站 net/http 已自行
// 判 400，此层是纵深防御。
func valueClean(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == 0x7f || (c < 0x20 && c != '\t') {
			return false
		}
	}
	return true
}
