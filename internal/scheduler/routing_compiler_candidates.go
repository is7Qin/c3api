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

func filterCandidates(candidates []*accountSnapshot, health map[HealthKey]HealthState, latched map[LatchKey]bool, rk routeKey, op domain.OperationTag) []*accountSnapshot {
	out := make([]*accountSnapshot, 0, len(candidates))
	for _, a := range candidates {
		av := a.static.Load()
		if !av.acc.Enabled || av.acc.LifecycleRevision < 0 {
			continue
		}
		fp, err := candidateFingerprint(&av.acc)
		if err != nil {
			fp = ""
		}
		rev := av.acc.LifecycleRevision
		if fp != "" {
			lk := LatchKey{AccountID: av.acc.ID, Fingerprint: fp, Revision: rev}
			if latched != nil {
				if v, ok := latched[lk]; ok && v {
					continue
				}
				mismatch := false
				for k, v := range latched {
					if !v {
						continue
					}
					if k.AccountID == av.acc.ID && (k.Fingerprint != fp || k.Revision != rev) {
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
				if k.AccountID == av.acc.ID {
					hasLatch = true
					break
				}
			}
			if hasLatch {
				continue
			}
		}
		if health != nil && len(health) > 0 {
			resolved := rk.model
			if av.tpl != nil {
				if m, ok := av.tpl.ModelMapping[rk.model]; ok {
					resolved = m.MappedModel
				}
			}
			qc := qualityClassHexForWithOp(rk.format, resolved, op)
			hkSpec := HealthKey{AccountID: av.acc.ID, Quality: qc, Revision: rev}
			hkWild := HealthKey{AccountID: av.acc.ID, Quality: "*", Revision: rev}
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
					if hk.AccountID != av.acc.ID {
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
		out = append(out, a)
	}
	return out
}

func qualityClassHexFor(rk routeKey) string {
	op := operationTagForFormat(string(rk.format))
	return qualityClassHexForWithOp(rk.format, rk.model, op)
}

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

func compilerHealthKeyFor(acc *domain.Account, format domain.RequestFormat, model string) HealthKey {
	rev := acc.LifecycleRevision
	resolved := model
	if acc.Template != nil {
		if m, ok := acc.Template.ModelMapping[model]; ok {
			resolved = m.MappedModel
		}
	}
	op := operationTagForFormat(string(format))
	qc := qualityClassHexForWithOp(format, resolved, op)
	return HealthKey{AccountID: acc.ID, Quality: qc, Revision: rev}
}

func compilerLatchKeyFor(acc *domain.Account) LatchKey {
	fp, _ := candidateFingerprint(acc)
	return LatchKey{AccountID: acc.ID, Fingerprint: fp, Revision: acc.LifecycleRevision}
}
