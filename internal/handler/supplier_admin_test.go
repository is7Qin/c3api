// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/service"
)

// TestAdminFundsCommandsRequireNamedJWT I5（spec 2026-10-09 §6.5）：所有资金命令
// 在缺少具名 JWT 操作者（静态 admin token 路径）时一律 403——在触及 service 之前
// 就被 requireFundsActor 拦下（AdminAPI{} 无 svc，若继续执行会 panic；未 panic 即
// 证明已拦下）。
func TestAdminFundsCommandsRequireNamedJWT(t *testing.T) {
	api := &AdminAPI{}
	calls := []struct {
		name string
		fn   func(http.ResponseWriter, *http.Request)
	}{
		{"approve", func(w http.ResponseWriter, r *http.Request) { api.PostAdminSupplierSettlementsIdApprove(w, r, 1) }},
		{"reject", func(w http.ResponseWriter, r *http.Request) { api.PostAdminSupplierSettlementsIdReject(w, r, 1) }},
		{"claim", func(w http.ResponseWriter, r *http.Request) { api.PostAdminSupplierSettlementsIdClaim(w, r, 1) }},
		{"confirm-failed", func(w http.ResponseWriter, r *http.Request) { api.PostAdminSupplierSettlementsIdConfirmFailed(w, r, 1) }},
		{"paid", func(w http.ResponseWriter, r *http.Request) { api.PostAdminSupplierSettlementsIdPaid(w, r, 1) }},
		{"admin-request", func(w http.ResponseWriter, r *http.Request) { api.PostAdminSupplierSettlementsAdminRequest(w, r) }},
	}
	for _, tc := range calls {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/admin/supplier/settlements/1/"+tc.name, strings.NewReader(`{}`))
			rec := httptest.NewRecorder()
			tc.fn(rec, req)
			require.Equal(t, http.StatusForbidden, rec.Code, "静态 admin token（无具名操作者）⇒ 403")
		})
	}
}

// TestFundsActorRoundTrip 具名操作者经 context 透传（adminAuth JWT 路径注入）。
func TestFundsActorRoundTrip(t *testing.T) {
	ctx := domain.WithFundsActor(httptest.NewRequest(http.MethodGet, "/", nil).Context(), domain.FundsActor{UserID: 7, TokenVersion: 3})
	req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
	actor, ok := fundsActorFrom(req)
	require.True(t, ok)
	require.Equal(t, int64(7), actor.UserID)
	require.Equal(t, int64(3), actor.TokenVersion)
}

// TestAdminClaimRequestStrictContract A22/I8：风险核对证据在**真实 claim 请求边界**
// 严格拒绝——未知键/错类型（旧实现把任意 record 字符串当不透明 reference 放行）/
// 缺 evidence/错 revision 均 400；只有结构化良构请求到达持久层。
func TestAdminClaimRequestStrictContract(t *testing.T) {
	store := newFakeStore()
	store.supplierAdmin = &fakeSupplierAdminStore{
		settlements: []*domain.SupplierSettlement{{ID: 1, Status: domain.SettlementApproved, Revision: 3, AmountMillis: 1000}},
		balances:    map[int64]*domain.SupplierBalance{},
	}
	svc := service.New(service.Deps{Store: store, Scheduler: fakeSched{}, Invalidate: service.NopInvalidator{}, Keys: &fakeKeys{}, EmailCodeStore: store})
	api := New(svc)

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/supplier/settlements/1/claim", strings.NewReader(body))
		req = req.WithContext(domain.WithFundsActor(req.Context(), domain.FundsActor{UserID: 7, TokenVersion: 1}))
		rec := httptest.NewRecorder()
		api.PostAdminSupplierSettlementsIdClaim(rec, req, 1)
		return rec
	}

	// 良构结构化体 ⇒ 200（到达持久层）。
	good := `{"expected_revision":3,"amount_millis":1000,"payee_snapshot":{"payee_name":"A","account":"1","unit":"millis"},"risk_evidence":{"reference":"r","summary":"s","approved_revision":3}}`
	require.Equal(t, http.StatusOK, post(good).Code, "良构结构化体应放行")

	// 未知键 ⇒ 400。
	unknown := `{"expected_revision":3,"amount_millis":1000,"payee_snapshot":{"payee_name":"A","account":"1","unit":"millis"},"risk_evidence":{"reference":"r","summary":"s","approved_revision":3},"bogus":1}`
	require.Equal(t, http.StatusBadRequest, post(unknown).Code, "未知键必须拒绝")

	// 错类型：risk_evidence 传字符串（旧现状）⇒ 400，不得当不透明 reference 签为放行。
	wrongType := `{"expected_revision":3,"amount_millis":1000,"payee_snapshot":{"payee_name":"A","account":"1","unit":"millis"},"risk_evidence":"plain-record-json"}`
	require.Equal(t, http.StatusBadRequest, post(wrongType).Code, "自由文本证据必须拒绝")

	// 缺 evidence（reference/summary 空）⇒ 400。
	missingEvidence := `{"expected_revision":3,"amount_millis":1000,"payee_snapshot":{"payee_name":"A","account":"1","unit":"millis"},"risk_evidence":{"reference":"","summary":"","approved_revision":3}}`
	require.Equal(t, http.StatusBadRequest, post(missingEvidence).Code, "缺 evidence 必须拒绝")

	// 错 revision（approved_revision != expected_revision）⇒ 400。
	wrongRev := `{"expected_revision":3,"amount_millis":1000,"payee_snapshot":{"payee_name":"A","account":"1","unit":"millis"},"risk_evidence":{"reference":"r","summary":"s","approved_revision":4}}`
	require.Equal(t, http.StatusBadRequest, post(wrongRev).Code, "错 revision 必须拒绝")
}
