// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import "time"

// RedemptionType 兑换码类型：资源发放的通用载体（Phase 5 计费前基础设施）。
// balance = users.balance += value；concurrency = users.max_concurrency（0 = 不限特判）；
// temp_balance = 插入 temp_balances 行（resource_expires_at 必填）。
// 类型 → applier 注册表：新类型 = 新增 applier，兑换流程零改动。
type RedemptionType string

const (
	RedemptionTypeBalance     RedemptionType = "balance"
	RedemptionTypeConcurrency RedemptionType = "concurrency"
	RedemptionTypeTempBalance RedemptionType = "temp_balance"
)

func (t RedemptionType) Valid() bool {
	switch t {
	case RedemptionTypeBalance, RedemptionTypeConcurrency, RedemptionTypeTempBalance:
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

func (s RedemptionStatus) Valid() bool {
	switch s {
	case RedemptionStatusActive, RedemptionStatusDisabled:
		return true
	}
	return false
}

// RedemptionCode 兑换码（不可编辑：生成后仅可失效）。
type RedemptionCode struct {
	ID                int64
	Code              string
	Type              RedemptionType
	Value             int64            // 毫分（1 USD = 100,000 毫分；concurrency 类型为并发数）
	Remark            *string          // 运营备注，可选
	ExpiresAt         *time.Time       // 码未兑换即过期；nil = 永久
	ResourceExpiresAt *time.Time       // 兑换后资源到期；temp_balance 必填（service 校验）
	MaxUses           int              // 1 = 单次码；>1 = 多人码
	UsedCount         int              // 已兑换次数（条件递增，防并发超卖）
	Status            RedemptionStatus
	CreatedBy         int64 // 0 = 系统（静态 admin token）；>0 = platform_admin user_id
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

// RedemptionUse 兑换审计记录（全量留痕；UNIQUE(code_id, user_id) = DB 兜底幂等）。
type RedemptionUse struct {
	ID                int64
	CodeID            int64
	UserID            int64
	Value             int64 // 兑换时的值快照（毫分；concurrency 类型为并发数）
	ResourceExpiresAt *time.Time
	CreatedAt         time.Time
}

// RedemptionApply 兑换成功回执（/api/user/redemptions POST 响应体）。
// ResourceExpiresAt = 兑换后资源到期（temp_balance 必有；balance/concurrency 恒 nil）。
type RedemptionApply struct {
	Type              RedemptionType
	Value             int64            // 兑换值（毫分；concurrency 类型为并发数）
	ResourceExpiresAt *time.Time
}

// RedemptionRecord 我的兑换记录（/api/user/redemptions GET 行）：use 快照 + 码的
// type/remark 联查（use 行不存码类型；码生成后 type/remark 不可变，失效不影响）。
type RedemptionRecord struct {
	ID                int64
	CodeID            int64
	Code              string
	CodeType          RedemptionType
	Value             int64            // 兑换值快照（毫分；concurrency 类型为并发数）
	Remark            *string
	ResourceExpiresAt *time.Time
	CreatedAt         time.Time
}
