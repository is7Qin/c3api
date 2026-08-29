// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/scheduler"
)

func TestSelectErrorMapping_AttemptsExhaustedDistinct(t *testing.T) {
	require.Equal(t, http.StatusTooManyRequests, statusFor(scheduler.ErrAttemptsExhausted))
	require.NotEqual(t, "group not found", selectErrorMessage(scheduler.ErrAttemptsExhausted))
	require.Equal(t, "no available account", selectErrorMessage(scheduler.ErrAttemptsExhausted))

	require.Equal(t, http.StatusTooManyRequests, statusFor(scheduler.ErrNoAvailable))
	require.Equal(t, "no available account", selectErrorMessage(scheduler.ErrNoAvailable))

	require.Equal(t, http.StatusNotFound, statusFor(scheduler.ErrGroupNotFound))
	require.Equal(t, "group not found", selectErrorMessage(scheduler.ErrGroupNotFound))

	require.Equal(t, http.StatusNotFound, statusFor(scheduler.ErrFormatUnavailable))
	require.Equal(t, "no account supports this request format", selectErrorMessage(scheduler.ErrFormatUnavailable))
}

func TestHandleSelectErrorAttemptsExhaustedRESTSearchWS(t *testing.T) {
	for _, err := range []error{scheduler.ErrAttemptsExhausted, scheduler.ErrNoAvailable} {
		rec := httptest.NewRecorder()
		p := &Proxy{}
		p.handleSelectError(rec, err)
		require.Equal(t, http.StatusTooManyRequests, rec.Code)
		require.Equal(t, "1", rec.Header().Get("Retry-After"))
		require.Contains(t, rec.Body.String(), "no available account")
		require.NotContains(t, rec.Body.String(), "group not found")
	}
	for _, err := range []error{scheduler.ErrGroupNotFound} {
		rec := httptest.NewRecorder()
		p := &Proxy{}
		p.handleSelectError(rec, err)
		require.Equal(t, http.StatusNotFound, rec.Code)
		require.Contains(t, rec.Body.String(), "group not found")
	}
}
