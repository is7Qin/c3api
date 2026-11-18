// SPDX-License-Identifier: AGPL-3.0-or-later
package sdkbridge

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/egress"
)

func TestBPSRefreshUsesPreparedEgressHTTPClient(t *testing.T) {
	var urls []string
	client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		urls = append(urls, req.URL.String())
		if strings.Contains(req.URL.Path, "/oauth/token") {
			return &http.Response{
				StatusCode: http.StatusOK, Status: "200 OK", Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(`{"access_token":"refreshed","refresh_token":"refresh-2","expires_in":3600}`)),
			}, nil
		}
		return bpsHTTPResponse(), nil
	})}
	adapter := NewBPS(bpsRedisClient(t))
	cred := bpsCredential(t)
	cred.OAuthToken = bpsJWTWithExpiry("upstream-acct", 1)
	eg := egress.NewPool(client).Direct()
	prepared, err := adapter.Prepare(context.Background(), BPSRequestSource{Credential: cred, Format: domain.FormatOpenAIResponses, Egress: eg})
	require.NoError(t, err)
	_, err = prepared.Responses(context.Background(), []byte(`{"model":"gpt-6-astra","input":"hi"}`))
	require.NoError(t, err)
	var sawRefresh, sawBPS bool
	for _, url := range urls {
		sawRefresh = sawRefresh || strings.Contains(url, "auth.openai.com/oauth/token")
		sawBPS = sawBPS || strings.HasPrefix(url, domain.BPSDefaultEndpoint)
	}
	require.True(t, sawRefresh, "refresh must use the prepared egress HTTP client: %v", urls)
	require.True(t, sawBPS, "upstream call must use the fixed BPS endpoint: %v", urls)
}
