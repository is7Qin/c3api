// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"sort"

	"github.com/is7qin/c3api/internal/domain"
)

func fullCandidateUnion(gs *groupSnapshot, rk routeKey) []*accountSnapshot {
	if gs == nil {
		return nil
	}
	seen := make(map[int64]*accountSnapshot)
	for _, a := range gs.accounts {
		if a == nil {
			continue
		}
		tpl := a.static.Load().tpl
		if tpl == nil {
			continue
		}
		if !routeSupportsAccount(tpl, rk) {
			continue
		}
		id := a.static.Load().acc.ID
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
	accountID                int64
	templateID               int64
	baseURL                  string
	fingerprint              string
	identityFingerprint      domain.CandidateFingerprintVal
	revision                 int64
	account                  *accountSnapshot
	static                   *snapshotStatic
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
	f.revision = st.acc.LifecycleRevision
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
	f.upstreamCostMultiplierBp = st.acc.UpstreamCostMultiplierBp
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
	for _, account := range candidates {
		fact := compilerCandidateFacts{requestedModel: rk.model, mappedModel: rk.model}
		var st *snapshotStatic
		if account != nil {
			st = account.static.Load()
		}
		if st == nil {
			fact.account = account
			facts = append(facts, fact)
			continue
		}
		if cf, ok := rootFacts[st.acc.ID]; ok && cf.account == account && cf.static == st {
			fact.compilerAccountFacts = cf
		} else {
			fact.compilerAccountFacts = deriveCompilerAccountFacts(account, st)
		}
		if st.tpl != nil {
			if mapping, ok := st.tpl.ModelMapping[rk.model]; ok {
				fact.mappedModel = mapping.MappedModel
				fact.mappingMode = mapping.Mode
			}
		}
		format := rk.format
		fact.quality = qualityClassHexForWithOp(format, fact.mappedModel, op)
		fact.qualityRaw = qualityClassHexForWithOp(format, fact.requestedModel, op)
		facts = append(facts, fact)
	}
	return facts
}

func filterCandidates(candidates []compilerCandidateFacts, health map[HealthKey]HealthState, latched map[LatchKey]bool) []compilerCandidateFacts {
	out := make([]compilerCandidateFacts, 0, len(candidates))
	for _, fact := range candidates {
		if fact.static == nil || !fact.static.acc.Enabled || fact.static.acc.LifecycleRevision < 0 {
			continue
		}
		fp := fact.fingerprint
		rev := fact.revision
		if fp != "" {
			lk := LatchKey{AccountID: fact.accountID, Fingerprint: fp, Revision: rev}
			if latched != nil {
				if v, ok := latched[lk]; ok && v {
					continue
				}
				mismatch := false
				for k, v := range latched {
					if !v {
						continue
					}
					if k.AccountID == fact.accountID && (k.Fingerprint != fp || k.Revision != rev) {
						mismatch = true
						break
					}
				}
				if mismatch {
					continue
				}
			}
		} else if latched != nil && len(latched) > 0 {
			hasLatch := false
			for k, v := range latched {
				if !v {
					continue
				}
				if k.AccountID == fact.accountID {
					hasLatch = true
					break
				}
			}
			if hasLatch {
				continue
			}
		}
		if health != nil && len(health) > 0 {
			qc := fact.quality
			hkSpec := HealthKey{AccountID: fact.accountID, Quality: qc, Revision: rev}
			hkWild := HealthKey{AccountID: fact.accountID, Quality: "*", Revision: rev}
			excluded := false
			if st, ok := health[hkSpec]; ok && st != StateReady {
				excluded = true
			} else if st, ok := health[hkWild]; ok && st != StateReady {
				excluded = true
			} else {
				for hk, st := range health {
					if st == StateReady {
						continue
					}
					if hk.AccountID != fact.accountID {
						continue
					}
					if hk.Quality != qc && hk.Quality != "*" {
						continue
					}
					if hk.Revision != rev {
						excluded = true
						break
					}
				}
			}
			if excluded {
				continue
			}
		}
		out = append(out, fact)
	}
	return out
}
