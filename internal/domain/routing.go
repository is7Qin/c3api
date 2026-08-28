// SPDX-License-Identifier: AGPL-3.0-or-later
package domain

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/is7qin/c3api/internal/credential"
)

const RoutingIdentityVersion byte = 1

type RouteClassIDVal [32]byte
type QualityClassIDVal [32]byte
type CandidateFingerprintVal [32]byte

type OperationTag string

const (
	OpChatCompletions   OperationTag = "chat_completions"
	OpResponses         OperationTag = "responses"
	OpResponsesWS       OperationTag = "responses_ws"
	OpAnthropicMessages OperationTag = "anthropic_messages"
	OpImagesGenerations OperationTag = "images_generations"
	OpImagesEdits       OperationTag = "images_edits"
	OpSearch            OperationTag = "search"
)

func (o OperationTag) Valid() bool {
	switch o {
	case OpChatCompletions, OpResponses, OpResponsesWS, OpAnthropicMessages, OpImagesGenerations, OpImagesEdits, OpSearch:
		return true
	}
	return false
}

type CallerKind string

const (
	CallerChat      CallerKind = "chat"
	CallerResponses CallerKind = "responses"
	CallerAnthropic CallerKind = "anthropic"
	CallerImages    CallerKind = "images"
	CallerSearch    CallerKind = "search"
	CallerWS        CallerKind = "responses_ws"
	CallerConverted CallerKind = "converted"
)

func (c CallerKind) Valid() bool {
	switch c {
	case CallerChat, CallerResponses, CallerAnthropic, CallerImages, CallerSearch, CallerWS, CallerConverted:
		return true
	}
	return false
}

func encodeUvarint(n uint64) []byte {
	var buf [10]byte
	l := binary.PutUvarint(buf[:], n)
	return buf[:l]
}

func hashFields(fields ...[]byte) [32]byte {
	h := sha256.New()
	h.Write([]byte{RoutingIdentityVersion})
	for _, f := range fields {
		h.Write(encodeUvarint(uint64(len(f))))
		h.Write(f)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func fieldInt64(v int64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(v))
	return b[:]
}

func fieldUint64(v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return b[:]
}

func fieldString(s string) ([]byte, error) {
	if !utf8.ValidString(s) {
		return nil, fmt.Errorf("routing: string not valid UTF-8")
	}
	return []byte(s), nil
}

func fieldBool(v bool) []byte {
	if v {
		return []byte{1}
	}
	return []byte{0}
}

func IDToHex(id [32]byte) string { return hex.EncodeToString(id[:]) }

func RouteClassIDHex(id RouteClassIDVal) string { return hex.EncodeToString(id[:]) }
func QualityClassIDHex(id QualityClassIDVal) string { return hex.EncodeToString(id[:]) }
func CandidateFPHex(id CandidateFingerprintVal) string { return hex.EncodeToString(id[:]) }

func HexToID(s string) ([32]byte, error) {
	var out [32]byte
	if len(s) != 64 {
		return out, fmt.Errorf("routing: hex id must be 64 chars")
	}
	b, err := hex.DecodeString(s)
	if err != nil {
		return out, err
	}
	copy(out[:], b)
	return out, nil
}

func CanonicalOrigin(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("routing: empty origin")
	}
	if strings.Contains(raw, "?") {
		return "", fmt.Errorf("routing: origin must not contain query marker %q", raw)
	}
	if strings.Contains(raw, "#") {
		return "", fmt.Errorf("routing: origin must not contain fragment marker %q", raw)
	}
	rawTrim := strings.TrimSpace(raw)
	u, err := url.Parse(rawTrim)
	if err != nil {
		return "", fmt.Errorf("routing: invalid origin %q: %w", raw, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("routing: origin missing scheme or host %q", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return "", fmt.Errorf("routing: origin scheme must be http or https %q", raw)
	}
	if u.User != nil {
		return "", fmt.Errorf("routing: origin must not contain userinfo %q", raw)
	}
	path := u.EscapedPath()
	if path != "" && path != "/" {
		return "", fmt.Errorf("routing: origin must be naked root without path %q", raw)
	}
	if u.Opaque != "" {
		return "", fmt.Errorf("routing: invalid origin %q", raw)
	}
	hostname := u.Hostname()
	if hostname == "" {
		return "", fmt.Errorf("routing: origin missing hostname %q", raw)
	}
	hostname = strings.ToLower(hostname)
	if strings.Contains(hostname, "_") {
		return "", fmt.Errorf("routing: invalid hostname %q", raw)
	}
	if ip := net.ParseIP(hostname); ip == nil {
		if len(hostname) > 253 {
			return "", fmt.Errorf("routing: hostname too long %q", raw)
		}
	}
	portStr := u.Port()
	var port string
	if portStr == "" {
		if scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	} else {
		if _, err := fmt.Sscanf(portStr, "%d", new(int)); err != nil {
			return "", fmt.Errorf("routing: invalid port %q", raw)
		}
		var p int
		fmt.Sscanf(portStr, "%d", &p)
		if p <= 0 || p > 65535 {
			return "", fmt.Errorf("routing: invalid port %q", raw)
		}
		port = portStr
	}
	host := net.JoinHostPort(hostname, port)
	return scheme + "://" + host, nil
}

func RouteClassID(groupID int64, clientFormat RequestFormat, requestedModel string, opTag OperationTag) (RouteClassIDVal, error) {
	if !clientFormat.Valid() {
		return RouteClassIDVal{}, fmt.Errorf("routing: invalid client_format %q", clientFormat)
	}
	if !opTag.Valid() {
		return RouteClassIDVal{}, fmt.Errorf("routing: invalid operation_tag %q", opTag)
	}
	if !utf8.ValidString(requestedModel) {
		return RouteClassIDVal{}, fmt.Errorf("routing: requested_model not valid UTF-8")
	}
	g := fieldInt64(groupID)
	cf, _ := fieldString(string(clientFormat))
	rm, _ := fieldString(requestedModel)
	ot, _ := fieldString(string(opTag))
	h := hashFields(g, cf, rm, ot)
	return RouteClassIDVal(h), nil
}

func QualityClassID(callerKind CallerKind, upstreamFormat RequestFormat, resolvedModel string, opTag OperationTag) (QualityClassIDVal, error) {
	if !callerKind.Valid() {
		return QualityClassIDVal{}, fmt.Errorf("routing: invalid caller_kind %q", callerKind)
	}
	if !upstreamFormat.Valid() {
		return QualityClassIDVal{}, fmt.Errorf("routing: invalid upstream_format %q", upstreamFormat)
	}
	if !opTag.Valid() {
		return QualityClassIDVal{}, fmt.Errorf("routing: invalid operation_tag %q", opTag)
	}
	if !utf8.ValidString(resolvedModel) {
		return QualityClassIDVal{}, fmt.Errorf("routing: resolved_model not valid UTF-8")
	}
	ck, _ := fieldString(string(callerKind))
	uf, _ := fieldString(string(upstreamFormat))
	rm, _ := fieldString(resolvedModel)
	ot, _ := fieldString(string(opTag))
	h := hashFields(ck, uf, rm, ot)
	return QualityClassIDVal(h), nil
}

func stableCredentialDigest(credType credential.Type, upstreamKey string, patKey string) ([]byte, error) {
	switch credType {
	case credential.TypeAPIKey, credential.TypeResponsesSpecial:
		if upstreamKey == "" {
			return nil, fmt.Errorf("routing: upstream key empty for %s", credType)
		}
		h := sha256.Sum256([]byte(upstreamKey))
		b := make([]byte, 32)
		copy(b, h[:])
		return b, nil
	case credential.TypeCodexPAT:
		if patKey == "" {
			return nil, fmt.Errorf("routing: pat key empty")
		}
		h := sha256.Sum256([]byte(patKey))
		b := make([]byte, 32)
		copy(b, h[:])
		return b, nil
	case credential.TypeCodexOAuth:
		h := sha256.Sum256([]byte("codex-oauth-stable-v1"))
		b := make([]byte, 32)
		copy(b, h[:])
		return b, nil
	default:
		return nil, fmt.Errorf("routing: unsupported credential type %q", credType)
	}
}

func CandidateFingerprint(
	accountID int64,
	templateID int64,
	credType credential.Type,
	effectiveBaseURL string,
	upstreamKey string,
	patKey string,
	codexEmail string,
	codexAccountID string,
	stripImageTools bool,
	installationID string,
	sessionID string,
	threadID string,
	windowID string,
) (CandidateFingerprintVal, error) {
	if !credType.Valid() {
		return CandidateFingerprintVal{}, fmt.Errorf("routing: invalid credential_type %q", credType)
	}
	canonicalOrigin := ""
	if effectiveBaseURL != "" {
		var err error
		canonicalOrigin, err = CanonicalOrigin(effectiveBaseURL)
		if err != nil {
			return CandidateFingerprintVal{}, err
		}
	}
	digest, err := stableCredentialDigest(credType, upstreamKey, patKey)
	if err != nil {
		return CandidateFingerprintVal{}, err
	}
	for _, s := range []string{installationID, sessionID, threadID, windowID, codexAccountID} {
		if !utf8.ValidString(s) {
			return CandidateFingerprintVal{}, fmt.Errorf("routing: codex identity field not valid UTF-8")
		}
	}
	if !utf8.ValidString(canonicalOrigin) {
		return CandidateFingerprintVal{}, fmt.Errorf("routing: canonical origin not valid UTF-8")
	}
	acc := fieldInt64(accountID)
	tpl := fieldInt64(templateID)
	ct, _ := fieldString(string(credType))
	orig, _ := fieldString(canonicalOrigin)
	strip := fieldBool(stripImageTools)
	inst, _ := fieldString(installationID)
	sess, _ := fieldString(sessionID)
	thr, _ := fieldString(threadID)
	win, _ := fieldString(windowID)
	caid, _ := fieldString(codexAccountID)
	h := hashFields(acc, tpl, ct, orig, digest, strip, inst, sess, thr, win, caid)
	return CandidateFingerprintVal(h), nil
}

var _ = fieldUint64
