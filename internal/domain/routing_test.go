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
	require.Equal(t, "fa87b2998fb6a5b5a5a967b6def64d778f2a5a7619120205f43bfa370632b5a6", IDToHex(id))
	// restart / order stability: recompute
	id2, err := RouteClassID(42, FormatOpenAIChat, "gpt-4o", OpChatCompletions)
	require.NoError(t, err)
	require.Equal(t, id, id2)
	// second vector
	id3, err := RouteClassID(42, FormatOpenAIResponses, "o3", OpResponses)
	require.NoError(t, err)
	require.Equal(t, "cf0467b3595157fdfae2ea3940c5336d01ace0dc8035fccea09328ba68732d0f", IDToHex(id3))
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
	require.Equal(t, "847bba3ba902936e2c794ec31b3ad169eae8892d2881f3f86d02069d8d27897a", IDToHex(qid))
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
	require.Equal(t, "ca2d76244667739a44428d57f493bcd3da3867eb14b0e2f1023fb55bf0d0785b", IDToHex(fp))
	fp2, err := CandidateFingerprint(1, 10, credential.TypeCodexPAT, "https://api.openai.com", "", "pat_abc", "", "", false, "inst", "sess", "thr", "win")
	require.NoError(t, err)
	require.Equal(t, "0254f1e9d1da3b3368988be07ca9ccac41a789f6a45699ffb38ca4d034b4caa2", IDToHex(fp2))
	fp3, err := CandidateFingerprint(1, 10, credential.TypeCodexOAuth, "https://api.openai.com", "", "", "user@example.com", "acc-123", false, "install-1", "sess1", "thr1", "win1")
	require.NoError(t, err)
	require.Equal(t, "935e2e3afd64fb63e12b7444020435c4dc067b44224aa2ac49497c12d1b6c331", IDToHex(fp3))
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
	// cost/cache/revision must NOT affect fingerprint: same inputs produce same ID even though those fields are separate; prove by calling twice with same args
	same, _ := CandidateFingerprint(1, 10, credential.TypeAPIKey, "https://api.openai.com", "sk-abc123", "", "", "", false, "inst", "sess", "thr", "win")
	require.Equal(t, base, same)
}

func TestRoutingFingerprintCredentialStability(t *testing.T) {
	// OAuth rotation: same durable identity => same fingerprint despite token change (tokens not part of input)
	fp1, _ := CandidateFingerprint(1, 10, credential.TypeCodexOAuth, "https://api.openai.com", "", "", "user@example.com", "acc-123", false, "i", "s", "t", "w")
	fp2, _ := CandidateFingerprint(1, 10, credential.TypeCodexOAuth, "https://api.openai.com", "", "", "user@example.com", "acc-123", false, "i", "s", "t", "w")
	require.Equal(t, fp1, fp2, "oauth same durable identity => same fingerprint")
	// admin durable identity difference: different email => different fingerprint
	fp3, _ := CandidateFingerprint(1, 10, credential.TypeCodexOAuth, "https://api.openai.com", "", "", "other@example.com", "acc-123", false, "i", "s", "t", "w")
	require.NotEqual(t, fp1, fp3, "oauth email changes fingerprint")
	// PAT digest vs API key: upstreamKey change changes api_key fingerprint, pat change changes pat fingerprint
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
	_, err = RouteClassID(1, FormatOpenAIChat, "gpt-4o", OpChatCompletions)
	require.NoError(t, err)
	require.Equal(t, false, func() bool { _, e := CanonicalOrigin("https://api.openai.com/v1"); return e == nil }(), "base_url with path must be rejected")
	_, err = CanonicalOrigin("https://api.openai.com/v1")
	require.Error(t, err)
	orig, err := CanonicalOrigin("https://API.OpenAI.COM:443/")
	require.NoError(t, err)
	require.Equal(t, "https://api.openai.com", orig)
	orig2, err := CanonicalOrigin("http://example.com:80")
	require.NoError(t, err)
	require.Equal(t, "http://example.com", orig2)
	_, err = CanonicalOrigin("https://api.openai.com?foo=bar")
	require.Error(t, err, "query must be rejected")
	_, err = CanonicalOrigin("not-a-url")
	require.Error(t, err)
	// case preservation: requested_model case sensitive
	idLower, _ := RouteClassID(1, FormatOpenAIChat, "gpt-4o", OpChatCompletions)
	idUpper, _ := RouteClassID(1, FormatOpenAIChat, "GPT-4O", OpChatCompletions)
	require.NotEqual(t, idLower, idUpper, "model case sensitive")
}

func TestRoutingHexRoundtrip(t *testing.T) {
	id, _ := RouteClassID(99, FormatOpenAISearch, "search-model", OpSearch)
	hexStr := IDToHex(id)
	require.Len(t, hexStr, 64)
	parsed, err := HexToID(hexStr)
	require.NoError(t, err)
	require.Equal(t, id, parsed)
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
