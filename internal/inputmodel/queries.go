// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package inputmodel 定义**跨层输入契约**（查询筛选 / 写补丁 / 手动设价入参）。
//
// 为什么单独成包（R1-c）：这些类型原本住在 internal/repository，于是 handler 为了
// 拼一个查询参数就得 import 整个存储实现层（ent、pgx、事务门面全部进依赖闭包），
// 而它其实**一个存储函数都不调**。根因是**输入契约住在实现包里**，与 R1-b 的
// 行契约（qualitymodel）同型：方向必须是"实现 → 契约"，不是反过来。
//
// 为什么不并进 internal/qualitymodel（R1-b）：那个包是 quality 域的**行**契约
// （输出侧，落库后长什么样）；本包是**输入**侧，且横跨 user / order / key / usage /
// payment / price 多个域。两者混在一起会变成垃圾桶，而拆开各自仍能独立演进。
//
// 为什么不并进 internal/storeerr：那是**错误轴**。这里装的是数据。
//
// 边界：只依赖 internal/domain + stdlib。**不**依赖 repository（否则环回来）。
// 9 个类型里 6 个的字段引用 domain 枚举，3 个（UsageQuery / ErrLogQuery /
// AccountPatch）不引用——但包级依赖"有一个就算"，故整体仍需 domain 一条边。
package inputmodel

import (
	"time"

	"github.com/is7qin/c3api/internal/domain"
)

// ListQuery 列表查询：分页/筛选/排序。Sort 为白名单内字段名（如 "name"），
// 非法值返回 ErrInvalidSort；Order 仅 "asc"/"desc"（空 = desc）。
type ListQuery struct {
	Limit           int    // <=0 → 20
	Offset          int    // <0 → 0
	Name            string // 模糊匹配（不区分大小写）
	Email           string // 用户专属：邮箱模糊匹配
	Role            string // 用户专属：角色精确过滤（空 = 不过滤）；未知非空值 → 空列表
	SupplierSurface bool   // 用户专属：true = 仅供应商面可达角色（domain.SupplierSurfaceRoles），覆盖 Role
	Sort            string // 空 → id
	Order           string // asc/desc；空 → desc
	TemplateID      int64  // 账号专属：0 = 不过滤
	// Enabled 账号专属三态过滤：nil = 不过滤；&true = 仅启用；&false = 仅禁用。
	// 与运行时失效（failed_at）无关——管理面启停是独立维度。
	Enabled *bool
	// AccountGroupID 账号列表专属分组筛选（三态，与 keys 的 GroupID 语义不同，
	// 故必须是独立字段 + 指针）：nil = 不过滤；&0 = 未分组；&n(>0) = 该分组。
	// 负数由 service 层拒绝（ErrInvalidInput → 400），本层只认三态、不重复校验。
	AccountGroupID *int64
	UserID         int64 // keys 管理端专属：0 = 不过滤（对齐 TemplateID 先例）
	GroupID        int64 // keys 管理端专属：0 = 不过滤（对齐 TemplateID 先例）
}

type UsageQuery struct {
	GroupID   int64 // 0 = 不过滤
	AccountID int64
	UserID    int64 // 0 = 不过滤（/api/user/usages 强制 = 自己）
	KeyID     int64
	Model     string
	Format    string // 空 = 不过滤（无效值自然查空——与 model 同语义，契约不校验值域）
	ErrorType string // usage_logs = 纯计费明细（仅 cost>0）→ 值域收敛 none/abort（err_logs 分表后）
	From      *time.Time
	To        *time.Time
	Cursor    int64 // keyset 游标（上页最后一条 id；<=0 = 首页无 id 谓词）
	Limit     int
}

// OrderQuery 订单列表筛选（§7-3 管理面 / §7-1 用户面）：
// 零值 = 不过滤；Limit <=0 → 20，>100 → 100；Offset <0 → 0。
type OrderQuery struct {
	UserID      int64 // 0 = 全部；用户面由 service 强制 = 本人
	Status      *domain.OrderStatus
	OutTradeNo  string
	CreatedFrom *time.Time // created_at >= (nil = 不限)
	CreatedTo   *time.Time // created_at <= (nil = 不限)
	Limit       int
	Offset      int
}

// ErrLogQuery err_logs 查询过滤面（/errlogs API 同构 /usages：分页 + error_type/
// status_code/时间范围/model/group 等——完整错误面，status_code/error_type 全值）。
type ErrLogQuery struct {
	GroupID    int64 // 0 = 不过滤
	AccountID  int64
	UserID     int64 // 0 = 不过滤（/api/user/errlogs 强制 = 自己）
	KeyID      int64
	Model      string
	Format     string // 空 = 不过滤（无效值自然查空——与 model 同语义，契约不校验值域）
	StatusCode int    // 0 = 不过滤
	ErrorType  string
	From       *time.Time
	To         *time.Time
	Cursor     int64 // keyset 游标（上页最后一条 id；<=0 = 首页无 id 谓词）
	Limit      int
}
