// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/api"
)

func TestSelectErrorMapping_AttemptsExhaustedDistinct(t *testing.T) {
	require.Equal(t, http.StatusTooManyRequests, statusFor(api.ErrAttemptsExhausted))
	require.NotEqual(t, "group not found", selectErrorMessage(api.ErrAttemptsExhausted))
	require.Equal(t, "no available account", selectErrorMessage(api.ErrAttemptsExhausted))

	require.Equal(t, http.StatusTooManyRequests, statusFor(api.ErrNoAvailable))
	require.Equal(t, "no available account", selectErrorMessage(api.ErrNoAvailable))

	require.Equal(t, http.StatusNotFound, statusFor(api.ErrGroupNotFound))
	require.Equal(t, "group not found", selectErrorMessage(api.ErrGroupNotFound))

	require.Equal(t, http.StatusNotFound, statusFor(api.ErrFormatUnavailable))
	require.Equal(t, "no account supports this request format", selectErrorMessage(api.ErrFormatUnavailable))

	require.Equal(t, http.StatusServiceUnavailable, statusFor(api.ErrPlanNotReady))
	require.Equal(t, "routing plan not ready", selectErrorMessage(api.ErrPlanNotReady))
}

func TestHandleSelectErrorAttemptsExhaustedRESTSearchWS(t *testing.T) {
	for _, err := range []error{api.ErrAttemptsExhausted, api.ErrNoAvailable} {
		rec := httptest.NewRecorder()
		p := &Proxy{}
		p.handleSelectError(rec, err)
		require.Equal(t, http.StatusTooManyRequests, rec.Code)
		require.Equal(t, "1", rec.Header().Get("Retry-After"))
		require.Contains(t, rec.Body.String(), "no available account")
		require.NotContains(t, rec.Body.String(), "group not found")
	}
	for _, err := range []error{api.ErrGroupNotFound} {
		rec := httptest.NewRecorder()
		p := &Proxy{}
		p.handleSelectError(rec, err)
		require.Equal(t, http.StatusNotFound, rec.Code)
		require.Contains(t, rec.Body.String(), "group not found")
	}
	for _, err := range []error{api.ErrPlanNotReady} {
		rec := httptest.NewRecorder()
		p := &Proxy{}
		p.handleSelectError(rec, err)
		require.Equal(t, http.StatusServiceUnavailable, rec.Code)
		require.Equal(t, "1", rec.Header().Get("Retry-After"))
		require.Contains(t, rec.Body.String(), "routing plan not ready")
	}
}
