// SPDX-License-Identifier: AGPL-3.0-or-later
package sdkbridge

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
	require.Equal(t, "", u.Path, "target must be bare host without path; path preserved from official URL")
	require.Empty(t, u.RawQuery, "target must be bare host without query")
	return &officialRewriteTransport{targetURL: u, t: t, underlying: http.DefaultTransport}
}

func newOfficialRewriteTransportWithDial(t *testing.T, target string, underlying http.RoundTripper) http.RoundTripper {
	t.Helper()
	u, err := url.Parse(target)
	require.NoError(t, err)
	require.Equal(t, "", u.Path, "target must be bare host without path")
	require.Empty(t, u.RawQuery, "target must be bare host without query")
	return &officialRewriteTransport{targetURL: u, t: t, underlying: underlying, isDial: true}
}

type officialRewriteTransport struct {
	targetURL  *url.URL
	t          *testing.T
	underlying http.RoundTripper
	isDial     bool
}

func (rt *officialRewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	got := req.URL.String()
	if !officialURLs[got] {
		return nil, fmt.Errorf("unexpected official URL: %s (want one of %v)", got, officialURLList())
	}
	if rt.isDial && req.URL.Scheme == "wss" {
		return nil, fmt.Errorf("transport must observe https, not wss: %s", got)
	}
	clone := req.Clone(req.Context())
	clone.URL.Scheme = rt.targetURL.Scheme
	clone.URL.Host = rt.targetURL.Host
	return rt.underlying.RoundTrip(clone)
}

func officialURLList() []string {
	out := make([]string, 0, len(officialURLs))
	for k := range officialURLs {
		out = append(out, k)
	}
	return out
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestOfficialRewriteTransport_ConcurrentNoHangAndMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()

	// Happy path: 32 concurrent official URL rewrites must all succeed without hang
	tr := newOfficialRewriteTransport(t, srv.URL)
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
		t.Fatal("concurrent official rewrites hung")
	}
	for err := range errs {
		require.NoError(t, err)
	}

	// Mismatch path: must return descriptive error, not panic/hang, and be assertable from owner goroutine
	tr2 := newOfficialRewriteTransport(t, srv.URL)
	req, _ := http.NewRequest("GET", "https://chatgpt.com/backend-api/codex/responses?unexpected=1", nil)
	_, err := tr2.RoundTrip(req)
	require.Error(t, err)
	require.Contains(t, err.Error(), "unexpected official URL")
}
