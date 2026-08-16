// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"errors"
	"net/http"
	"strings"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/repository"
)

// accountFromBody 生成类型 body → 领域对象（create/update 共用；GroupIDs
// nil = 不设置/不变，非 nil = 替换语义含空数组清空）。
func accountFromBody(in AccountCreate) *domain.Account {
	a := &domain.Account{
		Name:           in.Name,
		TemplateID:     in.TemplateId,
		UpstreamKey:    in.UpstreamKey,
		Status:         domain.AccountStatus(deref(in.Status)),
		Weight:         deref(in.Weight),
		MaxConcurrency: deref(in.MaxConcurrency),
		GroupIDs:       in.GroupIds,
	}
	// base_url 空串归一 nil（create/update 路径 "" 与 null 合并为「继承
	// 模板」——防 "" 落库产生存储漂移，DB 恒 nil|非空两种形态）。
	if in.BaseUrl != nil && *in.BaseUrl != "" {
		a.BaseURL = in.BaseUrl
	}
	return a
}

// PostAccounts 创建账号（ServerInterface）。
func (h *AdminAPI) PostAccounts(w http.ResponseWriter, r *http.Request) {
	var in AccountCreate
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	created, err := h.svc.CreateAccount(r.Context(), accountFromBody(in))
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIAccount(created))
}

// GetAccounts 账号列表（分页/筛选/排序，含运行时视图，ServerInterface）。
func (h *AdminAPI) GetAccounts(w http.ResponseWriter, r *http.Request, params GetAccountsParams) {
	q := repository.ListQuery{
		Limit:      httpface.ClampLimit(int(deref(params.Limit))),
		Offset:     int(deref(params.Offset)),
		Name:       deref(params.Name),
		Sort:       deref(params.Sort),
		Order:      string(deref(params.Order)),
		TemplateID: deref(params.TemplateId),
	}
	if params.Status != nil && *params.Status != "" {
		sts := strings.Split(*params.Status, ",")
		for _, s := range sts {
			if !validAccountStatus(s) {
				httpface.WriteErr(w, http.StatusBadRequest, "invalid status "+s)
				return
			}
		}
		q.StatusList = sts
	}
	rows, total, err := h.svc.ListAccountViews(r.Context(), q)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]AccountView, 0, len(rows))
	for _, v := range rows {
		out = append(out, toAPIAccountView(v))
	}
	httpface.WriteJSON(w, http.StatusOK, AccountListResponse{Total: total, Rows: out})
}

// validAccountStatus 校验 status 多值参数（逗号分隔）的枚举值
// （active/unhealthy/429/disabled）。openapi 的 status 参数是纯 string
// （多值无法用 enum），生成类型不校验；必须在 handler 显式校验，
// 否则非法值落到 repo 兜底返回裸 error → 500（Task 1→2 handoff 硬性要求）。
func validAccountStatus(s string) bool {
	switch domain.AccountStatus(s) {
	case domain.StatusActive, domain.StatusUnhealthy, domain.Status429, domain.StatusDisabled:
		return true
	}
	return false
}

// GetAccountsId 账号详情（ServerInterface）。
func (h *AdminAPI) GetAccountsId(w http.ResponseWriter, r *http.Request, id int64) {
	acc, err := h.svc.GetAccount(r.Context(), id)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIAccount(acc))
}

// GetAccountsIdGroups 账号的全部分组 id（编辑回显；账号缺 id → 404，
// ServerInterface）。
func (h *AdminAPI) GetAccountsIdGroups(w http.ResponseWriter, r *http.Request, id int64) {
	ids, err := h.svc.GetAccountGroups(r.Context(), id)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, AccountGroupsResponse{GroupIds: ids})
}

// PutAccountsId 全量更新账号（ServerInterface；group_ids 缺省 = 分组不变，
// 空数组 = 清空）。
func (h *AdminAPI) PutAccountsId(w http.ResponseWriter, r *http.Request, id int64) {
	var in AccountCreate
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	acc := accountFromBody(in)
	acc.ID = id
	updated, err := h.svc.UpdateAccount(r.Context(), acc)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIAccount(updated))
}

// DeleteAccountsId 删除账号（ServerInterface）。
func (h *AdminAPI) DeleteAccountsId(w http.ResponseWriter, r *http.Request, id int64) {
	if err := h.svc.DeleteAccount(r.Context(), id); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, DeletedResponse{Deleted: true})
}

// PostAccountsBatchDelete 批量删除账号（事务，全成或全败；删除后调度快照
// 失效由 service invalidate 完成，ServerInterface）。
func (h *AdminAPI) PostAccountsBatchDelete(w http.ResponseWriter, r *http.Request) {
	var in BatchDeleteBody
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	ids, err := normalizeIDs(in.Ids)
	if err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.svc.DeleteAccountsBatch(r.Context(), ids); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, BatchDeleteResponse{Deleted: len(ids)})
}

// PostAccountsBatchUpdate 批量更新账号（fields 任意子集；更新后调度快照
// 失效由 service invalidate 完成，ServerInterface）。
func (h *AdminAPI) PostAccountsBatchUpdate(w http.ResponseWriter, r *http.Request) {
	var in BatchUpdateAccountsBody
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	ids, err := normalizeIDs(in.Ids)
	if err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	p, err := accountPatchFromBody(&in.Fields)
	if err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.svc.UpdateAccountsBatch(r.Context(), ids, p); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, BatchUpdateResponse{Updated: len(ids)})
}

// accountPatchFromBody 生成类型 fields → repo patch（nil 字段 = 不更新；
// GroupIDs nil = 不变，非 nil（含空数组） = 替换/清空）。
// 契约 status 枚举不参与解码校验（与列表参数一致），此处显式校验；
// 空 fields（无任何字段）视为非法输入——判定用 == nil（而非 len），
// group_ids: [] 算「提供」。
func accountPatchFromBody(f *AccountPatch) (repository.AccountPatch, error) {
	if f.Status != nil && !validAccountStatus(string(*f.Status)) {
		return repository.AccountPatch{}, errors.New("invalid status " + string(*f.Status))
	}
	p := repository.AccountPatch{
		Name:           f.Name,
		TemplateID:     f.TemplateId,
		UpstreamKey:    f.UpstreamKey,
		BaseURL:        f.BaseUrl, // 透传不归一：批量 "" = 清空语义（与 create 路径归一语义分写）
		Status:         (*domain.AccountStatus)(f.Status),
		Weight:         f.Weight,
		MaxConcurrency: f.MaxConcurrency,
		GroupIDs:       f.GroupIds,
	}
	if p.Name == nil && p.TemplateID == nil && p.UpstreamKey == nil &&
		p.BaseURL == nil && p.Status == nil && p.Weight == nil &&
		p.MaxConcurrency == nil && p.GroupIDs == nil {
		return repository.AccountPatch{}, errors.New("fields must contain at least one field")
	}
	return p, nil
}
