// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

// BPSDefaultEndpoint is the fixed endpoint identity used by BPS routing. The
// adapter owns URL construction; this value is only a domain validation and
// fingerprint input.
const BPSDefaultEndpoint = "https://bps.openai.com/basispoints/api"

// BPSBaseURLAllowed accepts only the SDK-owned endpoint or an empty value. An
// empty value means "use the fixed endpoint"; arbitrary origins are never
// allowed to receive BPS OAuth credentials.
func BPSBaseURLAllowed(raw string) bool {
	return raw == "" || raw == BPSDefaultEndpoint
}

// BPSUsesWebSocket derives the BPS transport from the selected request format.
// There is intentionally no BPS transport field in template/schema/Selection.
func BPSUsesWebSocket(format RequestFormat) bool {
	return format == FormatOpenAIResponsesWS
}

// IdentityEnvelope is the raw account_ext.codex_identity object. Unknown keys
// are retained intentionally so old writers cannot erase newer BPS profile
// data. The map is not comparable; use IdentityJSONEqual for semantic checks.
type IdentityEnvelope map[string]json.RawMessage

// IdentityPatch describes fields owned by a writer. A nil InstallationID means
// "leave it untouched"; SetBPSProfile with a nil/empty profile removes bps.
type IdentityPatch struct {
	InstallationID *string
	BPSProfile     json.RawMessage
	SetBPSProfile  bool
}

// ParseIdentityEnvelope accepts only a JSON object. SQL NULL and JSON null are
// represented as an empty envelope so callers can apply a merge-on-write.
func ParseIdentityEnvelope(raw []byte) (IdentityEnvelope, error) {
	if len(bytes.TrimSpace(raw)) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return IdentityEnvelope{}, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, fmt.Errorf("domain: invalid identity envelope: %w", err)
	}
	if object == nil {
		return IdentityEnvelope{}, nil
	}
	return IdentityEnvelope(object), nil
}

// MarshalJSON makes the envelope usable by existing JSONB writers while
// keeping unknown raw values intact. CanonicalIdentityJSON should be used when
// semantic equality or a stable digest is required.
func (e IdentityEnvelope) MarshalJSON() ([]byte, error) {
	return e.CanonicalJSON()
}

func (e *IdentityEnvelope) UnmarshalJSON(raw []byte) error {
	parsed, err := ParseIdentityEnvelope(raw)
	if err != nil {
		return err
	}
	if parsed == nil {
		parsed = IdentityEnvelope{}
	}
	out := parsed.Clone()
	if out == nil {
		out = IdentityEnvelope{}
	}
	*e = out
	return nil
}

// CanonicalJSON returns deterministic JSON for any JSON value: object keys are
// sorted recursively, arrays retain order, numbers use one canonical decimal
// spelling, and insignificant whitespace is removed.
func CanonicalJSON(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("domain: multiple JSON values")
		}
		return nil, err
	}
	var out bytes.Buffer
	if err := writeCanonicalJSON(&out, value); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func CanonicalIdentityJSON(raw []byte) ([]byte, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = []byte("null")
	}
	env, err := ParseIdentityEnvelope(raw)
	if err != nil {
		return nil, err
	}
	return env.CanonicalJSON()
}

func (e IdentityEnvelope) CanonicalJSON() ([]byte, error) {
	if e == nil {
		e = IdentityEnvelope{}
	}
	raw, err := json.Marshal(map[string]json.RawMessage(e))
	if err != nil {
		return nil, err
	}
	return CanonicalJSON(raw)
}

func (e IdentityEnvelope) Clone() IdentityEnvelope {
	out := make(IdentityEnvelope, len(e))
	for k, v := range e {
		out[k] = append(json.RawMessage(nil), v...)
	}
	return out
}

func (e IdentityEnvelope) InstallationID() string {
	var id string
	if raw := e["installation_id"]; len(raw) != 0 {
		_ = json.Unmarshal(raw, &id)
	}
	return id
}

func (e IdentityEnvelope) BPSProfile() json.RawMessage {
	return append(json.RawMessage(nil), e["bps"]...)
}

func (e IdentityEnvelope) MergeCodexInstallation(id string) (IdentityEnvelope, error) {
	return mergeIdentityEnvelope(e, IdentityPatch{InstallationID: &id})
}

func (e IdentityEnvelope) ReplaceBPSProfile(profile json.RawMessage) (IdentityEnvelope, error) {
	return mergeIdentityEnvelope(e, IdentityPatch{BPSProfile: profile, SetBPSProfile: true})
}

func mergeIdentityEnvelope(e IdentityEnvelope, patch IdentityPatch) (IdentityEnvelope, error) {
	out := e.Clone()
	if out == nil {
		out = IdentityEnvelope{}
	}
	if patch.InstallationID != nil {
		b, err := json.Marshal(*patch.InstallationID)
		if err != nil {
			return nil, err
		}
		out["installation_id"] = b
	}
	if patch.SetBPSProfile {
		if len(bytes.TrimSpace(patch.BPSProfile)) == 0 || bytes.Equal(bytes.TrimSpace(patch.BPSProfile), []byte("null")) {
			delete(out, "bps")
		} else {
			canonical, err := CanonicalJSON(patch.BPSProfile)
			if err != nil {
				return nil, fmt.Errorf("domain: invalid BPS profile: %w", err)
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(canonical, &object); err != nil || object == nil {
				return nil, fmt.Errorf("domain: BPS profile must be a JSON object")
			}
			out["bps"] = canonical
		}
	}
	return out, nil
}

// MergeIdentityEnvelope updates only the requested owned fields and returns
// canonical JSON while preserving installation_id, bps, and unknown keys.
func MergeIdentityEnvelope(raw []byte, patch IdentityPatch) ([]byte, error) {
	env, err := ParseIdentityEnvelope(raw)
	if err != nil {
		return nil, err
	}
	merged, err := mergeIdentityEnvelope(env, patch)
	if err != nil {
		return nil, err
	}
	return merged.CanonicalJSON()
}

func IdentityJSONEqual(a, b []byte) bool {
	ca, errA := CanonicalIdentityJSON(a)
	cb, errB := CanonicalIdentityJSON(b)
	if errA != nil || errB != nil {
		return false
	}
	return bytes.Equal(ca, cb)
}

// CanonicalJSONHash returns the digest of canonical JSON. Empty input hashes as
// null; callers needing an absent profile hash should use BPSProfileHash.
func CanonicalJSONHash(raw []byte) ([32]byte, error) {
	canonical, err := CanonicalJSON(raw)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(canonical), nil
}

func BPSProfileHash(raw []byte) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return ""
	}
	if err := ValidateBPSProfileJSON(trimmed); err != nil {
		return ""
	}
	h, err := CanonicalJSONHash(trimmed)
	if err != nil {
		return ""
	}
	return hex.EncodeToString(h[:])
}

func BPSProfileDigest(raw []byte) [32]byte {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return [32]byte{}
	}
	if err := ValidateBPSProfileJSON(trimmed); err != nil {
		return [32]byte{}
	}
	h, err := CanonicalJSONHash(trimmed)
	if err != nil {
		return [32]byte{}
	}
	return h
}

// BPSFormatsOnly reports the format gate for the BPS provider.
func BPSFormatsOnly(formats []RequestFormat) bool {
	for _, f := range formats {
		if f != FormatOpenAIResponses && f != FormatOpenAIResponsesWS {
			return false
		}
	}
	return true
}

// BPSProfileJSONEqual compares profile JSON semantically, ignoring object key
// order and whitespace while retaining unknown fields.
func BPSProfileJSONEqual(a, b []byte) bool {
	ca, errA := CanonicalJSON(a)
	cb, errB := CanonicalJSON(b)
	if errA != nil || errB != nil {
		return false
	}
	return bytes.Equal(ca, cb)
}

// ValidateBPSProfileJSON enforces the phase-1 storage boundary: a profile is
// a complete non-null JSON object. Detailed DeviceProfile validation remains in
// sdkbridge, where bps-sdk is the authority.
func ValidateBPSProfileJSON(raw []byte) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return fmt.Errorf("domain: BPS profile is required")
	}
	canonical, err := CanonicalJSON(trimmed)
	if err != nil {
		return fmt.Errorf("domain: invalid BPS profile: %w", err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(canonical, &object); err != nil || object == nil {
		return fmt.Errorf("domain: BPS profile must be a JSON object")
	}
	return nil
}

func writeCanonicalJSON(out *bytes.Buffer, value any) error {
	switch v := value.(type) {
	case nil:
		out.WriteString("null")
	case bool:
		if v {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
	case string:
		b, _ := json.Marshal(v)
		out.Write(b)
	case json.Number:
		canonical, err := canonicalNumber(v.String())
		if err != nil {
			return err
		}
		out.WriteString(canonical)
	case []any:
		out.WriteByte('[')
		for i, item := range v {
			if i != 0 {
				out.WriteByte(',')
			}
			if err := writeCanonicalJSON(out, item); err != nil {
				return err
			}
		}
		out.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		out.WriteByte('{')
		for i, key := range keys {
			if i != 0 {
				out.WriteByte(',')
			}
			encodedKey, _ := json.Marshal(key)
			out.Write(encodedKey)
			out.WriteByte(':')
			if err := writeCanonicalJSON(out, v[key]); err != nil {
				return err
			}
		}
		out.WriteByte('}')
	default:
		return fmt.Errorf("domain: unsupported JSON value %T", value)
	}
	return nil
}

func canonicalNumber(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("domain: empty JSON number")
	}
	// Parse the JSON number as an exact decimal rational. This makes 1, 1.0,
	// and 1e0 semantically identical without float64 precision loss.
	s := strings.ToLower(raw)
	exponent := 0
	if i := strings.IndexByte(s, 'e'); i >= 0 {
		parsed, err := strconv.Atoi(s[i+1:])
		if err != nil || parsed < -10000 || parsed > 10000 {
			return "", fmt.Errorf("domain: JSON number exponent out of range")
		}
		exponent = parsed
		s = s[:i]
	}
	sign := ""
	if strings.HasPrefix(s, "-") {
		sign = "-"
		s = s[1:]
	}
	parts := strings.Split(s, ".")
	if len(parts) > 2 || parts[0] == "" || (len(parts) == 2 && parts[1] == "") {
		return "", fmt.Errorf("domain: invalid JSON number %q", raw)
	}
	integer := parts[0]
	fraction := ""
	if len(parts) == 2 {
		fraction = strings.TrimRight(parts[1], "0")
	}
	digits := strings.TrimLeft(integer+fraction, "0")
	if digits == "" {
		return "0", nil
	}
	decimalPlaces := len(fraction) - exponent
	if decimalPlaces <= 0 {
		return sign + digits + strings.Repeat("0", -decimalPlaces), nil
	}
	if decimalPlaces >= len(digits) {
		return sign + "0." + strings.Repeat("0", decimalPlaces-len(digits)) + digits, nil
	}
	return sign + digits[:len(digits)-decimalPlaces] + "." + digits[len(digits)-decimalPlaces:], nil
}
