// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"github.com/is7qin/c3api/internal/domain"
)

// CompiledCandidate is one immutable request-independent candidate. All
// request-time identity (fingerprint, mapped model, quality) is resolved once
// at compile; the request path only inspects and leases. Leaf/Static are the
// captured immutable static leaf for stale fencing (pointer equality against
// the current RoutingView); all other fields are detached values.
type CompiledCandidate struct {
	AccountID         int64
	Lane              AttemptLane
	TemplateID        int64
	BaseURL           string
	Fingerprint       string
	RequestedModel    string
	MappedModel       string
	MappingMode       domain.ModelMappingMode
	Quality           string
	QualityRaw        string
	LifecycleRevision int64
	Leaf              *accountSnapshot
	Static            *snapshotStatic
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
		AccountID:         facts.accountID,
		Lane:              lane,
		TemplateID:        facts.templateID,
		BaseURL:           facts.baseURL,
		Fingerprint:       facts.fingerprint,
		RequestedModel:    facts.requestedModel,
		MappedModel:       facts.mappedModel,
		MappingMode:       facts.mappingMode,
		Quality:           facts.quality,
		QualityRaw:        facts.qualityRaw,
		LifecycleRevision: facts.revision,
		Leaf:              facts.account,
		Static:            facts.static,
	}
}

// cloneCompiled copies a compiled slice without aliasing the published array.
func cloneCompiled(in []CompiledCandidate) []CompiledCandidate {
	if in == nil {
		return nil
	}
	return append([]CompiledCandidate(nil), in...)
}
