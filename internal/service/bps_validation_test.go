// SPDX-License-Identifier: AGPL-3.0-or-later
package service

import (
	"testing"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/stretchr/testify/require"
)

func TestBPSValidateTemplateFormatsAndFixedEndpoint(t *testing.T) {
	base := func(formats []domain.RequestFormat, endpoint string) *domain.Template {
		return &domain.Template{
			Name:             "bps",
			BaseURL:          endpoint,
			CredentialType:   credential.TypeBPS,
			SupportedFormats: formats,
		}
	}
	for _, formats := range [][]domain.RequestFormat{
		{domain.FormatOpenAIResponses},
		{domain.FormatOpenAIResponsesWS},
		{domain.FormatOpenAIResponses, domain.FormatOpenAIResponsesWS},
	} {
		require.NoError(t, validateTemplate(base(formats, "")))
		require.NoError(t, validateTemplate(base(formats, domain.BPSDefaultEndpoint)))
	}
	for _, format := range []domain.RequestFormat{
		domain.FormatOpenAIChat,
		domain.FormatOpenAIImages,
		domain.FormatOpenAISearch,
		domain.FormatAnthropic,
	} {
		require.ErrorIs(t, validateTemplate(base([]domain.RequestFormat{format}, "")), ErrInvalidInput)
	}
	require.ErrorIs(t, validateTemplate(base([]domain.RequestFormat{domain.FormatOpenAIResponses}, "https://evil.example/api")), ErrInvalidInput)
}

func TestBPSValidateAccountExtUsesOAuthProfileNotCodexIdentityOrPAT(t *testing.T) {
	valid := &domain.AccountExt{
		AccountID:       7,
		CredentialType:  credential.TypeBPS,
		CodexOAuthToken: strPtr("access-token"),
		BPSProfile:      []byte(`{"profile_id":"p","device_id":"d"}`),
	}
	require.NoError(t, validateAccountExt(valid))
	require.Nil(t, valid.CodexIdentity, "BPS must not require Codex installation identity")

	missingProfile := *valid
	missingProfile.BPSProfile = nil
	require.ErrorIs(t, validateAccountExt(&missingProfile), ErrInvalidInput)

	missingToken := *valid
	missingToken.CodexOAuthToken = nil
	require.ErrorIs(t, validateAccountExt(&missingToken), ErrInvalidInput)

	withPAT := *valid
	withPAT.CodexPATKey = strPtr("pat")
	require.ErrorIs(t, validateAccountExt(&withPAT), ErrInvalidInput)

	withScalarProfile := *valid
	withScalarProfile.BPSProfile = []byte(`"not-an-object"`)
	require.ErrorIs(t, validateAccountExt(&withScalarProfile), ErrInvalidInput)
}

func TestBPSDoesNotUseStaticKeyCapability(t *testing.T) {
	require.False(t, credential.TypeBPS.UsesStaticKey())
}
