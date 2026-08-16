// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

import (
	"net/http"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
)

// PostUserAuthRegister 注册（signup_enabled 开关检查在 service；注册即登录：
// 直接签发 JWT 返回，ServerInterface）。
func (h *UserAPI) PostUserAuthRegister(w http.ResponseWriter, r *http.Request) {
	var in UserAuthRegister
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	u, err := h.svc.RegisterUser(r.Context(), in.Email, in.Password)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	h.writeAuthResponse(w, u)
}

// PostUserAuthLogin 登录：bcrypt 校验 → JWT（ServerInterface）。
func (h *UserAPI) PostUserAuthLogin(w http.ResponseWriter, r *http.Request) {
	var in UserAuthLogin
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	u, err := h.svc.LoginUser(r.Context(), in.Email, in.Password)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	h.writeAuthResponse(w, u)
}

// GetUserAuthMe 当前用户信息（JWT 已由 Router 中间件验证，ServerInterface）。
func (h *UserAPI) GetUserAuthMe(w http.ResponseWriter, r *http.Request) {
	u, err := h.svc.GetUserMe(r.Context(), currentUserID(r))
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIUser(u))
}

// PostUserAuthChangePassword 修改密码：旧密码校验复用登录语义（失败 401 同
// 登录文案防枚举）+ 新密码非空/≤72 字节（非法 400）→ bcrypt 重哈希落库。
// **不撤销既有 JWT**（无状态 token 无撤销机制——新密码下次登录生效，
// ServerInterface）。
func (h *UserAPI) PostUserAuthChangePassword(w http.ResponseWriter, r *http.Request) {
	var in UserAuthChangePassword
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if err := h.svc.ChangePassword(r.Context(), currentUserID(r), in.OldPassword, in.NewPassword); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, ChangePasswordResponse{Updated: true})
}

func (h *UserAPI) writeAuthResponse(w http.ResponseWriter, u *domain.User) {
	token, err := h.iss.Issue(u.ID, u.Email, string(u.Role))
	if err != nil {
		httpface.WriteErr(w, http.StatusInternalServerError, "token issuance failed")
		return
	}
	httpface.WriteJSON(w, http.StatusOK, UserAuthResponse{Token: token, User: toAPIUser(u)})
}

// currentUserID 取 RequireJWT 写入 context 的 claims.UserID（JWT 保护端点用）。
func currentUserID(r *http.Request) int64 {
	if claims, ok := auth.ClaimsFrom(r.Context()); ok {
		return claims.UserID
	}
	return 0
}
