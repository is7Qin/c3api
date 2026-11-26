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
	"github.com/is7qin/c3api/internal/ent/balancelog"
	"github.com/is7qin/c3api/internal/ent/group"
	"github.com/is7qin/c3api/internal/ent/key"
	"github.com/is7qin/c3api/internal/ent/priceentry"
	"github.com/is7qin/c3api/internal/ent/redemptioncode"
	"github.com/is7qin/c3api/internal/ent/redemptionuse"
	"github.com/is7qin/c3api/internal/ent/tempbalance"
	"github.com/is7qin/c3api/internal/ent/template"
	"github.com/is7qin/c3api/internal/ent/user"
	"github.com/is7qin/c3api/internal/inputmodel"
)

// ListQuery 列表查询：分页/筛选/排序。Sort 为白名单内字段名（如 "name"），
// 非法值返回 ErrInvalidSort；Order 仅 "asc"/"desc"（空 = desc）。
//
// R1-c：canonical 定义搬到 internal/inputmodel（输入契约不该住在存储实现包里），
// 本处只留**类型别名**（带 `=`）。handler/service 与本包必须与 canonical 是
// **同一个类型**：写成新定义会静默变成"两份同形 struct"，跨包赋值与同一性判定
// 全部失真。编译期双向可赋值断言见 inputmodel_identity.go。
type ListQuery = inputmodel.ListQuery

var ErrInvalidSort = errors.New("invalid sort field")

// sortOrder 把 ListQuery 翻译为 ent 排序项。返回 ent.Asc/ent.Desc 的原生无名类型
// func(*sql.Selector)：各实体 Query.Order 接受同底层类型的 OrderOption（named type），
// 计划初稿的 ent.OrderFunc 返回类型与 OrderOption 互为不同 named type 不可直接赋值，
// 故返回无名类型（编译验证通过）。
// 白名单外 sort → ErrInvalidSort；order 非 ""/asc/desc → "invalid order"；order 空 = desc。
//
// 为什么是**包级函数**而不是方法（R1-c）——两条约束合在一起：
//  1. 方法体要 entgo.io/ent/dialect/sql（返回 func(*sql.Selector)），而 ListQuery 住在
//     internal/inputmodel——那个包必须是只依赖 domain 的叶子，装不下这个依赖；
//  2. Go 不允许在别人的类型上定义方法，所以也不能在本包写
//     func (q inputmodel.ListQuery) sortOrder(...)。
//
// （注意：别名**本身是能带方法的**，真正的阻碍是上面两条。）
//
// 本包是唯一需要该行为的包（排序白名单是 ent 列名，属于存储实现细节，
// 不该进契约包），所以保留在这里、显式收 q。语义与原先的方法逐字相同：
// 形参按值传入，函数内对 q.Sort 的缺省改写原本也只影响副本。
func sortOrder(q ListQuery, sortFields map[string]string) (func(*sql.Selector), error) {
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
		"max_concurrency": account.FieldMaxConcurrency,
		"last_used_at":    account.FieldLastUsedAt, "created_at": account.FieldCreatedAt,
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
	// tempBalanceSortFields /api/admin/temp-balances sort 白名单（spec 2026-08-15：
	// 仅 expires_at/amount/created_at 三键——默认 expires_at asc 由 handler 显式
	// 设置，不走 sortOrder 的 id 缺省）。
	tempBalanceSortFields = map[string]string{
		"expires_at": tempbalance.FieldExpiresAt, "amount": tempbalance.FieldAmount,
		"created_at": tempbalance.FieldCreatedAt,
	}
	priceEntrySortFields = map[string]string{
		"id": priceentry.FieldID, "model": priceentry.FieldModel,
		"updated_at": priceentry.FieldUpdatedAt,
	}
	// balanceLogSortFields /api/admin/users/{id}/balance-logs sort 白名单
	// （默认 id——按用户分页走 (user_id, id) 索引）。
	balanceLogSortFields = map[string]string{
		"id": balancelog.FieldID, "amount": balancelog.FieldAmount,
		"balance_after": balancelog.FieldBalanceAfter, "created_at": balancelog.FieldCreatedAt,
	}
)
