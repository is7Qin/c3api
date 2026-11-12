// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/service"
)

// PostAccountsIdTest 单账号单模型探测（ServerInterface；account-model-test）。
//
// 契约：请求体只有 model/format/input/image_mode/image_url（不含 URL/host/路径
// 以外的目标字段——出站目标只能来自存储配置）；HTTP 200 恒为探测结论
// （success=false 也是 200），400 参数/模型/格式/image 参数/出口绑定不合法，
// 404 账号不存在/越域，500 内部故障或探测组件未装配。
//
// 请求体上限为 domain.ProbeMaxRequestBytes（8 MiB；其他管理端点仍 1 MiB）；
// input/image_mode/image_url 用 nullable.Nullable 的三态（缺席/null/显式值）
// 投影，显式空串与缺席不是同一输入。
func (h *AdminAPI) PostAccountsIdTest(w http.ResponseWriter, r *http.Request, id int64) {
	var in AccountTestRequest
	if err := httpface.DecodeLimit(r, &in, domain.ProbeMaxRequestBytes); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	res, err := h.svc.ProbeAccount(r.Context(), id, service.AccountProbeRequest{
		Model:     in.Model,
		Format:    domain.RequestFormat(in.Format),
		Input:     probeOptionalString(in.Input),
		ImageMode: probeOptionalString(in.ImageMode),
		ImageURL:  probeOptionalString(in.ImageUrl),
	})
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	previews := res.ImagePreviews
	if previews == nil {
		previews = []string{}
	}
	httpface.WriteJSON(w, http.StatusOK, AccountTestResult{
		Success:        res.Success,
		LatencyMs:      res.LatencyMS,
		StatusCode:     res.StatusCode,
		Format:         string(res.Format),
		RequestedModel: res.RequestedModel,
		UpstreamModel:  res.UpstreamModel,
		Error:          res.Error,
		Detail:         res.Detail,
		OutputPreview:  res.OutputPreview,
		ImagePreviews:  previews,
	})
}

// probeOptionalString 把 nullable.Nullable 的三态投影为 *string：缺席与显式
// null 同为 nil（探测语义中二者同义），显式值（含空串）返回非 nil 指针——
// service 才能区分「缺省」与「显式空串」并给出各自的 400/默认值。
func probeOptionalString(f interface {
	IsSpecified() bool
	IsNull() bool
	MustGet() string
}) *string {
	if !f.IsSpecified() || f.IsNull() {
		return nil
	}
	v := f.MustGet()
	return &v
}
