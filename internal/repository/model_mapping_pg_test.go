// SPDX-License-Identifier: AGPL-3.0-or-later
package repository_test

import (
	"context"
	"errors"
	"os"
	"testing"

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
	defer conn.Close(ctx)

	repos := newPGRepos(t)

	tpl, err := repos.Templates.CreateTemplate(ctx, &domain.Template{
		Name:             "c3api-model-mapping-null-tpl",
		BaseURL:          "https://api.example.com",
		CredentialType:   "api_key",
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
		ModelMapping:     domain.ModelMapping{"a": {MappedModel: "b", Mode: domain.ModelMappingModeExplicit}},
	})
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = conn.Exec(context.Background(), `DELETE FROM templates WHERE id = $1`, tpl.ID) })

	// SQL NULL should be rejected by NOT NULL constraint (SQLSTATE 23502)
	_, err = conn.Exec(ctx, `UPDATE templates SET model_mapping = NULL WHERE id = $1`, tpl.ID)
	require.Error(t, err, "SQL NULL should be rejected by NOT NULL schema")
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		require.Equal(t, "23502", pgErr.Code, "not-null violation SQLSTATE")
	} else {
		require.Contains(t, err.Error(), "23502")
	}

	// JSONB literal 'null' should be rejected on read (fail closed)
	_, err = conn.Exec(ctx, `UPDATE templates SET model_mapping = 'null'::jsonb WHERE id = $1`, tpl.ID)
	require.NoError(t, err, "setting JSONB literal null should succeed at SQL level")
	_, err = repos.Templates.GetTemplate(ctx, tpl.ID)
	require.Error(t, err, "reading JSONB literal null should fail closed via ModelMapping Unmarshal")
	require.Contains(t, err.Error(), "must not be null")

	// Also test INSERT with SQL NULL via raw (SQLSTATE 23502)
	_, err = conn.Exec(ctx, `INSERT INTO templates (name, base_url, credential_type, supported_formats, models, format_models, model_mapping, updated_at, created_at) VALUES ('c3api-model-mapping-null-insert', 'https://api.example.com', 'api_key', '[]'::jsonb, '[]'::jsonb, '{}'::jsonb, NULL, now(), now())`)
	require.Error(t, err, "INSERT SQL NULL should be rejected")
	if errors.As(err, &pgErr) {
		require.Equal(t, "23502", pgErr.Code)
	} else {
		require.Contains(t, err.Error(), "23502")
	}
}
