// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
	handleruser "github.com/is7qin/c3api/internal/handler/user"
	"github.com/is7qin/c3api/internal/server"
	"github.com/is7qin/c3api/internal/service"
)

func TestUserBalanceWarningThreshold_Handler(t *testing.T) {
	store := newFakeStore()
	svc := service.New(store, nil, service.NopInvalidator{}, nil, nil, nil, nil)
	// seed user
	u, err := store.CreateUser(nil, &domain.User{Email: "u@example.com", PasswordHash: "x", Role: domain.RoleUser, Status: domain.UserStatusActive})
	require.NoError(t, err)
	iss := auth.NewIssuer("test-secret-bw")
	provider := &fakeUserProviderBW{users: map[int64]domain.UserSnapshot{
		u.ID: {Status: domain.UserStatusActive, Role: domain.RoleUser, TokenVersion: 0},
	}}
	token, err := iss.Issue(u.ID, "u@example.com", string(domain.RoleUser), 0)
	require.NoError(t, err)
	userRouter := handleruser.Router(svc, iss, provider, nil)
	authed := &bwAuthedHandler{r: userRouter, token: token}

	// Unauthenticated
	req := httptest.NewRequest(http.MethodPut, "/api/user/balance-warning-threshold", strings.NewReader(`{"balance_warning_threshold":5}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	plain := handleruser.Router(svc, iss, provider, nil)
	plain.ServeHTTP(rec, req)
	require.Equal(t, http.StatusUnauthorized, rec.Code)

	// Happy path 5 USD
	req = httptest.NewRequest(http.MethodPut, "/api/user/balance-warning-threshold", strings.NewReader(`{"balance_warning_threshold":5}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	authed.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp map[string]float64
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.InDelta(t, 5.0, resp["balance_warning_threshold"], 0.00001)

	// GET returns same
	req = httptest.NewRequest(http.MethodGet, "/api/user/balance-warning-threshold", nil)
	rec = httptest.NewRecorder()
	authed.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.InDelta(t, 5.0, resp["balance_warning_threshold"], 0.00001)

	// Disable 0
	req = httptest.NewRequest(http.MethodPut, "/api/user/balance-warning-threshold", strings.NewReader(`{"balance_warning_threshold":0}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	authed.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.InDelta(t, 0.0, resp["balance_warning_threshold"], 0.00001)
	// idempotent
	req = httptest.NewRequest(http.MethodPut, "/api/user/balance-warning-threshold", strings.NewReader(`{"balance_warning_threshold":0}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	authed.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	// Negative rejected
	req = httptest.NewRequest(http.MethodPut, "/api/user/balance-warning-threshold", strings.NewReader(`{"balance_warning_threshold":-1}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	authed.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// Rounds to zero
	req = httptest.NewRequest(http.MethodPut, "/api/user/balance-warning-threshold", strings.NewReader(`{"balance_warning_threshold":0.000001}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	authed.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// Strict decode unknown field
	req = httptest.NewRequest(http.MethodPut, "/api/user/balance-warning-threshold", strings.NewReader(`{"balance_warning_threshold":1, "unknown":1}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	authed.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// Trailing data
	req = httptest.NewRequest(http.MethodPut, "/api/user/balance-warning-threshold", strings.NewReader(`{"balance_warning_threshold":1} trailing`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	authed.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)
}

type fakeUserProviderBW struct {
	users map[int64]domain.UserSnapshot
}

func (f *fakeUserProviderBW) LoadUsers() (map[int64]domain.UserSnapshot, error) { return f.users, nil }
func (f *fakeUserProviderBW) UserSnapshot(id int64) (domain.UserSnapshot, bool) {
	s, ok := f.users[id]
	return s, ok
}

type bwAuthedHandler struct {
	r     http.Handler
	token string
}

func (a *bwAuthedHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.Header.Set("Authorization", "Bearer "+a.token)
	a.r.ServeHTTP(w, r)
}

func TestMailChannelTest_IsolationAndAuth(t *testing.T) {
	store := newFakeStore()
	svc := service.New(store, nil, service.NopInvalidator{}, nil, nil, nil, nil)
	adminAPI := New(svc)
	u, err := store.CreateUser(nil, &domain.User{Email: "u2@example.com", PasswordHash: "x", Role: domain.RoleUser, Status: domain.UserStatusActive, Balance: 50000, BalanceWarningThreshold: 100000})
	require.NoError(t, err)
	// Not configured => 500
	req := httptest.NewRequest(http.MethodPost, "/api/admin/mail/channel-test", strings.NewReader(`{"email":"to@example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	// Bypass auth by calling handler directly (no middleware)
	adminAPI.PostMailChannelTest(rec, req)
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	// Ensure threshold untouched
	got, err := store.GetUser(nil, u.ID)
	require.NoError(t, err)
	require.Equal(t, int64(100000), got.BalanceWarningThreshold)

	// Invalid email => 400
	req = httptest.NewRequest(http.MethodPost, "/api/admin/mail/channel-test", strings.NewReader(`{"email":"not-an-email"}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	adminAPI.PostMailChannelTest(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// Unknown field => 400
	req = httptest.NewRequest(http.MethodPost, "/api/admin/mail/channel-test", strings.NewReader(`{"email":"to@example.com","unknown":1}`))
	req.Header.Set("Content-Type", "application/json")
	rec = httptest.NewRecorder()
	adminAPI.PostMailChannelTest(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code)

	// Ensure threshold still untouched after all channel-test attempts
	got, err = store.GetUser(nil, u.ID)
	require.NoError(t, err)
	require.Equal(t, int64(100000), got.BalanceWarningThreshold)
}

func TestUserBalanceWarningThreshold_OverflowViaHandler(t *testing.T) {
	store := newFakeStore()
	svc := service.New(store, nil, service.NopInvalidator{}, nil, nil, nil, nil)
	u, err := store.CreateUser(nil, &domain.User{Email: "ovh@example.com", PasswordHash: "x", Role: domain.RoleUser, Status: domain.UserStatusActive, BalanceWarningThreshold: 500000})
	require.NoError(t, err)
	iss := auth.NewIssuer("test-secret-bw")
	provider := &fakeUserProviderBW{users: map[int64]domain.UserSnapshot{u.ID: {Status: domain.UserStatusActive, Role: domain.RoleUser, TokenVersion: 0}}}
	token, err := iss.Issue(u.ID, "ovh@example.com", string(domain.RoleUser), 0)
	require.NoError(t, err)
	router := handleruser.Router(svc, iss, provider, nil)
	authed := &bwAuthedHandler{r: router, token: token}
	overflow := float64(math.MaxInt64)/1e5 + 10000
	body := strings.NewReader(`{"balance_warning_threshold":` + jsonNumber(overflow) + `}`)
	req := httptest.NewRequest(http.MethodPut, "/api/user/balance-warning-threshold", body)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	authed.ServeHTTP(rec, req)
	require.Equal(t, http.StatusBadRequest, rec.Code, "overflow must 400: %s", rec.Body.String())
	got, err := store.GetUser(nil, u.ID)
	require.NoError(t, err)
	require.Equal(t, int64(500000), got.BalanceWarningThreshold, "overflow must not mutate threshold")
	req = httptest.NewRequest(http.MethodGet, "/api/user/balance-warning-threshold", nil)
	rec = httptest.NewRecorder()
	authed.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	var resp map[string]float64
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.InDelta(t, 5.0, resp["balance_warning_threshold"], 0.00001)
}

func jsonNumber(v float64) string {
	b, _ := json.Marshal(v)
	return string(b)
}

type fakeAdminStatus struct {
	roles map[int64]domain.Role
}

func (f fakeAdminStatus) UserSnapshot(id int64) (domain.UserSnapshot, bool) {
	role := f.roles[id]
	if role == "" {
		role = domain.RoleUser
	}
	return domain.UserSnapshot{Status: domain.UserStatusActive, Role: role, TokenVersion: 0}, true
}

func TestMailChannelTest_AdminAuthViaServer(t *testing.T) {
	store := newFakeStore()
	svc := service.New(store, nil, service.NopInvalidator{}, nil, nil, nil, nil)
	adminAPI := New(svc)
	iss := auth.NewIssuer("test-secret")
	adminTok, err := iss.Issue(1, "admin@example.com", string(domain.RolePlatformAdmin), 0)
	require.NoError(t, err)
	userTok, err := iss.Issue(2, "user@example.com", string(domain.RoleUser), 0)
	require.NoError(t, err)
	s := server.NewServer(server.Options{
		AdminToken:   "admin-tok",
		JWTIssuer:    iss,
		UserStatus:   fakeAdminStatus{roles: map[int64]domain.Role{1: domain.RolePlatformAdmin}},
		AdminHandler: adminAPI.Router(),
	})
	do := func(auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/admin/mail/channel-test", strings.NewReader(`{"email":"to@example.com"}`))
		req.Header.Set("Content-Type", "application/json")
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec
	}
	require.Equal(t, http.StatusUnauthorized, do("").Code, "no token must 401")
	require.Equal(t, http.StatusUnauthorized, do("Bearer wrong").Code, "wrong token must 401")
	require.Equal(t, http.StatusUnauthorized, do("Bearer "+userTok).Code, "non-admin JWT must 401")
	require.Equal(t, http.StatusInternalServerError, do("Bearer admin-tok").Code, "valid admin token reaches handler (mail not configured => 500)")
	require.Equal(t, http.StatusInternalServerError, do("Bearer "+adminTok).Code, "platform_admin JWT reaches handler => 500")
}
