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
// 契约：请求体只有 model/format（不含 URL/host/路径——出站目标只能来自存储
// 配置）；HTTP 200 恒为探测结论（success=false 也是 200），400 参数/模型/格式/
// 出口绑定不合法，404 账号不存在/越域，500 内部故障或探测组件未装配。
func (h *AdminAPI) PostAccountsIdTest(w http.ResponseWriter, r *http.Request, id int64) {
	var in AccountTestRequest
	if err := httpface.Decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	res, err := h.svc.ProbeAccount(r.Context(), id, service.AccountProbeRequest{
		Model:  in.Model,
		Format: domain.RequestFormat(in.Format),
	})
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
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
	})
}
