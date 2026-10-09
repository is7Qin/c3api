// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oapi-codegen/nullable"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/service"
)

// TestAccountPatchFromBodySupplierUserID §2.5 唯一字段级例外：
//   - 管理面：正整数 ⇒ 分配目标（&uid）；显式 null ⇒ 清空（&0）；缺席 ⇒ nil。
//   - 供应商面：请求体出现该字段（含 null）⇒ 400（归属恒为 JWT 本人，不可改）。
func TestAccountPatchFromBodySupplierUserID(t *testing.T) {
	mgmt := context.Background()

	assign := AccountConfigPatch{SupplierUserId: nullable.NewNullableWithValue(int64(9))}
	p, err := accountPatchFromBody(mgmt, &assign)
	require.NoError(t, err)
	require.NotNil(t, p.SupplierUserID)
	require.Equal(t, int64(9), *p.SupplierUserID)

	clear := AccountConfigPatch{SupplierUserId: nullable.NewNullNullable[int64]()}
	p, err = accountPatchFromBody(mgmt, &clear)
	require.NoError(t, err)
	require.NotNil(t, p.SupplierUserID)
	require.Equal(t, int64(0), *p.SupplierUserID, "null = 清空（回平台自有）")

	absent := AccountConfigPatch{}
	p, err = accountPatchFromBody(mgmt, &absent)
	require.NoError(t, err)
	require.Nil(t, p.SupplierUserID, "缺席 = 不变")

	supplierCtx := domain.WithAccountScope(mgmt, domain.SupplierAccountScope(7))
	_, err = accountPatchFromBody(supplierCtx, &assign)
	require.Error(t, err, "供应商面出现 supplier_user_id ⇒ 400")
	_, err = accountPatchFromBody(supplierCtx, &clear)
	require.Error(t, err, "供应商面出现 supplier_user_id: null ⇒ 400")

	// 供应商面未提及该字段 ⇒ 正常（服务端钉死 jwtUser）。
	{
		name := "acc"
		tid := int64(1)
		body := AccountConfigPatch{Name: nullable.NewNullableWithValue(name), TemplateId: nullable.NewNullableWithValue(tid)}
		p, err := accountPatchFromBody(supplierCtx, &body)
		require.NoError(t, err)
		require.Nil(t, p.SupplierUserID)
	}
}

// TestPostAccountsAdminProjectsSupplierUserID spec I3：管理面单建必须投影请求体的
// supplier_user_id（前端创建表单就在发该字段——此前被吞掉，账号静默建成平台自有）。
func TestPostAccountsAdminProjectsSupplierUserID(t *testing.T) {
	h, store, tplID, _ := codexImportTestAPI(t)

	t.Run("admin create assigns the requested owner", func(t *testing.T) {
		body := `{"name":"owned-acc","template_id":` + itoa(tplID) + `,"upstream_key":"sk-o","supplier_user_id":42}`
		rec := doImport(t, h, http.MethodPost, "/api/admin/accounts", body)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		var acc Account
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))
		require.NotNil(t, acc.SupplierUserId, "响应必须回显归属")
		require.Equal(t, int64(42), *acc.SupplierUserId)
		// 落库侧同值（不是只在响应里回显）。
		stored := store.accs[*acc.ID]
		require.NotNil(t, stored)
		require.Equal(t, int64(42), stored.SupplierUserID)
	})

	t.Run("admin create without the field stays platform-owned", func(t *testing.T) {
		body := `{"name":"platform-acc","template_id":` + itoa(tplID) + `,"upstream_key":"sk-p"}`
		rec := doImport(t, h, http.MethodPost, "/api/admin/accounts", body)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())
		var acc Account
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))
		require.Equal(t, int64(0), store.accs[*acc.ID].SupplierUserID, "缺席 ⇒ 平台自有（0/NULL）")
	})

	t.Run("supplier create pins to the jwt user", func(t *testing.T) {
		store := newFakeStore()
		store.tpls[1] = &domain.Template{ID: 1, Name: "codex-tpl", CredentialType: credential.TypeCodexOAuth,
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}}
		svc := service.New(service.Deps{Store: store, Scheduler: fakeSched{}, Invalidate: service.NopInvalidator{}, Auth: &fakeKeys{}, EmailCodeStore: store})
		api := New(svc)

		req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts",
			strings.NewReader(`{"name":"sup-acc","template_id":1,"upstream_key":"sk-s"}`))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(domain.WithAccountScope(req.Context(), domain.SupplierAccountScope(77)))
		rec := httptest.NewRecorder()
		api.Router().ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

		var acc Account
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &acc))
		require.Equal(t, int64(77), store.accs[*acc.ID].SupplierUserID, "供应商面恒写 JWT 本人")

		// 供应商面**请求体出现**该字段 ⇒ 400（不覆盖服务端钉死值）。
		req = httptest.NewRequest(http.MethodPost, "/api/admin/accounts",
			strings.NewReader(`{"name":"sup-acc2","template_id":1,"supplier_user_id":88}`))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(domain.WithAccountScope(req.Context(), domain.SupplierAccountScope(77)))
		rec = httptest.NewRecorder()
		api.Router().ServeHTTP(rec, req)
		require.Equal(t, http.StatusBadRequest, rec.Code, "供应商面出现归属字段 ⇒ 400：%s", rec.Body.String())
	})
}
