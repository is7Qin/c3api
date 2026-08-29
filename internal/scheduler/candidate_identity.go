// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"fmt"

	"github.com/is7qin/c3api/internal/domain"
)

func candidateFingerprint(a *domain.Account) (string, error) {
	return CandidateFingerprint(a)
}

// CandidateFingerprint is the canonical fingerprint authority (exported for sdkbridge reuse).
// Single source of truth: domain.AccountCandidateFingerprint.
func CandidateFingerprint(a *domain.Account) (string, error) {
	fp, err := domain.AccountCandidateFingerprint(a)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrMissingCandidateFingerprint, err)
	}
	return domain.CandidateFPHex(fp), nil
}
