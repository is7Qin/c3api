// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package httpface

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	serviceerr "github.com/is7qin/c3api/internal/service/errors"
)

// TestWriteJSON 契约：Content-Type application/json + encoder 编码含尾换行
// （与 handler/server 历史 writeJSON 副本逐字节一致——各包既有用例零回归的前提）。
func TestWriteJSON(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteJSON(rec, http.StatusCreated, map[string]any{"ok": true})
	require.Equal(t, http.StatusCreated, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	require.Equal(t, "{\"ok\":true}\n", rec.Body.String(), "encoder 编码必须含尾换行")
}

// TestWriteErr JSON 信封 {"error": msg} + encoder 编码（含尾换行）。
func TestWriteErr(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteErr(rec, http.StatusForbidden, "forbidden")
	require.Equal(t, http.StatusForbidden, rec.Code)
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
	require.Equal(t, "{\"error\":\"forbidden\"}\n", rec.Body.String())
}

// TestWriteServiceErr 映射表全分支（含 default internal error）+ %w 包装链
// 命中 + Content-Type/尾换行契约。
func TestWriteServiceErr(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
		body   string
	}{
		{"not found", serviceerr.ErrNotFound, http.StatusNotFound, "{\"error\":\"service: not found\"}\n"},
		{"not found wrapped", fmt.Errorf("id=5 missing: %w", serviceerr.ErrNotFound), http.StatusNotFound, "{\"error\":\"id=5 missing: service: not found\"}\n"},
		{"invalid input", serviceerr.ErrInvalidInput, http.StatusBadRequest, "{\"error\":\"service: invalid input\"}\n"},
		{"conflict", serviceerr.ErrConflict, http.StatusConflict, "{\"error\":\"service: conflict\"}\n"},
		{"invalid credentials", serviceerr.ErrInvalidCredentials, http.StatusUnauthorized, "{\"error\":\"service: invalid email or password\"}\n"},
		{"signup disabled", serviceerr.ErrSignupDisabled, http.StatusForbidden, "{\"error\":\"service: signup disabled\"}\n"},
		{"default internal error", errors.New("boom"), http.StatusInternalServerError, "{\"error\":\"internal error\"}\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			WriteServiceErr(rec, tc.err)
			require.Equal(t, tc.status, rec.Code)
			require.Equal(t, "application/json", rec.Header().Get("Content-Type"))
			require.Equal(t, tc.body, rec.Body.String(), "响应体必须逐字节精确")
		})
	}
}

// TestWriteServiceErrWindowFields 400 机读字段（spec §4.4(c)/(d)）：四类拒绝各自的
// 字段集合，逐字节精确。载体用合成构造——**不走墙钟**（coverage 拒绝的 cutoff
// 与生效窗口由固定字面量给出）。
//
// `window_invalid` 一例是 J2b 的判据：参数本身非法时**没有**存储/上限/保留期/
// 生效窗口可言，故这些字段必须**省略**，不得序列化 `"storage":"cube"`
// （StatsStorage 零值）或 `limit_seconds:0` 这类伪造事实。
func TestWriteServiceErrWindowFields(t *testing.T) {
	// 分组原始行的成本上限（8d）与其十进制文本——两处都用常量推导，
	// 使本包不含任何上限秒值字面量。
	groupedRawCapSec := int64(domain.MaxGroupedRawSpan / time.Second)
	capSecText := strconv.FormatInt(groupedRawCapSec, 10)

	effFrom := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	effTo := time.Date(2026, 8, 9, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		err  error
		body string
	}{
		{
			"window_invalid 只报 reason",
			&serviceerr.StatsWindowError{StatsWindowError: domain.StatsWindowError{
				Kind: domain.KindTrend, Reject: domain.StatsRejectWindowInvalid,
				EffectiveFrom: effFrom, EffectiveTo: effTo}},
			"{\"error\":\"service: stats window invalid: from/to must be set and from \\u003c to, or window must be a duration string\",\"reason\":\"window_invalid\"}\n",
		},
		{
			// 上限秒数取自矩阵常量（本包不出现裸的上限秒值字面量——A16'② 的
			// 零命中审计覆盖 internal/handler 全部文件；绝对值金标准在 internal/domain）。
			"window_too_long 携带 storage/limit_seconds/effective_*",
			&serviceerr.StatsWindowError{StatsWindowError: domain.StatsWindowError{
				Kind: domain.KindTrend, Reject: domain.StatsRejectWindowTooLong,
				Storage: domain.StatsStorageRaw, LimitSeconds: groupedRawCapSec,
				EffectiveFrom: effFrom, EffectiveTo: effTo}},
			"{\"error\":\"service: stats window too long: trend raw supports windows up to 192h0m0s\"," +
				"\"reason\":\"window_too_long\",\"storage\":\"raw\",\"limit_seconds\":" + capSecText + "," +
				"\"effective_from\":\"2026-08-01T00:00:00Z\",\"effective_to\":\"2026-08-09T00:00:00Z\"}\n",
		},
		{
			"raw_horizon 携带 retention_days 而非 limit_seconds",
			&serviceerr.StatsWindowError{StatsWindowError: domain.StatsWindowError{
				Kind: domain.KindUsageList, Reject: domain.StatsRejectRawHorizon,
				Storage: domain.StatsStorageRaw, Cutoff: effFrom, RetentionDays: 2,
				EffectiveFrom: effFrom, EffectiveTo: effTo}},
			"{\"error\":\"service: stats raw window starts before retained partitions" +
				" (cutoff 2026-08-01T00:00:00Z, retention 2d)\",\"reason\":\"raw_horizon\",\"storage\":\"raw\"," +
				"\"effective_from\":\"2026-08-01T00:00:00Z\",\"effective_to\":\"2026-08-09T00:00:00Z\"," +
				"\"retention_days\":2}\n",
		},
		{
			"cube_horizon storage=cube",
			&serviceerr.StatsWindowError{StatsWindowError: domain.StatsWindowError{
				Kind: domain.KindTop, Reject: domain.StatsRejectCubeHorizon,
				Storage: domain.StatsStorageCube, Cutoff: effFrom, RetentionDays: 30,
				EffectiveFrom: effFrom, EffectiveTo: effTo}},
			"{\"error\":\"service: stats cube window starts before retained partitions" +
				" (cutoff 2026-08-01T00:00:00Z, retention 30d)\",\"reason\":\"cube_horizon\",\"storage\":\"cube\"," +
				"\"effective_from\":\"2026-08-01T00:00:00Z\",\"effective_to\":\"2026-08-09T00:00:00Z\"," +
				"\"retention_days\":30}\n",
		},
		{
			"包装链上的载体仍被 errors.As 取到（禁字符串嗅探）",
			fmt.Errorf("query failed: %w", &serviceerr.StatsWindowError{StatsWindowError: domain.StatsWindowError{
				Kind: domain.KindTrend, Reject: domain.StatsRejectWindowInvalid}}),
			"{\"error\":\"query failed: service: stats window invalid: from/to must be set and from " +
				"\\u003c to, or window must be a duration string\",\"reason\":\"window_invalid\"}\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			WriteServiceErr(rec, tc.err)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Equal(t, tc.body, rec.Body.String(), "响应体必须逐字节精确")
		})
	}
}
