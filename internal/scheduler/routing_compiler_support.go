// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"encoding/binary"
	"slices"

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

// tplSupportsFormat 是**无模型**（blanket）桶的模板格式门：逐 Type 格式政策行 ∧
// SupportedFormats，与 Template.FormatSupports 的 ①② 两步共用同一个判据
// （domain.FormatAllowedFor），故两条路径不可能分叉。
//
// 这里此前写的是「BPS 通道谓词 ∧ 切片版政策判定」：
// blanket 分支只调本函数，于是 responses-special 的格式行在"无 model"桶上不生效
// （需要一条违反写面策略的历史行才可达，但既然发现就修）。改法是把逐 Type 判定
// 交给政策表的单格视图，判据只剩数据源一处。
func tplSupportsFormat(tpl *domain.Template, format domain.RequestFormat) bool {
	if !domain.FormatAllowedFor(tpl.CredentialType, format) {
		return false
	}
	return slices.Contains(tpl.SupportedFormats, format)
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
