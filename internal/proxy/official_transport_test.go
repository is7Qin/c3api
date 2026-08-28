// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

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
	require.Empty(t, u.RawQuery, "target must be bare host without query")
	return &proxyOfficialRewriteTransport{targetURL: u, t: t}
}

type proxyOfficialRewriteTransport struct {
	targetURL *url.URL
	t         *testing.T
}

func (rt *proxyOfficialRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	got := req.URL.String()
	if !proxyOfficialURLs[got] {
		return nil, fmt.Errorf("unexpected official URL: %s (want one of %v)", got, proxyOfficialURLList())
	}
	clone := req.Clone(req.Context())
	clone.URL.Scheme = rt.targetURL.Scheme
	clone.URL.Host = rt.targetURL.Host
	return http.DefaultTransport.RoundTrip(clone)
}

func proxyOfficialURLList() []string {
	out := make([]string, 0, len(proxyOfficialURLs))
	for k := range proxyOfficialURLs {
		out = append(out, k)
	}
	return out
}

func TestProxyOfficialRewriteTransport_ConcurrentNoHangAndMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()
	tr := newProxyOfficialRewriteTransportWithAssert(t, srv.URL)
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req, _ := http.NewRequest("GET", "https://chatgpt.com/backend-api/codex/responses", nil)
			_, err := tr.RoundTrip(req)
			errs <- err
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done); close(errs) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent proxy rewrites hung")
	}
	for err := range errs {
		require.NoError(t, err)
	}
	req, _ := http.NewRequest("GET", "https://chatgpt.com/backend-api/codex/responses?bad=1", nil)
	_, err := tr.RoundTrip(req)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unexpected official URL")
}

type roundTripperFuncProxy func(*http.Request) (*http.Response, error)

func (f roundTripperFuncProxy) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// helper for legacy test that needs assert
func newProxyOfficialRewriteTransportAssert(t *testing.T, target string) http.RoundTripper {
	return newProxyOfficialRewriteTransportWithAssert(t, target)
}
