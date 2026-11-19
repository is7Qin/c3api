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

func bpsProbeSSE(output string) *http.Response {
	body := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r-1\",\"status\":\"completed\",\"output\":" + output + "}}\n\n"
	return &http.Response{
		StatusCode: http.StatusOK, Status: "200 OK",
		Header: http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:   io.NopCloser(strings.NewReader(body)),
	}
}

func bpsProbeError(status int, code string) *http.Response {
	return &http.Response{
		StatusCode: status, Status: http.StatusText(status),
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(strings.NewReader(`{"error":{"code":"` + code + `","message":"nope"}}`)),
	}
}

func bpsProbeClient(t *testing.T, resp func(*http.Request) *http.Response) *BPS {
	t.Helper()
	hc := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return resp(req), nil
	})}
	eg := egress.NewPool(hc).Direct()
	adapter := NewBPS(bpsRedisClient(t))
	t.Cleanup(func() { _ = adapter.Close() })
	t.Cleanup(func() { _ = eg })
	return adapter
}

func TestBPSProbeAccountSuccess(t *testing.T) {
	adapter := bpsProbeClient(t, func(*http.Request) *http.Response {
		return bpsProbeSSE(`[{"type":"message","content":[{"type":"output_text","text":"pong"}]},{"type":"reasoning","content":[{"type":"reasoning_text","text":"hidden"}]}]`)
	})
	eg := egress.NewPool(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return bpsProbeSSE(`[{"type":"message","content":[{"type":"output_text","text":"pong"}]}]`), nil
	})}).Direct()
	res, err := adapter.ProbeAccount(context.Background(), BPSProbeRequest{
		Credential: bpsCredential(t), Egress: eg, Format: domain.FormatOpenAIResponses,
		RequestedModel: "gpt-5.6-sol", UpstreamModel: "gpt-5.6-sol", Input: "ping",
	})
	require.NoError(t, err)
	require.True(t, res.Success)
	require.Equal(t, "pong", res.OutputPreview)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Equal(t, "gpt-5.6-sol", res.UpstreamModel)
	require.Empty(t, res.Detail)
}

func TestBPSProbeAccountMapsFailures(t *testing.T) {
	cred := bpsCredential(t)
	t.Run("auth", func(t *testing.T) {
		adapter := bpsProbeClient(t, func(*http.Request) *http.Response { return bpsProbeError(http.StatusUnauthorized, "invalid_api_key") })
		eg := egress.NewPool(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return bpsProbeError(http.StatusUnauthorized, "invalid_api_key"), nil
		})}).Direct()
		res, err := adapter.ProbeAccount(context.Background(), BPSProbeRequest{Credential: cred, Egress: eg, Format: domain.FormatOpenAIResponses, UpstreamModel: "m", Input: "x"})
		require.NoError(t, err)
		require.False(t, res.Success)
		require.Equal(t, domain.ProbeErrorUpstreamAuth, res.Error)
		require.Equal(t, "invalid_api_key", res.Detail)
		require.Equal(t, http.StatusUnauthorized, res.StatusCode)
	})
	t.Run("rate_limit", func(t *testing.T) {
		adapter := bpsProbeClient(t, func(*http.Request) *http.Response {
			return bpsProbeError(http.StatusTooManyRequests, "rate_limit_exceeded")
		})
		eg := egress.NewPool(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return bpsProbeError(http.StatusTooManyRequests, "rate_limit_exceeded"), nil
		})}).Direct()
		res, err := adapter.ProbeAccount(context.Background(), BPSProbeRequest{Credential: cred, Egress: eg, Format: domain.FormatOpenAIResponses, UpstreamModel: "m", Input: "x"})
		require.NoError(t, err)
		require.Equal(t, domain.ProbeErrorUpstreamRateLimit, res.Error)
	})
	t.Run("no_usable_output", func(t *testing.T) {
		adapter := bpsProbeClient(t, func(*http.Request) *http.Response { return bpsProbeSSE(`[]`) })
		eg := egress.NewPool(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return bpsProbeSSE(`[]`), nil
		})}).Direct()
		res, err := adapter.ProbeAccount(context.Background(), BPSProbeRequest{Credential: cred, Egress: eg, Format: domain.FormatOpenAIResponses, UpstreamModel: "m", Input: "x"})
		require.NoError(t, err)
		require.Equal(t, domain.ProbeErrorNoUsableOutput, res.Error)
		require.False(t, res.Success)
	})
	t.Run("reasoning_only", func(t *testing.T) {
		body := `[{"type":"reasoning","content":[{"type":"reasoning_text","text":"hidden"}]}]`
		adapter := bpsProbeClient(t, func(*http.Request) *http.Response { return bpsProbeSSE(body) })
		eg := egress.NewPool(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return bpsProbeSSE(body), nil
		})}).Direct()
		res, err := adapter.ProbeAccount(context.Background(), BPSProbeRequest{Credential: cred, Egress: eg, Format: domain.FormatOpenAIResponses, UpstreamModel: "m", Input: "x"})
		require.NoError(t, err)
		require.Equal(t, domain.ProbeErrorNoUsableOutput, res.Error)
	})
}

func TestBPSProbeDoesNotMutateAccount(t *testing.T) {
	t.Run("probe_401", func(t *testing.T) {
		var failures int
		adapter := NewBPS(bpsRedisClient(t), func(CredentialFailure) { failures++ })
		t.Cleanup(func() { _ = adapter.Close() })
		eg := egress.NewPool(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return bpsProbeError(http.StatusUnauthorized, "invalid_api_key"), nil
		})}).Direct()
		_, err := adapter.ProbeAccount(context.Background(), BPSProbeRequest{
			Credential: bpsCredential(t), Egress: eg, Format: domain.FormatOpenAIResponses, UpstreamModel: "m", Input: "x",
		})
		require.NoError(t, err)
		require.Zero(t, failures, "probe 401 must not write failed_at")
	})

	t.Run("prepare_claim_mismatch", func(t *testing.T) {
		var failures int
		adapter := NewBPS(bpsRedisClient(t), func(CredentialFailure) { failures++ })
		t.Cleanup(func() { _ = adapter.Close() })
		bad := bpsCredential(t)
		bad.OAuthToken = "not-a-jwt"
		eg := egress.NewPool(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return bpsHTTPResponse(), nil
		})}).Direct()
		_, err := adapter.ProbeAccount(context.Background(), BPSProbeRequest{
			Credential: bad, Egress: eg, Format: domain.FormatOpenAIResponses, UpstreamModel: "m", Input: "x",
		})
		require.Error(t, err)
		require.Zero(t, failures, "Prepare credential error must not write failed_at")
	})

	t.Run("shared_auth_across_transports", func(t *testing.T) {
		adapter := NewBPS(bpsRedisClient(t))
		t.Cleanup(func() { _ = adapter.Close() })
		cred := bpsCredential(t)
		httpEG := egress.NewPool(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return bpsHTTPResponse(), nil })}).Direct()
		wsEG := egress.NewPool(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return bpsHTTPResponse(), nil })}).Direct()
		p1, err := adapter.Prepare(context.Background(), BPSRequestSource{Credential: cred, Format: domain.FormatOpenAIResponses, Egress: httpEG})
		require.NoError(t, err)
		p2, err := adapter.Prepare(context.Background(), BPSRequestSource{Credential: cred, Format: domain.FormatOpenAIResponsesWS, Egress: wsEG})
		require.NoError(t, err)
		require.NotSame(t, p1.client, p2.client)
		require.Len(t, adapter.auths, 1, "HTTP+WS clients of one account share one TokenAuth")
	})
}
