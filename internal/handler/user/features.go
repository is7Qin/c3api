// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"net/http"

	"github.com/is7qin/c3api/internal/handler/httpface"
)

// GetUserFeatures 功能开关。控制台据此决定是否展示供应商与支付网关的入口和页面；
// 内容只含部署/系统级开关，无任何用户/租户数据。鉴权与 /api/user/* 其余非公开路径
// 相同（RequireIdentity：JWT 或 mk-）。
//
//   - supplier：启动配置的只读投影（构造期注入，改动需重启）。
//   - payment：运行时设置 payment.enabled，每次请求读内存设置快照（不读库），
//     管理面改设置即时生效；构造期传入的 Payment 一律被覆盖。
func (h *UserAPI) GetUserFeatures(w http.ResponseWriter, r *http.Request) {
	f := h.features
	f.Payment = h.svc.PaymentEnabled()
	httpface.WriteJSON(w, http.StatusOK, f)
}
