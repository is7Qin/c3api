// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
)

// toAPIManagementKey 管理 key 领域对象 → 用户面契约类型（明文长期回显——与客户端
// key 同款自托管权衡）。owner 恒为当前身份。
func toAPIManagementKey(k *domain.ManagementKey) ManagementKey {
	st := ManagementKeyStatus(k.Status)
	return ManagementKey{
		CreatedAt: &k.CreatedAt,
		Id:        &k.ID,
		KeyRaw:    &k.KeyRaw,
		Name:      &k.Name,
		Status:    &st,
		UpdatedAt: &k.UpdatedAt,
		UserId:    &k.UserID,
	}
}

// GetUserManagementKeys 我的管理 key 列表（软删过滤；含明文，ServerInterface）。
func (h *UserAPI) GetUserManagementKeys(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.ListManagementKeys(r.Context(), currentUserID(r))
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]ManagementKey, 0, len(rows))
	for _, k := range rows {
		out = append(out, toAPIManagementKey(k))
	}
	httpface.WriteJSON(w, http.StatusOK, ManagementKeyListResponse{Rows: out})
}

// PostUserManagementKeys 创建管理 key（以自身身份鉴权管理面；返回明文 mk-，
// ServerInterface）。
func (h *UserAPI) PostUserManagementKeys(w http.ResponseWriter, r *http.Request) {
	var in ManagementKeyCreate
	if err := httpface.Decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	k, err := h.svc.CreateManagementKey(r.Context(), currentUserID(r), in.Name)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIManagementKey(k))
}

// PutUserManagementKeysId 更新管理 key（name/status；仅本人；disabled 即时 401
// 可再启用，ServerInterface）。
func (h *UserAPI) PutUserManagementKeysId(w http.ResponseWriter, r *http.Request, id int64) {
	var in ManagementKeyUpdate
	if err := httpface.Decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	var status *domain.ManagementKeyStatus
	if in.Status != nil {
		st := domain.ManagementKeyStatus(*in.Status)
		status = &st
	}
	k, err := h.svc.UpdateManagementKey(r.Context(), currentUserID(r), id, in.Name, status)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIManagementKey(k))
}

// DeleteUserManagementKeysId 删除管理 key（仅本人；软删 + 本实例快照即时移除，
// ServerInterface）。
func (h *UserAPI) DeleteUserManagementKeysId(w http.ResponseWriter, r *http.Request, id int64) {
	if err := h.svc.DeleteManagementKey(r.Context(), currentUserID(r), id); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, DeletedResponse{Deleted: true})
}
