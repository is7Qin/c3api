// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

func TestModelMappingSQLNullRejected(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	conn := pgSharedConn(t)
	var templateID int64
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if templateID != 0 {
			_, err := conn.Exec(cleanupCtx, `DELETE FROM templates WHERE id = $1`, templateID)
			require.NoError(t, err)
		}
	})
	fixtureName := fmt.Sprintf("c3api-model-mapping-null-%d", time.Now().UnixNano())

	tpl, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name:             fixtureName,
		BaseURL:          "https://api.example.com",
		CredentialType:   "api_key",
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		ModelMapping:     domain.ModelMapping{"a": {MappedModel: "b", Mode: domain.ModelMappingModeExplicit}},
	})
	require.NoError(t, err)
	templateID = tpl.ID

	_, err = conn.Exec(ctx, `UPDATE templates SET model_mapping = NULL WHERE id = $1`, tpl.ID)
	require.Error(t, err, "SQL NULL should be rejected by NOT NULL schema")
	var pgErr *pgconn.PgError
	require.True(t, errors.As(err, &pgErr))
	require.Equal(t, "23502", pgErr.Code, "not-null violation SQLSTATE")

	_, err = conn.Exec(ctx, `UPDATE templates SET model_mapping = 'null'::jsonb WHERE id = $1`, tpl.ID)
	require.NoError(t, err, "setting JSONB literal null should succeed at SQL level")
	_, err = repos.Templates.GetTemplate(ctx, tpl.ID)
	require.Error(t, err, "reading JSONB literal null should fail closed via ModelMapping Unmarshal")
	require.Contains(t, err.Error(), "must not be null")

	_, err = conn.Exec(ctx, `INSERT INTO templates (name, base_url, credential_type, supported_formats, models, format_models, model_mapping, updated_at, created_at) VALUES ($1, 'https://api.example.com', 'api_key', '[]'::jsonb, '[]'::jsonb, '{}'::jsonb, NULL, now(), now())`, fixtureName+"-insert")
	require.Error(t, err, "INSERT SQL NULL should be rejected")
	pgErr = nil
	require.True(t, errors.As(err, &pgErr))
	require.Equal(t, "23502", pgErr.Code)
}

func TestModelMappingPGStructuredLifecycle(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	conn := pgSharedConn(t)
	var templateID int64
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if templateID != 0 {
			_, err := conn.Exec(cleanupCtx, `DELETE FROM templates WHERE id = $1`, templateID)
			require.NoError(t, err)
		}
	})

	initial := domain.ModelMapping{
		"explicit-id": {MappedModel: "explicit-id", Mode: domain.ModelMappingModeExplicit},
		"implicit-id": {MappedModel: "implicit-id", Mode: domain.ModelMappingModeImplicit},
	}
	template, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name:             fmt.Sprintf("c3api-model-mapping-structured-%d", time.Now().UnixNano()),
		CredentialType:   "api_key",
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		ModelMapping:     initial,
	})
	require.NoError(t, err)
	templateID = template.ID
	fetched, err := repos.Templates.GetTemplate(ctx, template.ID)
	require.NoError(t, err)
	require.Equal(t, initial, fetched.ModelMapping)

	updatedMapping := domain.ModelMapping{"put": {MappedModel: "put-target", Mode: domain.ModelMappingModeImplicit}}
	template.ModelMapping = updatedMapping
	_, err = repos.Templates.UpdateTemplate(ctx, template)
	require.NoError(t, err)
	fetched, err = repos.Templates.GetTemplate(ctx, template.ID)
	require.NoError(t, err)
	require.Equal(t, updatedMapping, fetched.ModelMapping)

	name := template.Name + "-preserved"
	require.NoError(t, repos.Templates.UpdateTemplatesBatch(ctx, []int64{template.ID}, repository.TemplatePatch{Name: &name}))
	fetched, err = repos.Templates.GetTemplate(ctx, template.ID)
	require.NoError(t, err)
	require.Equal(t, updatedMapping, fetched.ModelMapping)

	replacement := domain.ModelMapping{"batch": {MappedModel: "batch-target", Mode: domain.ModelMappingModeExplicit}}
	require.NoError(t, repos.Templates.UpdateTemplatesBatch(ctx, []int64{template.ID}, repository.TemplatePatch{ModelMapping: &replacement}))
	fetched, err = repos.Templates.GetTemplate(ctx, template.ID)
	require.NoError(t, err)
	require.Equal(t, replacement, fetched.ModelMapping)

	empty := domain.ModelMapping{}
	require.NoError(t, repos.Templates.UpdateTemplatesBatch(ctx, []int64{template.ID}, repository.TemplatePatch{ModelMapping: &empty}))
	fetched, err = repos.Templates.GetTemplate(ctx, template.ID)
	require.NoError(t, err)
	require.NotNil(t, fetched.ModelMapping)
	require.Empty(t, fetched.ModelMapping)
}
