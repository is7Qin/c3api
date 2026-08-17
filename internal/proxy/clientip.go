// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"net"
	"net/http"
	"net/textproto"
	"strings"
)

// clientIPHeaders 供应商头名（init 时 CanonicalMIMEHeaderKey 归一）：Header.Get
// 对非规范键（如 "CF-Connecting-IP"）每次调用都走 canonicalMIMEHeaderKey 规范化
// 路径恒分配（本三头不在 commonHeader 常用头缓存内——缓存仅含 ~40 个常见头如
// Accept/Content-Type/X-Forwarded-For 等，CF-Connecting-IP/True-Client-IP/
// X-Real-IP 本就不在其中）；规范键（"Cf-Connecting-Ip"）走 quick-check 零分配
// 直返——init 归一使请求路径零分配。HTTP/1.1 与 HTTP/2 入站请求头均以规范形
// 存储（textproto 归一），Get(规范键) 与 Get(任意大小写) 语义等价——零分配
// 且不牺牲查找正确性。AllocsPerRun==0 测试钉住。
var clientIPHeaders = func() []string {
	canon := func(s string) string { return textproto.CanonicalMIMEHeaderKey(s) }
	return []string{canon("CF-Connecting-IP"), canon("True-Client-IP"), canon("X-Real-IP")}
}()

// clientIP 提取客户端 IP（审计/排障的尽力而为标识，非安全边界——不做鉴权/
// 限流决策依据；spec 2026-08-17 用户裁决方案：供应商头识别 + RemoteAddr 兜底）：
//   - behindCDN=false（默认）：完全不读供应商头（零伪造面，与直连行为一致），
//     直取 RemoteAddr 剥端口（IPv6 [::1]:port → ::1；net.SplitHostPort 失败
//     原样返回）。
//   - behindCDN=true：按序采信首个非空头 CF-Connecting-IP → True-Client-IP →
//     X-Real-IP（TrimSpace；值域截断 64 字符——真实 IP 最长 IPv6 45 字符）；
//     全空 → RemoteAddr 剥端口。部署前提：源站只对 CDN/反向代理暴露（防火墙
//     层封直连），客户端直连时可自填任意值——布尔开关即极简门控形态
//     （nginx realip 的粗粒度版）。
//
// 热路径零分配（≤64 路径）：Header.Get map 查找、TrimSpace 子串、切片、
// SplitHostPort 均零分配；>64 防御截断必须 strings.Clone（string(s[:64]) 在
// string 操作数上是恒等转换零拷贝——防 retain 意图静默失败；Clone 才真正分配
// 新串，防 retain 巨型头值直到日志 flush——客户端可塞 1MB 头值，切片引用会
// retain 整个头 map 值；64 字节小分配走大小类，GC 廉价）。AllocsPerRun==0
// 测试钉住。
func clientIP(r *http.Request, behindCDN bool) string {
	if behindCDN {
		for _, name := range clientIPHeaders {
			if v := strings.TrimSpace(r.Header.Get(name)); v != "" {
				return truncateClientIP(v)
			}
		}
	}
	return stripPort(r.RemoteAddr)
}

// truncateClientIP 值域截断（>64 必须 strings.Clone——切片引用会 retain 整个
// 头 map 值；正常 IP 恒 ≤64 直返零分配）。
func truncateClientIP(s string) string {
	if len(s) > 64 {
		return strings.Clone(s[:64])
	}
	return s
}

// stripPort RemoteAddr 剥端口（IPv4 1.2.3.4:8080 → 1.2.3.4；IPv6
// [::1]:8080 → ::1；无端口/畸形 → 原样返回——SplitHostPort 失败不丢值）。
func stripPort(s string) string {
	if host, _, err := net.SplitHostPort(s); err == nil {
		return host
	}
	return s
}
