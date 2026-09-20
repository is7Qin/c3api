// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"github.com/is7qin/c3api/internal/domain"
)

// CompiledCandidate is one immutable request-independent candidate. All
// request-time identity (fingerprint, mapped model, quality) is resolved once
// at compile; the request path only inspects and leases. Leaf/Static are the
// captured immutable static leaf for selection assembly (always refreshed to
// the current leaf before use); PlanKey is the value-identity fence captured
// at compile (compared against the current leaf's planKey); all other fields
// are detached values.
type CompiledCandidate struct {
	AccountID        int64
	Lane             AttemptLane
	TemplateID       int64
	BaseURL          string
	Fingerprint      string
	RequestedModel   string
	MappedModel      string
	MappingMode      domain.ModelMappingMode
	Quality          string
	QualityRaw       string
	IdentityRevision int64
	PlanKey          planKey
	Leaf             *accountSnapshot
	Static           *snapshotStatic
}

// QualityFor selects the precompiled quality class for the request's mapping
// mode without recomputation: opaque (search) dispatches use the raw class.
func (c *CompiledCandidate) QualityFor(applyMapping bool) string {
	if c == nil {
		return ""
	}
	if !applyMapping {
		return c.QualityRaw
	}
	return c.Quality
}

// compileCandidate copies the already-derived route facts into the immutable
// candidate metadata. The compiler never re-reads the account leaf here.
func compileCandidate(facts compilerCandidateFacts, lane AttemptLane) CompiledCandidate {
	return CompiledCandidate{
		AccountID:        facts.accountID,
		Lane:             lane,
		TemplateID:       facts.templateID,
		BaseURL:          facts.baseURL,
		Fingerprint:      facts.fingerprint,
		RequestedModel:   facts.requestedModel,
		MappedModel:      facts.mappedModel,
		MappingMode:      facts.mappingMode,
		Quality:          facts.quality,
		QualityRaw:       facts.qualityRaw,
		IdentityRevision: facts.revision,
		PlanKey:          facts.planKey,
		Leaf:             facts.account,
		Static:           facts.static,
	}
}

// cloneCompiled copies a compiled slice without aliasing the published array.
// append(nil) 语义是故意的：空非 nil 输入归一化为 nil（TestRed_Blocker5_ViewImmutability
// 钉住该语义），故不用 slices.Clone（它会保留空非 nil）。
func cloneCompiled(in []CompiledCandidate) []CompiledCandidate {
	if in == nil {
		return nil
	}
	return append([]CompiledCandidate(nil), in...)
}
