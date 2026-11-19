// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/scheduler"
)

type bpsTestStatusError int

func (e bpsTestStatusError) Error() string   { return "BPS test upstream status" }
func (e bpsTestStatusError) StatusCode() int { return int(e) }

func TestBPSResponsesPreservesProviderNeutralStatus(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code int
	}{
		{name: "rate", err: bpsTestStatusError(http.StatusTooManyRequests), code: http.StatusTooManyRequests},
		{name: "policy", err: bpsTestStatusError(http.StatusForbidden), code: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, safe := bpsErrorResponse(tc.err)
			require.Equal(t, tc.code, code)
			require.ErrorIs(t, safe, tc.err)
			// 固定前缀 + 数值状态 + 分类：WS 面直接写这句，err_logs 也落它；
			// 上游正文仍然不出现。
			require.Equal(t, fmt.Sprintf("BPS upstream rejected request (status %d, unknown)", tc.code), safe.Error())
		})
	}
}

func TestBPSResponsesNeverFallsThroughStaticCaller(t *testing.T) {
	p := &Proxy{cfg: Config{UsageCapture: false}}
	sel := &scheduler.Selection{
		AccountID:      7,
		CredentialType: credential.TypeBPS,
		Model:          "gpt-4o",
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	w := httptest.NewRecorder()
	code, _, handled, err := (&responsesCaller{p: p}).Call(
		context.Background(), w, req, "rid", 10, time.Now(), sel, "must-not-be-used", []byte(`{"model":"gpt-4o"}`), false,
	)
	require.NoError(t, err)
	require.True(t, handled)
	require.Zero(t, code)
	require.Equal(t, http.StatusServiceUnavailable, w.Code)
	require.Contains(t, w.Body.String(), "BPS responses unavailable")
}
