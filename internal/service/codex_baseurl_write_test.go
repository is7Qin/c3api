// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

func TestNonCodexBaseURLWritesRemainAccepted(t *testing.T) {
	ctx := context.Background()
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}}

	for _, typ := range []credential.Type{credential.TypeAPIKey, credential.TypeResponsesSpecial} {
		tpl, err := svc.CreateTemplate(ctx, codexWriteTemplate("tpl-"+string(typ), typ, "https://template.example.com"))
		require.NoError(t, err)
		accountURL := "https://account.example.com"
		account, err := svc.CreateAccount(ctx, &domain.Account{
			Name: "account-" + string(typ), TemplateID: tpl.ID, BaseURL: &accountURL,
			UpstreamKey: "sk-test", MaxConcurrency: 8,
		})
		require.NoError(t, err)
		batchURL := "https://batch.example.com"
		require.NoError(t, svc.UpdateAccountsBatch(ctx, []int64{account.ID}, repository.AccountPatch{BaseURL: &batchURL}))
	}
}

func TestCodexTemplateCreateAndFullUpdateRejectBaseURL(t *testing.T) {
	ctx := context.Background()
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}}

	for _, typ := range []credential.Type{credential.TypeCodexOAuth, credential.TypeCodexPAT} {
		for _, baseURL := range []string{"https://override.example.com", "   "} {
			_, err := svc.CreateTemplate(ctx, codexWriteTemplate("bad-"+string(typ)+baseURL, typ, baseURL))
			require.ErrorIs(t, err, ErrInvalidInput)
		}
		created, err := svc.CreateTemplate(ctx, codexWriteTemplate("empty-"+string(typ), typ, ""))
		require.NoError(t, err)

		apiTemplate, err := svc.CreateTemplate(ctx, codexWriteTemplate("switch-"+string(typ), credential.TypeAPIKey, "https://api.example.com"))
		require.NoError(t, err)
		apiTemplate.CredentialType = typ
		apiTemplate.SupportedFormats = []domain.RequestFormat{domain.FormatOpenAIResponses}
		_, err = svc.UpdateTemplate(ctx, apiTemplate)
		require.ErrorIs(t, err, ErrInvalidInput)
		apiTemplate.BaseURL = ""
		_, err = svc.UpdateTemplate(ctx, apiTemplate)
		require.NoError(t, err)
		require.Empty(t, created.BaseURL)
	}
}

func TestCodexTemplateBatchRejectsMixedTargetsAtomically(t *testing.T) {
	for _, typ := range []credential.Type{credential.TypeCodexOAuth, credential.TypeCodexPAT} {
		t.Run(string(typ), func(t *testing.T) {
			ctx := context.Background()
			svc := &Service{store: newFakeStore(), inv: &invRecorder{}}
			codexTemplate, err := svc.CreateTemplate(ctx, codexWriteTemplate("codex", typ, ""))
			require.NoError(t, err)
			apiTemplate, err := svc.CreateTemplate(ctx, codexWriteTemplate("api", credential.TypeAPIKey, "https://api.example.com"))
			require.NoError(t, err)

			override := "https://override.example.com"
			err = svc.UpdateTemplatesBatch(ctx, []int64{apiTemplate.ID, codexTemplate.ID}, repository.TemplatePatch{BaseURL: &override})
			require.ErrorIs(t, err, ErrInvalidInput)
			gotAPI, err := svc.GetTemplate(ctx, apiTemplate.ID)
			require.NoError(t, err)
			require.Equal(t, "https://api.example.com", gotAPI.BaseURL)

			empty := ""
			require.NoError(t, svc.UpdateTemplatesBatch(ctx, []int64{codexTemplate.ID}, repository.TemplatePatch{BaseURL: &empty}))
		})
	}
}

func TestCodexAccountCreateAndFullUpdateRejectBaseURL(t *testing.T) {
	ctx := context.Background()
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}}

	for _, typ := range []credential.Type{credential.TypeCodexOAuth, credential.TypeCodexPAT} {
		tpl, err := svc.CreateTemplate(ctx, codexWriteTemplate("tpl-"+string(typ), typ, ""))
		require.NoError(t, err)
		for _, baseURL := range []string{"https://override.example.com", "   "} {
			_, err = svc.CreateAccount(ctx, &domain.Account{Name: "bad", TemplateID: tpl.ID, BaseURL: &baseURL})
			require.ErrorIs(t, err, ErrInvalidInput)
		}
		account, err := svc.CreateAccount(ctx, &domain.Account{Name: "nil", TemplateID: tpl.ID})
		require.NoError(t, err)
		empty := ""
		_, err = svc.CreateAccount(ctx, &domain.Account{Name: "empty", TemplateID: tpl.ID, BaseURL: &empty})
		require.NoError(t, err)
		override := "https://override.example.com"
		account.BaseURL = &override
		_, err = svc.UpdateAccount(ctx, account)
		require.ErrorIs(t, err, ErrInvalidInput)
		account.BaseURL = nil
		_, err = svc.UpdateAccount(ctx, account)
		require.NoError(t, err)
	}
}

func TestCodexAccountTemplateTransitionsValidatePostPatchState(t *testing.T) {
	ctx := context.Background()
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}}
	apiTemplate, err := svc.CreateTemplate(ctx, codexWriteTemplate("api", credential.TypeAPIKey, "https://api.example.com"))
	require.NoError(t, err)
	codexTemplate, err := svc.CreateTemplate(ctx, codexWriteTemplate("codex", credential.TypeCodexPAT, ""))
	require.NoError(t, err)
	override := "https://override.example.com"
	account, err := svc.CreateAccount(ctx, &domain.Account{
		Name: "account", TemplateID: apiTemplate.ID, BaseURL: &override,
		UpstreamKey: "sk-test", MaxConcurrency: 8,
	})
	require.NoError(t, err)

	account.TemplateID = codexTemplate.ID
	_, err = svc.UpdateAccount(ctx, account)
	require.ErrorIs(t, err, ErrInvalidInput)
	account.BaseURL = nil
	_, err = svc.UpdateAccount(ctx, account)
	require.NoError(t, err)

	account.TemplateID = apiTemplate.ID
	account.BaseURL = &override
	_, err = svc.UpdateAccount(ctx, account)
	require.NoError(t, err)
	err = svc.UpdateAccountsBatch(ctx, []int64{account.ID}, repository.AccountPatch{TemplateID: &codexTemplate.ID})
	require.ErrorIs(t, err, ErrInvalidInput)
	got, err := svc.GetAccount(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, apiTemplate.ID, got.TemplateID)
	require.NotNil(t, got.BaseURL)

	empty := ""
	require.NoError(t, svc.UpdateAccountsBatch(ctx, []int64{account.ID}, repository.AccountPatch{
		TemplateID: &codexTemplate.ID, BaseURL: &empty,
	}))
	got, err = svc.GetAccount(ctx, account.ID)
	require.NoError(t, err)
	require.Equal(t, codexTemplate.ID, got.TemplateID)
	require.Nil(t, got.BaseURL)
}

func TestCodexAccountBatchRejectsBaseURLForOAuthAndPAT(t *testing.T) {
	for _, typ := range []credential.Type{credential.TypeCodexOAuth, credential.TypeCodexPAT} {
		t.Run(string(typ), func(t *testing.T) {
			ctx := context.Background()
			svc := &Service{store: newFakeStore(), inv: &invRecorder{}}
			tpl, err := svc.CreateTemplate(ctx, codexWriteTemplate("codex", typ, ""))
			require.NoError(t, err)
			account, err := svc.CreateAccount(ctx, &domain.Account{Name: "account", TemplateID: tpl.ID})
			require.NoError(t, err)
			override := "https://override.example.com"
			err = svc.UpdateAccountsBatch(ctx, []int64{account.ID}, repository.AccountPatch{BaseURL: &override})
			require.ErrorIs(t, err, ErrInvalidInput)
			got, err := svc.GetAccount(ctx, account.ID)
			require.NoError(t, err)
			require.Nil(t, got.BaseURL)
		})
	}
}

func TestTemplateSwitchToCodexRejectsReferencedAccountOverride(t *testing.T) {
	ctx := context.Background()
	svc := &Service{store: newFakeStore(), inv: &invRecorder{}}
	tpl, err := svc.CreateTemplate(ctx, codexWriteTemplate("api", credential.TypeAPIKey, "https://api.example.com"))
	require.NoError(t, err)
	override := "https://override.example.com"
	_, err = svc.CreateAccount(ctx, &domain.Account{
		Name: "account", TemplateID: tpl.ID, BaseURL: &override,
		UpstreamKey: "sk-test", MaxConcurrency: 8,
	})
	require.NoError(t, err)

	tpl.CredentialType = credential.TypeCodexOAuth
	tpl.BaseURL = ""
	_, err = svc.UpdateTemplate(ctx, tpl)
	require.ErrorIs(t, err, ErrInvalidInput)
	got, err := svc.GetTemplate(ctx, tpl.ID)
	require.NoError(t, err)
	require.Equal(t, credential.TypeAPIKey, got.CredentialType)
}

func codexWriteTemplate(name string, typ credential.Type, baseURL string) *domain.Template {
	format := domain.FormatOpenAIResponses
	if typ == credential.TypeAPIKey {
		format = domain.FormatOpenAIChat
	}
	return &domain.Template{
		Name: name, CredentialType: typ, BaseURL: baseURL,
		SupportedFormats: []domain.RequestFormat{format},
	}
}
