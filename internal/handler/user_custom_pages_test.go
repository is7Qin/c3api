// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	userapi "github.com/is7qin/c3api/internal/handler/user"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
)

// userRouterDo newTestUserRouter 返回的请求函数签名（helper 参数用它，免得每个
// helper 都抄一遍四参函数类型）。
type userRouterDo = func(method, path, body, token string) *httptest.ResponseRecorder

// customPagesConfig 三条配置，顺序即展示顺序（spec §4.1）：
//
//	0 user-console   enabled, visibility=user   → 两侧都该看到
//	1 admin-console  enabled, visibility=admin  → 只有 admin 看得到
//	2 hidden-console disabled(visibility=user)  → 两侧都不该看到
//
// id 刻意让「配置顺序」≠「按 id 字典序」（admin-console < user-console），
// 否则保序断言在排序实现下也会通过。
const customPagesConfig = `[
  {"id":"user-console","label":"User Console","url":"https://user.example.com/console","visibility":"user","icon":"globe","enabled":true},
  {"id":"admin-console","label":"Admin Console","url":"https://admin.example.com/console","visibility":"admin","icon":"chart","enabled":true},
  {"id":"hidden-console","label":"Hidden Console","url":"https://hidden.example.com/","visibility":"user","icon":"book","enabled":false}
]`

// customPagesConfigUserDisabled 同 customPagesConfig，但把 user-console 停用
// （模拟管理面再保存一次：只有目标条目的 enabled 变了，顺序不动）。
const customPagesConfigUserDisabled = `[
  {"id":"user-console","label":"User Console","url":"https://user.example.com/console","visibility":"user","icon":"globe","enabled":false},
  {"id":"admin-console","label":"Admin Console","url":"https://admin.example.com/console","visibility":"admin","icon":"chart","enabled":true},
  {"id":"hidden-console","label":"Hidden Console","url":"https://hidden.example.com/","visibility":"user","icon":"book","enabled":false}
]`

// TestUserCustomPages spec 2026-11-16 §11-T2：角色过滤 + 实时性 + 损坏配置。
// 复用同包 newTestUserRouter（定义在 user_endpoints_test.go），每个子用例一套
// 独立 fake store——注册 bootstrap 语义依赖"空表首个注册 = platform_admin"，
// 共享 store 会让第二个用例的 admin 变成普通用户。
func TestUserCustomPages(t *testing.T) {
	t.Run("无凭据401", func(t *testing.T) {
		do, _, _, _ := newTestUserRouter(t)
		rec := do(http.MethodGet, "/api/user/custom-pages", "", "")
		require.Equal(t, http.StatusUnauthorized, rec.Code, "无凭据必须 401: %s", rec.Body.String())
	})

	// §19-1 权限边界：注册成 platform_admin 后被降权为 user，**不重新登录**、同一 token
	// 再请求，响应里不得出现任何 admin 条目。
	//
	// 这条用例钉的是"角色取鉴权快照、不取 claims.Role"：UpdateUser 改角色只
	// upsertUserSnapshot、不 bump token_version，所以旧 JWT 里的 role 仍是
	// platform_admin。M8 变异（改回读 claims.Role）会让 admin 条目重新出现，用例必红；
	// 反向的"新升权用户看不到 admin 条目"同理被这条覆盖。
	t.Run("降权后旧token不再拿到admin条目", func(t *testing.T) {
		do, store, svc, adminToken, _ := newCustomPagesHarness(t)
		_, err := svc.UpdateSetting(t.Context(), domain.SettingCustomPages, customPagesConfig)
		require.NoError(t, err)

		// 降权前：同一 token 看得见 admin 条目（否则后面的"看不见"没有对照）。
		require.Equal(t, []string{"user-console", "admin-console"},
			customPagesIDs(t, do(http.MethodGet, "/api/user/custom-pages", "", adminToken)),
			"降权前的基线")

		admin, err := store.GetUserByEmail(t.Context(), "custom-admin@example.com")
		require.NoError(t, err)
		role := domain.RoleUser
		// 走真实的角色变更入口（不是直接改 fake 表）：服务层在这里做 upsertUserSnapshot，
		// 正是"快照变了、token 没变"的产生方式。
		_, err = svc.UpdateUser(t.Context(), &repository.UserPatch{ID: admin.ID, Role: &role}, admin.ID)
		require.NoError(t, err)

		rec := do(http.MethodGet, "/api/user/custom-pages", "", adminToken)
		require.Equal(t, http.StatusOK, rec.Code, "旧 token 仍可用（改角色不 bump token_version）: %s", rec.Body.String())
		require.Equal(t, []string{"user-console"}, customPagesIDs(t, rec),
			"降权后同一 token 只应看到 user 条目（不得再用 claims.Role 判定）")
		require.NotContains(t, rec.Body.String(), "admin-console")
		require.NotContains(t, rec.Body.String(), "https://admin.example.com/console")
	})

	// 角色过滤 + 保序 + 字段面（§11-T2 ②③④ + D10）。
	t.Run("角色过滤与字段面", func(t *testing.T) {
		do, _, svc, adminToken, userToken := newCustomPagesHarness(t)
		_, err := svc.UpdateSetting(t.Context(), domain.SettingCustomPages, customPagesConfig)
		require.NoError(t, err)

		adminRec := do(http.MethodGet, "/api/user/custom-pages", "", adminToken)
		require.Equal(t, http.StatusOK, adminRec.Code, "admin: %s", adminRec.Body.String())
		require.Equal(t, []string{"user-console", "admin-console"}, customPagesIDs(t, adminRec),
			"platform_admin = 全部启用项，且保持配置顺序")
		require.NotContains(t, adminRec.Body.String(), "hidden-console", "停用项两侧都不下发")
		require.NotContains(t, adminRec.Body.String(), "https://hidden.example.com/")
		assertCustomPageShape(t, adminRec)

		userRec := do(http.MethodGet, "/api/user/custom-pages", "", userToken)
		require.Equal(t, http.StatusOK, userRec.Code, "user: %s", userRec.Body.String())
		require.Equal(t, []string{"user-console"}, customPagesIDs(t, userRec),
			"普通 user = 仅启用且 visibility=user 的条目")
		// 断在原始响应体上（不是只看解析后的结构）：admin 条目的 id 与 URL
		// 一个字节都不该出现在普通用户的响应里——这是权限边界，不是展示偏好。
		require.NotContains(t, userRec.Body.String(), "admin-console")
		require.NotContains(t, userRec.Body.String(), "https://admin.example.com/console")
		require.NotContains(t, userRec.Body.String(), "hidden-console")
		assertCustomPageShape(t, userRec)
	})

	// §11-T2 ⑤：管理面保存后下一次 GET 立即反映（读设置快照，不重启、无外部
	// 失效广播参与）。M3 变异点（改成读库不读快照）会在这里变红。
	t.Run("停用即时生效", func(t *testing.T) {
		do, _, svc, _, userToken := newCustomPagesHarness(t)
		_, err := svc.UpdateSetting(t.Context(), domain.SettingCustomPages, customPagesConfig)
		require.NoError(t, err)
		require.Equal(t, []string{"user-console"},
			customPagesIDs(t, do(http.MethodGet, "/api/user/custom-pages", "", userToken)),
			"停用前的基线")

		_, err = svc.UpdateSetting(t.Context(), domain.SettingCustomPages, customPagesConfigUserDisabled)
		require.NoError(t, err)

		rec := do(http.MethodGet, "/api/user/custom-pages", "", userToken)
		require.Equal(t, http.StatusOK, rec.Code, "停用后: %s", rec.Body.String())
		require.Empty(t, customPagesIDs(t, rec), "停用后 user 面立即为空")
		require.NotContains(t, rec.Body.String(), "user-console")
		// 空列表必须是 []，不是 null（前端直接 .map）：make(..., 0, len) 的意义。
		require.Contains(t, rec.Body.String(), `"Pages":[]`)
	})

	// §11-T2 ⑥：两类损坏（JSON 语法错 / 能解析但违反 V2–V9）都 → 500 固定文案，
	// 且响应体不含配置原文。绕过 UpdateSetting（写入路径会 400 拒掉）直接写库，
	// 模拟运维手改 DB / 迁移脚本 / 旧版本回滚。
	t.Run("损坏配置500固定文案", func(t *testing.T) {
		cases := []struct {
			name string
			raw  string
		}{
			{"JSON语法错", `{oops`},
			{"可解析但url是javascript伪协议", `[{"id":"docs","label":"Docs","url":"javascript:alert(1)"}]`},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				do, store, svc, _, userToken := newCustomPagesHarness(t)
				_, err := store.SetSetting(t.Context(), domain.SettingCustomPages, domain.SettingTypeString, tc.raw)
				require.NoError(t, err)
				require.NoError(t, svc.ReloadSettings(t.Context()))

				rec := do(http.MethodGet, "/api/user/custom-pages", "", userToken)
				require.Equal(t, http.StatusInternalServerError, rec.Code, "body: %s", rec.Body.String())
				var body struct {
					Error string `json:"error"`
				}
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
				require.Equal(t, "custom pages config is invalid", body.Error, "错误文案必须固定可判别")
				// 不回显配置内容：service 错误的 %w 详情里带条目下标与 url 原文，
				// 而本端点任意登录用户都能打——回显等于广播管理员的配错内容。
				require.NotContains(t, rec.Body.String(), tc.raw)
				require.NotContains(t, rec.Body.String(), "custom pages config is invalid:")
				require.NotContains(t, rec.Body.String(), "javascript")
				require.NotContains(t, rec.Body.String(), "https://")
			})
		}
	})
}

// newCustomPagesHarness 一套独立 store + 两个已注册主体：首个注册 = platform_admin
// （bootstrap），第二个 = 普通 user。注册响应里的 role 先断言再用——bootstrap 语义
// 一旦变了，"admin 侧"其实是个普通用户，后面的过滤断言就全是假绿。
func newCustomPagesHarness(t *testing.T) (userRouterDo, *fakeStore, *service.Service, string, string) {
	t.Helper()
	do, store, _, svc := newTestUserRouter(t)
	adminToken := registerAndAssertRole(t, do, "custom-admin@example.com", userapi.UserRolePlatformAdmin)
	userToken := registerAndAssertRole(t, do, "custom-user@example.com", userapi.UserRoleUser)
	return do, store, svc, adminToken, userToken
}

// registerAndAssertRole 注册并返回 token，同时断言响应里的角色。
func registerAndAssertRole(t *testing.T, do userRouterDo, email string, want userapi.UserRole) string {
	t.Helper()
	rec := do(http.MethodPost, "/api/user/auth/register",
		`{"email":"`+email+`","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "register %s: %s", email, rec.Body.String())
	var resp userapi.UserAuthResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.User.Role, "注册响应必须带 role：%s", rec.Body.String())
	require.Equal(t, want, *resp.User.Role, "注册角色不符（bootstrap 顺序变了？）：%s", email)
	require.NotEmpty(t, resp.Token, "注册即登录")
	return resp.Token
}

// customPagesIDs 解析 200 体并返回 id 序列（保持响应顺序）。
func customPagesIDs(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	var resp userapi.CustomPagesResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), "200 体: %s", rec.Body.String())
	ids := make([]string, 0, len(resp.Pages))
	for _, page := range resp.Pages {
		ids = append(ids, page.ID)
	}
	return ids
}

// assertCustomPageShape 每条响应项恰好 5 个字段，且 Enabled 不得下发（D10）。
// 用 map 解而不是结构体：结构体反序列化会静默丢掉不认识的字段，测不出"多下发"。
func assertCustomPageShape(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	var raw struct {
		Pages []map[string]json.RawMessage `json:"Pages"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw), "200 体: %s", rec.Body.String())
	require.NotEmpty(t, raw.Pages, "字段面断言必须跑在非空列表上才会生效")
	for i, item := range raw.Pages {
		require.Len(t, item, 5, "第 %d 条应恰好 5 个字段: %v", i, item)
		require.NotContains(t, item, "Enabled", "第 %d 条不得下发 Enabled", i)
		for _, key := range []string{"ID", "Label", "URL", "Visibility", "Icon"} {
			require.Contains(t, item, key, "第 %d 条缺字段 %s", i, key)
		}
	}
}
