// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// spec 2026-11-17 §3.2/§3.3：`group_id` 的三态必须**原样**穿到仓储层。
//
// 这条用例专打「0 = 不过滤」的旧先例（keys 列表的 GroupID 就是两态语义）：
// 若 handler 用 `httpface.Deref(params.GroupId) > 0` 之类的读法，`group_id=0`
// 会被翻成 nil（= 不过滤），前端「未分组」这一档就静默变成「全部」——空结果
// 与全量结果都是 200，只有断言仓储收到的 `*int64` 才能抓住。
//
// 两侧入口（管理面 /api/admin/accounts 与供应商面 /api/user/supplier/accounts）
// 走同一条 service 路径，故两侧都断言：供应链条的 wrapper 是具名 struct literal，
// 字段遗漏编译器不报（supplier_surface.go 注释所述），只能由用例兜底。

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/handler/supplier"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
)

// groupFilterHarness 同一份 fakeStore 同时挂在管理面与供应商面（两条入口，
// 一份仓储记录）。
type groupFilterHarness struct {
	store    *fakeStore
	admin    http.Handler
	supplier http.Handler
	iss      *auth.Issuer
}

func newGroupFilterHarness(t *testing.T) *groupFilterHarness {
	t.Helper()
	store := newFakeStore()
	svc := service.New(service.Deps{
		Store: store, Scheduler: fakeSched{}, Invalidate: service.NopInvalidator{},
		Publisher: nil, RuleReload: nil, Auth: &fakeKeys{}, Log: nil, EmailCodeStore: store,
	})
	api := New(svc)

	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "Bearer admin-tok" {
				httpface.WriteErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Mount("/", api.Router())

	iss := auth.NewIssuer("s")
	users := fakeUsers{sn: domain.UserSnapshot{Status: domain.UserStatusActive, Role: domain.RoleSupplier}}
	return &groupFilterHarness{
		store:    store,
		admin:    r,
		supplier: NewSupplierSurface(api, iss, fakeSnapshot{fakeUsers: users, fakeMgmt: fakeMgmt{}}),
		iss:      iss,
	}
}

func (h *groupFilterHarness) getAdmin(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, http.NoBody)
	req.Header.Set("Authorization", "Bearer admin-tok")
	rec := httptest.NewRecorder()
	h.admin.ServeHTTP(rec, req)
	return rec
}

func (h *groupFilterHarness) getSupplier(t *testing.T, uid int64, path string) *httptest.ResponseRecorder {
	t.Helper()
	tok, err := h.iss.Issue(uid, "s@example.com", string(domain.RoleSupplier), 0)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodGet, SupplierSurfaceBaseURL+path, http.NoBody)
	req.Header.Set("Authorization", "Bearer "+tok)
	rec := httptest.NewRecorder()
	h.supplier.ServeHTTP(rec, req)
	return rec
}

// lastGroupFilter 最近一次到达仓储的分组筛选（nil = 缺席 = 不过滤）。
// 返回 (是否收到过调用, 值)。
func (h *groupFilterHarness) lastGroupFilter(t *testing.T) (bool, *int64) {
	t.Helper()
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	if len(h.store.listQueries) == 0 {
		return false, nil
	}
	return true, h.store.listQueries[len(h.store.listQueries)-1].AccountGroupID
}

func (h *groupFilterHarness) listCalls() int {
	h.store.mu.Lock()
	defer h.store.mu.Unlock()
	return len(h.store.listQueries)
}

// TestGetAccountsGroupFilterTriState `group_id=0` / `group_id=7` / 缺席 三种入参
// 都能到达仓储，且 `group_id=-1` ⇒ 400（字段级说明）且**不进仓储**。
func TestGetAccountsGroupFilterTriState(t *testing.T) {
	for _, surface := range []struct {
		name string
		path string
	}{
		{"admin", "/api/admin/accounts"},
		{"supplier", "/accounts"},
	} {
		t.Run(surface.name, func(t *testing.T) {
			h := newGroupFilterHarness(t)
			call := func(path string) *httptest.ResponseRecorder {
				if surface.name == "admin" {
					return h.getAdmin(t, path)
				}
				return h.getSupplier(t, 77, path)
			}

			// 1) 缺席 ⇒ nil（不过滤）——与下面 &0 必须可分。
			rec := call(surface.path)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			got, ptr := h.lastGroupFilter(t)
			require.True(t, got, "list 必须到达仓储")
			require.Nil(t, ptr, "缺省 group_id ⇒ nil（不过滤）")

			// 2) group_id=0 ⇒ &0（未分组）——**不是**「不过滤」。
			rec = call(surface.path + "?group_id=0")
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			got, ptr = h.lastGroupFilter(t)
			require.True(t, got)
			require.NotNil(t, ptr, "group_id=0 不得被读成「不过滤」")
			require.Equal(t, int64(0), *ptr, "group_id=0 必须原样穿到仓储")

			// 3) group_id=7 ⇒ &7（该分组）。
			rec = call(surface.path + "?group_id=7")
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			got, ptr = h.lastGroupFilter(t)
			require.True(t, got)
			require.NotNil(t, ptr)
			require.Equal(t, int64(7), *ptr)

			// 4) 负数 ⇒ 400 + 字段级说明，且 service 在仓储之前拦截。
			before := h.listCalls()
			rec = call(surface.path + "?group_id=-1")
			require.Equal(t, http.StatusBadRequest, rec.Code, "负数必须 400: %s", rec.Body.String())
			require.Contains(t, errMsg(t, rec), "group_id must be >= 0", "400 带字段级说明")
			require.Equal(t, before, h.listCalls(), "非法入参不得进仓储")
		})
	}
}

// TestGetAccountsGroupFilterSupplierWrapperForwards 供应商面 wrapper 是**类型化
// 直调**（具名 struct literal，字段遗漏编译器不报）——这里钉「同一条 service
// 路径、同一个入参」，避免 wrapper 哪天自己漏了 GroupId 字段而两侧语义悄悄分叉。
// 作用域（只本人账号）由 ctx 注入 + 仓储谓词负责，既有 PG 作用域用例覆盖。
func TestGetAccountsGroupFilterSupplierWrapperForwards(t *testing.T) {
	h := newGroupFilterHarness(t)
	rec := h.getSupplier(t, 77, "/accounts?group_id=0")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	h.store.mu.Lock()
	qs := make([]repository.ListQuery, len(h.store.listQueries))
	copy(qs, h.store.listQueries)
	h.store.mu.Unlock()
	require.Len(t, qs, 1, "供应商面必须走同一条 ListAccounts（无旁路）")
	require.NotNil(t, qs[0].AccountGroupID, "wrapper 必须直传 group_id（否则「未分组」在供应商面失效）")
	require.Equal(t, int64(0), *qs[0].AccountGroupID)
}

// 契约形状：两侧生成物的该参数都是**指针**（缺席与 0 可分的前提）。若有人把
// openapi 里该参数改成 required 或换掉生成选项退化为值类型，这条先炸。
func TestGetAccountsGroupIdParamIsPointer(t *testing.T) {
	var adminParams GetAccountsParams
	require.Nil(t, adminParams.GroupId, "管理面 GetAccountsParams.GroupId 必须是指针")
	var supplierParams supplier.GetSupplierAccountsParams
	require.Nil(t, supplierParams.GroupId, "供应商面 GetSupplierAccountsParams.GroupId 必须是指针")
}
