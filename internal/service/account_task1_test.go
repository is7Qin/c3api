// SPDX-License-Identifier: AGPL-3.0-or-later
package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

func TestAccountValidationCostCache(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}}
	// need template
	_, err := svc.CreateTemplate(context.Background(), &domain.Template{Name: "t", BaseURL: "https://u", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}})
	require.NoError(t, err)

	// negative multiplier -> invalid
	_, err = svc.CreateAccount(context.Background(), &domain.Account{Name: "a", TemplateID: 1, UpstreamKey: "sk-x", UpstreamCostMultiplierBp: -1})
	require.ErrorIs(t, err, ErrInvalidInput)

	// invalid domain -> invalid
	bad := "not a domain!"
	_, err = svc.CreateAccount(context.Background(), &domain.Account{Name: "a2", TemplateID: 1, UpstreamKey: "sk-x", CacheDomain: &bad})
	require.ErrorIs(t, err, ErrInvalidInput)

	// valid shared domain
	good := "shared.example.com"
	acc, err := svc.CreateAccount(context.Background(), &domain.Account{Name: "a3", TemplateID: 1, UpstreamKey: "sk-x", CacheDomain: &good, UpstreamCostMultiplierBp: 25000})
	require.NoError(t, err)
	require.Equal(t, 25000, acc.UpstreamCostMultiplierBp)

	// empty domain (nil) allowed - private
	acc2, err := svc.CreateAccount(context.Background(), &domain.Account{Name: "a4", TemplateID: 1, UpstreamKey: "sk-x"})
	require.NoError(t, err)
	require.NotNil(t, acc2)

	// batch patch validation: negative cost
	costNeg := -5
	err = svc.UpdateAccountsBatch(context.Background(), []int64{acc.ID}, repository.AccountPatch{UpstreamCostMultiplierBp: &costNeg})
	require.ErrorIs(t, err, ErrInvalidInput)

	// batch invalid domain
	badDom := "bad_domain!"
	err = svc.UpdateAccountsBatch(context.Background(), []int64{acc.ID}, repository.AccountPatch{CacheDomain: &badDom})
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestAccountValidationLifecycle(t *testing.T) {
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}}
	_, err := svc.CreateTemplate(context.Background(), &domain.Template{Name: "t", BaseURL: "https://u", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat}})
	require.NoError(t, err)
	acc, err := svc.CreateAccount(context.Background(), &domain.Account{Name: "a", TemplateID: 1, UpstreamKey: "sk-x"})
	require.NoError(t, err)
	// lifecycle revision negative invalid
	acc.LifecycleRevision = -1
	_, err = svc.UpdateAccount(context.Background(), acc)
	require.ErrorIs(t, err, ErrInvalidInput)
}

func TestCodexImportTask1(t *testing.T) {
	// Verify codex import path still works after Task1 schema changes (no regression)
	fs := newFakeStore()
	svc := &Service{store: fs, inv: &invRecorder{}}
	// need templates of codex type
	tplOauth, err := svc.CreateTemplate(context.Background(), &domain.Template{Name: "tpl-oauth", BaseURL: "https://u", CredentialType: "codex-oauth", SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses}})
	require.NoError(t, err)
	// import oauth
	items := []domain.CodexOAuthImportItem{{CodexEmail: "a@example.com", CodexAccountID: "acc1", CodexOAuthToken: "tok", CodexOAuthRefreshToken: "rt"}}
	res, err := svc.ImportCodexOAuthAccounts(context.Background(), items, &tplOauth.ID, nil)
	require.NoError(t, err)
	require.Equal(t, 1, res.Imported)
}
