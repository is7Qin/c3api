// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/scheduler"
)

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
