// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"encoding/binary"

	"github.com/is7qin/c3api/internal/domain"
)

func qualityClassHexForWithOp(format domain.RequestFormat, model string, op domain.OperationTag) string {
	ck := callerKindForFormat(format)
	if ck == "" || op == "" {
		return ""
	}
	id, err := domain.QualityClassID(ck, format, model, op)
	if err != nil {
		return ""
	}
	return domain.QualityClassIDHex(id)
}

// formatSpec is the single per-RequestFormat dispatch record: it binds the
// canonical caller kind and the operation(s) a format maps to. One table,
// consulted by callerKindForFormat (quality class), operationTagForFormat
// (route class / RouteRef) and operationTagsForFormat (compile fan-out), so the
// three views cannot drift apart.
type formatSpec struct {
	callerKind      domain.CallerKind
	operation       domain.OperationTag   // primary op ("" = unsupported format)
	extraOperations []domain.OperationTag // additional compile ops (images edits)
}

var formatSpecs = map[domain.RequestFormat]formatSpec{
	domain.FormatOpenAIChat:        {callerKind: domain.CallerChat, operation: domain.OpChatCompletions},
	domain.FormatOpenAIResponses:   {callerKind: domain.CallerResponses, operation: domain.OpResponses},
	domain.FormatOpenAIResponsesWS: {callerKind: domain.CallerWS, operation: domain.OpResponsesWS},
	domain.FormatAnthropic:         {callerKind: domain.CallerAnthropic, operation: domain.OpAnthropicMessages},
	domain.FormatOpenAIImages:      {callerKind: domain.CallerImages, operation: domain.OpImagesGenerations, extraOperations: []domain.OperationTag{domain.OpImagesEdits}},
	domain.FormatOpenAISearch:      {callerKind: domain.CallerSearch, operation: domain.OpSearch},
}

func callerKindForFormat(f domain.RequestFormat) domain.CallerKind {
	return formatSpecs[f].callerKind
}

func tplSupportsFormat(tpl *domain.Template, format domain.RequestFormat) bool {
	for _, f := range tpl.SupportedFormats {
		if f == format {
			return true
		}
	}
	return false
}

func candidateIdentityFingerprint(fingerprint string, accountID int64) domain.CandidateFingerprintVal {
	if fingerprint != "" {
		if v, err := domain.HexToID(fingerprint); err == nil {
			return domain.CandidateFingerprintVal(v)
		}
	}
	var b [32]byte
	binary.BigEndian.PutUint64(b[:8], uint64(accountID))
	return domain.CandidateFingerprintVal(b)
}
