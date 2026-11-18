package domain

import (
	"encoding/json"
	"testing"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/stretchr/testify/require"
)

func TestIdentityEnvelopeJSONRoundTripPreservesUnknownKeys(t *testing.T) {
	raw := []byte(`{"unknown":{"keep":true},"bps":{"profile_id":"p"},"installation_id":"inst"}`)
	var env IdentityEnvelope
	require.NoError(t, json.Unmarshal(raw, &env))
	encoded, err := json.Marshal(env)
	require.NoError(t, err)
	require.JSONEq(t, string(raw), string(encoded))
}

func TestIdentityEnvelopeCanonicalMergePreservesUnknownKeys(t *testing.T) {
	raw := []byte(`{"unknown":{"keep":true},"bps":{"profile_id":"old"},"installation_id":"inst-1"}`)
	profile := json.RawMessage(`{"z":1,"a":"new"}`)
	got, err := MergeIdentityEnvelope(raw, IdentityPatch{BPSProfile: profile, SetBPSProfile: true})
	require.NoError(t, err)
	require.JSONEq(t, `{"unknown":{"keep":true},"bps":{"z":1,"a":"new"},"installation_id":"inst-1"}`, string(got))
	env, err := ParseIdentityEnvelope(got)
	require.NoError(t, err)
	require.JSONEq(t, string(profile), string(env.BPSProfile()))

	withInstallation, err := env.MergeCodexInstallation("inst-2")
	require.NoError(t, err)
	require.Equal(t, "inst-2", withInstallation.InstallationID())
	require.JSONEq(t, string(profile), string(withInstallation.BPSProfile()))
	require.JSONEq(t, `{"keep":true}`, string(withInstallation["unknown"]))
	withReplacement, err := withInstallation.ReplaceBPSProfile([]byte(`{"profile_id":"replacement"}`))
	require.NoError(t, err)
	require.Equal(t, "inst-2", withReplacement.InstallationID())
	require.JSONEq(t, `{"profile_id":"replacement"}`, string(withReplacement.BPSProfile()))
	require.JSONEq(t, `{"keep":true}`, string(withReplacement["unknown"]))
}

func TestIdentityEnvelopeCanonicalHashIgnoresObjectOrder(t *testing.T) {
	a := []byte(`{"b":2,"a":1}`)
	b := []byte(` { "a": 1, "b": 2 } `)
	require.True(t, BPSProfileJSONEqual(a, b))
	require.Equal(t, BPSProfileHash(a), BPSProfileHash(b))
	require.True(t, BPSProfileJSONEqual([]byte(`{"n":1.2300,"e":1e3}`), []byte(`{"e":1000,"n":1.23}`)))
	require.NoError(t, ValidateBPSProfileJSON(a))
	require.Error(t, ValidateBPSProfileJSON([]byte(`[]`)))
}

func TestBPSFixedEndpointAndFormatDerivedTransport(t *testing.T) {
	tpl := Template{CredentialType: credential.TypeBPS, SupportedFormats: []RequestFormat{FormatOpenAIChat, FormatOpenAIResponses, FormatOpenAIResponsesWS}}
	require.False(t, tpl.FormatSupports(FormatOpenAIChat, "m"))
	require.True(t, tpl.FormatSupports(FormatOpenAIResponses, "m"))
	require.True(t, tpl.FormatSupports(FormatOpenAIResponsesWS, "m"))
	require.True(t, BPSBaseURLAllowed(""))
	require.True(t, BPSBaseURLAllowed(BPSDefaultEndpoint))
	require.False(t, BPSBaseURLAllowed("https://example.invalid/bps"))
	require.False(t, BPSUsesWebSocket(FormatOpenAIResponses))
	require.True(t, BPSUsesWebSocket(FormatOpenAIResponsesWS))
}

func TestCredentialFromExtBPSProjection(t *testing.T) {
	profile := json.RawMessage(`{"profile_id":"dp_x","nested":{"keep":true}}`)
	account := "acct"
	ext := &AccountExt{
		AccountID:              7,
		CredentialType:         credential.TypeBPS,
		CodexAccountID:         &account,
		CodexOAuthToken:        ptr("at"),
		CodexOAuthRefreshToken: ptr("rt"),
		BPSProfile:             profile,
	}
	got := CredentialFromExt(ext)
	require.Equal(t, "acct", got.CodexAccountID)
	require.Equal(t, "at", got.OAuthToken)
	require.Equal(t, "rt", got.OAuthRefreshToken)
	require.NotEmpty(t, got.BPSProfile)
	require.Equal(t, string(profile), string(got.BPSProfile))
	require.Empty(t, got.InstallationID)
	got.BPSProfile[0] = 'X'
	require.Equal(t, string(profile), string(ext.BPSProfile))
}

func ptr(v string) *string { return &v }
