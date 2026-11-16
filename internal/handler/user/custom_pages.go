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

// customPagesConfigInvalid 配置损坏时的固定错误文案（spec 2026-11-16 §4.4）。
// 刻意不拼接 service 错误的 %w 详情：那条详情里带条目下标与配置片段（甚至 url
// 原文），而本端点是任意登录用户都能打的——回显配置内容等于把管理员配错的东西
// 广播给所有人。可判别性由固定文案承担，排障细节留在管理面/日志。
const customPagesConfigInvalid = "custom pages config is invalid"

// GetUserCustomPages 当前主体可见的自定义嵌入页条目（ServerInterface）。
//
// 权限边界（§4.4）：platform_admin 看到全部**启用**项（管理台要能进 user 条目）；
// 其他角色（含 supplier）只看 enabled && visibility=user。过滤只此一处——前端再
// 过滤一遍就成了第二份权限判定，两边迟早分叉。
//
// 配置损坏 → 500 固定文案，不静默降级为空数组：静默等于配置坏了没人知道，管理员
// 只会看到侧栏入口"莫名消失"。空列表（未配置）仍是正常的 200 + `"Pages": []`。
func (h *UserAPI) GetUserCustomPages(w http.ResponseWriter, r *http.Request) {
	pages, err := h.svc.CustomPages()
	if err != nil {
		httpface.WriteErr(w, http.StatusInternalServerError, customPagesConfigInvalid)
		return
	}
	platformAdmin := h.isPlatformAdmin(r)
	// 预分配 + 空切片（不是 nil）：空列表必须序列化成 `[]`，否则前端拿到 null
	// 会在 .map/.length 上炸开，而"没配置"是最常见的一态。
	out := make([]CustomPage, 0, len(pages))
	for _, page := range pages {
		if !customPageVisibleTo(page, platformAdmin) {
			continue
		}
		out = append(out, CustomPage{
			ID:    page.ID,
			Label: page.Label,
			URL:   page.URL,
			// Enabled 不下发（D10）：能走到这里的都已启用，字段本身没有信息量，
			// 而"下发禁用了什么"会把管理员的草稿配置泄露给普通用户。
			Visibility: CustomPageVisibility(page.Visibility),
			// Icon 在契约里是开集 string：Go 侧是枚举，新增图标不该让旧前端
			// 编译失败（schema 刻意不给 enum，同 payment kind）。
			Icon: string(page.Icon),
		})
	}
	httpface.WriteJSON(w, http.StatusOK, CustomPagesResponse{Pages: out})
}

// customPageVisibleTo 单条可见性判定（唯一的过滤实现）。拆成函数是为了让两侧
// 语义能并排看：管理员看"启用"，其他角色看"启用且面向用户台"。
func customPageVisibleTo(page domain.CustomPage, platformAdmin bool) bool {
	if platformAdmin {
		return page.Enabled
	}
	return page.IsVisibleToUser()
}

// isPlatformAdmin 当前请求主体是否 platform_admin。判定只认**鉴权快照**
// （UsersSnapshot(claims.UserID).Role + Status==active），不认 claims.Role：
// /user 面的 claims 来自 JWT，而 UpdateUser 改角色只 upsertUserSnapshot、不 bump
// token_version，用 claims 会让被降权的旧 JWT 继续走 platform_admin 分支，把
// visibility=admin 条目的 id/label/URL 下发给已降权用户（§8-S5 破防，spec §19-1）。
//
// 任何一种"取不到"都按非管理员处理（fail-closed）：claims 缺失、provider 未装配
// （nil）、快照查不到。本端点虽在鉴权中间件之后，但"没身份就当普通用户"永远比
// "没身份就放宽"安全——与 /admin 面 fail-closed 纪律一致。
func (h *UserAPI) isPlatformAdmin(r *http.Request) bool {
	claims, ok := auth.ClaimsFrom(r.Context())
	if !ok || h.users == nil {
		return false
	}
	sn, ok := h.users.UserSnapshot(claims.UserID)
	if !ok {
		return false
	}
	// 快照里 disabled 的用户不该再拿到任何条目；RequireIdentity 已拦一道，这里不依赖它
	// （判定函数要能独立成立，被测时不靠中间件兜底）。
	return sn.Role == domain.RolePlatformAdmin && sn.Status == domain.UserStatusActive
}
