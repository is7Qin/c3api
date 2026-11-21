// SPDX-License-Identifier: AGPL-3.0-or-later
package service

import (
	"testing"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/stretchr/testify/require"
)

// TestBPSValidateTemplateFixedEndpoint BPS 模板的 base_url 只接受空或固定端点。
// 格式维度的准入（哪种格式能被 BPS 表达）由 domain 的矩阵与 format_gate_test.go
// 的行为用例负责，这里不再逐格式复述。
func TestBPSValidateTemplateFixedEndpoint(t *testing.T) {
	require.ErrorIs(t, validateTemplate(&domain.Template{
		Name:             "bps",
		BaseURL:          "https://evil.example/api",
		CredentialType:   credential.TypeBPS,
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
	}), ErrInvalidInput)
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
