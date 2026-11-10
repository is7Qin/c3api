// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package egress 是出口代理的**纯值层**：连接规格（Spec）与它的决策摘要（Key）。
// 它不碰数据库、不建连接、不认识任何业务实体——调度器的静态键只吃这里给出的
// 摘要，M3 的出口 client 工厂只吃这里的 Spec。
package egress

import (
	"crypto/sha256"
	"encoding/binary"
	"net"
	"strconv"
	"strings"
)

// Spec 是一条出口代理的**连接规格值**（spec-proxy-pool §2 协议矩阵 / §4.2）。
// 一条 Spec 唯一对应一个 egress client（内含专属 http.Transport = 专属连接池），
// 所以它的摘要必须是"还是不是同一条线"的**完全判据**：scheme / host / port /
// 认证四项任一变化都要换摘要（改端口 = 换出口 = 换上游看到的身份）。
type Spec struct {
	// Scheme ∈ socks5 / socks5h / http / https（白名单校验在写面）。**存小写**：
	// 只做大小写归一，不做 socks5h → socks5 的改写（二者在本项目语义相同，
	// 但用户输入的拼法原样保留、摘要各自独立）。
	Scheme string
	// Host 不带 IPv6 方括号（去括号是摘要的规范化，不是校验——校验在写面）。
	Host string
	// Port ∈ 1..65535（校验在写面）。
	Port int
	// Username / Password 可选认证；Password 是明文（§3.3：DB 是信任边界）。
	// 二者进摘要：改凭据 = 换一条出口线。
	Username string
	Password string
}

// New 组装规格——**唯一构造点**：scheme 归一为小写、host 去掉 IPv6 方括号。
// 规范化放在这里而不是调用点，是为了让「同一条代理」的不同写法得到同一个摘要
// （否则 `[::1]` 与 `::1` 会让静态键抖动，白白作废在途计划）。
func New(scheme, host string, port int, user, pass string) Spec {
	return Spec{
		Scheme:   strings.ToLower(scheme),
		Host:     strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"),
		Port:     port,
		Username: user,
		Password: pass,
	}
}

// Key 是规格的决策摘要（sha256 取前 16 字节）：进调度器的 planKey（静态键必须
// 保持值语义、可 `==`，故用 [16]byte 而非指针/切片）。
//
// 编码与调度器 digestParts 同构：8 字节大端长度前缀 + 内容，段数也计入摘要——
// 裸拼接会让 ("ab","c") 与 ("a","bc") 撞成同一个摘要，即改配置而键判等。
func (s Spec) Key() [16]byte {
	var lenBuf [8]byte
	h := sha256.New()
	parts := [5]string{
		s.Scheme,
		s.Host,
		strconv.Itoa(s.Port),
		s.Username,
		s.Password,
	}
	binary.BigEndian.PutUint64(lenBuf[:], uint64(len(parts)))
	h.Write(lenBuf[:])
	for _, p := range parts {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(p)))
		h.Write(lenBuf[:])
		h.Write([]byte(p))
	}
	var out [16]byte
	copy(out[:], h.Sum(nil))
	return out
}

// ResourceKey 一条出口 client 的**资源键**（`domain.EgressResourceKey` 的取值面）：
// 直连（零值 Spec，含 nil client）→ **零值**，代理 → Spec.Key()。
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

// Addr 返回 `host:port`（IPv6 自动补方括号）：M3 的 Dial/代理 URL 直接用它，
// 调用点不得自己拼（拼错方括号 = 拨错地址）。
func (s Spec) Addr() string {
	return net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
}

// String 返回脱敏摘要 `scheme://host:port`：**用户名与密码恒不出**（日志与
// err_logs.message 会用到，§5.2）。零值 Spec = 未绑定（直连）。
func (s Spec) String() string {
	if s == (Spec{}) {
		return "direct"
	}
	return s.Scheme + "://" + s.Addr()
}
