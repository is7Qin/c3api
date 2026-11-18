package repository

import (
	"encoding/json"
	"testing"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/stretchr/testify/require"
)

func TestIdentityWritersPreserveProviderAndUnknownKeys(t *testing.T) {
	cur := &ent.AccountExt{CodexIdentity: &domain.IdentityEnvelope{
		"installation_id": json.RawMessage(`"old-installation"`),
		"bps":             json.RawMessage(`{"profile_id":"old-profile","unknown_profile":true}`),
		"future":          json.RawMessage(`{"keep": [1, 2]}`),
	}}
	codex := &domain.AccountExt{
		CredentialType: credential.TypeCodexOAuth,
		CodexIdentity:  &domain.CodexIdentity{InstallationID: "new-installation"},
	}
	merged, err := mergeIdentityForWrite(cur, codex)
	require.NoError(t, err)
	require.JSONEq(t, `{"installation_id":"new-installation","bps":{"profile_id":"old-profile","unknown_profile":true},"future":{"keep":[1,2]}}`, string(mustCanonicalIdentity(t, merged)))

	at, rt, upstream := "new-at", "new-rt", "chatgpt-account"
	bps := &domain.AccountExt{
		CredentialType:         credential.TypeBPS,
		CodexOAuthToken:        &at,
		CodexOAuthRefreshToken: &rt,
		CodexAccountID:         &upstream,
		BPSProfile:             json.RawMessage(` { "profile_id": "new-profile", "extra": "preserve" } `),
	}
	merged, err = mergeIdentityForWrite(cur, bps)
	require.NoError(t, err)
	require.JSONEq(t, `{"installation_id":"old-installation","bps":{"profile_id":"new-profile","extra":"preserve"},"future":{"keep":[1,2]}}`, string(mustCanonicalIdentity(t, merged)))
}

func TestMalformedBPSIdentityProjectionFailsClosed(t *testing.T) {
	at, rt, upstream := "at", "rt", "account"
	ext := &domain.AccountExt{AccountID: 7, CredentialType: credential.TypeBPS, CodexOAuthToken: &at, CodexOAuthRefreshToken: &rt, CodexAccountID: &upstream}
	err := projectAccountIdentity(ext, &domain.IdentityEnvelope{"bps": json.RawMessage(`[]`)})
	require.ErrorIs(t, err, errInvalidBPSAccountExt)
}

func TestBPSIdentityWriteRequiresCompleteStructuralProjection(t *testing.T) {
	at, rt, upstream := "at", "rt", "account"
	_, err := mergeIdentityForWrite(nil, &domain.AccountExt{
		CredentialType:         credential.TypeBPS,
		CodexOAuthToken:        &at,
		CodexOAuthRefreshToken: &rt,
		CodexAccountID:         &upstream,
	})
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestIdentityEnvelopeSemanticChangeIgnoresJSONFormatting(t *testing.T) {
	cur := &ent.AccountExt{CodexIdentity: &domain.IdentityEnvelope{
		"bps": json.RawMessage(`{"a":1,"b":[true]}`),
	}}
	next := &domain.IdentityEnvelope{
		"bps": json.RawMessage(` { "b": [ true ], "a": 1 } `),
	}
	changed, err := identityEnvelopeChanged(cur, next)
	require.NoError(t, err)
	require.False(t, changed)
}

func mustCanonicalIdentity(t *testing.T, env *domain.IdentityEnvelope) []byte {
	t.Helper()
	raw, err := env.CanonicalJSON()
	require.NoError(t, err)
	return raw
}
