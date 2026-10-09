// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"sort"

	"github.com/is7qin/c3api/internal/domain"
)

func fullCandidateUnion(gs *groupSnapshot, rootFacts map[int64]compilerAccountFacts, rk routeKey) []*accountSnapshot {
	if gs == nil {
		return nil
	}
	seen := make(map[int64]*accountSnapshot)
	for _, a := range gs.accounts {
		if a == nil {
			continue
		}
		fact, ok := rootFacts[a.accountID]
		if !ok || fact.account != a || fact.static == nil {
			continue
		}
		tpl := fact.static.tpl
		if tpl == nil {
			continue
		}
		if !routeSupportsAccount(tpl, rk) {
			continue
		}
		id := fact.accountID
		if _, ok := seen[id]; !ok {
			seen[id] = a
		}
	}
	ids := make([]int64, 0, len(seen))
	for id := range seen {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	out := make([]*accountSnapshot, 0, len(ids))
	for _, id := range ids {
		out = append(out, seen[id])
	}
	return out
}

func routeSupportsAccount(tpl *domain.Template, rk routeKey) bool {
	if rk.model == "" {
		if tpl.HasModelSpace() {
			return false
		}
		return tplSupportsFormat(tpl, rk.format)
	}
	if !tpl.FormatSupports(rk.format, rk.model) {
		return false
	}
	if tpl.Serves(rk.model) {
		return true
	}
	return !tpl.HasModelSpace()
}

type compilerAccountFacts struct {
	accountID           int64
	templateID          int64
	baseURL             string
	fingerprint         string
	identityFingerprint domain.CandidateFingerprintVal
	// revision is the candidate content generation = the account's
	// identity_revision (K), NOT the client lifecycle token (C): C is
	// unrelated to compiled content identity (spec §5.7(c)).
	revision int64
	account  *accountSnapshot
	static   *snapshotStatic
	// planKey 是编译期从 static 一次派生的计划有效性判据（与 baseURL/
	// fingerprint 同源，避免第二处字段清单）。routePlanCandidates 与预留
	// fence 凭它判定在途计划是否仍有效。
	planKey                  planKey
	upstreamCostMultiplierBp int
}

type compilerCandidateFacts struct {
	compilerAccountFacts
	requestedModel string
	mappedModel    string
	mappingMode    domain.ModelMappingMode
	quality        string
	qualityRaw     string
}

// deriveCompilerAccountFacts resolves the route-independent account facts
// once per static root: identity, base URL precedence, and cost multiplier.
// Only mapping and quality stay route-specific per buildCandidateFacts call.
func deriveCompilerAccountFacts(account *accountSnapshot, st *snapshotStatic) compilerAccountFacts {
	f := compilerAccountFacts{account: account, static: st}
	if st == nil {
		return f
	}
	f.accountID = st.acc.ID
	f.revision = st.acc.IdentityRevision
	f.templateID = st.acc.TemplateID
	if st.tpl != nil {
		f.baseURL = st.tpl.BaseURL
	}
	if st.acc.BaseURL != nil && *st.acc.BaseURL != "" {
		f.baseURL = *st.acc.BaseURL
	}
	if fp, err := candidateFingerprint(&st.acc); err == nil {
		f.fingerprint = fp
	}
	f.identityFingerprint = candidateIdentityFingerprint(f.fingerprint, f.accountID)
	f.planKey = planKeyOf(st)
	f.upstreamCostMultiplierBp = domain.MultBp(st.acc.UpstreamCostMultiplierBp)
	return f
}

// attachCompilerFacts builds the static-root-owned facts map and links each
// unpublished leaf to its entry. Published leaves already carry facts from
// their first staging and are never rewritten here.
func attachCompilerFacts(byID map[int64]*accountSnapshot) map[int64]compilerAccountFacts {
	facts := make(map[int64]compilerAccountFacts, len(byID))
	for id, account := range byID {
		if account == nil {
			continue
		}
		st := account.static.Load()
		if st == nil {
			continue
		}
		facts[id] = deriveCompilerAccountFacts(account, st)
	}
	return facts
}

func buildCandidateFacts(candidates []*accountSnapshot, rootFacts map[int64]compilerAccountFacts, rk routeKey, op domain.OperationTag) []compilerCandidateFacts {
	facts := make([]compilerCandidateFacts, 0, len(candidates))
	// quality hex is a pure function of (format, model, op) — and format/op are
	// fixed for this call — so memoize it compile-locally and keep the account
	// loop free of repeated sha256+hex. The memo does not affect QualityRaw
	// (the canonical wire field just holds the same string).
	qualityMemo := make(map[string]string, 4)
	qualityFor := func(model string) string {
		if h, ok := qualityMemo[model]; ok {
			return h
		}
		h := qualityClassHexForWithOp(rk.format, model, op)
		qualityMemo[model] = h
		return h
	}
	for _, account := range candidates {
		fact := compilerCandidateFacts{requestedModel: rk.model, mappedModel: rk.model}
		if account == nil {
			fact.account = account
			facts = append(facts, fact)
			continue
		}
		cf, ok := rootFacts[account.accountID]
		if !ok || cf.account != account || cf.static == nil {
			facts = append(facts, fact)
			continue
		}
		fact.compilerAccountFacts = cf
		if cf.static.tpl != nil {
			if mapping, ok := cf.static.tpl.ModelMapping[rk.model]; ok {
				fact.mappedModel = mapping.MappedModel
				fact.mappingMode = mapping.Mode
			}
		}
		fact.quality = qualityFor(fact.mappedModel)
		if fact.mappedModel == fact.requestedModel {
			// Identity mapping ⇒ the quality class is the same; reuse the raw
			// value (no second memo probe / recompute).
			fact.qualityRaw = fact.quality
		} else {
			fact.qualityRaw = qualityFor(fact.requestedModel)
		}
		facts = append(facts, fact)
	}
	return facts
}

// filterCandidates keeps statically eligible candidates only, compacting them
// IN PLACE (stable order preserved).
//
// CONSUMING CONTRACT: after this call the input slice MUST NOT be used again —
// its backing array is overwritten with the surviving entries (zero extra
// allocation). A caller that needs the original entries must copy first.
//
// v5-§5.1A (COMPILED-HEALTH-FREE): the health/latch branches are DELETED —
// serving gates live solely in the live reserveOnView path
// (attempt_plan_reservation.go: latch, mapping-aware EffectiveState,
// StatusDisabled, plus leaf-freshness and concurrency CAS), which applies the
// identical gates per attempt, strictly fresher, with skip-and-continue.
// Compiled health additionally churned generations and invalidated in-flight
// plans — deleting the class removes a harm (anti-harm clause).
func filterCandidates(candidates []compilerCandidateFacts) []compilerCandidateFacts {
	out := candidates[:0]
	for _, fact := range candidates {
		// The `< 0` arm is dead defence: identity_revision is a non-negative
		// generation (>=1 in practice), unlike the lifecycle token it replaced.
		if fact.static == nil || !fact.static.acc.Enabled || fact.static.acc.IdentityRevision < 0 {
			continue
		}
		if fact.static.tpl == nil || (fact.static.tpl.CredentialType != "" && !fact.static.tpl.CredentialType.Valid()) {
			continue
		}
		out = append(out, fact)
	}
	return out
}
