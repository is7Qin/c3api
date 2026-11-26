// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package identityenv 持有 account_ext.codex_identity 的**线缆契约**：可扩展的
// identity envelope、写入者补丁（IdentityPatch）、merge-on-write 语义，以及判定
// "两份 envelope 是否语义相同"的规范化 JSON 引擎。
//
// 它是**中立叶子包**：只 import stdlib（bytes / crypto/sha256 /
// encoding/json / fmt / io / sort / strconv / strings），绝不反向 import 任何
// internal/* 或 pkg/*。allow 矩阵里本包的规则是显式的 `to: []string{}`——
// 将来有人想往里加依赖，会得到一条明确的拒绝，而不是被 leafRule 顺手放行。
//
// 为什么单独成包（而不是留在 internal/domain）：ent 的生成模型要
// AccountExt.CodexIdentity 这个字段类型，ent 又不能反向依赖 domain 值类型层。
// 把"纯序列化契约"下沉成叶子包之后，生成代码只依赖契约，账本里那条
// `internal/ent → internal/domain` 的边就消失了。domain 侧保留同名别名
// （`type IdentityEnvelope = identityenv.IdentityEnvelope` + 四个同值函数别名），
// 所以 repository / sdkbridge / service 的消费方零改动。
//
// 已知遗留（**不在本包职责内收口**）：CanonicalJSON / writeCanonicalJSON /
// canonicalNumber 是通用的 JSON 规范化引擎，envelope 只是它当前唯一的使用者；
// 本包是它的**当前** owner，但它最终的归属（独立的中立包）是已知问题。之所以
// 现在不拆：这三个 helper 在 internal/domain 之外零消费者，为一个零外部消费者的
// 东西再开第三个包是过度设计。等第二个使用者出现时再拆，那时归属自然清晰。
package identityenv

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
)

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
		return nil, fmt.Errorf("identityenv: invalid identity envelope: %w", err)
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
			return nil, fmt.Errorf("identityenv: multiple JSON values")
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
	return MergeIdentityEnvelope(e, IdentityPatch{InstallationID: &id})
}

func (e IdentityEnvelope) ReplaceBPSProfile(profile json.RawMessage) (IdentityEnvelope, error) {
	return MergeIdentityEnvelope(e, IdentityPatch{BPSProfile: profile, SetBPSProfile: true})
}

// MergeIdentityEnvelope applies a writer-owned patch to a copy of e, leaving
// every other key (including keys this writer does not know about) intact.
// Exported because the owner of the merge semantics is this package: a wrapper
// in the consumer package would create a second implementation.
func MergeIdentityEnvelope(e IdentityEnvelope, patch IdentityPatch) (IdentityEnvelope, error) {
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
				return nil, fmt.Errorf("identityenv: invalid BPS profile: %w", err)
			}
			var object map[string]json.RawMessage
			if err := json.Unmarshal(canonical, &object); err != nil || object == nil {
				return nil, fmt.Errorf("identityenv: BPS profile must be a JSON object")
			}
			out["bps"] = canonical
		}
	}
	return out, nil
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
		return fmt.Errorf("identityenv: unsupported JSON value %T", value)
	}
	return nil
}

func canonicalNumber(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("identityenv: empty JSON number")
	}
	// Parse the JSON number as an exact decimal rational. This makes 1, 1.0,
	// and 1e0 semantically identical without float64 precision loss.
	s := strings.ToLower(raw)
	exponent := 0
	if i := strings.IndexByte(s, 'e'); i >= 0 {
		parsed, err := strconv.Atoi(s[i+1:])
		if err != nil || parsed < -10000 || parsed > 10000 {
			return "", fmt.Errorf("identityenv: JSON number exponent out of range")
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
		return "", fmt.Errorf("identityenv: invalid JSON number %q", raw)
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
