// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestModelMappingSQLNullRejected(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping real-PostgreSQL test")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	require.NoError(t, err)
	repos := newPGRepos(t)
	var templateID int64
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var deleteErr error
		if templateID != 0 {
			_, deleteErr = conn.Exec(cleanupCtx, `DELETE FROM templates WHERE id = $1`, templateID)
		}
		closeErr := conn.Close(cleanupCtx)
		require.NoError(t, deleteErr)
		require.NoError(t, closeErr)
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
