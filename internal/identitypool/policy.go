// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package identitypool

import (
	"github.com/is7qin/c3api/internal/sdkbridge"
)

// Policy 会话池的轮换策略：每线程最多活几个窗口（开线程时在 [Lo, Hi] 内均匀抽；
// Hi == 0 表示不退休）。
//
// 这是**部署策略**而不是 wire 语义：宿主有权选"线程活多久"，窗口跨度怎么算仍在 SDK。
// 值类型可直接比较——resize 靠它判断"要不要换新会话"（见 Pool.RotatePolicy）。
//
// 为什么策略的所有者是本包而不是 SDK 桥：策略的唯一消费者是会话池（它决定
// "什么时候换线程"），SDK 桥只把它当**构造参数**接受；所有权搬过来之后，本包
// 不必再用别人的类型说话，接口面（Spec.Policy / Pool.RotatePolicy /
// Session.Policy / scheduler.Config.RotatePolicy）也不再出现 SDK 类型。这一搬动
// **没有新增任何到 sdkbridge 的边**——identitypool → sdkbridge 本来就在（线程
// 工厂与句柄在其间），只是它不再负责命名"策略"。
//
// 字段名是 Lo/Hi 而不是 SDK 侧的拼写：本包在代际符号门禁的在途扫描面内，借用
// SDK 的窗口上限字段名会让"策略"和"代际"在同一个词表里长得一样（同名不等于同义，
// 但读的人会先猜）。这一形状由代际符号门禁的 structFieldSpecs 显式登记
// （`internal/identitypool` / `Policy` / 必须带 Lo+Hi / 不得带任何代际拼写），
// 所以"策略里混进一个代际字段"是机械失败而不是评审遗漏；登记它的那一步同时要求
// 它**真的是一个结构体**（别名不进那张表）。
type Policy struct {
	Lo uint64
	Hi uint64
}

// DefaultPolicy 默认策略（每线程 16–48 个窗口，与 SDK 默认一致）。数值是**契约**：
// 它等于 P5-2b 之前 SDK 桥那条默认策略（16/48），装配点（cmd/server）唯一读取。
func DefaultPolicy() Policy { return Policy{Lo: 16, Hi: 48} }

// sdk 归一成 SDK 桥接受的策略。**唯一转换点**（私有）：只许构造边界调用——
// 即 NewSession 把本包的策略交给 sdkbridge.NewThreads 的那一行。任何第二处调用
// 都会让"策略唯一所有者"这句话变成空话。
func (p Policy) sdk() sdkbridge.RotatePolicy {
	return sdkbridge.RotatePolicy{WMaxLo: p.Lo, WMaxHi: p.Hi}
}

// policyOf 是 sdk 的反向：把工厂的**只读副本**读回本包的类型
// （Session.Policy 的派生值。工厂只会被 NewSession 用 sdk() 造出来，所以这一次
// 转换只是把同两个数搬回来，不做归一、不做校验）。
func policyOf(sp sdkbridge.RotatePolicy) Policy {
	return Policy{Lo: sp.WMaxLo, Hi: sp.WMaxHi}
}
