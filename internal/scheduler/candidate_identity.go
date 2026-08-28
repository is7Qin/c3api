// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"fmt"
	"strings"

	"github.com/is7qin/c3api/internal/domain"
)

func candidateFingerprint(a *domain.Account) (string, error) {
	if a == nil || a.Template == nil {
		return "", ErrMissingCandidateFingerprint
	}
	baseURL := a.Template.BaseURL
	if a.BaseURL != nil && *a.BaseURL != "" {
		baseURL = *a.BaseURL
	}
	if strings.HasSuffix(baseURL, "/v1") {
		baseURL = strings.TrimSuffix(baseURL, "/v1")
	}
	var patKey, email, codexAccountID, installationID, sessionID, threadID, windowID string
	if a.Ext != nil {
		if a.Ext.CodexPATKey != nil {
			patKey = *a.Ext.CodexPATKey
		}
		if a.Ext.CodexEmail != nil {
			email = *a.Ext.CodexEmail
		}
		if a.Ext.CodexAccountID != nil {
			codexAccountID = *a.Ext.CodexAccountID
		}
		if a.Ext.CodexIdentity != nil {
			identity := a.Ext.CodexIdentity
			installationID, sessionID, threadID, windowID = identity.InstallationID, identity.SessionID, identity.ThreadID, identity.WindowID
		}
	}
	fp, err := domain.CandidateFingerprint(a.ID, a.TemplateID, a.Template.CredentialType, baseURL, a.UpstreamKey, patKey, email, codexAccountID, a.Template.StripImageTools, installationID, sessionID, threadID, windowID)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrMissingCandidateFingerprint, err)
	}
	return domain.CandidateFPHex(fp), nil
}
