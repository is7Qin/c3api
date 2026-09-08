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

func callerKindForFormat(f domain.RequestFormat) domain.CallerKind {
	switch f {
	case domain.FormatOpenAIChat:
		return domain.CallerChat
	case domain.FormatOpenAIResponses:
		return domain.CallerResponses
	case domain.FormatOpenAIResponsesWS:
		return domain.CallerWS
	case domain.FormatAnthropic:
		return domain.CallerAnthropic
	case domain.FormatOpenAIImages:
		return domain.CallerImages
	case domain.FormatOpenAISearch:
		return domain.CallerSearch
	default:
		return ""
	}
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
