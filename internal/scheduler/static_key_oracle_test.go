// SPDX-License-Identifier: AGPL-3.0-or-later
package scheduler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// frozenPlanKey re-implements the ORIGINAL plan-key derivation independently of
// planKeyOf / newSnapshotStatic (a frozen old-algorithm oracle). The constructor
// must produce a key equal to this derivation for the same inputs; comparing the
// constructor against itself would prove nothing.
func frozenPlanKey(acc domain.Account, tpl *domain.Template, gids []int64) planKey {
	var k planKey
	k.accountID = acc.ID
	k.name = acc.Name
	k.templateID = acc.TemplateID
	if tpl != nil {
		k.baseURL = tpl.BaseURL
	}
	if acc.BaseURL != nil && *acc.BaseURL != "" {
		k.baseURL = *acc.BaseURL
	}
	k.upstreamKey = acc.UpstreamKey
	k.maxConcurrency = acc.MaxConcurrency
	k.enabled = acc.Enabled
	k.cacheDomain = normalizeCacheDomain(acc.CacheDomain)
	k.upstreamCostMultiplierBp = domain.MultBp(acc.UpstreamCostMultiplierBp)
	k.identityRevision = acc.IdentityRevision
	k.supplierUserID = acc.SupplierUserID
	if ext := acc.Ext; ext != nil {
		k.codexAccountID = derefString(ext.CodexAccountID)
		k.codexEmail = derefString(ext.CodexEmail)
		k.codexPATKey = derefString(ext.CodexPATKey)
		if id := ext.CodexIdentity; id != nil {
			k.codexInstallation = id.InstallationID
		}
	}
	if tpl != nil {
		k.credentialType = tpl.CredentialType
		k.stripImageTools = tpl.StripImageTools
		k.modelsDigest = digestStrings(tpl.Models)
		k.formatModelsKey = digestFormatModels(tpl.FormatModels)
		k.supportedFormats = digestFormats(tpl.SupportedFormats)
		k.modelMappingKey = digestModelMapping(tpl.ModelMapping)
	}
	k.groupIDsDigest = digestInt64s(gids)
	return k
}

// TestFrozenPlanKeyOracleMatchesConstructor checks the cached key against the
// frozen old-algorithm oracle for nil/empty inputs, groupIDs order permutation,
// duplicates, and content changes, and confirms the sensitivity that the key is
// meant to carry.
func TestFrozenPlanKeyOracleMatchesConstructor(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m-a", "m-b"})
	tpl.FormatModels = map[domain.RequestFormat][]string{domain.FormatOpenAIChat: {"m-a"}}
	tpl.ModelMapping = map[string]domain.ModelMappingEntry{"m-a": {MappedModel: "x", Mode: domain.ModelMappingModeExplicit}}
	a := *acc(7, tpl, 4)
	a.Ext = &domain.AccountExt{CodexAccountID: strPtr("cid"), CodexEmail: strPtr("e@x"), CodexPATKey: strPtr("pat")}
	a.SupplierUserID = 42

	cases := []struct {
		name string
		acc  domain.Account
		tpl  *domain.Template
		gids []int64
	}{
		{"nil-tpl-empty-gids", domain.Account{ID: 1}, nil, nil},
		{"base", a, tpl, []int64{3, 1, 2}},
		{"permuted", a, tpl, []int64{1, 2, 3}},
		{"duplicates", a, tpl, []int64{1, 1, 2}},
	}
	for _, tc := range cases {
		got := planKeyOf(newSnapshotStatic(tc.acc, tc.tpl, tc.gids))
		require.Equal(t, frozenPlanKey(tc.acc, tc.tpl, tc.gids), got, tc.name)
	}

	// Permutation of the same groupID set is invariant (canonical digest).
	require.Equal(t,
		planKeyOf(newSnapshotStatic(a, tpl, []int64{3, 1, 2})),
		planKeyOf(newSnapshotStatic(a, tpl, []int64{1, 2, 3})),
		"groupIDs permutation must not change the key")
	// Raw duplicate elements are sensitive (digest does not de-dup).
	require.NotEqual(t,
		planKeyOf(newSnapshotStatic(a, tpl, []int64{1, 2})),
		planKeyOf(newSnapshotStatic(a, tpl, []int64{1, 1, 2})))

	// Mapping-mode change is sensitive.
	tplMode := *tpl
	tplMode.ModelMapping = map[string]domain.ModelMappingEntry{"m-a": {MappedModel: "x", Mode: domain.ModelMappingModeImplicit}}
	require.NotEqual(t,
		planKeyOf(newSnapshotStatic(a, tpl, nil)),
		planKeyOf(newSnapshotStatic(a, &tplMode, nil)),
		"mapping mode change must change the key")
	// Model-content change is sensitive.
	tplModels := *tpl
	tplModels.Models = []string{"m-a", "m-c"}
	require.NotEqual(t,
		planKeyOf(newSnapshotStatic(a, tpl, nil)),
		planKeyOf(newSnapshotStatic(a, &tplModels, nil)),
		"model content change must change the key")
	// payloadKey (OAuth rotation) must NOT affect planKey.
	aRot := a
	aRot.Ext = &domain.AccountExt{CodexAccountID: strPtr("cid"), CodexEmail: strPtr("e@x"), CodexPATKey: strPtr("pat"),
		CodexOAuthToken: strPtr("new-at")}
	require.Equal(t,
		planKeyOf(newSnapshotStatic(a, tpl, nil)),
		planKeyOf(newSnapshotStatic(aRot, tpl, nil)),
		"OAuth payload rotation must not change planKey")
}
