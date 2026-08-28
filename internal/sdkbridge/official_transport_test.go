// SPDX-License-Identifier: AGPL-3.0-or-later
package sdkbridge

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

var officialURLs = map[string]bool{
	"https://chatgpt.com/backend-api/codex/responses":               true,
	"https://chatgpt.com/backend-api/codex/alpha/search":            true,
	"https://chatgpt.com/backend-api/codex/images/generations":      true,
	"https://chatgpt.com/backend-api/codex/images/edits":            true,
	"https://chatgpt.com/backend-api/wham/usage":                     true,
}

func newOfficialRewriteTransport(t *testing.T, target string) http.RoundTripper {
	t.Helper()
	u, err := url.Parse(target)
	require.NoError(t, err)
	return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		got := req.URL.String()
		require.True(t, officialURLs[got], "unexpected official URL: %s", got)
		clone := req.Clone(req.Context())
		clone.URL.Scheme = u.Scheme
		clone.URL.Host = u.Host
		// preserve Path and RawQuery via Clone (only Scheme/Host rewritten)
		require.Equal(t, u.Path, "", "target must be bare host without path; path preserved from official URL")
		return http.DefaultTransport.RoundTrip(clone)
	})
}

func newOfficialRewriteTransportWithDial(t *testing.T, target string, underlying http.RoundTripper) http.RoundTripper {
	t.Helper()
	u, err := url.Parse(target)
	require.NoError(t, err)
	return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		got := req.URL.String()
		// Dial HTTPS upgrade is observed as https, not wss
		require.True(t, officialURLs[got], "unexpected official URL for Dial: %s", got)
		require.NotEqual(t, "wss", req.URL.Scheme, "transport must observe https, not wss")
		clone := req.Clone(req.Context())
		clone.URL.Scheme = u.Scheme
		clone.URL.Host = u.Host
		return underlying.RoundTrip(clone)
	})
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }
