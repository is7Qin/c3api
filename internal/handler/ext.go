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
// 别处的凭据列（codex_oauth_token/codex_pat_key 等一律在 account_ext）——忽略不产生配置
// （既有契约，TestTemplatesIdExt 锁定，行为不随严格化改变）。
func decodeLenient(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	return dec.Decode(v)
}

// GetTemplatesIdExt 模板类型化扩展（编辑回显；生态三类型与 custom 模板有 ext
// 行，ServerInterface）。模板缺 id / 无 ext 行 → 404。
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
// 面：可空列 strip_image_tools（公共能力开关）+ custom 的 upstream_paths——
// 凭据（oauth/pat）一律在账号级 account_ext。
//
// `?confirm=` 是出站传输降级门（spec §4.3）的确认令牌：本层只读、只透传，
// **不判 409**——判定（含令牌字面量比对与 WARN）只在 service 一处。
func (h *AdminAPI) PutTemplatesIdExt(w http.ResponseWriter, r *http.Request, id int64, params PutTemplatesIdExtParams) {
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
	if in.UpstreamPaths != nil {
		e.UpstreamPaths = *in.UpstreamPaths // nil = 未配置（清列由 service/repo 语义决定）
	}
	// 字段灌入必须显式做：decodeLenient 宽松解码只会丢未知字段，不会替我们把
	// 契约字段搬进领域对象——漏掉这行则 PUT 恒落 ""，409 门与 WARN 永不可达。
	// nil（缺省或 JSON null）⇒ 零值 "" ⇒ 全列替换的"清空"；**不** trim/归一/折叠
	// 大小写——脏值原样交给 service 去 400（归一比较会把 400 变成静默放行）。
	if in.CodexResponsesTransport != nil {
		e.CodexResponsesTransport = domain.CodexResponsesTransport(*in.CodexResponsesTransport)
	}
	// confirm 原样透传（空串 = 未确认）：降级门的判定与 WARN 都在 service 一侧。
	// 生成接口的参数绑定已把确认令牌解到 params.Confirm（可选 query）。
	confirm := ""
	if params.Confirm != nil {
		confirm = *params.Confirm
	}
	saved, err := h.svc.UpsertTemplateExt(r.Context(), e, confirm)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPITemplateExt(saved))
}

// toAPITemplateExt 模板 ext 领域对象 → 契约类型（template_id 只读，响应带）。
func toAPITemplateExt(e *domain.TemplateExt) TemplateExt {
	out := TemplateExt{
		TemplateId:      &e.TemplateID,
		CredentialType:  TemplateExtCredentialType(e.CredentialType),
		StripImageTools: e.StripImageTools,
	}
	// 出站传输**恒回显**（含 ""）：与 strip_image_tools 的三态不同，"" 本身
	// 就是有意义的取值（默认面），省略会让 web 无法区分"没读取"与"未配置"。
	transport := string(e.CodexResponsesTransport)
	out.CodexResponsesTransport = &transport
	if e.UpstreamPaths != nil {
		paths := e.UpstreamPaths
		out.UpstreamPaths = &paths
	}
	return out
}
