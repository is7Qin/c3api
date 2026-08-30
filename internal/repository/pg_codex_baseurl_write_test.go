// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

func TestPGCodexWritesRejectBaseURLAndRemainAtomic(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	codexTemplate := createPGWriteTemplate(t, repos, "codex", credential.TypeCodexOAuth)
	apiTemplate := createPGWriteTemplate(t, repos, "api", credential.TypeAPIKey)
	override := "https://override.example.com"

	_, err := repos.Accounts.CreateAccount(ctx, &domain.Account{
		Name: "rejected", TemplateID: codexTemplate.ID, BaseURL: &override, MaxConcurrency: 8,
	})
	require.ErrorIs(t, err, repository.ErrInvalidInput)
	t.Logf("codex account create rejected: %v", err)

	account := createPGWriteAccount(t, repos, apiTemplate.ID, "account", &override)
	apiTemplate.CredentialType = credential.TypeCodexPAT
	apiTemplate.BaseURL = ""
	_, err = repos.Templates.UpdateTemplate(ctx, apiTemplate)
	require.ErrorIs(t, err, repository.ErrInvalidInput)
	storedTemplate, err := repos.Templates.GetTemplate(ctx, apiTemplate.ID)
	require.NoError(t, err)
	require.Equal(t, credential.TypeAPIKey, storedTemplate.CredentialType)
	t.Logf("failed template switch left credential_type=%s base_url=%q", storedTemplate.CredentialType, storedTemplate.BaseURL)

	err = repos.Accounts.UpdateAccountsBatch(ctx, []int64{account.ID}, repository.AccountPatch{TemplateID: &codexTemplate.ID})
	require.ErrorIs(t, err, repository.ErrInvalidInput)
	storedAccount, err := repos.Accounts.GetAccount(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, apiTemplate.ID, storedAccount.TemplateID)
	require.NotNil(t, storedAccount.BaseURL)
	t.Logf("failed account batch left template_id=%d base_url=%q", storedAccount.TemplateID, *storedAccount.BaseURL)

	empty := ""
	require.NoError(t, repos.Accounts.UpdateAccountsBatch(ctx, []int64{account.ID}, repository.AccountPatch{
		TemplateID: &codexTemplate.ID, BaseURL: &empty,
	}))
	storedAccount, err = repos.Accounts.GetAccount(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, codexTemplate.ID, storedAccount.TemplateID)
	require.Nil(t, storedAccount.BaseURL)
}

func TestPGCodexTemplateCreateRejectsBaseURLWithoutRow(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	for _, typ := range []credential.Type{credential.TypeCodexOAuth, credential.TypeCodexPAT} {
		for index, baseURL := range []string{"https://override.example.com", "   "} {
			name := fmt.Sprintf("rejected-%s-%d", typ, index)
			_, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
				Name: name, BaseURL: baseURL, CredentialType: typ,
				SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
				ModelMapping:     domain.ModelMapping{},
			})
			require.ErrorIs(t, err, repository.ErrInvalidInput)
			rows, total, listErr := repos.Templates.ListTemplates(ctx, repository.ListQuery{Name: name})
			require.NoError(t, listErr)
			require.Zero(t, total)
			require.Empty(t, rows)
		}
	}
}

func TestPGTemplateSwitchAndAccountURLWriteCannotViolateCodexInvariant(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	for iteration := range 20 {
		tpl := createPGWriteTemplate(t, repos, fmt.Sprintf("race-template-%d", iteration), credential.TypeAPIKey)
		account := createPGWriteAccount(t, repos, tpl.ID, fmt.Sprintf("race-account-%d", iteration), nil)
		override := "https://override.example.com"
		codexUpdate := *tpl
		codexUpdate.CredentialType = credential.TypeCodexOAuth
		codexUpdate.BaseURL = ""
		accountUpdate := *account
		accountUpdate.BaseURL = &override

		errorsSeen := runPGWriteWave(t,
			func() error {
				_, err := repos.Templates.UpdateTemplate(ctx, &codexUpdate)
				return err
			},
			func() error {
				_, err := repos.Accounts.UpdateAccount(ctx, &accountUpdate, nil)
				return err
			},
		)
		require.Equal(t, 1, countInvalidInput(errorsSeen))
		requirePGCodexBaseURLInvariant(t, repos, account.ID)
	}
}

func TestPGBatchAccountTemplateIDDriftCannotEscapeLockedTemplates(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	for iteration := range 20 {
		apiTemplate := createPGWriteTemplate(t, repos, fmt.Sprintf("drift-api-%d", iteration), credential.TypeAPIKey)
		codexTemplate := createPGWriteTemplate(t, repos, fmt.Sprintf("drift-codex-%d", iteration), credential.TypeCodexPAT)
		account := createPGWriteAccount(t, repos, apiTemplate.ID, fmt.Sprintf("drift-account-%d", iteration), nil)
		override := "https://override.example.com"
		fullUpdate := *account
		fullUpdate.TemplateID = codexTemplate.ID
		fullUpdate.BaseURL = nil

		runPGWriteWave(t,
			func() error {
				return repos.Accounts.UpdateAccountsBatch(ctx, []int64{account.ID}, repository.AccountPatch{BaseURL: &override})
			},
			func() error {
				_, err := repos.Accounts.UpdateAccount(ctx, &fullUpdate, nil)
				return err
			},
		)
		requirePGCodexBaseURLInvariant(t, repos, account.ID)
	}
}

func TestPGCancelledTemplateLockWaitRollsBackCleanly(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	tpl := createPGWriteTemplate(t, repos, "cancel-template", credential.TypeAPIKey)

	conn := pgSharedConn(t)
	tx, err := conn.Begin(ctx)
	require.NoError(t, err)
	_, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, int64(0x4333415049540000)^tpl.ID)
	require.NoError(t, err)

	blockedCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	_, err = repos.Accounts.CreateAccount(blockedCtx, &domain.Account{
		Name: "cancelled", TemplateID: tpl.ID, UpstreamKey: "sk-test", MaxConcurrency: 8,
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NoError(t, tx.Rollback(ctx))

	created := createPGWriteAccount(t, repos, tpl.ID, "after-cancel", nil)
	require.Positive(t, created.ID)
}

func runPGWriteWave(t *testing.T, writes ...func() error) []error {
	t.Helper()
	gate := make(chan struct{})
	ready := make(chan struct{}, len(writes))
	results := make(chan error, len(writes))
	for _, write := range writes {
		go func() {
			ready <- struct{}{}
			<-gate
			results <- write()
		}()
	}
	watchdog := time.NewTimer(5 * time.Second)
	defer watchdog.Stop()
	for range writes {
		select {
		case <-ready:
		case <-watchdog.C:
			t.Fatal("writers did not reach start barrier")
		}
	}
	close(gate)
	errorsSeen := make([]error, 0, len(writes))
	for range writes {
		select {
		case err := <-results:
			errorsSeen = append(errorsSeen, err)
		case <-watchdog.C:
			t.Fatal("writers did not complete before watchdog")
		}
	}
	return errorsSeen
}

func createPGWriteTemplate(t *testing.T, repos *repository.Repository, name string, typ credential.Type) *domain.Template {
	t.Helper()
	baseURL := "https://api.example.com"
	format := domain.FormatOpenAIChat
	if typ == credential.TypeCodexOAuth || typ == credential.TypeCodexPAT {
		baseURL = ""
		format = domain.FormatOpenAIResponses
	}
	tpl, err := repos.Templates.CreateTemplate(context.Background(), &domain.Template{
		Name: name, BaseURL: baseURL, CredentialType: typ,
		SupportedFormats: []domain.RequestFormat{format},
		ModelMapping:     domain.ModelMapping{},
	})
	require.NoError(t, err)
	return tpl
}

func createPGWriteAccount(t *testing.T, repos *repository.Repository, templateID int64, name string, baseURL *string) *domain.Account {
	t.Helper()
	account, err := repos.Accounts.CreateAccount(context.Background(), &domain.Account{
		Name: name, TemplateID: templateID, BaseURL: baseURL,
		UpstreamKey: "sk-test", Weight: 1, MaxConcurrency: 8,
	})
	require.NoError(t, err)
	return account
}

func requirePGCodexBaseURLInvariant(t *testing.T, repos *repository.Repository, accountID int64) {
	t.Helper()
	account, err := repos.Accounts.GetAccount(context.Background(), accountID)
	require.NoError(t, err)
	tpl, err := repos.Templates.GetTemplate(context.Background(), account.TemplateID)
	require.NoError(t, err)
	if tpl.CredentialType == credential.TypeCodexOAuth || tpl.CredentialType == credential.TypeCodexPAT {
		require.True(t, account.BaseURL == nil || *account.BaseURL == "")
	}
}

func countInvalidInput(values []error) int {
	count := 0
	for _, err := range values {
		if errors.Is(err, repository.ErrInvalidInput) {
			count++
		}
	}
	return count
}
