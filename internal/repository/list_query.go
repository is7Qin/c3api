// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"errors"
	"strings"

	"entgo.io/ent/dialect/sql"

	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/account"
	"github.com/is7qin/c3api/internal/ent/functionprice"
	"github.com/is7qin/c3api/internal/ent/group"
	"github.com/is7qin/c3api/internal/ent/imageprice"
	"github.com/is7qin/c3api/internal/ent/key"
	"github.com/is7qin/c3api/internal/ent/pricing"
	"github.com/is7qin/c3api/internal/ent/redemptioncode"
	"github.com/is7qin/c3api/internal/ent/redemptionuse"
	"github.com/is7qin/c3api/internal/ent/tempbalance"
	"github.com/is7qin/c3api/internal/ent/template"
	"github.com/is7qin/c3api/internal/ent/user"
)

// ListQuery 列表查询：分页/筛选/排序。Sort 为白名单内字段名（如 "name"），
// 非法值返回 ErrInvalidSort；Order 仅 "asc"/"desc"（空 = desc）。
type ListQuery struct {
	Limit      int      // <=0 → 20
	Offset     int      // <0 → 0
	Name       string   // 模糊匹配（不区分大小写）
	Email      string   // 用户专属：邮箱模糊匹配
	Sort       string   // 空 → id
	Order      string   // asc/desc；空 → desc
	StatusList []string // 账号专属：多值 status
	TemplateID int64    // 账号专属：0 = 不过滤
	UserID     int64    // keys 管理端专属：0 = 不过滤（对齐 TemplateID 先例）
	GroupID    int64    // keys 管理端专属：0 = 不过滤（对齐 TemplateID 先例）
}

var ErrInvalidSort = errors.New("invalid sort field")

// sortOrder 把 ListQuery 翻译为 ent 排序项。返回 ent.Asc/ent.Desc 的原生无名类型
// func(*sql.Selector)：各实体 Query.Order 接受同底层类型的 OrderOption（named type），
// 计划初稿的 ent.OrderFunc 返回类型与 OrderOption 互为不同 named type 不可直接赋值，
// 故返回无名类型（编译验证通过，见 Task 1 报告）。
// 白名单外 sort → ErrInvalidSort；order 非 ""/asc/desc → "invalid order"；order 空 = desc。
func (q ListQuery) sortOrder(sortFields map[string]string) (func(*sql.Selector), error) {
	if q.Sort == "" {
		q.Sort = "id"
	}
	field, ok := sortFields[q.Sort]
	if !ok {
		return nil, ErrInvalidSort
	}
	switch strings.ToLower(q.Order) {
	case "", "desc":
		return ent.Desc(field), nil
	case "asc":
		return ent.Asc(field), nil
	default:
		return nil, errors.New("invalid order")
	}
}

var (
	templateSortFields = map[string]string{
		"id": template.FieldID, "name": template.FieldName, "base_url": template.FieldBaseURL,
		"created_at": template.FieldCreatedAt, "updated_at": template.FieldUpdatedAt,
	}
	accountSortFields = map[string]string{
		"id": account.FieldID, "name": account.FieldName, "template_id": account.FieldTemplateID,
		"status": account.FieldStatus, "cooldown_until": account.FieldCooldownUntil,
		"weight": account.FieldWeight, "max_concurrency": account.FieldMaxConcurrency,
		"last_used_at": account.FieldLastUsedAt, "created_at": account.FieldCreatedAt,
		"updated_at": account.FieldUpdatedAt,
	}
	groupSortFields = map[string]string{
		"id": group.FieldID, "name": group.FieldName, "created_at": group.FieldCreatedAt,
		"updated_at": group.FieldUpdatedAt,
	}
	userSortFields = map[string]string{
		"id": user.FieldID, "email": user.FieldEmail, "role": user.FieldRole,
		"status": user.FieldStatus, "max_concurrency": user.FieldMaxConcurrency,
		"created_at": user.FieldCreatedAt, "updated_at": user.FieldUpdatedAt,
	}
	keySortFields = map[string]string{
		"id": key.FieldID, "name": key.FieldName, "status": key.FieldStatus,
		"max_concurrency": key.FieldMaxConcurrency, "quota": key.FieldQuota,
		"quota_used": key.FieldQuotaUsed, "created_at": key.FieldCreatedAt,
		"updated_at": key.FieldUpdatedAt,
	}
	redemptionCodeSortFields = map[string]string{
		"id": redemptioncode.FieldID, "code": redemptioncode.FieldCode, "type": redemptioncode.FieldType,
		"value": redemptioncode.FieldValue, "max_uses": redemptioncode.FieldMaxUses,
		"used_count": redemptioncode.FieldUsedCount, "status": redemptioncode.FieldStatus,
		"created_by": redemptioncode.FieldCreatedBy, "created_at": redemptioncode.FieldCreatedAt,
		"updated_at": redemptioncode.FieldUpdatedAt,
	}
	redemptionUseSortFields = map[string]string{
		"id": redemptionuse.FieldID, "code_id": redemptionuse.FieldCodeID,
		"user_id": redemptionuse.FieldUserID, "value": redemptionuse.FieldValue,
		"created_at": redemptionuse.FieldCreatedAt,
	}
	// tempBalanceSortFields /admin/temp-balances sort 白名单（spec 2026-08-15：
	// 仅 expires_at/amount/created_at 三键——默认 expires_at asc 由 handler 显式
	// 设置，不走 sortOrder 的 id 缺省）。
	tempBalanceSortFields = map[string]string{
		"expires_at": tempbalance.FieldExpiresAt, "amount": tempbalance.FieldAmount,
		"created_at": tempbalance.FieldCreatedAt,
	}
	pricingSortFields = map[string]string{
		"id": pricing.FieldID, "model": pricing.FieldModel,
		"updated_at": pricing.FieldUpdatedAt,
	}
	imagePriceSortFields = map[string]string{
		"id": imageprice.FieldID, "model": imageprice.FieldModel,
		"updated_at": imageprice.FieldUpdatedAt,
	}
	functionPriceSortFields = map[string]string{
		"id": functionprice.FieldID, "model": functionprice.FieldModel,
		"updated_at": functionprice.FieldUpdatedAt,
	}
)
