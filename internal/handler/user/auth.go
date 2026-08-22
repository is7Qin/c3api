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
// 直接签发 JWT 返回，ServerInterface）。code 可选：verif=on 时必填（缺→400 sentinel），verif=off 时忽略。
func (h *UserAPI) PostUserAuthRegister(w http.ResponseWriter, r *http.Request) {
	var in UserAuthRegister
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	code := ""
	if in.Code != nil {
		code = *in.Code
	}
	u, err := h.svc.RegisterUserWithCode(r.Context(), in.Email, in.Password, code)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	h.writeAuthResponse(w, u)
}

// PostUserAuthRegisterCode 发送注册验证码（public）。
func (h *UserAPI) PostUserAuthRegisterCode(w http.ResponseWriter, r *http.Request) {
	var in RegisterCodeRequest
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if err := h.svc.SendRegisterCode(r.Context(), in.Email); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, SentResponse{Sent: true})
}

// PostUserAuthForgotPassword 忘记密码发码（恒 200 反枚举）。
func (h *UserAPI) PostUserAuthForgotPassword(w http.ResponseWriter, r *http.Request) {
	var in ForgotPasswordRequest
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	_ = h.svc.SendForgotPasswordCode(r.Context(), in.Email)
	httpface.WriteJSON(w, http.StatusOK, SentResponse{Sent: true})
}

// PostUserAuthResetPassword 重置密码（验证码校验→更新密码；不撤销 JWT）。
func (h *UserAPI) PostUserAuthResetPassword(w http.ResponseWriter, r *http.Request) {
	var in ResetPasswordRequest
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	if err := h.svc.ResetPassword(r.Context(), in.Email, in.Code, in.NewPassword); err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, ChangePasswordResponse{Updated: true})
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
