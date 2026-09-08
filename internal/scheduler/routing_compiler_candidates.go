// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"encoding/binary"
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

type compilerCandidateFacts struct {
	account             *accountSnapshot
	static              *snapshotStatic
	accountID           int64
	revision            int64
	templateID          int64
	baseURL             string
	fingerprint         string
	identityFingerprint domain.CandidateFingerprintVal
	requestedModel      string
	mappedModel         string
	mappingMode         domain.ModelMappingMode
	quality             string
	qualityRaw          string
}

func buildCandidateFacts(candidates []*accountSnapshot, rk routeKey, op domain.OperationTag) []compilerCandidateFacts {
	facts := make([]compilerCandidateFacts, 0, len(candidates))
	for _, account := range candidates {
		fact := compilerCandidateFacts{account: account, requestedModel: rk.model, mappedModel: rk.model}
		if account != nil {
			fact.static = account.static.Load()
		}
		if fact.static != nil {
			fact.accountID = fact.static.acc.ID
			fact.revision = fact.static.acc.LifecycleRevision
			fact.templateID = fact.static.acc.TemplateID
			if fact.static.tpl != nil {
				fact.baseURL = fact.static.tpl.BaseURL
			}
			if fact.static.acc.BaseURL != nil && *fact.static.acc.BaseURL != "" {
				fact.baseURL = *fact.static.acc.BaseURL
			}
			if fp, err := candidateFingerprint(&fact.static.acc); err == nil {
				fact.fingerprint = fp
			}
			fact.identityFingerprint = candidateIdentityFingerprint(fact.fingerprint, fact.accountID)
			if fact.static.tpl != nil {
				if mapping, ok := fact.static.tpl.ModelMapping[rk.model]; ok {
					fact.mappedModel = mapping.MappedModel
					fact.mappingMode = mapping.Mode
				}
			}
			format := rk.format
			fact.quality = qualityClassHexForWithOp(format, fact.mappedModel, op)
			fact.qualityRaw = qualityClassHexForWithOp(format, fact.requestedModel, op)
		}
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
