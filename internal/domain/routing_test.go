// SPDX-License-Identifier: AGPL-3.0-or-later
package domain

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
)

func TestRoutingRouteClassGolden(t *testing.T) {
	id, err := RouteClassID(42, FormatOpenAIChat, "gpt-4o", OpChatCompletions)
	require.NoError(t, err)
	require.Equal(t, "fa87b2998fb6a5b5a5a967b6def64d778f2a5a7619120205f43bfa370632b5a6", RouteClassIDHex(id))
	id2, err := RouteClassID(42, FormatOpenAIChat, "gpt-4o", OpChatCompletions)
	require.NoError(t, err)
	require.Equal(t, id, id2)
	id3, err := RouteClassID(42, FormatOpenAIResponses, "o3", OpResponses)
	require.NoError(t, err)
	require.Equal(t, "cf0467b3595157fdfae2ea3940c5336d01ace0dc8035fccea09328ba68732d0f", RouteClassIDHex(id3))
}

func TestRoutingRouteClassComposition(t *testing.T) {
	base, _ := RouteClassID(1, FormatOpenAIChat, "gpt-4o", OpChatCompletions)
	changedGroup, _ := RouteClassID(2, FormatOpenAIChat, "gpt-4o", OpChatCompletions)
	require.NotEqual(t, base, changedGroup, "group_id changes ID")
	changedFormat, _ := RouteClassID(1, FormatOpenAIResponses, "gpt-4o", OpChatCompletions)
	require.NotEqual(t, base, changedFormat, "client_format changes ID")
	changedModel, _ := RouteClassID(1, FormatOpenAIChat, "gpt-4o-mini", OpChatCompletions)
	require.NotEqual(t, base, changedModel, "requested_model changes ID")
	changedOp, _ := RouteClassID(1, FormatOpenAIChat, "gpt-4o", OpResponses)
	require.NotEqual(t, base, changedOp, "operation_tag changes ID")
}

func TestRoutingQualityClassGolden(t *testing.T) {
	qid, err := QualityClassID(CallerChat, FormatOpenAIChat, "gpt-4o", OpChatCompletions)
	require.NoError(t, err)
	require.Equal(t, "847bba3ba902936e2c794ec31b3ad169eae8892d2881f3f86d02069d8d27897a", QualityClassIDHex(qid))
}

func TestRoutingQualityClassComposition(t *testing.T) {
	base, _ := QualityClassID(CallerChat, FormatOpenAIChat, "gpt-4o", OpChatCompletions)
	changedCaller, _ := QualityClassID(CallerResponses, FormatOpenAIChat, "gpt-4o", OpChatCompletions)
	require.NotEqual(t, base, changedCaller)
	changedFormat, _ := QualityClassID(CallerChat, FormatAnthropic, "gpt-4o", OpChatCompletions)
	require.NotEqual(t, base, changedFormat)
	changedModel, _ := QualityClassID(CallerChat, FormatOpenAIChat, "o3", OpChatCompletions)
	require.NotEqual(t, base, changedModel, "resolved model changes quality class")
	changedOp, _ := QualityClassID(CallerChat, FormatOpenAIChat, "gpt-4o", OpResponses)
	require.NotEqual(t, base, changedOp)
}

func TestRoutingFingerprintGolden(t *testing.T) {
	fp, err := CandidateFingerprint(1, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-abc123", "", "", "", false, "inst", "sess", "thr", "win")
	require.NoError(t, err)
	require.Equal(t, "950eabbcaf4c61629bde89e96855d9aa602ade67a086f049eed3670fa8798011", CandidateFPHex(fp))
	fp2, err := CandidateFingerprint(1, 10, credential.TypeCodexPAT, "https://api.openai.com", "", "pat_abc", "", "", false, "inst", "sess", "thr", "win")
	require.NoError(t, err)
	require.Equal(t, "9c7021f9b5c6817d49bb8ea4dbc0a9964f1c6c6d9e0d050713af6aaff85cd9ac", CandidateFPHex(fp2))
	fp3, err := CandidateFingerprint(1, 10, credential.TypeCodexOAuth, "https://api.openai.com", "", "", "user@example.com", "acc-123", false, "install-1", "sess1", "thr1", "win1")
	require.NoError(t, err)
	require.Equal(t, "8a0849c54c65ebc2ac97d7da2931d47f5e80c55b3dcfdc023381a033dc9d5c8b", CandidateFPHex(fp3))
	// email must not affect oauth fingerprint
	fp3b, err := CandidateFingerprint(1, 10, credential.TypeCodexOAuth, "https://api.openai.com", "", "", "other@example.com", "acc-123", false, "install-1", "sess1", "thr1", "win1")
	require.NoError(t, err)
	require.Equal(t, fp3, fp3b, "oauth email must not change fingerprint")
}

func TestRoutingFingerprintInvalidation(t *testing.T) {
	base, _ := CandidateFingerprint(1, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-abc123", "", "", "", false, "inst", "sess", "thr", "win")
	changedAccount, _ := CandidateFingerprint(2, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-abc123", "", "", "", false, "inst", "sess", "thr", "win")
	require.NotEqual(t, base, changedAccount, "account_id changes fingerprint")
	changedTpl, _ := CandidateFingerprint(1, 11, credential.TypeAPIKey, "https://api.openai.com", "sk-abc123", "", "", "", false, "inst", "sess", "thr", "win")
	require.NotEqual(t, base, changedTpl, "template_id changes fingerprint")
	changedType, _ := CandidateFingerprint(1, 10, credential.TypeResponsesSpecial, "https://api.openai.com", "sk-abc123", "", "", "", false, "inst", "sess", "thr", "win")
	require.NotEqual(t, base, changedType, "credential_type changes fingerprint")
	changedURL, _ := CandidateFingerprint(1, 10, credential.TypeAPIKey, "https://api.anthropic.com", "sk-abc123", "", "", "", false, "inst", "sess", "thr", "win")
	require.NotEqual(t, base, changedURL, "baseURL changes fingerprint")
	changedStrip, _ := CandidateFingerprint(1, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-abc123", "", "", "", true, "inst", "sess", "thr", "win")
	require.NotEqual(t, base, changedStrip, "strip flag changes fingerprint")
	changedInst, _ := CandidateFingerprint(1, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-abc123", "", "", "", false, "inst2", "sess", "thr", "win")
	require.NotEqual(t, base, changedInst, "installation_id changes fingerprint")
	changedSess, _ := CandidateFingerprint(1, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-abc123", "", "", "", false, "inst", "sess2", "thr", "win")
	require.NotEqual(t, base, changedSess, "session_id changes fingerprint")
	changedCodexAcc, _ := CandidateFingerprint(1, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-abc123", "", "", "other-acc", false, "inst", "sess", "thr", "win")
	require.NotEqual(t, base, changedCodexAcc, "codex_account_id changes fingerprint")
	same, _ := CandidateFingerprint(1, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-abc123", "", "", "", false, "inst", "sess", "thr", "win")
	require.Equal(t, base, same)
}

func TestRoutingFingerprintCredentialStability(t *testing.T) {
	fp1, _ := CandidateFingerprint(1, 10, credential.TypeCodexOAuth, "https://api.openai.com", "", "", "user@example.com", "acc-123", false, "i", "s", "t", "w")
	fp2, _ := CandidateFingerprint(1, 10, credential.TypeCodexOAuth, "https://api.openai.com", "", "", "other@example.com", "acc-123", false, "i", "s", "t", "w")
	require.Equal(t, fp1, fp2, "oauth email must not change fingerprint")
	fp3, _ := CandidateFingerprint(1, 10, credential.TypeCodexOAuth, "https://api.openai.com", "", "", "user@example.com", "acc-123", false, "i2", "s", "t", "w")
	require.NotEqual(t, fp1, fp3, "oauth installation change must change fingerprint")
	fpA1, _ := CandidateFingerprint(5, 1, credential.TypeAPIKey, "https://api.openai.com", "sk-one", "", "", "", false, "", "", "", "")
	fpA2, _ := CandidateFingerprint(5, 1, credential.TypeAPIKey, "https://api.openai.com", "sk-two", "", "", "", false, "", "", "", "")
	require.NotEqual(t, fpA1, fpA2, "upstreamKey change changes api_key fingerprint")
	fpP1, _ := CandidateFingerprint(5, 1, credential.TypeCodexPAT, "https://api.openai.com", "", "pat-one", "", "", false, "", "", "", "")
	fpP2, _ := CandidateFingerprint(5, 1, credential.TypeCodexPAT, "https://api.openai.com", "", "pat-two", "", "", false, "", "", "", "")
	require.NotEqual(t, fpP1, fpP2, "pat key change changes pat fingerprint")
}

func TestRoutingUTF8AndURL(t *testing.T) {
	_, err := RouteClassID(1, FormatOpenAIChat, string([]byte{0xff, 0xfe}), OpChatCompletions)
	require.Error(t, err, "invalid UTF-8 model must be rejected")
	_, err = CanonicalOrigin("https://api.openai.com/v1")
	require.Error(t, err)
	orig, err := CanonicalOrigin("https://API.OpenAI.COM:443/")
	require.NoError(t, err)
	require.Equal(t, "https://api.openai.com:443", orig)
	orig2, err := CanonicalOrigin("http://example.com:80")
	require.NoError(t, err)
	require.Equal(t, "http://example.com:80", orig2)
	orig3, err := CanonicalOrigin("https://api.openai.com")
	require.NoError(t, err)
	require.Equal(t, "https://api.openai.com:443", orig3)
	orig4, err := CanonicalOrigin("http://[::1]/")
	require.NoError(t, err)
	require.Equal(t, "http://[::1]:80", orig4)
	orig5, err := CanonicalOrigin("https://[2001:db8::1]:8443")
	require.NoError(t, err)
	require.Equal(t, "https://[2001:db8::1]:8443", orig5)
	orig6, err := CanonicalOrigin("http://192.168.1.1")
	require.NoError(t, err)
	require.Equal(t, "http://192.168.1.1:80", orig6)
	_, err = CanonicalOrigin("https://api.openai.com?foo=bar")
	require.Error(t, err, "query must be rejected")
	_, err = CanonicalOrigin("https://api.openai.com?")
	require.Error(t, err, "empty query marker must be rejected")
	_, err = CanonicalOrigin("https://api.openai.com#frag")
	require.Error(t, err, "fragment must be rejected")
	_, err = CanonicalOrigin("https://api.openai.com#")
	require.Error(t, err, "empty fragment marker must be rejected")
	_, err = CanonicalOrigin("")
	require.Error(t, err, "empty origin must be rejected")
	_, err = CanonicalOrigin("http://user:pass@api.openai.com")
	require.Error(t, err, "userinfo must be rejected")
	_, err = CanonicalOrigin("not-a-url")
	require.Error(t, err)
	_, err = CanonicalOrigin("https://api.openai.com:99999")
	require.Error(t, err, "invalid port must be rejected")
	idLower, _ := RouteClassID(1, FormatOpenAIChat, "gpt-4o", OpChatCompletions)
	idUpper, _ := RouteClassID(1, FormatOpenAIChat, "GPT-4O", OpChatCompletions)
	require.NotEqual(t, idLower, idUpper, "model case sensitive")
}

func TestRoutingHexRoundtrip(t *testing.T) {
	id, _ := RouteClassID(99, FormatOpenAISearch, "search-model", OpSearch)
	hexStr := RouteClassIDHex(id)
	require.Len(t, hexStr, 64)
	parsed, err := HexToID(hexStr)
	require.NoError(t, err)
	require.Equal(t, [32]byte(id), parsed)
	_, err = HexToID("zzzz")
	require.Error(t, err)
}

func TestRoutingOperationTagsFixed(t *testing.T) {
	tags := []OperationTag{OpChatCompletions, OpResponses, OpResponsesWS, OpAnthropicMessages, OpImagesGenerations, OpImagesEdits, OpSearch}
	require.Len(t, tags, 7, "fixed seven operation tags")
	for _, tg := range tags {
		require.True(t, tg.Valid(), "each tag must be valid")
	}
	require.False(t, OperationTag("unknown").Valid())
}
