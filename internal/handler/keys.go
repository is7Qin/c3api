// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/repository"
)

// 管理端密钥列表（/admin/keys，spec 2026-08-16）：全量视角（不限归属用户）+
// name 模糊 / user_id / group_id 等值收窄 + sort 白名单 id/name/created_at。
// 脱敏铁律（用户裁决）：AdminKey 响应 schema 无 key 明文字段——转换面必须
// 剥掉 KeyRaw，密钥明文绝不下发管理端。

// GetKeys 密钥列表（脱敏，不含 key 明文；platform_admin 专属，ServerInterface）。
// 参数为 limit/offset 直接范式（不走 pageToQuery 的 page/page_size 范式——
// 本端点分页规格即 limit/offset）；limit/offset 缺省归一在 ListQuery 内
// （20/0），sort/order 缺省（id/desc）与白名单校验在 sortOrder。
func (h *AdminAPI) GetKeys(w http.ResponseWriter, r *http.Request, params GetKeysParams) {
	q := repository.ListQuery{
		Limit:   httpface.ClampLimit(int(deref(params.Limit))),
		Offset:  int(deref(params.Offset)),
		Name:    deref(params.Name),
		UserID:  deref(params.UserId),
		GroupID: deref(params.GroupId),
		Sort:    deref(params.Sort),
		Order:   string(deref(params.Order)),
	}
	rows, total, err := h.svc.ListAdminKeys(r.Context(), q)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]AdminKey, 0, len(rows))
	for _, k := range rows {
		out = append(out, toAPIAdminKey(k))
	}
	httpface.WriteJSON(w, http.StatusOK, AdminKeyListResponse{Total: total, Rows: out})
}

// toAPIAdminKey key 领域对象 → 契约类型（脱敏面：不映射 KeyRaw——响应
// 无 key 明文字段，用户裁决）。
func toAPIAdminKey(k *domain.Key) AdminKey {
	st := KeyStatus(k.Status)
	return AdminKey{
		ID:             &k.ID,
		UserID:         &k.UserID,
		GroupID:        &k.GroupID,
		Name:           &k.Name,
		Status:         &st,
		MaxConcurrency: &k.MaxConcurrency,
		Quota:          &k.Quota,
		QuotaUsed:      &k.QuotaUsed,
		CreatedAt:      &k.CreatedAt,
		UpdatedAt:      &k.UpdatedAt,
		DeletedAt:      k.DeletedAt, // 软删除时间戳（只读字段，入参不接收；列表已过滤已删）
	}
}
