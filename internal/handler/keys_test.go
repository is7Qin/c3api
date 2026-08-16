// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/service"
)

// TestGetKeys /admin/keys（fake store，spec 2026-08-16）：name 模糊 / user_id /
// group_id 收窄与 AND 组合 + 分页 + 非法 sort/order 400 + 脱敏铁律（响应 JSON
// 无 key/key_raw 键、密钥明文不出现在响应体）+ 越权 401。
func TestGetKeys(t *testing.T) {
	store := newFakeStore()
	svc := service.New(store, fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil)
	h := New(svc)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler { // admin token 中间件
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "Bearer admin-tok" {
				httpface.WriteErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Mount("/", h.Router())
	do := func(path, auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, strings.NewReader(""))
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	ctx := context.Background()
	base := time.Date(2026, 8, 16, 10, 0, 0, 0, time.UTC)
	mk := func(name string, userID, groupID int64) *domain.Key {
		k, err := store.CreateKey(ctx, &domain.Key{
			UserID: userID, GroupID: groupID, Name: name,
			KeyRaw: "sk-secret-" + name,
			Status: domain.KeyStatusActive, MaxConcurrency: 2,
			Quota: 1000, QuotaUsed: 5,
			CreatedAt: base, UpdatedAt: base,
		})
		require.NoError(t, err)
		return k
	}
	k1 := mk("alpha", 1, 10)
	k2 := mk("beta-test", 1, 20)
	k3 := mk("gamma-test", 2, 10)

	// 全量（默认 limit 20 / 排序由 fake 不保证，断言集合与 total）
	rec := do("/admin/keys", "admin-tok")
	require.Equal(t, http.StatusOK, rec.Code, "list: %s", rec.Body.String())
	var all AdminKeyListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &all))
	require.Equal(t, int64(3), all.Total)
	require.Len(t, all.Rows, 3)
	ids := map[int64]bool{}
	for _, row := range all.Rows {
		ids[*row.ID] = true
	}
	require.True(t, ids[k1.ID] && ids[k2.ID] && ids[k3.ID], "三 key 全量可见: %v", ids)

	// name 模糊命中（同大小写；ILIKE 语义由 repo 真实 PG 测试覆盖）
	rec = do("/admin/keys?name=test", "admin-tok")
	require.Equal(t, http.StatusOK, rec.Code)
	var byName AdminKeyListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &byName))
	require.Equal(t, int64(2), byName.Total, "name=test → beta-test/gamma-test: %s", rec.Body.String())
	for _, row := range byName.Rows {
		require.Contains(t, *row.Name, "test")
	}

	// user_id 收窄
	rec = do(fmt.Sprintf("/admin/keys?user_id=%d", 1), "admin-tok")
	require.Equal(t, http.StatusOK, rec.Code)
	var byUser AdminKeyListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &byUser))
	require.Equal(t, int64(2), byUser.Total)
	for _, row := range byUser.Rows {
		require.Equal(t, int64(1), *row.UserID)
	}

	// group_id 收窄
	rec = do(fmt.Sprintf("/admin/keys?group_id=%d", 10), "admin-tok")
	require.Equal(t, http.StatusOK, rec.Code)
	var byGroup AdminKeyListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &byGroup))
	require.Equal(t, int64(2), byGroup.Total)
	for _, row := range byGroup.Rows {
		require.Equal(t, int64(10), *row.GroupID)
	}

	// AND 组合：name + user_id + group_id → 唯一命中
	rec = do("/admin/keys?name=test&user_id=1&group_id=20", "admin-tok")
	require.Equal(t, http.StatusOK, rec.Code)
	var andResp AdminKeyListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &andResp))
	require.Equal(t, int64(1), andResp.Total)
	require.Len(t, andResp.Rows, 1)
	require.Equal(t, "beta-test", *andResp.Rows[0].Name)

	// 分页：limit=2 → rows 2、total 恒 3；offset 翻页
	rec = do("/admin/keys?limit=2", "admin-tok")
	require.Equal(t, http.StatusOK, rec.Code)
	var page1 AdminKeyListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page1))
	require.Equal(t, int64(3), page1.Total, "total = 满足筛选总数，不分页裁剪")
	require.Len(t, page1.Rows, 2)
	rec = do("/admin/keys?limit=2&offset=2", "admin-tok")
	var page2 AdminKeyListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &page2))
	require.Equal(t, int64(3), page2.Total)
	require.Len(t, page2.Rows, 1)

	// 空结果：user_id 无匹配 → total 0 + rows 空数组（非 null）
	rec = do("/admin/keys?user_id=999", "admin-tok")
	require.Equal(t, http.StatusOK, rec.Code)
	var empty AdminKeyListResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &empty))
	require.Equal(t, int64(0), empty.Total)
	require.Empty(t, empty.Rows)
	require.Contains(t, rec.Body.String(), `"rows":[]`, "rows 序列化为空数组而非 null")

	// 非法 sort（白名单外）→ 400；非法 order → 400
	for _, path := range []string{"/admin/keys?sort=status", "/admin/keys?sort=quota", "/admin/keys?order=sideways"} {
		rec = do(path, "admin-tok")
		require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", path, rec.Body.String())
	}
	// sort 白名单三键可用（fake 不排序，仅验证 200）
	for _, path := range []string{"/admin/keys?sort=id", "/admin/keys?sort=name", "/admin/keys?sort=created_at&order=asc"} {
		rec = do(path, "admin-tok")
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", path, rec.Body.String())
	}

	// 脱敏铁律：明文恒存在于 fake 存库（种子 KeyRaw），响应体必须不含——
	// JSON 断言无 key/key_raw 键 + 明文串不出现在响应体
	rec = do("/admin/keys", "admin-tok")
	require.Equal(t, http.StatusOK, rec.Code)
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	rowsArr, ok := body["rows"].([]any)
	require.True(t, ok)
	for _, r := range rowsArr {
		row := r.(map[string]any)
		_, hasKey := row["key"]
		_, hasKeyRaw := row["key_raw"]
		require.False(t, hasKey, "响应行不得含 key 明文键: %v", row)
		require.False(t, hasKeyRaw, "响应行不得含 key_raw 键: %v", row)
	}
	require.NotContains(t, rec.Body.String(), "sk-secret-", "密钥明文绝不出现在响应体")
	// 行内其他字段齐全（ID/Name/Quota 等，按用户 AdminKey schema 映射）
	found := 0
	for _, r := range rowsArr {
		row := r.(map[string]any)
		if row["Name"] == "alpha" {
			found++
			require.Equal(t, float64(k1.ID), row["ID"], "ID 映射")
			require.Equal(t, float64(1000), row["Quota"], "Quota 映射")
			require.Equal(t, "active", row["Status"], "Status 映射")
		}
	}
	require.Equal(t, 1, found)

	// 越权面：非 platform_admin（无 admin token）→ 401
	rec = do("/admin/keys", "")
	require.Equal(t, http.StatusUnauthorized, rec.Code, "非 admin token → 401")
	rec = do("/admin/keys", "user-tok")
	require.Equal(t, http.StatusUnauthorized, rec.Code, "非 admin token（普通用户 token）→ 401")
}
