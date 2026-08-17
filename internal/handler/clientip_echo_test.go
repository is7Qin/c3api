// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// client_ip 回显透传（gate M3/m4，spec 2026-08-17 S-E）：四转换器（管理面
// toAPIUsageLog/toAPIErrLog + 用户面 toAPIUsageLog/toAPIErrLog）经 logs 端点
// 端到端回显——store 灌入带 ClientIP 的域行 → HTTP 响应 ClientIP 非空。
// repo 查询映射红绿由 PG roundtrip 承载（pg_clientip_test.go）。

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	userapi "github.com/is7qin/c3api/internal/handler/user"
)

// TestAdminUsageLogsClientIPEcho 管理面 /admin/usage_logs 回显 client_ip
// （toAPIUsageLog 投影）。
func TestAdminUsageLogsClientIPEcho(t *testing.T) {
	doAdmin, _, store := newSharedRouters(t)
	base := time.Now().UTC().Truncate(time.Second)
	store.mu.Lock()
	store.logs = []*domain.UsageLog{
		{ID: 1, UserID: 7, RequestID: "cip-adm-u1", Model: "gpt-4o", Format: domain.FormatOpenAIChat,
			ClientIP: "9.9.9.9", CreatedAt: base},
	}
	store.mu.Unlock()
	win := "from=" + base.Add(-time.Hour).Format(time.RFC3339) + "&to=" + base.Add(time.Hour).Format(time.RFC3339)

	rec := doAdmin(http.MethodGet, "/admin/usage_logs?"+win, "", "")
	require.Equal(t, http.StatusOK, rec.Code, "logs: %s", rec.Body.String())
	var body LogsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Rows, 1)
	require.Equal(t, "9.9.9.9", *body.Rows[0].ClientIP, "管理面 usage 行回显 client_ip 非空（四转换器之一）")
}

// TestAdminErrLogsClientIPEcho 管理面 /admin/err_logs 回显 client_ip
// （toAPIErrLog 投影）。
func TestAdminErrLogsClientIPEcho(t *testing.T) {
	doAdmin, _, store := newSharedRouters(t)
	base := time.Now().UTC().Truncate(time.Second)
	store.mu.Lock()
	msg := "no available account"
	store.logs = []*domain.UsageLog{
		{ID: 7, UserID: 3, RequestID: "cip-adm-e1", Model: "gpt-4o", Format: domain.FormatOpenAIChat,
			StatusCode: 429, ErrorType: domain.Err429, ErrorMessage: &msg,
			ClientIP: "9.9.9.9", CreatedAt: base},
	}
	store.mu.Unlock()
	win := "from=" + base.Add(-time.Hour).Format(time.RFC3339) + "&to=" + base.Add(time.Hour).Format(time.RFC3339)

	rec := doAdmin(http.MethodGet, "/admin/err_logs?"+win, "", "")
	require.Equal(t, http.StatusOK, rec.Code, "err logs: %s", rec.Body.String())
	var body ErrLogsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Rows, 1)
	require.Equal(t, "9.9.9.9", *body.Rows[0].ClientIP, "管理面 err 行回显 client_ip 非空（四转换器之二）")
}

// TestUserUsageLogsClientIPEcho 用户面 /user/usage_logs 回显 client_ip
// （用户面 toAPIUsageLog → UserUsageLog 投影）。
func TestUserUsageLogsClientIPEcho(t *testing.T) {
	_, doUser, store := newSharedRouters(t)
	tokenA, userA := registerAndGet(t, doUser, "cip-echo@example.com")
	base := time.Now().UTC().Truncate(time.Second)
	store.mu.Lock()
	store.logs = []*domain.UsageLog{
		{ID: 1, UserID: userA, RequestID: "cip-u1", Model: "gpt-4o", Format: domain.FormatOpenAIChat,
			ClientIP: "9.9.9.9", CreatedAt: base},
	}
	store.mu.Unlock()
	win := "from=" + base.Add(-time.Hour).Format(time.RFC3339) + "&to=" + base.Add(time.Hour).Format(time.RFC3339)

	rec := doUser(http.MethodGet, "/user/usage_logs?"+win, "", tokenA)
	require.Equal(t, http.StatusOK, rec.Code, "user logs: %s", rec.Body.String())
	var body userapi.UserLogsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Rows, 1)
	require.Equal(t, "9.9.9.9", *body.Rows[0].ClientIP, "用户面 usage 行回显 client_ip 非空（四转换器之三）")
}

// TestUserErrLogsClientIPEcho 用户面 /user/err_logs 回显 client_ip（用户面
// toAPIErrLog → UserErrLog 投影——漏则用户面错误明细回显恒缺，gate m4）。
func TestUserErrLogsClientIPEcho(t *testing.T) {
	_, doUser, store := newSharedRouters(t)
	tokenA, userA := registerAndGet(t, doUser, "cip-echo-err@example.com")
	base := time.Now().UTC().Truncate(time.Second)
	store.mu.Lock()
	msg := "invalid gateway key"
	store.logs = []*domain.UsageLog{
		{ID: 1, UserID: userA, RequestID: "cip-e1", Model: "gpt-4o", Format: domain.FormatOpenAIChat,
			StatusCode: 401, ErrorType: domain.ErrAuth, ErrorMessage: &msg,
			ClientIP: "9.9.9.9", CreatedAt: base},
	}
	store.mu.Unlock()
	win := "from=" + base.Add(-time.Hour).Format(time.RFC3339) + "&to=" + base.Add(time.Hour).Format(time.RFC3339)

	rec := doUser(http.MethodGet, "/user/err_logs?"+win, "", tokenA)
	require.Equal(t, http.StatusOK, rec.Code, "user err logs: %s", rec.Body.String())
	var body userapi.UserErrLogsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Rows, 1)
	require.Equal(t, "9.9.9.9", *body.Rows[0].ClientIP, "用户面 err 行回显 client_ip 非空（四转换器之四）")
}
