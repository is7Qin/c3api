// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

// clientIP 提取单测（spec 2026-08-17 S-E 行为契约）：
//   - off：完全不读供应商头（伪造头无效，恒 RemoteAddr 剥端口——零伪造面）
//   - on：三头按序采信（CF 优先；空跳下一）+ TrimSpace + 值域截断 64（>64
//     必须 strings.Clone 真正分配——防 retain 巨型头值；≤64 直返零分配）
//   - on 全空 → RemoteAddr 剥端口（SplitHostPort 失败原样返回）
//   - 热路径零分配断言（AllocsPerRun==0，≤64 路径——对齐 usage_extract 先例）

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func ipReq(remoteAddr string, headers map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	r.RemoteAddr = remoteAddr
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

// off：伪造三头全部无效——恒 RemoteAddr 剥端口（零伪造面，与直连行为一致）。
func TestClientIPOffIgnoresHeaders(t *testing.T) {
	r := ipReq("1.2.3.4:8080", map[string]string{
		"CF-Connecting-IP": "9.9.9.9", "True-Client-IP": "8.8.8.8", "X-Real-IP": "7.7.7.7",
	})
	require.Equal(t, "1.2.3.4", clientIP(r, false), "off 伪造头无效，恒 RemoteAddr")
	require.Equal(t, "::1", clientIP(ipReq("[::1]:5678", nil), false), "IPv6 带端口剥")
	require.Equal(t, "no-port", clientIP(ipReq("no-port", nil), false), "SplitHostPort 失败原样返回")
}

// on：三头按序采信（CF 优先；空跳下一）。
func TestClientIPBehindCDNHeaderOrder(t *testing.T) {
	r := ipReq("1.2.3.4:8080", map[string]string{
		"CF-Connecting-IP": "9.9.9.9", "True-Client-IP": "8.8.8.8", "X-Real-IP": "7.7.7.7",
	})
	require.Equal(t, "9.9.9.9", clientIP(r, true), "CF 优先")

	r = ipReq("1.2.3.4:8080", map[string]string{
		"CF-Connecting-IP": "  ", "True-Client-IP": "8.8.8.8", "X-Real-IP": "7.7.7.7",
	})
	require.Equal(t, "8.8.8.8", clientIP(r, true), "CF 空白跳下一")

	r = ipReq("1.2.3.4:8080", map[string]string{"True-Client-IP": "", "X-Real-IP": "7.7.7.7"})
	require.Equal(t, "7.7.7.7", clientIP(r, true), "CF/TCI 空 → X-Real-IP")

	r = ipReq("1.2.3.4:8080", map[string]string{"X-Real-IP": "  7.7.7.7  "})
	require.Equal(t, "7.7.7.7", clientIP(r, true), "TrimSpace 去空白")
}

// on：全空 → RemoteAddr 剥端口（SplitHostPort 失败原样）。
func TestClientIPBehindCDNAllEmptyFallback(t *testing.T) {
	require.Equal(t, "1.2.3.4", clientIP(ipReq("1.2.3.4:8080", nil), true), "无头 → RemoteAddr")
	require.Equal(t, "2001:db8::1", clientIP(ipReq("[2001:db8::1]:443", map[string]string{"CF-Connecting-IP": " "}), true), "IPv6 剥端口")
	require.Equal(t, "no-port", clientIP(ipReq("no-port", nil), true), "SplitHostPort 失败原样")
}

// >64 截断：strings.Clone 真正分配新串（防 retain 巨型头值——客户端可塞 1MB
// 头值，切片引用会 retain 整个头 map 值直到日志 flush；string(s[:64]) 是恒等
// 转换零拷贝，防 retain 意图静默失败）。截断路径恰 1 次分配/run（Clone），
// ≤64 恒零分配。
func TestClientIPTruncate64(t *testing.T) {
	long := strings.Repeat("x", 100)
	r := ipReq("1.2.3.4:8080", map[string]string{"CF-Connecting-IP": long})
	got := clientIP(r, true)
	require.Len(t, got, 64, "值域截断 64（真实 IP 最长 IPv6 45 字符）")
	require.Equal(t, long[:64], got)
	require.Equal(t, float64(1), testing.AllocsPerRun(200, func() { clientIP(r, true) }), ">64 截断恰 1 次分配 = strings.Clone（新分配断言）")

	// 恰好 64 不截断（零分配路径）
	r64 := ipReq("1.2.3.4:8080", map[string]string{"CF-Connecting-IP": strings.Repeat("x", 64)})
	require.Len(t, clientIP(r64, true), 64)
}

// 热路径零分配：≤64 路径（off 短路 + on 头命中 + on 全空回退）AllocsPerRun == 0。
func TestClientIPZeroAlloc(t *testing.T) {
	off := ipReq("1.2.3.4:8080", map[string]string{"CF-Connecting-IP": "9.9.9.9"})
	require.Zero(t, testing.AllocsPerRun(200, func() { clientIP(off, false) }), "off 短路零分配（不开 Header.Get）")
	on := ipReq("1.2.3.4:8080", map[string]string{"CF-Connecting-IP": "9.9.9.9"})
	require.Zero(t, testing.AllocsPerRun(200, func() { clientIP(on, true) }), "on 头命中（≤64）零分配")
	fallback := ipReq("[::1]:5678", nil)
	require.Zero(t, testing.AllocsPerRun(200, func() { clientIP(fallback, true) }), "on 全空回退 RemoteAddr 零分配")
}
