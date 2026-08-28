// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"net/http"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"
)

var proxyOfficialURLs = map[string]bool{
	"https://chatgpt.com/backend-api/codex/responses":          true,
	"https://chatgpt.com/backend-api/codex/alpha/search":       true,
	"https://chatgpt.com/backend-api/codex/images/generations": true,
	"https://chatgpt.com/backend-api/codex/images/edits":       true,
	"https://chatgpt.com/backend-api/wham/usage":                true,
}

func newProxyOfficialRewriteTransport(target string) http.RoundTripper {
	u, err := url.Parse(target)
	if err != nil {
		panic(err)
	}
	return roundTripperFuncProxy(func(req *http.Request) (*http.Response, error) {
		// fallback without assert for legacy call sites – still preserve path/query
		clone := req.Clone(req.Context())
		clone.URL.Scheme = u.Scheme
		clone.URL.Host = u.Host
		return http.DefaultTransport.RoundTrip(clone)
	})
}

func newProxyOfficialRewriteTransportWithAssert(t *testing.T, target string) http.RoundTripper {
	t.Helper()
	u, err := url.Parse(target)
	require.NoError(t, err)
	require.Equal(t, "", u.Path, "target must be bare host without path; path preserved from official URL")
	return roundTripperFuncProxy(func(req *http.Request) (*http.Response, error) {
		got := req.URL.String()
		require.True(t, proxyOfficialURLs[got], "unexpected official URL: %s", got)
		clone := req.Clone(req.Context())
		clone.URL.Scheme = u.Scheme
		clone.URL.Host = u.Host
		// preserve Path and RawQuery from official URL
		return http.DefaultTransport.RoundTrip(clone)
	})
}

type roundTripperFuncProxy func(*http.Request) (*http.Response, error)

func (f roundTripperFuncProxy) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// helper for legacy test that needs assert
func newProxyOfficialRewriteTransportAssert(t *testing.T, target string) http.RoundTripper {
	return newProxyOfficialRewriteTransportWithAssert(t, target)
}
