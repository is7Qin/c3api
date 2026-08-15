// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// GetUserKeys 我的 key 列表（分页/排序；明文长期回显，ServerInterface）。
func (h *UserAPI) GetUserKeys(w http.ResponseWriter, r *http.Request, params GetUserKeysParams) {
	q := repository.ListQuery{
		Limit:  int(deref(params.Limit)),
		Offset: int(deref(params.Offset)),
		Name:   deref(params.Name),
		Sort:   deref(params.Sort),
		Order:  string(deref(params.Order)),
	}
	rows, total, err := h.svc.ListKeys(r.Context(), currentUserID(r), q)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	out := make([]Key, 0, len(rows))
	for _, k := range rows {
		out = append(out, toAPIKey(k))
	}
	writeJSON(w, http.StatusOK, KeyListResponse{Total: total, Rows: out})
}

// PostUserKeys 创建 key（组可选性校验：public 或已授予 private；明文长期回显，
// ServerInterface）。
func (h *UserAPI) PostUserKeys(w http.ResponseWriter, r *http.Request) {
	var in KeyCreate
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	k, _, err := h.svc.CreateKey(r.Context(), currentUserID(r), in.Name, in.GroupId,
		deref(in.MaxConcurrency), deref(in.Quota))
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIKey(k))
}

// GetUserKeysId key 详情（仅本人；他人 key → 404，ServerInterface）。
func (h *UserAPI) GetUserKeysId(w http.ResponseWriter, r *http.Request, id int64) {
	k, err := h.svc.GetKey(r.Context(), currentUserID(r), id)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIKey(k))
}

// PutUserKeysId 更新 key（name/status/max_concurrency/quota；仅本人，
// ServerInterface）。
func (h *UserAPI) PutUserKeysId(w http.ResponseWriter, r *http.Request, id int64) {
	var in KeyUpdate
	if err := decode(r, &in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	var status *domain.KeyStatus
	if in.Status != nil {
		st := domain.KeyStatus(*in.Status)
		status = &st
	}
	k, err := h.svc.UpdateKey(r.Context(), currentUserID(r), id, in.Name, status,
		in.MaxConcurrency, in.Quota)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIKey(k))
}

// DeleteUserKeysId 删除 key（仅本人；Auth 快照增量移除——立即失效，
// ServerInterface）。
func (h *UserAPI) DeleteUserKeysId(w http.ResponseWriter, r *http.Request, id int64) {
	if err := h.svc.DeleteKey(r.Context(), currentUserID(r), id); err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, DeletedResponse{Deleted: true})
}

// PostUserKeysIdRotate 轮换 key（仅本人；新明文生效，旧 key 立即失效，
// ServerInterface）。
func (h *UserAPI) PostUserKeysIdRotate(w http.ResponseWriter, r *http.Request, id int64) {
	_, k, err := h.svc.RotateKey(r.Context(), currentUserID(r), id)
	if err != nil {
		writeServiceErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, toAPIKey(k))
}
