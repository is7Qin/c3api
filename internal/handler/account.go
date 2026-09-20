// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/repository"
)

// PostAccounts 创建账号（ServerInterface）。创建体是唯一字段模型的别名
// （AccountCreate = AccountConfigPatch + required[name,template_id]），直接复用
// 同一个三态转换；Name/TemplateID 必填由 service 收口判定。
func (h *AdminAPI) PostAccounts(w http.ResponseWriter, r *http.Request) {
	var in AccountConfigPatch
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	p, err := accountPatchFromBody(&in)
	if err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	created, err := h.svc.CreateAccount(r.Context(), p)
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
		Enabled:    params.Enabled,
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

// PatchAccountsId 账号配置的部分更新（ServerInterface）。三态见
// accountPatchFromBody；If-Match 可选（缺席 = 不做前置条件检查），陈旧 → 412，
// 语法非法 → 400。
func (h *AdminAPI) PatchAccountsId(w http.ResponseWriter, r *http.Request, id int64, params PatchAccountsIdParams) {
	var in AccountConfigPatch
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	p, err := accountPatchFromBody(&in)
	if err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	expected, err := parseIfMatch(params.IfMatch)
	if err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, err.Error())
		return
	}
	acc, err := h.svc.PatchAccount(r.Context(), id, p, expected)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIAccount(acc))
}

// parseIfMatch 解析 If-Match 前置条件：nil = 缺席（不做前置条件检查）。接受裸
// 整数或一对双引号包裹（`"12"`）；拒绝 W/ 弱校验、多值（逗号）、`*` 与非整数。
func parseIfMatch(raw *string) (*int64, error) {
	if raw == nil {
		return nil, nil
	}
	v := strings.TrimSpace(*raw)
	if strings.HasPrefix(v, `"`) && strings.HasSuffix(v, `"`) && len(v) >= 2 {
		v = v[1 : len(v)-1]
	}
	if v == "" || v == "*" || strings.HasPrefix(v, "W/") || strings.Contains(v, ",") {
		return nil, fmt.Errorf("invalid If-Match %q", *raw)
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid If-Match %q", *raw)
	}
	return &n, nil
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
// 失效由 service invalidate 完成，ServerInterface）。响应携带每账号的新配置
// 代际。
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
	revs, err := h.svc.UpdateAccountsBatch(r.Context(), ids, p)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	items := make([]AccountRevision, 0, len(revs))
	for _, rev := range revs {
		items = append(items, AccountRevision{AccountId: rev.AccountID, LifecycleRevision: rev.LifecycleRevision})
	}
	httpface.WriteJSON(w, http.StatusOK, AccountBatchUpdateResponse{Updated: len(revs), Items: items})
}

// PostAccountsIdRecover 失效恢复（fenced CAS）：清失效三字段 + revision +1 →
// 新代际置 PROBING；stale → 409（ServerInterface）。
func (h *AdminAPI) PostAccountsIdRecover(w http.ResponseWriter, r *http.Request, id int64) {
	var in AccountRecoverBody
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	acc, err := h.svc.RecoverAccount(r.Context(), id, in.ExpectedRevision)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIAccount(acc))
}

// accountPatchFromBody 生成类型补丁 → repo 补丁：唯一的**线格式投影**
// （tri-state → 内部编码），只做类型转换不做语义判定（字段 bounds/域名语法/
// group 元素规则的唯一权威是 service.validateAccountPatch）。
//
// 按字段类分派（nullable.Nullable[T] 的 IsSpecified/IsNull/MustGet）：
//   - 不可空标量（name/template_id/upstream_key/max_concurrency/enabled/
//     upstream_cost_multiplier）：缺席 → nil（不变）；显式 null → 400（具名字段
//     错误）；有值 → &v（倍率走 normalToMult 换算为 basis points）。
//   - 可空标量（base_url/cache_domain）：缺席 → nil（不变）；显式 null → &""
//     （repo 层"清空"编码：落 NULL）；"" → 400；非空 → &v。
//   - group_ids：原样透传（nil = 不变，[] = 清空，列表 = 替换）。
func accountPatchFromBody(f *AccountConfigPatch) (repository.AccountPatch, error) {
	var p repository.AccountPatch
	if v, err := requiredScalar(f.Name, "name"); err != nil {
		return p, err
	} else {
		p.Name = v
	}
	if v, err := requiredScalar(f.TemplateId, "template_id"); err != nil {
		return p, err
	} else {
		p.TemplateID = v
	}
	if v, err := requiredScalar(f.UpstreamKey, "upstream_key"); err != nil {
		return p, err
	} else {
		p.UpstreamKey = v
	}
	if v, err := requiredScalar(f.MaxConcurrency, "max_concurrency"); err != nil {
		return p, err
	} else {
		p.MaxConcurrency = v
	}
	if v, err := requiredScalar(f.Enabled, "enabled"); err != nil {
		return p, err
	} else {
		p.Enabled = v
	}
	if f.UpstreamCostMultiplier.IsSpecified() {
		if f.UpstreamCostMultiplier.IsNull() {
			return p, errors.New("upstream_cost_multiplier must not be null")
		}
		bp := normalToMult(f.UpstreamCostMultiplier.MustGet())
		p.UpstreamCostMultiplierBp = &bp
	}
	if v, err := nullableScalar(f.BaseUrl, "base_url"); err != nil {
		return p, err
	} else {
		p.BaseURL = v
	}
	if v, err := nullableScalar(f.CacheDomain, "cache_domain"); err != nil {
		return p, err
	} else {
		p.CacheDomain = v
	}
	p.GroupIDs = f.GroupIds
	return p, nil
}

// requiredScalar 不可空标量的三态投影：缺席 → nil；显式 null → 400；有值 → &v。
func requiredScalar[T any](f interface {
	IsSpecified() bool
	IsNull() bool
	MustGet() T
}, field string) (*T, error) {
	if !f.IsSpecified() {
		return nil, nil
	}
	if f.IsNull() {
		return nil, fmt.Errorf("%s must not be null", field)
	}
	v := f.MustGet()
	return &v, nil
}

// nullableScalar 可空标量的三态投影：缺席 → nil；显式 null → &""（repo 清空
// 编码）；"" → 400；非空 → &v。
func nullableScalar(f interface {
	IsSpecified() bool
	IsNull() bool
	MustGet() string
}, field string) (*string, error) {
	if !f.IsSpecified() {
		return nil, nil
	}
	if f.IsNull() {
		empty := ""
		return &empty, nil
	}
	v := f.MustGet()
	if v == "" {
		return nil, fmt.Errorf("%s must not be empty", field)
	}
	return &v, nil
}
