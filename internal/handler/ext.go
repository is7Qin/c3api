// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/httpface"
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

// decodeLenientUseNumber 与 decodeLenient 同语义（宽松：忽略未知字段），但开启
// json.Decoder.UseNumber。**只给 template ext 端点的 request_patch 用**（端点级，
// account ext 端点行为不变）：生成的契约类型里 set 只能是 map[string]interface{}，
// 默认解码会把大整数与 1e18 变成 float64，再 marshal 回字节就丢精度（spec §5.4
// 评审 momus I5 / oracle 附录 C）。这里保留字面量，交给 domain.ParseRequestPatch
// 做唯一 wire 校验。
func decodeLenientUseNumber(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.UseNumber()
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
	out, err := toAPITemplateExt(e)
	if err != nil {
		// 回显编码失败属服务端不变量被破坏（CanonicalBytes 产物不可解码），
		// 不泄露细节，按既有 500 口径写 internal error。
		httpface.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpface.WriteJSON(w, http.StatusOK, out)
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
	if err := decodeLenientUseNumber(r, &in); err != nil {
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
	// request_patch：先把生成类型 marshal 回字节（UseNumber 下 json.Number 按原字面量
	// 编码，大整数不丢精度），再交给**唯一** wire 校验入口 domain.ParseRequestPatch。
	// handler 不复制第二套判据；缺省 / null ⇒ domain nil = 全列替换的清空语义
	// （与 upstream_paths 一致）。失败 400 且消息不回显任何 header/body 值。
	if in.RequestPatch != nil {
		raw, err := json.Marshal(in.RequestPatch)
		if err != nil {
			httpface.WriteErr(w, http.StatusBadRequest, "invalid request_patch: "+err.Error())
			return
		}
		patch, err := domain.ParseRequestPatch(raw)
		if err != nil {
			httpface.WriteErr(w, http.StatusBadRequest, "invalid request_patch: "+err.Error())
			return
		}
		e.RequestPatch = patch
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
	out, err := toAPITemplateExt(saved)
	if err != nil {
		httpface.WriteErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	httpface.WriteJSON(w, http.StatusOK, out)
}

// toAPITemplateExt 模板 ext 领域对象 → 契约类型（template_id 只读，响应带）。
func toAPITemplateExt(e *domain.TemplateExt) (TemplateExt, error) {
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
	patch, err := toAPIRequestPatch(e.RequestPatch)
	if err != nil {
		return TemplateExt{}, err
	}
	out.RequestPatch = patch
	return out, nil
}

// toAPIRequestPatch domain.RequestPatch → 契约类型（GET/PUT 回显）。
//
// 用 json.Decoder.UseNumber 把 CanonicalBytes 解到生成类型：set 值走
// map[string]interface{}，只有 json.Number 能保住 9007199254740993 / 1e18 的字面量
// （默认 float64 会把前者变成 9007199254740992；spec §5.4/§11.11）。这里解的是
// 我们自己产出的规范形，结构上必定可解码；失败仅表示不变量被破坏，向上抛 500。
func toAPIRequestPatch(p *domain.RequestPatch) (*TemplateRequestPatch, error) {
	if p.IsEmpty() {
		return nil, nil
	}
	var out TemplateRequestPatch
	dec := json.NewDecoder(bytes.NewReader(p.CanonicalBytes()))
	dec.UseNumber()
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("handler: decode canonical request_patch: %w", err)
	}
	return &out, nil
}
