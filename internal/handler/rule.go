// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/service"
)

// whenToAPI 领域 RuleWhen → 契约 when 对象（json round-trip，与 repo 序列化同法）。
func whenToAPI(w domain.RuleWhen) map[string]any {
	b, _ := json.Marshal(w)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	return m
}

// thenToAPI 领域 RuleThen → 契约 then 对象。
func thenToAPI(t domain.RuleThen) map[string]any {
	b, _ := json.Marshal(t)
	m := map[string]any{}
	_ = json.Unmarshal(b, &m)
	return m
}

func toAPIRule(r *domain.Rule) Rule {
	return Rule{
		ID: r.ID, Name: r.Name, Enabled: r.Enabled, Priority: r.Priority,
		When: whenToAPI(r.When), Then: thenToAPI(r.Then),
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, DeletedAt: r.DeletedAt, // 软删除时间戳（只读字段）
	}
}

// ruleInputFromCreate 契约 RuleCreate → service 入参（enabled 缺省 true）。
func ruleInputFromCreate(in RuleCreate) service.RuleInput {
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	ri := service.RuleInput{Name: in.Name, Enabled: enabled, Priority: in.Priority}
	if in.When != nil {
		ri.When = *in.When
	}
	if in.Then != nil {
		ri.Then = *in.Then
	}
	return ri
}

// CreateRule 创建规则（ServerInterface）。
func (h *AdminAPI) CreateRule(w http.ResponseWriter, r *http.Request) {
	var in RuleCreate
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	created, err := h.svc.CreateRule(r.Context(), ruleInputFromCreate(in))
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusCreated, toAPIRule(created))
}

// ListRules 规则列表（enabled 过滤 + priority 升序，ServerInterface）。
func (h *AdminAPI) ListRules(w http.ResponseWriter, r *http.Request, params ListRulesParams) {
	rows, total, err := h.svc.ListRules(r.Context(), params.Enabled)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]Rule, 0, len(rows))
	for i := range rows {
		out = append(out, toAPIRule(&rows[i]))
	}
	httpface.WriteJSON(w, http.StatusOK, RuleListResponse{Total: total, Rows: out})
}

// UpdateRule 部分更新规则（未提供字段保持原值，ServerInterface）。
func (h *AdminAPI) UpdateRule(w http.ResponseWriter, r *http.Request, id int64) {
	var in RulePatch
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	p := service.RulePatch{Name: in.Name, Enabled: in.Enabled, Priority: in.Priority}
	if in.When != nil {
		p.When = *in.When
	}
	if in.Then != nil {
		p.Then = *in.Then
	}
	updated, err := h.svc.UpdateRule(r.Context(), id, p)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIRule(updated))
}

// DeleteRule 删除规则（ServerInterface；204 No Content）。
func (h *AdminAPI) DeleteRule(w http.ResponseWriter, r *http.Request, id int64) {
	if err := h.svc.DeleteRule(r.Context(), id); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PostRulesBatchDelete 批量删除规则（事务，全成或全败，ServerInterface）。
func (h *AdminAPI) PostRulesBatchDelete(w http.ResponseWriter, r *http.Request) {
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
	if err := h.svc.DeleteRulesBatch(r.Context(), ids); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, BatchDeleteResponse{Deleted: len(ids)})
}
