// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package inputmodel

import "github.com/is7qin/c3api/internal/domain"

type TemplatePatch struct {
	Name             *string
	BaseURL          *string
	SupportedFormats *[]domain.RequestFormat
	Models           *[]string
	FormatModels     *map[domain.RequestFormat][]string
	ModelMapping     *domain.ModelMapping
}

type AccountPatch struct {
	Name        *string
	TemplateID  *int64
	UpstreamKey *string
	// BaseURL 批量三态（定死）：nil = 不变；&"" = 清空（落 NULL = 继承
	// 模板）；&非空 = 落值。
	BaseURL        *string
	MaxConcurrency *int
	// GroupIDs nil = 不变；非 nil = 替换账号全部分组（含空数组 = 清空）。
	GroupIDs                 *[]int64
	Enabled                  *bool
	UpstreamCostMultiplierBp *int
	CacheDomain              *string // nil=不变, &""=清空, &val=落值
	// SupplierUserID 归属（spec §2.5 管理面「把账号分配给供应商」入口）：nil =
	// 不变；&0 = 清空（回平台自有，落 NULL）；&uid(>0) = 分配给该用户（值域校验：
	// 目标须为供应商面可达用户，且与禁用路径锁同一 users 行）。
	SupplierUserID *int64
	// ProxyID 出口代理（spec-proxy-pool §3.2）：nil = 不变；&0 = 解绑（落 NULL，
	// 回落直连）；&id(>0) = 绑定该代理（存在性与可用性由 service 校验——仓库层
	// 只搬值，B 端 0 与"C 端 null"不可区分，故沿用 supplier_user_id 的 &0 先例）。
	ProxyID *int64
}

type GroupPatch struct {
	Name       *string
	Visibility *domain.GroupVisibility
}

// UserPatch 用户更新补丁（管理面 PUT patch 语义）：显式字段 = 请求显式提供的
// 字段；role/status 无条件写（无增量写者）；balance/max_concurrency 显式设置
// 时必带旧值条件（OldXxx = GET 快照，服务层重试时重读刷新）——旧值不满足
// （期间有扣费/并发变更）→ 0 行 → ErrConflict，绝不无条件覆盖并发增量
// （v02 核实：GET 快照陈旧值写回与 flusher 扣费双向覆盖，余额凭空复活）。
type UserPatch struct {
	ID                int64
	Role              *domain.Role
	Status            *domain.UserStatus
	MaxConcurrency    *int
	OldMaxConcurrency *int
	Balance           *int64
	OldBalance        *int64
}
