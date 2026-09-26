// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// 用户面与 admin 面共用 httpface.ResolveStatsWindow。这里不复制整张矩阵，只证明
// /api/user/stats 在注入时钟下：仅 window=168h → 200 且回显头双界整点、桶集合与
// 等价 from/to 相同；冲突 → 400 reason=window_ambiguous 且不带伪造字段；
// window=7d → 400 reason=window_invalid。排除端点的 from/to 仍必填、无 Window 字段。

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
	userapi "github.com/is7qin/c3api/internal/handler/user"
	"github.com/is7qin/c3api/internal/service"
	"github.com/is7qin/c3api/internal/settingssnap"
)

func TestUserStatsRelativeWindow(t *testing.T) {
	now := time.Date(2026, 9, 25, 15, 38, 12, 0, time.UTC)
	from, to := domain.WindowFromDuration(168*time.Hour, now)
	wantFrom, wantTo := from.Format(time.RFC3339), to.Format(time.RFC3339)

	store := newFakeStore()
	snap := settingssnap.New(store, nil)
	mw := service.NewMailWorker(service.MailDeps{Settings: snap, Templates: store})
	svc := service.New(store, fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil,
		service.ServiceDeps{EmailCodeStore: store, MailEnqueue: mw.Enqueue, SettingsSnapshot: snap})
	require.NoError(t, mw.Start(t.Context()))
	t.Cleanup(func() { _ = mw.Close(context.Background()) })

	iss := auth.NewIssuer("test-secret")
	api := userapi.New(svc, iss)
	api.SetClock(func() time.Time { return now })
	router := userapi.Mount(api, iss, fakeUserStatus{store: store})

	do := func(method, path, body, token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	rec := do(http.MethodPost, "/api/user/auth/register", `{"email":"win@example.com","password":"s3cret-pass"}`, "")
	require.Equal(t, http.StatusOK, rec.Code, "register: %s", rec.Body.String())
	var reg struct {
		Token string `json:"Token"`
		User  struct {
			ID int64 `json:"ID"`
		} `json:"User"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &reg))
	store.entityStats = []*domain.EntityStatBucket{
		{BucketTime: from.Add(-time.Hour), EntityType: "user", EntityID: reg.User.ID, RequestCount: 1},
		{BucketTime: from, EntityType: "user", EntityID: reg.User.ID, RequestCount: 11},
		{BucketTime: from.Add(24 * time.Hour), EntityType: "user", EntityID: reg.User.ID, RequestCount: 22},
		{BucketTime: to, EntityType: "user", EntityID: reg.User.ID, RequestCount: 44},
	}

	viaWindow := do(http.MethodGet, "/api/user/stats?window=168h&granularity=hour", "", reg.Token)
	require.Equal(t, http.StatusOK, viaWindow.Code, "body: %s", viaWindow.Body.String())
	require.Equal(t, wantFrom, viaWindow.Header().Get("X-Stats-Effective-From"))
	require.Equal(t, wantTo, viaWindow.Header().Get("X-Stats-Effective-To"))
	require.Equal(t, "cube", viaWindow.Header().Get("X-Stats-Storage"))
	require.Zero(t, from.Minute())
	require.Zero(t, to.Minute())

	viaBounds := do(http.MethodGet, "/api/user/stats?from="+wantFrom+"&to="+wantTo+"&granularity=hour", "", reg.Token)
	require.Equal(t, http.StatusOK, viaBounds.Code, "body: %s", viaBounds.Body.String())
	require.Equal(t, userBucketCounts(t, viaBounds.Body.Bytes()), userBucketCounts(t, viaWindow.Body.Bytes()))
	require.Equal(t, []int64{11, 22}, userBucketCounts(t, viaWindow.Body.Bytes()))

	ambiguous := do(http.MethodGet, "/api/user/stats?window=168h&from="+wantFrom+"&to="+wantTo, "", reg.Token)
	require.Equal(t, http.StatusBadRequest, ambiguous.Code, "body: %s", ambiguous.Body.String())
	body := map[string]any{}
	require.NoError(t, json.Unmarshal(ambiguous.Body.Bytes(), &body))
	require.Equal(t, "window_ambiguous", body["reason"])
	for _, k := range []string{"storage", "limit_seconds", "effective_from", "effective_to", "retention_days"} {
		require.NotContains(t, body, k)
	}

	invalid := do(http.MethodGet, "/api/user/stats?window=7d", "", reg.Token)
	require.Equal(t, http.StatusBadRequest, invalid.Code, "body: %s", invalid.Body.String())
	body = map[string]any{}
	require.NoError(t, json.Unmarshal(invalid.Body.Bytes(), &body))
	require.Equal(t, "window_invalid", body["reason"])
	require.NotContains(t, body, "storage")
	require.NotContains(t, body, "effective_from")
	require.NotContains(t, body, "effective_to")
	require.NotContains(t, body, "limit_seconds")
	require.NotContains(t, body, "retention_days")

	logs := do(http.MethodGet, "/api/user/usage_logs?window=24h", "", reg.Token)
	require.Equal(t, http.StatusBadRequest, logs.Code, "usage_logs 不接受 window: %s", logs.Body.String())
	errLogs := do(http.MethodGet, "/api/user/err_logs?window=24h", "", reg.Token)
	require.Equal(t, http.StatusBadRequest, errLogs.Code, "err_logs 不接受 window: %s", errLogs.Body.String())
	_, ok := reflect.TypeOf(userapi.GetUserUsageLogsParams{}).FieldByName("Window")
	require.False(t, ok)
	_, ok = reflect.TypeOf(userapi.GetUserErrLogsParams{}).FieldByName("Window")
	require.False(t, ok)
	require.IsType(t, time.Time{}, userapi.GetUserUsageLogsParams{}.From)
	_, ok = reflect.TypeOf(userapi.GetUserStatsParams{}).FieldByName("Window")
	require.True(t, ok, "纳入端点必须有 Window，防止断言写反")
}

func userBucketCounts(t *testing.T, raw []byte) []int64 {
	t.Helper()
	var pts []userapi.StatTrendPoint
	require.NoError(t, json.Unmarshal(raw, &pts))
	slices.SortFunc(pts, func(a, b userapi.StatTrendPoint) int { return a.BucketTime.Compare(*b.BucketTime) })
	out := make([]int64, 0, len(pts))
	for _, p := range pts {
		require.NotNil(t, p.RequestCount)
		out = append(out, *p.RequestCount)
	}
	return out
}
