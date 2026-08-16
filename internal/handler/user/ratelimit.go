// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"math"
	"net"
	"net/http"
	"sync"
	"time"
)

// IPRateLimiter per-IP token 桶（F3 bcrypt 登录节流——register/login 公开面）。
// 语义：每 IP 以 rate 速率补充 token，桶容量 burst；桶满即拒绝（429）。
// 键空间 = 客户端 IP，公网无界 → 容量上限（maxKeys，超限拒绝新 IP）+ 空闲
// 过期清理（ttl 无请求回收）——内存有界。
// per-IP 内存桶——多实例各自独立（无共享状态），防单点 CPU 烧毁的足够防线
// （本批不做多实例共享节流；重启清零可接受）。
// 热路径每请求一次桶查：单 Mutex 保护内存 map（并发安全），无 DB 无分配。
// 注：不用 fixedWindowLimiter（internal/proxy）——其键空间为分组 ID（有界）
// 且为固定窗口计数语义（分钟窗口），与 per-IP 无界键空间 + 连续速率语义不匹配。
type IPRateLimiter struct {
	mu      sync.Mutex
	rate    float64 // token/秒（每 IP）
	burst   float64 // 桶容量（突发上限）
	ttl     time.Duration
	maxKeys int
	gcEvery time.Duration
	lastGC  time.Time
	buckets map[string]*ipBucket
}

type ipBucket struct {
	tokens float64
	last   time.Time
}

// NewIPRateLimiter 构造 per-IP 限流器。生产参数（main 装配）：5 rps/IP +
// burst 10 + 空闲 10min 过期 + 100k 键空间上限。rate <= 0 = 禁用（恒放行，
// 防御性兜底——生产不传）。
func NewIPRateLimiter(rate float64, burst int, ttl time.Duration, maxKeys int) *IPRateLimiter {
	return &IPRateLimiter{
		rate:    rate,
		burst:   float64(burst),
		ttl:     ttl,
		maxKeys: maxKeys,
		gcEvery: ttl, // 过期清理节奏 = ttl（至多每秒一次量级，冷路径）
		buckets: make(map[string]*ipBucket),
	}
}

// Allow 消费一个 token：桶有余量 → true（扣减）；超限/新 IP 遇容量上限 →
// false。ip 已含"桶空则拒绝"语义——超速方需等待补充（速率整形）。
func (l *IPRateLimiter) Allow(ip string) bool {
	if l.rate <= 0 {
		return true
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweep(now)
	b, ok := l.buckets[ip]
	if !ok {
		if len(l.buckets) >= l.maxKeys {
			return false // 键空间容量上限：拒绝新 IP（防无界膨胀）
		}
		b = &ipBucket{tokens: l.burst, last: now}
		l.buckets[ip] = b
	} else if now.Sub(b.last) >= l.ttl {
		// 空闲超 ttl：满桶重置（等价于 refill 结果——空闲 ttl 后恒满桶）。
		b.tokens = l.burst
		b.last = now
	} else {
		b.tokens = math.Min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}

// sweep 过期条目清理（ttl 无请求回收；gcEvery 节奏执行，防 map 无界）。
func (l *IPRateLimiter) sweep(now time.Time) {
	if now.Sub(l.lastGC) < l.gcEvery {
		return
	}
	l.lastGC = now
	for ip, b := range l.buckets {
		if now.Sub(b.last) >= l.ttl {
			delete(l.buckets, ip)
		}
	}
}

// clientIP 取请求来源 IP（RemoteAddr 仅主机部分）。不用 X-Forwarded-For：
// 客户端可伪造该头，直连攻击者据此可自选桶绕过限流；RemoteAddr 为 TCP
// 对端地址，不可伪造。
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
