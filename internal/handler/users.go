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

// 用户管理面（/admin/users）：余额字段在 API 边界换算 USD float64（内部存储
// 毫分——1 USD = 100,000 毫分；usdToMillis/millisToUSD）。价格倍率按组
// （T3.5 修正）经 /admin/groups/{id}/assignments 的 multipliers 设置，用户
// 本体无倍率字段。

// GetUsers 用户列表（platform_admin 专属；/admin 组中间件已鉴权，
// ServerInterface）。
func (h *AdminAPI) GetUsers(w http.ResponseWriter, r *http.Request, params GetUsersParams) {
	q := repository.ListQuery{
		Limit:  httpface.ClampLimit(int(deref(params.Limit))),
		Offset: int(deref(params.Offset)),
		Email:  deref(params.Email),
		Sort:   deref(params.Sort),
		Order:  string(deref(params.Order)),
	}
	rows, total, err := h.svc.ListUsers(r.Context(), q)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]User, 0, len(rows))
	for _, u := range rows {
		out = append(out, toAPIUser(u))
	}
	httpface.WriteJSON(w, http.StatusOK, UserListResponse{Total: total, Rows: out})
}

// PostUsers 创建用户（platform_admin 专属；email 唯一/密码长度校验在
// service；balance 输入 USD 换算毫分，ServerInterface）。
func (h *AdminAPI) PostUsers(w http.ResponseWriter, r *http.Request) {
	var in UserCreate
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	role := domain.RoleUser
	if in.Role != nil {
		role = domain.Role(*in.Role)
	}
	status := domain.UserStatusActive
	if in.Status != nil {
		status = domain.UserStatus(*in.Status)
	}
	u, err := h.svc.CreateUser(r.Context(), in.Email, in.Password, role, status,
		deref(in.MaxConcurrency), usdToMillis(deref(in.Balance)))
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIUser(u))
}

// PutUsersId 更新用户（role/status/max_concurrency/balance；变更即时生效——
// Auth 快照刷新，ServerInterface）。
// patch 形态（v02 核实 P1 修复）：只把请求显式提供的字段传给更新——请求不带
// balance 时不再把 GET 快照陈旧值全量写回（与 flusher 扣费双向覆盖、余额凭空
// 复活）；balance/max_concurrency 显式设置时带 GET 快照旧值条件（期间有扣费
// → 0 行 → service 重读重试，new 保持管理员显式意图）。
func (h *AdminAPI) PutUsersId(w http.ResponseWriter, r *http.Request, id int64) {
	var in UserUpdate
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	u, err := h.svc.GetUser(r.Context(), id)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	patch := &repository.UserPatch{ID: u.ID}
	if in.Role != nil {
		role := domain.Role(*in.Role)
		patch.Role = &role
	}
	if in.Status != nil {
		st := domain.UserStatus(*in.Status)
		patch.Status = &st
	}
	if in.MaxConcurrency != nil {
		patch.MaxConcurrency = in.MaxConcurrency
		patch.OldMaxConcurrency = &u.MaxConcurrency // 旧值条件：GET 快照
	}
	if in.Balance != nil {
		bal := usdToMillis(*in.Balance)
		patch.Balance = &bal
		patch.OldBalance = &u.Balance // 旧值条件：GET 快照
	}
	updated, err := h.svc.UpdateUser(r.Context(), patch)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIUser(updated))
}

// GetUsersIdGroups 读取用户被授予的组与各专属倍率（platform_admin；用户视角，
// 与 /groups/{id}/assignments 对称；ServerInterface）。mults 只含有专属倍率的
// 组（null/缺省 = 未设置 → 用组倍率）。
func (h *AdminAPI) GetUsersIdGroups(w http.ResponseWriter, r *http.Request, id int64) {
	ids, mults, err := h.svc.GetUserGroups(r.Context(), id)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, UserGroupsResponse{
		GroupIds:    ids,
		Multipliers: toAPIMultipliers(mults),
	})
}

// PutUsersIdGroups 设置用户的授予分组（platform_admin；替换语义：group_ids =
// 完整授予组列表（未列出即撤销，空数组 = 清空）；ServerInterface）。
// multipliers 可选：group_id → 该用户在该组的专属价格倍率（正常值 0~10；
// null = 清除为未设置 → 回退组倍率；键必须 ∈ group_ids）。
func (h *AdminAPI) PutUsersIdGroups(w http.ResponseWriter, r *http.Request, id int64) {
	var in UserGroupsBody
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	var mults map[int64]*int
	if in.Multipliers != nil {
		m, err := apiMultiplierMap(*in.Multipliers) // map[string]*float64 → map[int64]*int（万分数）
		if err != nil {
			httpface.WriteErr(w, http.StatusBadRequest, err.Error())
			return
		}
		mults = m
	}
	applied, postMults, err := h.svc.SetUserGroups(r.Context(), id, in.GroupIds, mults)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, UserGroupsResponse{
		GroupIds:    applied,
		Multipliers: toAPIMultipliers(postMults),
	})
}
