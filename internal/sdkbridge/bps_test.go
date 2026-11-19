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

	"github.com/alicebob/miniredis/v2"
	bpssdk "github.com/is7Qin/bps-sdk"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/egress"
)

func bpsRedisClient(t *testing.T) *redis.Client {
	t.Helper()
	mr := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: mr.Addr(), ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func TestBPSLoginGeneratesSDKProfile(t *testing.T) {
	raw, err := NewBPSLogin(nil).GenerateProfile()
	require.NoError(t, err)
	var profile bpssdk.DeviceProfile
	require.NoError(t, json.Unmarshal(raw, &profile))
	require.NoError(t, profile.Validate())
	require.NotEmpty(t, profile.ProfileID)
	require.NotEmpty(t, profile.BPSDeviceID)
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

func TestBPSPrepareSharesClientAndEvictsFatalGeneration(t *testing.T) {
	var failures []CredentialFailure
	adapter := NewBPS(bpsRedisClient(t), func(ev CredentialFailure) { failures = append(failures, ev) })
	hc := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) { return bpsHTTPResponse(), nil })}
	eg := egress.NewPool(hc).Direct()
	cred := bpsCredential(t)
	p1, err := adapter.Prepare(context.Background(), BPSRequestSource{Credential: cred, Format: domain.FormatOpenAIResponses, Egress: eg})
	require.NoError(t, err)
	p2, err := adapter.Prepare(context.Background(), BPSRequestSource{Credential: cred, Format: domain.FormatOpenAIResponses, Egress: eg})
	require.NoError(t, err)
	require.Same(t, p1.client, p2.client, "same account/K/profile/egress shares TokenAuth singleflight")
	p1.report(&bpssdk.AuthFatalError{APIError: bpssdk.APIError{Status: http.StatusUnauthorized, Class: bpssdk.ClassAuthFatal}})
	require.Len(t, failures, 1)
	p3, err := adapter.Prepare(context.Background(), BPSRequestSource{Credential: cred, Format: domain.FormatOpenAIResponses, Egress: eg})
	require.NoError(t, err)
	require.NotSame(t, p1.client, p3.client, "fatal evicts poisoned client so recovery at same K can rebuild")
	p3.report(&bpssdk.AuthFatalError{APIError: bpssdk.APIError{Status: http.StatusUnauthorized, Class: bpssdk.ClassAuthFatal}})
	require.Len(t, failures, 2, "new cache generation can report a later fatal after recovery")
	require.NoError(t, adapter.Close())
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
	adapter := NewBPS(bpsRedisClient(t))
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

	adapter := NewBPS(bpsRedisClient(t))
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
	_, err = adapter.Prepare(context.Background(), BPSRequestSource{
		Credential: cred, Format: domain.FormatOpenAIResponses,
		Egress: egress.NewPool(&http.Client{}).Direct(),
	})
	require.ErrorIs(t, err, ErrBPSEgressRequired, "nil RoundTripper must not use http.DefaultTransport")
}

func TestBPSHTTPResponsesAndStreamUseFixedEndpoint(t *testing.T) {
	var gotURL string
	hc := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		gotURL = req.URL.String()
		return bpsHTTPResponse(), nil
	})}
	adapter := NewBPS(bpsRedisClient(t))
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

// TestBPSPrepareHonoursTemplateTransport HTTP 客户端车道复用 codex 的出站传输
// 开关：模板 ws ⇒ 上游走 WebSocket；未配置/显式 http ⇒ HTTP。WS 客户端车道
// （openai-responses-ws）本来就要求 WS，不受该开关影响。
func TestBPSPrepareHonoursTemplateTransport(t *testing.T) {
	adapter := NewBPS(bpsRedisClient(t))
	eg := egress.NewPool(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return bpsHTTPResponse(), nil
	})}).Direct()
	cred := bpsCredential(t)

	for _, tc := range []struct {
		name      string
		transport domain.CodexResponsesTransport
		want      BPSTransport
	}{
		{"default", domain.CodexResponsesTransportDefault, BPSTransportHTTP},
		{"http", domain.CodexResponsesTransportHTTP, BPSTransportHTTP},
		{"ws", domain.CodexResponsesTransportWS, BPSTransportWS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prepared, err := adapter.Prepare(context.Background(), BPSRequestSource{
				Credential: cred, Format: domain.FormatOpenAIResponses, Egress: eg, Transport: tc.transport,
			})
			require.NoError(t, err)
			require.Equal(t, tc.want, prepared.Transport())
		})
	}
	require.NoError(t, adapter.Close())
}

// TestBPSLocalRejectMessage 本地拒绝必须点名原因，而不是一律 "invalid request
// body"（线上无法据此定位是哪个字段不被支持）。
func TestBPSLocalRejectMessage(t *testing.T) {
	require.Equal(t, "invalid request body", BPSLocalRejectMessage(nil))
	require.Equal(t, "invalid request body", BPSLocalRejectMessage(errors.New("opaque")))

	// 工具目录错误必须点名可执行的原因：之前一律塌成同一句话，调用方无法判断
	// 是类型不支持、名字冲突还是参数非法。
	require.Equal(t, "model not allowed: gpt-6.2-astra",
		BPSLocalRejectMessage(fmt.Errorf("%w: gpt-6.2-astra", bpssdk.ErrModelNotAllowed)))
	require.Equal(t, BPSFailureLocalReject, ClassifyBPSFailure(fmt.Errorf("%w: gpt-6.2-astra", bpssdk.ErrModelNotAllowed)).Kind,
		"模型不在目录 ⇒ 本地拒绝，不换号重试")

	detail := BPSLocalRejectMessage(fmt.Errorf("%w: unsupported tool type %q", bpssdk.ErrToolCatalogInvalid, "functon"))
	require.Contains(t, detail, "tool definitions")
	require.Contains(t, detail, `unsupported tool type "functon"`)
	require.NotContains(t, detail, "bridge:")
}

// TestBPSStreamBodyAcceptsServerSideTools 端到端钉住 SDK 的工具桥行为：客户端
// 声明 web_search（服务端工具，BPS 自己执行）时请求必须照常发出；function 工具
// 仍进 preamble。此前 catalog 阶段把服务端工具当非法条目，整单 400。
func TestBPSStreamBodyAcceptsServerSideTools(t *testing.T) {
	adapter := NewBPS(bpsRedisClient(t))
	eg := egress.NewPool(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return bpsHTTPResponse(), nil
	})}).Direct()
	prepared, err := adapter.Prepare(context.Background(), BPSRequestSource{
		Credential: bpsCredential(t), Format: domain.FormatOpenAIResponses, Egress: eg,
	})
	require.NoError(t, err)
	defer prepared.Close()

	body := []byte(`{"model":"gpt-6-astra","input":"hi","tools":[` +
		`{"type":"web_search"},` +
		`{"type":"function","name":"probe_echo","parameters":{"type":"object"}}]}`)
	resp, err := prepared.StreamBody(context.Background(), body)
	require.NoError(t, err, "服务端工具声明必须被忽略，而不是 400")
	require.NotNil(t, resp)
	require.NoError(t, resp.Body.Close())
}

// catalogHTTPResponse 是 access/catalog 端点的最小合法响应（JSON，不是 SSE）。
func catalogHTTPResponse() *http.Response {
	body := `{"allowed":true,"model_catalog":{"source":"local","models":[{"id":"m1"}]}}`
	return &http.Response{
		StatusCode: http.StatusOK, Status: "200 OK",
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(strings.NewReader(body)),
	}
}

// TestBPSModelsUnderWSTransport 钉住「传输不再被适配器当成 catalog 的前置条件」：
// catalog 是 SDK 的 HTTP 端点，ws 只是 responses 车道；三处 `requires HTTP
// format` 守卫曾让 HTTP 客户端 + ws 上游在适配器层直接失败。
func TestBPSModelsUnderWSTransport(t *testing.T) {
	adapter := NewBPS(bpsRedisClient(t))
	eg := egress.NewPool(&http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return catalogHTTPResponse(), nil
	})}).Direct()
	cred := bpsCredential(t)

	prepared, err := adapter.Prepare(context.Background(), BPSRequestSource{
		Credential: cred, Format: domain.FormatOpenAIResponses, Egress: eg,
		Transport: domain.CodexResponsesTransportWS,
	})
	require.NoError(t, err)
	require.Equal(t, BPSTransportWS, prepared.Transport())

	catalog, err := prepared.Models(context.Background())
	require.NoError(t, err, "catalog 走 HTTP，ws 传输不得被适配器拒绝")
	require.NotEmpty(t, catalog.Models)
}

// TestBPSDropsUnsupportedParams 适配器的 HTTP 车道路径必须把不支持的参数交给 SDK
// 丢弃：fake egress 捕获的上行 body 不含 previous_response_id/background/text。
func TestBPSDropsUnsupportedParams(t *testing.T) {
	var got []byte
	eg := egress.NewPool(&http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/responses") {
			body, rerr := io.ReadAll(req.Body)
			if rerr != nil {
				t.Errorf("read upstream body: %v", rerr)
			}
			got = body
		}
		return bpsHTTPResponse(), nil
	})}).Direct()
	adapter := NewBPS(bpsRedisClient(t))
	prepared, err := adapter.Prepare(context.Background(), BPSRequestSource{
		Credential: bpsCredential(t), Format: domain.FormatOpenAIResponses, Egress: eg,
	})
	require.NoError(t, err)
	defer prepared.Close()

	body := []byte(`{"model":"gpt-6-astra","input":"hi","previous_response_id":"r","background":true,"text":{"format":{"type":"json_object"}}}`)
	resp, err := prepared.StreamBody(context.Background(), body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())

	var upstream map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(got, &upstream))
	for _, banned := range []string{"previous_response_id", "background", "text"} {
		_, ok := upstream[banned]
		require.False(t, ok, "unsupported top-level %q must be dropped: %.500s", banned, string(got))
	}
	require.Contains(t, upstream, "model")
	require.Contains(t, upstream, "input")
}
