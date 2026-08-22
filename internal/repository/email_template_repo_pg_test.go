// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

func TestPGEmailTemplateAbsentFallback(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	// no rows seeded → Get returns nil,nil (service falls back to DefaultEmailTemplate)
	got, err := repos.GetEmailTemplate(ctx, "register_code")
	require.NoError(t, err)
	require.Nil(t, got)

	rows, err := repos.ListEmailTemplates(ctx)
	require.NoError(t, err)
	require.Empty(t, rows, "empty DB returns empty list (service synthesizes defaults)")

	// also verify DefaultEmailTemplate is available
	d := domain.DefaultEmailTemplate(domain.EmailTemplateRegisterCode)
	require.Contains(t, d.Subject, "{{app_name}}")
	require.Contains(t, d.BodyText, "{{code}}")
}

func TestPGEmailTemplateUpsertAndDelete(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()

	t.Run("upsert create and overwrite", func(t *testing.T) {
		row, err := repos.UpsertEmailTemplate(ctx, "register_code", "subj1", "body1 {{code}}")
		require.NoError(t, err)
		require.Equal(t, "subj1", row.Subject)
		require.Equal(t, "body1 {{code}}", row.BodyText)

		row2, err := repos.GetEmailTemplate(ctx, "register_code")
		require.NoError(t, err)
		require.Equal(t, "subj1", row2.Subject)

		// overwrite same purpose
		row3, err := repos.UpsertEmailTemplate(ctx, "register_code", "subj2", "body2 {{code}}")
		require.NoError(t, err)
		require.Equal(t, "subj2", row3.Subject)
		require.Equal(t, "body2 {{code}}", row3.BodyText)

		rows, err := repos.ListEmailTemplates(ctx)
		require.NoError(t, err)
		require.Len(t, rows, 1, "same purpose does not duplicate")

		// second purpose independent
		_, err = repos.UpsertEmailTemplate(ctx, "reset_code", "reset subj", "reset body {{code}}")
		require.NoError(t, err)
		rows, err = repos.ListEmailTemplates(ctx)
		require.NoError(t, err)
		require.Len(t, rows, 2)
	})

	t.Run("PUT empty body deletes row (restore default)", func(t *testing.T) {
		// ensure register_code exists
		_, err := repos.UpsertEmailTemplate(ctx, "register_code", "to delete", "will be deleted")
		require.NoError(t, err)

		// delete via repo (mirrors service handling of empty body_text)
		require.NoError(t, repos.DeleteEmailTemplate(ctx, "register_code"))
		got, err := repos.GetEmailTemplate(ctx, "register_code")
		require.NoError(t, err)
		require.Nil(t, got, "deleted row should be absent → fallback")

		// delete missing → ErrNotFound
		err = repos.DeleteEmailTemplate(ctx, "register_code")
		require.ErrorIs(t, err, repository.ErrNotFound)

		// reset_code still present (independent)
		got2, err := repos.GetEmailTemplate(ctx, "reset_code")
		require.NoError(t, err)
		require.NotNil(t, got2)
	})
}
