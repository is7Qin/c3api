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
	"https://chatgpt.com/backend-api/wham/usage":               true,
}

func newProxyOfficialRewriteTransport(target string) http.RoundTripper {
	u, err := url.Parse(target)
	if err != nil {
		panic(err)
	}
	return roundTripperFuncProxy(func(req *http.Request) (*http.Response, error) {
		// fallback without assert – still preserve path/query
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
	for range 32 {
		wg.Go(func() {
			req, _ := http.NewRequest("GET", "https://chatgpt.com/backend-api/codex/responses", nil)
			_, err := tr.RoundTrip(req)
			errs <- err
		})
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

// newEgressRewriteTransport 出口池专用的上游指针：**官方 codex 端点**重写到本地
// 替身，其余请求原样放行（走 http.DefaultTransport）。
//
// 为什么需要它：混合类型夹具（同组内 codex 账号 + api_key 账号）里，api_key 面
// 的出栈目标是本地替身 URL（`Template.BaseURL` = 替身），它不在官方端点白名单里；
// 而 M3 起**两条面共用同一个出口池**（`sel.Egress.HTTP` 对 codex SDK 与 aiclient
// 是同一个 transport），所以夹具的池必须两种流量都能承载。codex-only 夹具继续用
// `...WithAssert`（只认官方端点，多了就是接线错）。
func newEgressRewriteTransport(t *testing.T, target string) http.RoundTripper {
	t.Helper()
	u, err := url.Parse(target)
	require.NoError(t, err)
	return roundTripperFuncProxy(func(req *http.Request) (*http.Response, error) {
		clone := req.Clone(req.Context())
		if proxyOfficialURLs[req.URL.String()] {
			clone.URL.Scheme = u.Scheme
			clone.URL.Host = u.Host
		}
		return http.DefaultTransport.RoundTrip(clone)
	})
}

type roundTripperFuncProxy func(*http.Request) (*http.Response, error)

func (f roundTripperFuncProxy) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// helper for the test that needs assert
func newProxyOfficialRewriteTransportAssert(t *testing.T, target string) http.RoundTripper {
	return newProxyOfficialRewriteTransportWithAssert(t, target)
}
