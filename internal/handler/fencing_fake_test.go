// SPDX-License-Identifier: AGPL-3.0-or-later
package handler

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

func TestHandlerFakeStaleFencing(t *testing.T) {
	store := newFakeStore()
	ctx := context.Background()
	// create template and account via fake
	tpl, err := store.CreateTemplate(ctx, &domain.Template{Name: "tpl", BaseURL: "https://u", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}})
	require.NoError(t, err)
	acc, err := store.CreateAccount(ctx, &domain.Account{Name: "acc", TemplateID: tpl.ID, UpstreamKey: "sk-1", MaxConcurrency: 8})
	require.NoError(t, err)
	// manually set revision to 1 for determinism (fake CreateAccount uses zero value, need to set)
	acc.LifecycleRevision = 1
	store.accs[acc.ID].LifecycleRevision = 1

	// first ext put with expected 1 should succeed to 2
	ext := &domain.AccountExt{AccountID: acc.ID, CredentialType: "codex-oauth", CodexIdentity: &domain.CodexIdentity{InstallationID: "inst-1", SessionID: "sess-1", ThreadID: "sess-1", WindowID: "sess-1:0"}, CodexOAuthToken: fenceStrPtr("tok1"), CodexOAuthRefreshToken: fenceStrPtr("rt1"), CodexEmail: fenceStrPtr("e@example.com"), CodexAccountID: fenceStrPtr("a1")}
	saved, err := store.AdminUpsertAccountExtCAS(ctx, ext, 1)
	require.NoError(t, err)
	require.Equal(t, "tok1", *saved.CodexOAuthToken)
	gotAcc, _ := store.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(2), gotAcc.LifecycleRevision)

	// stale with old expected 1 should fail and leave ext unchanged
	extStale := &domain.AccountExt{AccountID: acc.ID, CredentialType: "codex-oauth", CodexIdentity: &domain.CodexIdentity{InstallationID: "inst-1", SessionID: "sess-1", ThreadID: "sess-1", WindowID: "sess-1:0"}, CodexOAuthToken: fenceStrPtr("tok-stale"), CodexOAuthRefreshToken: fenceStrPtr("rt-stale"), CodexEmail: fenceStrPtr("e@example.com"), CodexAccountID: fenceStrPtr("a1")}
	_, err = store.AdminUpsertAccountExtCAS(ctx, extStale, 1)
	require.ErrorIs(t, err, repository.ErrConflict)
	gotAcc2, _ := store.GetAccount(ctx, acc.ID)
	require.Equal(t, int64(2), gotAcc2.LifecycleRevision, "stale must not increment")
	gotExt, _ := store.GetAccountExt(ctx, acc.ID)
	require.Equal(t, "tok1", *gotExt.CodexOAuthToken, "stale must not mutate ext")

	// account credential stale
	acc2, _ := store.CreateAccount(ctx, &domain.Account{Name: "acc2", TemplateID: tpl.ID, UpstreamKey: "sk-2", MaxConcurrency: 8})
	acc2.LifecycleRevision = 1
	store.accs[acc2.ID].LifecycleRevision = 1
	require.NoError(t, store.ReplaceAccountCredentialCAS(ctx, acc2.ID, 1, "sk-new", nil))
	got2, _ := store.GetAccount(ctx, acc2.ID)
	require.Equal(t, int64(2), got2.LifecycleRevision)
	require.Equal(t, "sk-new", got2.UpstreamKey)
	// stale should fail
	err = store.ReplaceAccountCredentialCAS(ctx, acc2.ID, 1, "sk-stale", nil)
	require.ErrorIs(t, err, repository.ErrConflict)
	got2After, _ := store.GetAccount(ctx, acc2.ID)
	require.Equal(t, int64(2), got2After.LifecycleRevision)
	require.Equal(t, "sk-new", got2After.UpstreamKey)
}

func fenceStrPtr(s string) *string { return &s }
