// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"net/http"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
)

// —— 模板类型化扩展（template_ext 1:1；通用框架——codex 专属账号 ext 见
// ext_codex.go） ——

// decodeLenient 宽松解码（ext 端点专用，handler.go 的 decode 已严格化——spec
// 2026-08-17 边界收敛）：忽略未知字段。ext 契约演进时客户端可能携带已迁移到
// 别处的凭据列（oauth_token/pat 等一律在 account_ext）——忽略不产生配置
// （既有契约，TestTemplatesIdExt 锁定，行为不随严格化改变）。
func decodeLenient(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	return dec.Decode(v)
}

// GetTemplatesIdExt 模板类型化扩展（编辑回显；仅生态三类型模板有 ext 行，
// ServerInterface）。模板缺 id / 无 ext 行 → 404。
func (h *AdminAPI) GetTemplatesIdExt(w http.ResponseWriter, r *http.Request, id int64) {
	e, err := h.svc.GetTemplateExt(r.Context(), id)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPITemplateExt(e))
}

// PutTemplatesIdExt 幂等写入模板类型化扩展（Create/Update 合一；全列更新含
// NULL 清空，ServerInterface）。credential_type 必填；类型一致性（ext 行类型
// 必须 == 父模板类型）与模板缺 id → 400/404（service 校验）。模板是共享配置
// 面：唯一可配置列 strip_image_tools（三类型公共能力开关）——凭据（oauth/pat）
// 一律在账号级 account_ext。
func (h *AdminAPI) PutTemplatesIdExt(w http.ResponseWriter, r *http.Request, id int64) {
	var in TemplateExt
	if err := decodeLenient(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	e := &domain.TemplateExt{
		TemplateID:      id, // 路径 {id} 为准（请求体 template_id 忽略）
		CredentialType:  credential.Type(in.CredentialType),
		StripImageTools: in.StripImageTools,
	}
	saved, err := h.svc.UpsertTemplateExt(r.Context(), e)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPITemplateExt(saved))
}

// toAPITemplateExt 模板 ext 领域对象 → 契约类型（template_id 只读，响应带）。
func toAPITemplateExt(e *domain.TemplateExt) TemplateExt {
	return TemplateExt{
		TemplateId:      &e.TemplateID,
		CredentialType:  TemplateExtCredentialType(e.CredentialType),
		StripImageTools: e.StripImageTools,
	}
}
