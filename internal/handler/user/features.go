// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"net/http"

	"github.com/is7qin/c3api/internal/handler/httpface"
)

// GetUserFeatures 部署级功能开关（启动配置的只读投影，改动需重启）。控制台据此
// 决定是否展示供应商入口与字段；内容只含部署常量，无任何用户/租户数据。鉴权与
// /api/user/* 其余非公开路径相同（RequireIdentity：JWT 或 mk-）。
func (h *UserAPI) GetUserFeatures(w http.ResponseWriter, r *http.Request) {
	httpface.WriteJSON(w, http.StatusOK, h.features)
}
