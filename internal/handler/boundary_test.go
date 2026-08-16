// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// S-C 边界收敛测试（spec 2026-08-17）：六端点 limit 钳制红绿（201 行种子区分
// "裁剪到 200"与"忽略 limit"）、uses 翻页取全、decode 严格 400。fake 已镜像
// repo 分页语义（paginate helper——Limit ≤0→20、Offset <0→0、total 全量）。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	userapi "github.com/is7qin/c3api/internal/handler/user"
)

// TestListEndpointsLimitClamp 六端点 limit 钳制红绿：201 行种子 + limit=500 →
// 恰 200 行（上限封顶）且 total 全量 201；limit=150 原样；limit=-5/缺省 → 20
// （repo ≤0 归一下限语义零变化）。
func TestListEndpointsLimitClamp(t *testing.T) {
	doAdmin, doUser, store := newSharedRouters(t)
	token, uid := registerAndGet(t, doUser, "clamp@example.com")

	store.mu.Lock()
	for i := 0; i < 201; i++ {
		id := store.nextID
		store.nextID++
		store.tpls[id] = &domain.Template{ID: id, Name: fmt.Sprintf("tpl-%d", i)}
		store.accs[id] = &domain.Account{ID: id, Name: fmt.Sprintf("acc-%d", i)}
		store.groups[id] = &domain.Group{ID: id, Name: fmt.Sprintf("grp-%d", i)}
		store.users[id] = &domain.User{ID: id, Email: fmt.Sprintf("u%d@example.com", i)} // 种子 201 + 注册用户 1 = total 202
		store.keys[id] = &domain.Key{ID: id, UserID: uid, Name: fmt.Sprintf("key-%d", i)}
	}
	store.mu.Unlock()

	// 六端点超限 → 200 行封顶（total 恒全量：users 202 = 种子 201 + 注册用户 1）
	for _, tc := range []struct {
		name  string
		rec   *httptest.ResponseRecorder
		total int64
	}{
		{"templates", doAdmin(http.MethodGet, "/admin/templates?limit=500", "", ""), 201},
		{"accounts", doAdmin(http.MethodGet, "/admin/accounts?limit=500", "", ""), 201},
		{"groups", doAdmin(http.MethodGet, "/admin/groups?limit=500", "", ""), 201},
		{"keys", doAdmin(http.MethodGet, "/admin/keys?limit=500", "", ""), 201},
		{"users", doAdmin(http.MethodGet, "/admin/users?limit=500", "", ""), 202},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, 200, tc.rec.Code, "%s: %s", tc.name, tc.rec.Body.String())
			var body struct {
				Total int64 `json:"total"`
				Rows  []any `json:"rows"`
			}
			require.NoError(t, json.Unmarshal(tc.rec.Body.Bytes(), &body))
			require.Equal(t, tc.total, body.Total, "%s total 全量", tc.name)
			require.Len(t, body.Rows, 200, "%s limit=500 裁剪到 200（201 行种子区分裁剪与忽略）", tc.name)
		})
	}

	// 用户面 /user/keys（第 6 处同构）
	rec := doUser(http.MethodGet, "/user/keys?limit=500", "", token)
	require.Equal(t, 200, rec.Code, "user keys: %s", rec.Body.String())
	var ukeys userapi.KeyListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &ukeys))
	require.Equal(t, int64(201), ukeys.Total, "user keys total 全量")
	require.Len(t, ukeys.Rows, 200, "user keys limit=500 裁剪到 200")

	// ≤200 原样 + 缺省/负值归一下限零回归（≤0 → repo 20）
	rec = doAdmin(http.MethodGet, "/admin/templates?limit=150", "", "")
	var tpls TemplateListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpls))
	require.Len(t, tpls.Rows, 150, "limit=150 原样")

	rec = doAdmin(http.MethodGet, "/admin/templates?limit=-5", "", "")
	tpls = TemplateListResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpls))
	require.Len(t, tpls.Rows, 20, "limit=-5 → repo ≤0 归一下限 20（行为零变化）")

	rec = doAdmin(http.MethodGet, "/admin/templates", "", "")
	tpls = TemplateListResponse{}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpls))
	require.Len(t, tpls.Rows, 20, "缺省 limit → 20（行为零变化）")
}

// TestRedemptionUsesPagination 管理面 uses 翻页取全（红绿）：25 条兑换记录，
// 无参数默认 20 行（Total 全量 25——既有默认行为零回归）；limit/offset 组合
// 翻页取全 25 条（此前恒 20 行不可翻页，Total 失真）。
func TestRedemptionUsesPagination(t *testing.T) {
	doAdmin, doUser, _ := newSharedRouters(t)

	gen := genCodes(t, doAdmin, `{"type":"balance","value":1,"max_uses":100}`)
	c := gen.Codes[0]
	// 同一用户复兑同一码 → 409（评审 M-1）；25 个不同用户各兑一次 → 25 条。
	for i := 0; i < 25; i++ {
		uToken, _ := registerAndGet(t, doUser, fmt.Sprintf("uses-p%d@example.com", i))
		rec := doUser(http.MethodPost, "/user/redemptions", `{"code":"`+c.Code+`"}`, uToken)
		require.Equal(t, 200, rec.Code, "redeem %d: %s", i, rec.Body.String())
	}

	// 无参数默认行为零回归：20 行 + Total 全量
	rec := doAdmin(http.MethodGet, fmt.Sprintf("/admin/redemption-codes/%d/uses", c.ID), "", "")
	require.Equal(t, 200, rec.Code, "uses default: %s", rec.Body.String())
	var uses RedemptionUseListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &uses))
	require.Equal(t, int64(25), uses.Total, "Total 恒全量（不随分页裁剪）")
	require.Len(t, uses.Rows, 20, "无参数默认 20 行")

	// limit/offset 翻页取全（此前恒 20 行不可翻页）
	var all []int64
	for _, page := range []struct{ limit, offset string }{
		{"10", "0"}, {"10", "10"}, {"10", "20"},
	} {
		rec = doAdmin(http.MethodGet,
			fmt.Sprintf("/admin/redemption-codes/%d/uses?limit=%s&offset=%s", c.ID, page.limit, page.offset), "", "")
		require.Equal(t, 200, rec.Code, "uses paged: %s", rec.Body.String())
		uses = RedemptionUseListResponse{}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &uses))
		require.Equal(t, int64(25), uses.Total, "分页 Total 仍全量")
		for _, u := range uses.Rows {
			all = append(all, u.ID)
		}
	}
	require.Len(t, all, 25, "limit=10 三页取全 25 条（此前 20 行封顶不可翻页）")
	require.True(t, uniqueIDs(all), "翻页行互不重复")
}

func uniqueIDs(ids []int64) bool {
	seen := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		if _, ok := seen[id]; ok {
			return false
		}
		seen[id] = struct{}{}
	}
	return true
}

// TestDecodeStrict 管理面/用户面 decode 严格化（红绿）：拼错字段名 → 400
// （此前 200 静默不生效——配置漂移）；尾随数据 → 400；合法请求零回归。
func TestDecodeStrict(t *testing.T) {
	_, _, do := newListTestRouter(t)

	// 拼错字段名 → 400 显式
	rec := do(http.MethodPost, "/admin/templates",
		`{"namee":"x","base_url":"https://u","supported_formats":["openai-chat"]}`)
	require.Equal(t, 400, rec.Code, "unknown field: %s", rec.Body.String())
	require.Contains(t, errMsg(t, rec), "namee", "错误消息含未知字段名")

	// 尾随数据（合法 JSON + 垃圾）→ 400
	rec = do(http.MethodPost, "/admin/templates",
		`{"name":"x","base_url":"https://u","supported_formats":["openai-chat"]} {"extra":1}`)
	require.Equal(t, 400, rec.Code, "trailing data: %s", rec.Body.String())

	// 合法请求零回归
	rec = do(http.MethodPost, "/admin/templates",
		`{"name":"ok","base_url":"https://u","supported_formats":["openai-chat"]}`)
	require.Equal(t, 200, rec.Code, "valid: %s", rec.Body.String())
}

// TestDecodeStrictUser 用户面 decode 同款（user/handler.go 独立同函数）：
// 拼错字段名 → 400；合法请求零回归。
func TestDecodeStrictUser(t *testing.T) {
	doAdmin, doUser, _ := newSharedRouters(t)
	token, _ := registerAndGet(t, doUser, "decode-strict@example.com")

	// 建 public 组（key 创建必需 group_id）
	rec := doAdmin(http.MethodPost, "/admin/groups", `{"name":"pub-g"}`, "")
	require.Equal(t, 200, rec.Code, "create group: %s", rec.Body.String())
	var g Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &g))

	rec = doUser(http.MethodPost, "/user/keys", `{"namee":"k"}`, token)
	require.Equal(t, 400, rec.Code, "unknown field: %s", rec.Body.String())

	rec = doUser(http.MethodPost, "/user/keys", `{"name":"k","group_id":`+itoa(*g.ID)+`}`, token)
	require.Equal(t, 200, rec.Code, "valid: %s", rec.Body.String())

	// admin 面另一入口（PUT 拼错字段——此前 200 静默不生效的典型场景）
	doAdmin(http.MethodPost, "/admin/templates",
		`{"name":"t1","base_url":"https://u","supported_formats":["openai-chat"]}`, "")
	rec = doAdmin(http.MethodPut, "/admin/templates/1", `{"namee":"x"}`, "")
	require.Equal(t, 400, rec.Code, "put unknown field: %s", rec.Body.String())
}
