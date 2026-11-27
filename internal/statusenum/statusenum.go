// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package statusenum 实体状态枚举的唯一权威定义（active / disabled）。
//
// 为什么单独一个包：这些值必须有两类消费者，而它们**不能**互相依赖——
//
//   - `internal/ent/schema` 用它声明 Ent 枚举。它是 codegen 的**输入**，
//     若为此依赖 `internal/domain`，整个 schema 包会被拖进 domain 的 200+ 个
//     传递依赖（实测含 crypto、net/http、sjson）——一个只声明几个字符串常量的包
//     不该背这些运行时。
//   - `internal/domain` 用它承载业务类型。domain 才是重的一侧。
//
// 把值放在双方都能依赖的中立叶子里，`domain` 做**类型别名**，
// `ent/schema` 直接引用本包 —— 单一事实源不变，依赖方向变干净。
// 与 `internal/modelmap` / `internal/identityenv` / `internal/codexproto` 同范式。
//
// ⚠ 本包**零内部依赖**（只 import stdlib）。加任何 `internal/*` 或 `pkg/*` 之前先问：
// 这个值还需要知道别的什么吗？不需要，它就是两个字符串。
package statusenum

// UserStatus 用户状态。
type UserStatus string

const (
	UserStatusActive   UserStatus = "active"
	UserStatusDisabled UserStatus = "disabled"
)

// Valid 报告 s 是否是已知的用户状态。
func (s UserStatus) Valid() bool {
	switch s {
	case UserStatusActive, UserStatusDisabled:
		return true
	}
	return false
}

// KeyStatus 客户端 key 状态。
type KeyStatus string

const (
	KeyStatusActive   KeyStatus = "active"
	KeyStatusDisabled KeyStatus = "disabled"
)

// Valid 报告 s 是否是已知的 key 状态。
func (s KeyStatus) Valid() bool {
	switch s {
	case KeyStatusActive, KeyStatusDisabled:
		return true
	}
	return false
}

// ManagementKeyStatus 管理 key（mk-）状态。active = 可用；disabled = 软禁用
// （快照即时 401，可再启用），区别于删除（软删，不可逆撤销）。
type ManagementKeyStatus string

const (
	ManagementKeyStatusActive   ManagementKeyStatus = "active"
	ManagementKeyStatusDisabled ManagementKeyStatus = "disabled"
)

// Valid 报告 s 是否是已知的管理 key 状态。
func (s ManagementKeyStatus) Valid() bool {
	switch s {
	case ManagementKeyStatusActive, ManagementKeyStatusDisabled:
		return true
	}
	return false
}

// RedemptionStatus 兑换码状态。
type RedemptionStatus string

const (
	RedemptionStatusActive   RedemptionStatus = "active"
	RedemptionStatusDisabled RedemptionStatus = "disabled"
)

// Valid 报告 s 是否是已知的兑换码状态。
func (s RedemptionStatus) Valid() bool {
	switch s {
	case RedemptionStatusActive, RedemptionStatusDisabled:
		return true
	}
	return false
}
