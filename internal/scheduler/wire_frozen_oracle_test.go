// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"bytes"
	"encoding/binary"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// baseEncodeDecisionView is a FROZEN, independent re-implementation of the
// canonical DecisionView byte encoding (the pre-T3 format). It deliberately
// shares NO code with writeCanonical/encBuf/decisionViewBytes so it can act as
// an oracle: if the production encoder and the compare sink drifted together,
// the compare oracle would still disagree with these independently-produced
// bytes. Used by TestCompareSinkFrozenBaseEncoderOracle.
func baseEncodeDecisionView(d *DecisionView) []byte {
	if d == nil {
		return nil
	}
	var buf bytes.Buffer
	refs := make([]RouteRef, 0, len(d.routes))
	for k := range d.routes {
		refs = append(refs, k)
	}
	sort.Slice(refs, func(i, j int) bool { return baseLessRouteRef(refs[i], refs[j]) })
	basePutUvarint(&buf, uint64(len(refs)))
	for _, ref := range refs {
		rd := d.routes[ref]
		basePutVarint(&buf, ref.GroupID)
		basePutStr(&buf, ref.Format)
		basePutStr(&buf, ref.Model)
		basePutStr(&buf, ref.OperationTag)
		basePutStr(&buf, ref.RouteClassID)
		basePutStr(&buf, rd.Format)
		basePutStr(&buf, rd.RequestedModel)
		basePutStr(&buf, rd.RouteClassID)
		basePutStr(&buf, rd.CallerCategory)
		basePutStr(&buf, rd.OperationTag)
		basePutCompiled(&buf, rd.Primary)
		basePutCompiled(&buf, rd.Degraded)
		basePutCompiled(&buf, rd.Explore.Ordered)
		ids := make([]int64, 0, len(rd.Explore.Weights))
		for id := range rd.Explore.Weights {
			ids = append(ids, id)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		basePutUvarint(&buf, uint64(len(ids)))
		for _, id := range ids {
			basePutVarint(&buf, id)
			basePutVarint(&buf, int64(rd.Explore.Weights[id]))
		}
		basePutUvarint(&buf, uint64(len(rd.Explore.Cumulative)))
		for _, c := range rd.Explore.Cumulative {
			basePutUvarint(&buf, c)
		}
		basePutUvarint(&buf, rd.Explore.Total)
		basePutVarint(&buf, int64(rd.Explore.ExploreBP))
		basePutUvarint(&buf, uint64(len(rd.Explore.Fallback)))
		for _, idx := range rd.Explore.Fallback {
			if int(idx) < len(rd.Explore.Ordered) {
				basePutCompiledCandidate(&buf, rd.Explore.Ordered[idx])
			}
		}
		basePutCacheDomainPlan(&buf, rd)
		basePutIncident(&buf, rd.Incident)
	}
	return buf.Bytes()
}

func baseLessRouteRef(a, b RouteRef) bool {
	if a.GroupID != b.GroupID {
		return a.GroupID < b.GroupID
	}
	if a.Format != b.Format {
		return a.Format < b.Format
	}
	if a.Model != b.Model {
		return a.Model < b.Model
	}
	if a.OperationTag != b.OperationTag {
		return a.OperationTag < b.OperationTag
	}
	return a.RouteClassID < b.RouteClassID
}

func basePutUvarint(buf *bytes.Buffer, v uint64) {
	buf.Write(binary.AppendUvarint(nil, v))
}

func basePutVarint(buf *bytes.Buffer, v int64) {
	buf.Write(binary.AppendVarint(nil, v))
}

func basePutStr(buf *bytes.Buffer, s string) {
	basePutUvarint(buf, uint64(len(s)))
	buf.WriteString(s)
}

func basePutCompiled(buf *bytes.Buffer, cs []CompiledCandidate) {
	basePutUvarint(buf, uint64(len(cs)))
	for _, c := range cs {
		basePutCompiledCandidate(buf, c)
	}
}

func basePutCompiledCandidate(buf *bytes.Buffer, c CompiledCandidate) {
	basePutVarint(buf, c.AccountID)
	basePutStr(buf, string(c.Lane))
	basePutVarint(buf, c.TemplateID)
	basePutStr(buf, c.BaseURL)
	basePutStr(buf, c.Fingerprint)
	basePutStr(buf, c.RequestedModel)
	basePutStr(buf, c.MappedModel)
	basePutStr(buf, string(c.MappingMode))
	basePutStr(buf, c.Quality)
	basePutStr(buf, c.QualityRaw)
	basePutVarint(buf, c.IdentityRevision)
}

func basePutCacheDomainPlan(buf *bytes.Buffer, decision *RouteDecision) {
	basePutUvarint(buf, uint64(len(decision.CacheDomainRing.Domains)))
	for _, domain := range decision.CacheDomainRing.Domains {
		basePutStr(buf, domain)
	}
	basePutUvarint(buf, uint64(len(decision.CacheDomainRing.Nodes)))
	for _, node := range decision.CacheDomainRing.Nodes {
		basePutUvarint(buf, node.Hash)
		basePutStr(buf, node.Domain)
	}
	basePutUvarint(buf, uint64(len(decision.CacheDomainAccounts)))
	for _, account := range decision.CacheDomainAccounts {
		basePutVarint(buf, account.AccountID)
		basePutStr(buf, account.Domain)
	}
}

func basePutIncident(buf *bytes.Buffer, inc RouteIncident) {
	if inc.Active {
		basePutUvarint(buf, 1)
	} else {
		basePutUvarint(buf, 0)
	}
	basePutStr(buf, inc.Kind)
	basePutUvarint(buf, uint64(inc.Comparable))
	basePutUvarint(buf, uint64(inc.Degraded))
	basePutUvarint(buf, uint64(inc.Domains))
	basePutVarint(buf, inc.EvaluatedMinute)
}

// TestCompareSinkFrozenBaseEncoderOracle drives the compare sink against bytes
// produced by the INDEPENDENT frozen encoder across multiple boundaries:
// first/middle/last byte differences, a longer and a shorter old, an empty old,
// a >64KB string field, and a difference inside a multi-byte (cross-block)
// groupID varint. The production encoder must byte-match the oracle first;
// only then are the compare-sink verdicts meaningful.
func TestCompareSinkFrozenBaseEncoderOracle(t *testing.T) {
	small := schedulerWithAccounts(t, 3, domain.ModelMapping{})
	big := strings.Repeat("z", 70000)
	bigSched := newSched(t, newMemLoader(map[int64][]*domain.Account{
		10: {accWithEnabled(1, tplWith(domain.FormatOpenAIChat, []string{big}), true, 4)},
	}))
	multi := newSched(t, newMemLoader(map[int64][]*domain.Account{
		1: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)},
		2: {acc(2, tpl(2, domain.FormatOpenAIChat, []string{"m"}), 4)},
	}))

	views := []*DecisionView{
		small.View().DecisionView(),
		bigSched.View().DecisionView(),
		multi.View().DecisionView(),
		{routes: map[RouteRef]*RouteDecision{}},
	}
	for i, dv := range views {
		frozen := baseEncodeDecisionView(dv)
		require.Equal(t, frozen, decisionViewBytes(dv),
			"view %d: production encoder must byte-match the frozen BASE oracle", i)
		if len(frozen) == 0 {
			continue
		}
		var e decisionEncoder
		require.True(t, e.compareCanonical(dv, frozen), "view %d: identical frozen bytes compare equal", i)
		for _, k := range []int{0, len(frozen) / 2, len(frozen) - 1} {
			cpy := append([]byte(nil), frozen...)
			cpy[k] ^= 0xff
			require.False(t, e.compareCanonical(dv, cpy), "view %d: byte %d diff ⇒ unequal", i, k)
		}
		require.False(t, e.compareCanonical(dv, append(append([]byte(nil), frozen...), 0x00)), "view %d: longer old ⇒ unequal", i)
		require.False(t, e.compareCanonical(dv, append([]byte(nil), frozen[:len(frozen)-1]...)), "view %d: shorter old ⇒ unequal", i)
		require.False(t, e.compareCanonical(dv, nil), "view %d: empty old ⇒ unequal", i)
	}

	// Multi-byte (cross-block) varint: a large group id makes the reference
	// groupID varint span several bytes; byte 0 is the route-count uvarint, so
	// byte 1 belongs to the groupID varint.
	bigGID := newSched(t, newMemLoader(map[int64][]*domain.Account{
		1 << 40: {acc(1, tpl(1, domain.FormatOpenAIChat, []string{"m"}), 4)},
	}))
	dvG := bigGID.View().DecisionView()
	frozenG := baseEncodeDecisionView(dvG)
	require.Equal(t, frozenG, decisionViewBytes(dvG), "large-group view must byte-match the oracle")
	require.GreaterOrEqual(t, len(frozenG), 3)
	var e decisionEncoder
	require.True(t, e.compareCanonical(dvG, frozenG))
	cpyG := append([]byte(nil), frozenG...)
	cpyG[1] ^= 0xff
	require.False(t, e.compareCanonical(dvG, cpyG), "difference inside the multi-byte groupID varint")
}
