// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"

	"github.com/is7qin/c3api/internal/handler/httpface"
)

// —— codex 设备码登录（发起 → 完成；无服务端会话）——
//
// 契约层只做结构解码 + 响应组装；协议在 sdkbridge、编排与落库在 service。
// 管理面与供应商面共用同一对实现（供应商面 wrapper 类型化直调，作用域经 ctx
// 注入），因此两面的字段语义、幂等键、配置缺省完全一致。
//
// **状态在前端**：发起响应自带完成所需凭据；完成端点由用户在浏览器授权后触发，
// 网关当场轮询一次。没有 Redis/进程内会话，任意实例、任意重启都成立。

// PostAccountsCodexDeviceLogin 发起设备码登录（ServerInterface）：向上游申请
// 用户码，返回展示面与完成凭据。
func (h *AdminAPI) PostAccountsCodexDeviceLogin(w http.ResponseWriter, r *http.Request) {
	ch, err := h.svc.StartCodexOAuthDeviceLogin(r.Context())
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, CodexDeviceLoginStart{
		DeviceAuthId:     ch.DeviceAuthID,
		UserCode:         ch.UserCode,
		VerificationUri:  ch.VerificationURL,
		IntervalSeconds:  ch.IntervalSeconds,
		ExpiresInSeconds: ch.ExpiresInSeconds,
	})
}

// PostAccountsCodexDeviceLoginComplete 完成设备码登录（ServerInterface）：单次
// 轮询；pending 时前端可再次调用，authorized 当场落库并返回账号定位。
func (h *AdminAPI) PostAccountsCodexDeviceLoginComplete(w http.ResponseWriter, r *http.Request) {
	var in CodexDeviceLoginCompleteBody
	if err := httpface.Decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	res, err := h.svc.CompleteCodexOAuthDeviceLogin(r.Context(), in.DeviceAuthId, in.UserCode, in.TemplateId, in.GroupId)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := CodexDeviceLoginResult{Status: CodexDeviceLoginResultStatus(res.Status)}
	if res.AccountID > 0 {
		id := res.AccountID
		imported := res.Imported
		out.AccountId = &id
		out.Imported = &imported
		if res.AccountName != "" {
			name := res.AccountName
			out.AccountName = &name
		}
		if res.Email != "" {
			email := res.Email
			out.Email = &email
		}
		if res.CodexAccountID != "" {
			codexID := res.CodexAccountID
			out.CodexAccountId = &codexID
		}
	}
	httpface.WriteJSON(w, http.StatusOK, out)
}
