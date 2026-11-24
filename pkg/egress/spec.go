// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package egress 是出口代理的**实现层**：进程级 client 注册表（Pool）、一次性出口
// client（NewClient）、代理 URL 装配（proxyURL）与 transport 派生（cloneTransport /
// codexClientOf）。
//
// 值类型不在本包：`Spec` / `Client` / `Flavour` / `ResourceKey` 的 canonical 定义在
// pkg/egressapi——路由核心只该依赖"出口是什么"，不该依赖"出口怎么被建出来"。本包
// 为既有调用方保留同名**别名**（compat.go），别名不是终态，移除条件写在那个文件里。
package egress

import "strings"

// New 组装规格——**唯一构造点**：scheme 归一为小写、host 去掉 IPv6 方括号。
// 规范化放在这里而不是调用点，是为了让「同一条代理」的不同写法得到同一个摘要
// （否则 `[::1]` 与 `::1` 会让静态键抖动，白白作废在途计划）。
//
// 为什么构造点没随类型搬进 pkg/egressapi：搬的判据是"类型别名带不了方法"，而它没有
// 方法；归一化本身仍是值层动作，与 `Spec.Key` 的稳定性判据是一件事，所以它留在能
// 同时看见两者的地方。它会与 pkg/egress 的实现一起收缩——那一步会顺带解开
// internal/domain → pkg/egress 的边（账本 559），属于另一单，不塞进本次纯搬运。
func New(scheme, host string, port int, user, pass string) Spec {
	return Spec{
		Scheme:   strings.ToLower(scheme),
		Host:     strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"),
		Port:     port,
		Username: user,
		Password: pass,
	}
}
