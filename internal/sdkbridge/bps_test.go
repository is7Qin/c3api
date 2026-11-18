// SPDX-License-Identifier: AGPL-3.0-or-later
package sdkbridge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	bpssdk "github.com/is7Qin/bps-sdk"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/egress"
)

type bpsStoreFixture struct {
	loadKey BPSTokenKey
	saveKey BPSTokenKey
	loadSet BPSTokenSet
	loads   int
	saves   int
}

func (s *bpsStoreFixture) Load(_ context.Context, key BPSTokenKey) (BPSTokenSet, int64, error) {
	s.loads++
	s.loadKey = key
	set := s.loadSet
	if set.AccessToken == "" {
		set = BPSTokenSet{AccessToken: "at", RefreshToken: "rt", ExpiresAt: time.Now().Add(time.Hour)}
	}
	return set, 4, nil
}

func (s *bpsStoreFixture) SaveCAS(_ context.Context, key BPSTokenKey, expected int64, set BPSTokenSet) (int64, error) {
	s.saves++
	s.saveKey = key
	if expected != 4 || set.AccessToken == "" {
		return 0, errors.New("unexpected CAS input")
	}
	return 5, nil
}

func validBPSProfile(t *testing.T) []byte {
	t.Helper()
	p, err := bpssdk.GenerateDeviceProfile(bpssdk.ProfileSeed{
		Browser: bpssdk.BrowserProfile{
			Name: "edge", Major: 157,
			UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/157.0.0.0 Safari/537.36 Edg/157.0.4341.46",
			UABrands:  "Not(A:Brand,Chromium,Microsoft Edge",
			SecCHUA:   `"Not(A:Brand";v="99", "Chromium";v="157", "Microsoft Edge";v="157"`,
		},
		Office: bpssdk.OfficeProfile{Host: "Excel", Platform: "PC", Version: "16.0.20527.20196"},
		Locale: "en-US", AcceptLanguage: "en-US,en;q=0.9", Timezone: "America/New_York",
		Persona: "us-en", Sanitized: true,
	})
	require.NoError(t, err)
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	return raw
}

func bpsJWT(accountID string) string {
	return bpsJWTWithExpiry(accountID, 4102444800)
}

func bpsJWTWithExpiry(accountID string, expiry int64) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + enc([]byte(`{"chatgpt_account_id":"`+accountID+`","exp":`+fmt.Sprint(expiry)+`}`)) + ".sig"
}

func bpsCredential(t *testing.T) domain.AccountCredential {
	return domain.AccountCredential{
		AccountID: 42, CredentialType: credential.TypeBPS, IdentityRevision: 9,
		CodexAccountID: "upstream-acct", OAuthToken: bpsJWT("upstream-acct"),
		OAuthRefreshToken: "refresh-token", BPSProfile: validBPSProfile(t),
	}
}

func TestBPSFormatTransportOnlyResponses(t *testing.T) {
	httpTransport, err := BPSFormatTransport(domain.FormatOpenAIResponses)
	require.NoError(t, err)
	require.Equal(t, BPSTransportHTTP, httpTransport)
	wsTransport, err := BPSFormatTransport(domain.FormatOpenAIResponsesWS)
	require.NoError(t, err)
	require.Equal(t, BPSTransportWS, wsTransport)
	for _, format := range []domain.RequestFormat{domain.FormatOpenAIChat, domain.FormatOpenAIImages, domain.FormatOpenAISearch, domain.FormatAnthropic} {
		_, err := BPSFormatTransport(format)
		require.ErrorIs(t, err, credential.ErrUnsupported)
	}
}

func TestBPSPrepareClonesProfileUsesFixedEndpointAndFreezesFacts(t *testing.T) {
	store := &bpsStoreFixture{}
	adapter := NewBPS(store)
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return bpsHTTPResponse(), nil
	})}
	pool := egress.NewPool(hc)
	eg := pool.Direct()
	cred := bpsCredential(t)
	prepared, err := adapter.Prepare(context.Background(), BPSRequestSource{Credential: cred, Format: domain.FormatOpenAIResponses, Egress: eg})
	require.NoError(t, err)
	require.Equal(t, int64(42), prepared.AccountID())
	require.Equal(t, int64(9), prepared.IdentityRevision())
	require.Equal(t, BPSTransportHTTP, prepared.Transport())
	require.Same(t, eg, prepared.Egress())
	require.Equal(t, []byte(cred.BPSProfile), prepared.ProfileJSON())
	cred.BPSProfile[0] = 'X'
	require.NotEqual(t, cred.BPSProfile, prepared.ProfileJSON())
	require.NoError(t, prepared.Close())
}

func TestBPSPrepareFailsClosedForMissingStoreProfileAndWrongProvider(t *testing.T) {
	cred := bpsCredential(t)
	_, err := NewBPS(nil).Prepare(context.Background(), BPSRequestSource{Credential: cred, Format: domain.FormatOpenAIResponses})
	require.ErrorIs(t, err, ErrBPSStoreUnavailable)

	adapter := NewBPS(&bpsStoreFixture{})
	eg := egress.NewPool(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return bpsHTTPResponse(), nil
	})}).Direct()
	missing := cred
	missing.BPSProfile = nil
	_, err = adapter.Prepare(context.Background(), BPSRequestSource{Credential: missing, Format: domain.FormatOpenAIResponses, Egress: eg})
	require.ErrorIs(t, err, ErrBPSProfileRequired)
	wrong := cred
	wrong.CredentialType = credential.TypeCodexOAuth
	_, err = adapter.Prepare(context.Background(), BPSRequestSource{Credential: wrong, Format: domain.FormatOpenAIResponses, Egress: eg})
	require.ErrorIs(t, err, credential.ErrUnsupported)
	_, err = adapter.Prepare(context.Background(), BPSRequestSource{Credential: cred, Format: domain.FormatOpenAIResponses})
	require.ErrorIs(t, err, ErrBPSEgressRequired)
}

func TestBPSTokenStoreAdapterFencesUpstreamAndOwnerK(t *testing.T) {
	store := &bpsStoreFixture{}
	key := BPSTokenKey{AccountID: 42, IdentityRevision: 9, Owner: "c3-account:42", UpstreamAccountID: "upstream-acct"}
	adapter := bpsTokenStoreAdapter{store: store, key: key}
	set, revision, err := adapter.Load(context.Background(), "upstream-acct")
	require.NoError(t, err)
	require.Equal(t, int64(4), revision)
	require.Equal(t, "at", set.AccessToken)
	require.Equal(t, key, store.loadKey)
	_, _, err = adapter.Load(context.Background(), "other-upstream")
	require.ErrorIs(t, err, ErrBPSIdentityMismatch)
	_, err = adapter.SaveCAS(context.Background(), "upstream-acct", 4, bpssdk.TokenSet{AccessToken: "new", RefreshToken: "rt"})
	require.NoError(t, err)
	require.Equal(t, key, store.saveKey)
}

func TestBPSHTTPResponsesAndStreamUseFixedEndpoint(t *testing.T) {
	var gotURL string
	hc := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotURL = req.URL.String()
		return bpsHTTPResponse(), nil
	})}
	adapter := NewBPS(&bpsStoreFixture{})
	prepared, err := adapter.Prepare(context.Background(), BPSRequestSource{Credential: bpsCredential(t), Format: domain.FormatOpenAIResponses, Egress: egress.NewPool(hc).Direct()})
	require.NoError(t, err)
	result, err := prepared.Responses(context.Background(), []byte(`{"model":"gpt-6-astra","input":"hi"}`))
	require.NoError(t, err)
	require.Contains(t, string(result.Raw), "r-1")
	require.Equal(t, domain.FormatOpenAIResponses, result.Format)
	require.Equal(t, "https://bps.openai.com/basispoints/api/responses", gotURL)
	resp, err := prepared.StreamBody(context.Background(), []byte(`{"model":"gpt-6-astra","input":"hi"}`))
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	require.Contains(t, string(body), "response.completed")
}

func TestClassifyBPSFailurePolicyDefaultAndWrappedStatus(t *testing.T) {
	policy := &bpssdk.PolicyError{APIError: bpssdk.APIError{Status: 403, Class: bpssdk.ClassPolicyBlock}}
	failure := ClassifyBPSFailure(errors.Join(errors.New("wrapped"), policy))
	require.Equal(t, BPSFailurePolicy, failure.Kind)
	require.Equal(t, 403, failure.Status)
	require.Equal(t, 30*time.Minute, failure.RetryAfter)
	require.False(t, failure.Kind == BPSFailureAccountFatal)

	fatal := &bpssdk.AuthFatalError{APIError: bpssdk.APIError{Status: 401, Class: bpssdk.ClassAuthFatal}}
	require.Equal(t, BPSFailureAccountFatal, ClassifyBPSFailure(fatal).Kind)
	denied := &bpssdk.ModelDeniedError{APIError: bpssdk.APIError{Status: 403, Class: bpssdk.ClassModelDenied}}
	require.Equal(t, BPSFailureModelDenied, ClassifyBPSFailure(denied).Kind)
	changed := &bpssdk.ModelAccessChangedError{APIError: bpssdk.APIError{Status: 403, Class: bpssdk.ClassModelDenied}, RetryAfter: time.Minute}
	classified := ClassifyBPSFailure(changed)
	require.Equal(t, BPSFailureModelDenied, classified.Kind)
	require.Equal(t, time.Minute, classified.RetryAfter)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func bpsHTTPResponse() *http.Response {
	body := "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"r-1\",\"status\":\"completed\",\"output\":[]}}\n\n"
	return &http.Response{
		StatusCode: http.StatusOK, Status: "200 OK",
		Header: http.Header{"Content-Type": []string{"text/event-stream"}, "X-Request-ID": []string{"req-1"}},
		Body:   io.NopCloser(strings.NewReader(body)),
	}
}
