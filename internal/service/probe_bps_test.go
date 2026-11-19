// SPDX-License-Identifier: AGPL-3.0-or-later
package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

type fakeBPSProbe struct {
	calls  int
	req    BPSAccountProbeRequest
	result domain.ProbeResult
	err    error
}

func (f *fakeBPSProbe) ProbeAccount(_ context.Context, in BPSAccountProbeRequest) (domain.ProbeResult, error) {
	f.calls++
	f.req = in
	return f.result, f.err
}

func bpsProbeTemplate() *domain.Template {
	return &domain.Template{
		ID:               7,
		Name:             "bps",
		BaseURL:          "https://bps.example.com",
		CredentialType:   credential.TypeBPS,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses, domain.FormatOpenAIResponsesWS},
		Models:           []string{"gpt-5.6-sol"},
	}
}

func bpsProbeStore(t *testing.T, tpl *domain.Template) *probeReadOnlyStore {
	t.Helper()
	acc := &domain.Account{ID: 11, Name: "bps-acc", TemplateID: tpl.ID, Template: tpl, Enabled: true}
	return &probeReadOnlyStore{
		acc:  acc,
		ext:  &domain.TemplateExt{TemplateID: tpl.ID, CredentialType: credential.TypeBPS, CodexResponsesTransport: domain.CodexResponsesTransportWS},
		cred: &domain.AccountCredential{AccountID: 11, CredentialType: credential.TypeBPS, IdentityRevision: 1, CodexAccountID: "acct", OAuthToken: "at", OAuthRefreshToken: "rt"},
	}
}

func newBPSProbeSvc(store Store, bps BPSAccountProbe, prober AccountProber) *Service {
	return New(Deps{Store: store, Invalidate: NopInvalidator{}, Publisher: &recordingPublisher{}, EmailCodeStore: testEmailCodes, Prober: prober, BPSProbe: bps})
}

func TestProbeAccountBPSDispatches(t *testing.T) {
	tpl := bpsProbeTemplate()
	store := bpsProbeStore(t, tpl)
	bps := &fakeBPSProbe{result: domain.ProbeResult{Success: true, OutputPreview: "pong", StatusCode: 200}}
	svc := newBPSProbeSvc(store, bps, nil)

	res, err := svc.ProbeAccount(context.Background(), 11, AccountProbeRequest{Model: "gpt-5.6-sol", Format: domain.FormatOpenAIResponses})
	require.NoError(t, err)
	require.True(t, res.Success)
	require.Equal(t, 1, bps.calls)
	require.Equal(t, domain.FormatOpenAIResponses, bps.req.Format)
	require.Equal(t, "gpt-5.6-sol", bps.req.UpstreamModel)
	require.Equal(t, domain.CodexResponsesTransportWS, bps.req.Transport)
	require.NotNil(t, bps.req.Credential)
	require.Equal(t, domain.FormatOpenAIResponses, res.Format)
	require.Equal(t, "gpt-5.6-sol", res.RequestedModel)
}

func TestProbeAccountBPSFormatGate(t *testing.T) {
	tpl := bpsProbeTemplate()
	store := bpsProbeStore(t, tpl)
	bps := &fakeBPSProbe{}
	svc := newBPSProbeSvc(store, bps, nil)

	_, err := svc.ProbeAccount(context.Background(), 11, AccountProbeRequest{Model: "gpt-5.6-sol", Format: domain.FormatAnthropic})
	require.ErrorIs(t, err, ErrInvalidInput)
	require.Zero(t, bps.calls)
}

func TestProbeAccountBPSFinalizesResult(t *testing.T) {
	tpl := bpsProbeTemplate()
	store := bpsProbeStore(t, tpl)
	bps := &fakeBPSProbe{result: domain.ProbeResult{
		Success: true, StatusCode: 200, Detail: "invalid_api_key",
		OutputPreview: "see https://user:pass@example.com and Bearer sk-secret " + strings.Repeat("x", domain.ProbePreviewMaxLen+100),
	}}
	svc := newBPSProbeSvc(store, bps, nil)

	res, err := svc.ProbeAccount(context.Background(), 11, AccountProbeRequest{Model: "gpt-5.6-sol", Format: domain.FormatOpenAIResponses})
	require.NoError(t, err)
	require.Equal(t, "code=invalid_api_key", res.Detail)
	require.NotContains(t, res.OutputPreview, "example.com")
	require.Contains(t, res.OutputPreview, "Bearer [redacted]")
	require.LessOrEqual(t, len(res.OutputPreview), domain.ProbePreviewMaxLen)
	require.True(t, strings.HasPrefix(res.OutputPreview, "see [url] and"))

	bps.result.Detail = "evil; DROP TABLE"
	res, err = svc.ProbeAccount(context.Background(), 11, AccountProbeRequest{Model: "gpt-5.6-sol", Format: domain.FormatOpenAIResponses})
	require.NoError(t, err)
	require.Empty(t, res.Detail)
}

func TestProbeAccountBPSAssembly(t *testing.T) {
	tpl := bpsProbeTemplate()
	store := bpsProbeStore(t, tpl)
	svc := newBPSProbeSvc(store, nil, nil)
	_, err := svc.ProbeAccount(context.Background(), 11, AccountProbeRequest{Model: "gpt-5.6-sol", Format: domain.FormatOpenAIResponses})
	require.ErrorContains(t, err, "prober not configured")

	// prober nil but BPS probe present: the BPS path still works.
	var failures int
	bps := &fakeBPSProbe{}
	svc = newBPSProbeSvc(store, bps, nil)
	res, err := svc.ProbeAccount(context.Background(), 11, AccountProbeRequest{Model: "gpt-5.6-sol", Format: domain.FormatOpenAIResponses})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, 1, bps.calls)
	require.Zero(t, failures)
}

func TestProbeAccountBPSContextPriority(t *testing.T) {
	tpl := bpsProbeTemplate()
	store := bpsProbeStore(t, tpl)
	bps := &fakeBPSProbe{err: errors.New("boom")}
	svc := newBPSProbeSvc(store, bps, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := svc.ProbeAccount(ctx, 11, AccountProbeRequest{Model: "gpt-5.6-sol", Format: domain.FormatOpenAIResponses})
	require.NoError(t, err)
	require.NotNil(t, res)
	require.Equal(t, domain.ProbeErrorCanceled, res.Error)
	require.Zero(t, bps.calls)
}
