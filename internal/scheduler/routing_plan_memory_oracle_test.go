// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

// Frozen BASE canonical-encoding oracle (routing-plan-memory spec B1).
//
// This is an INDEPENDENT copy of the pre-change decisionEncoder traversal, kept
// deliberately separate from the production canonicalSink traversal so that a
// shared omission cannot hide behind a shared implementation: the new streaming
// SHA-256 must equal sha256(baseCanonicalBytesOracle(view)) on the same inputs.
// It is frozen with the BASE source (commit d0d59648) and must not track later
// production refactors.

import (
	"bytes"
	"encoding/binary"
	"slices"
	"sort"
)

func baseOracleUvarint(buf *bytes.Buffer, v uint64) {
	var tmp [binary.MaxVarintLen64]byte
	buf.Write(tmp[:binary.PutUvarint(tmp[:], v)])
}

func baseOracleVarint(buf *bytes.Buffer, v int64) {
	var tmp [binary.MaxVarintLen64]byte
	buf.Write(tmp[:binary.PutVarint(tmp[:], v)])
}

func baseOracleStr(buf *bytes.Buffer, s string) {
	baseOracleUvarint(buf, uint64(len(s)))
	buf.WriteString(s)
}

func baseOracleCompiled(buf *bytes.Buffer, cs []CompiledCandidate) {
	baseOracleUvarint(buf, uint64(len(cs)))
	for _, c := range cs {
		baseOracleCompiledCandidate(buf, c)
	}
}

func baseOracleCompiledIndices(buf *bytes.Buffer, cs []CompiledCandidate, indexes []uint16) {
	baseOracleUvarint(buf, uint64(len(indexes)))
	for _, idx := range indexes {
		if int(idx) < len(cs) {
			baseOracleCompiledCandidate(buf, cs[idx])
		}
	}
}

func baseOracleCompiledCandidate(buf *bytes.Buffer, c CompiledCandidate) {
	baseOracleVarint(buf, c.AccountID)
	baseOracleStr(buf, string(c.Lane))
	baseOracleVarint(buf, c.TemplateID)
	baseOracleStr(buf, c.BaseURL)
	baseOracleStr(buf, c.Fingerprint)
	baseOracleStr(buf, c.RequestedModel)
	baseOracleStr(buf, c.MappedModel)
	baseOracleStr(buf, string(c.MappingMode))
	baseOracleStr(buf, c.Quality)
	baseOracleStr(buf, c.QualityRaw)
	baseOracleVarint(buf, c.IdentityRevision)
}

func baseOracleCacheDomainPlan(buf *bytes.Buffer, decision *RouteDecision) {
	baseOracleUvarint(buf, uint64(len(decision.CacheDomainRing.Domains)))
	for _, domain := range decision.CacheDomainRing.Domains {
		baseOracleStr(buf, domain)
	}
	baseOracleUvarint(buf, uint64(len(decision.CacheDomainRing.Nodes)))
	for _, node := range decision.CacheDomainRing.Nodes {
		baseOracleUvarint(buf, node.Hash)
		baseOracleStr(buf, node.Domain)
	}
	baseOracleUvarint(buf, uint64(len(decision.CacheDomainAccounts)))
	for _, account := range decision.CacheDomainAccounts {
		baseOracleVarint(buf, account.AccountID)
		baseOracleStr(buf, account.Domain)
	}
}

func baseOracleIncident(buf *bytes.Buffer, inc RouteIncident) {
	if inc.Active {
		baseOracleUvarint(buf, 1)
	} else {
		baseOracleUvarint(buf, 0)
	}
	baseOracleStr(buf, inc.Kind)
	baseOracleUvarint(buf, uint64(inc.Comparable))
	baseOracleUvarint(buf, uint64(inc.Degraded))
	baseOracleUvarint(buf, uint64(inc.Domains))
	baseOracleVarint(buf, inc.EvaluatedMinute)
}

// baseCanonicalBytesOracle is the frozen BASE byte encoding of a DecisionView
// (nil view -> nil bytes), used only in tests.
func baseCanonicalBytesOracle(d *DecisionView) []byte {
	if d == nil {
		return nil
	}
	var buf bytes.Buffer
	refs := make([]RouteRef, 0, len(d.routes))
	for k := range d.routes {
		refs = append(refs, k)
	}
	sort.Slice(refs, func(i, j int) bool { return lessRouteRef(refs[i], refs[j]) })
	baseOracleUvarint(&buf, uint64(len(refs)))
	for _, ref := range refs {
		rd := d.routes[ref]
		baseOracleVarint(&buf, ref.GroupID)
		baseOracleStr(&buf, ref.Format)
		baseOracleStr(&buf, ref.Model)
		baseOracleStr(&buf, ref.OperationTag)
		baseOracleStr(&buf, ref.RouteClassID)
		baseOracleStr(&buf, rd.Format)
		baseOracleStr(&buf, rd.RequestedModel)
		baseOracleStr(&buf, rd.RouteClassID)
		baseOracleStr(&buf, rd.CallerCategory)
		baseOracleStr(&buf, rd.OperationTag)
		baseOracleCompiled(&buf, rd.Primary)
		baseOracleCompiled(&buf, rd.Degraded)
		baseOracleCompiled(&buf, rd.Explore.Ordered)
		ids := make([]int64, 0, len(rd.Explore.Weights))
		for id := range rd.Explore.Weights {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		baseOracleUvarint(&buf, uint64(len(ids)))
		for _, id := range ids {
			baseOracleVarint(&buf, id)
			baseOracleVarint(&buf, int64(rd.Explore.Weights[id]))
		}
		baseOracleUvarint(&buf, uint64(len(rd.Explore.Cumulative)))
		for _, c := range rd.Explore.Cumulative {
			baseOracleUvarint(&buf, c)
		}
		baseOracleUvarint(&buf, rd.Explore.Total)
		baseOracleVarint(&buf, int64(rd.Explore.ExploreBP))
		baseOracleCompiledIndices(&buf, rd.Explore.Ordered, rd.Explore.Fallback)
		baseOracleCacheDomainPlan(&buf, rd)
		baseOracleIncident(&buf, rd.Incident)
	}
	return buf.Bytes()
}
