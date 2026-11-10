// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

// 传输层资源的**配置批次指令**（spec §3.3b-1："传输表不认识配置"）。
//
// 为什么这些类型在 domain：一份指令有两侧——配置权威（scheduler 的 accountRuntime）
// 产出、资源持有者（sdkbridge 的传输资源表）执行。若类型留在任一侧，另一侧就必须
// 反向 import（scheduler↔sdkbridge 会成环或形成倒置依赖），而两者都依赖 domain。
// 于是这里只放**值**：谁保留哪些资源键、哪个账号的资源作废、批次代号——不放任何
// "哪个账号算活跃""直连要不要保留"的策略，那类判断留在配置权威里。

// EgressResourceKey 一个出口资源的键：出口规格的决策摘要（`egress.Spec.Key()`）。
// 直连是零值——"直连也是一条出口"，由**配置权威**显式列入保留集，不是表的特例。
type EgressResourceKey [16]byte

// AccountResourceAction 配置权威对某账号传输资源的动作。
type AccountResourceAction uint8

const (
	// ResourceRetain 保留该账号的条目，并只保留 Keep 里列出的出口资源键（其余
	// 出口桶回收：代理地址/密码轮换后的历史摘要不得无界增长）。
	ResourceRetain AccountResourceAction = iota + 1
	// ResourceDelete 整账号资源作废：条目与其全部出口桶一起摘除。
	ResourceDelete
)

// AccountResourceDirective 一条**账号级**资源指令。Action 与 Keep 都是显式值：
// 表只执行，不做差分、不补默认、不推断"没被提到的账号该怎么办"。
type AccountResourceDirective struct {
	AccountID int64
	Action    AccountResourceAction
	// Keep 该账号要保留的出口资源键（仅 ResourceRetain 有意义）。**配置权威负责**
	// 把"直连恒保留"之类的策略折进来——表不认识这条策略。
	Keep []EgressResourceKey
}

// ResourceBatch 一个配置批次的不可变资源指令：`Gen` 是批次代号（配置权威在
// 自己的串行区内取号），资源表只接受**更新**的号——旧批次（先取号、后被新批次
// 插队，应用在锁外）必须整体 no-op，否则会把资源可见性回退到旧配置。
//
// 零值（Gen == 0 且无指令）= "没有指令"（该能力未装配）。批次必须**完整**：
// 该删的账号要用 ResourceDelete 点名（差分是配置权威的职责）。
type ResourceBatch struct {
	Gen        uint64
	Directives []AccountResourceDirective
}
