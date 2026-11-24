// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package egressapi 是出口代理的**中立类型层**：连接规格（Spec）与它的决策摘要
// （Key）、出口句柄（Client）、口味闭集（Flavour）与资源键派生（ResourceKey）。
//
// 它不碰数据库、不建连接、不认识任何业务实体，也**不 import 任何 internal/***
// ——依赖只有 stdlib。这条纪律就是它存在的原因：调度器的核心路由只该依赖"出口是
// 什么"，不该依赖"出口怎么被建出来"。连接池、代理 URL 装配、transport 派生与连接
// 配额全部留在实现包 pkg/egress；本包不知道它的存在。
package egressapi

import (
	"crypto/sha256"
	"encoding/binary"
	"net"
	"strconv"
)

// Spec 是一条出口代理的**连接规格值**（spec-proxy-pool §2 协议矩阵 / §4.2）。
// 一条 Spec 唯一对应一个 Client（内含专属 http.Transport = 专属连接池），
// 所以它的摘要必须是"还是不是同一条线"的**完全判据**：scheme / host / port /
// 认证四项任一变化都要换摘要（改端口 = 换出口 = 换上游看到的身份）。
//
// 零值 = 直连。字段全部导出，但**构造必须经归一化**（scheme 归一为小写、host 去掉
// IPv6 方括号）：`[::1]` 与 `::1` 若各得一个摘要，静态键会抖动、在途计划白白作废。
// 归一化点在实现侧（pkg/egress.New），不在本包复制一份——类型搬了、构造点没搬。
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
