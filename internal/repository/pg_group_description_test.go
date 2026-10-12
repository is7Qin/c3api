// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/testsupport/pgtest"
)

// TestPGGroupDescriptionMigration pins the no-destructive-migration contract on
// real PostgreSQL: a `groups` table that predates the `description` column, with
// an already-present row, gains the column on the next migrate with a NOT NULL
// DEFAULT '' (old rows backfilled to the empty string, never NULL). The old
// schema is reproduced by dropping the column from a migrated clone. Runs the
// real entry point repository.NewWithPG(ctx, drv, true, pool). Skips without
// TEST_DATABASE_URL (skip never counts as pass).
func TestPGGroupDescriptionMigration(t *testing.T) {
	ctx := context.Background()
	dsn := pgtest.Clone(t)
	conn := pgSharedConn(t) // raw session onto the same clone

	// Reproduce the pre-feature schema: groups without the description column.
	_, err := conn.Exec(ctx, `ALTER TABLE groups DROP COLUMN IF EXISTS description`)
	require.NoError(t, err)

	// An "old" row written by the pre-feature code — no description involved.
	var oldID int64
	err = conn.QueryRow(ctx,
		`INSERT INTO groups (name, visibility, price_multiplier, protocol_convert, updated_at, created_at)
		 VALUES ($1, 'public', 10000, $2, now(), now()) RETURNING id`,
		"legacy-group", "[]").Scan(&oldID)
	require.NoError(t, err)

	// Real migration entry point (ent auto-migrate over the existing table).
	pool := pgtest.OpenPool(t, dsn)
	db := stdlib.OpenDBFromPool(pool)
	t.Cleanup(func() { _ = db.Close() })
	_, err = repository.NewWithPG(ctx, entsql.OpenDB(dialect.Postgres, db), true, pool)
	require.NoError(t, err)

	// The pre-existing row was backfilled with a non-NULL empty string.
	var desc sql.NullString
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT description FROM groups WHERE id = $1`, oldID).Scan(&desc))
	require.True(t, desc.Valid, "description must be non-NULL after migrate")
	require.Equal(t, "", desc.String, "old row backfilled to empty string")

	// Column contract: NOT NULL with DEFAULT ''.
	requireGroupDescriptionColumnContract(t, ctx, conn)

	// Idempotent: a second migrate must not fail and must preserve the contract.
	_, err = repository.NewWithPG(ctx, entsql.OpenDB(dialect.Postgres, db), true, pool)
	require.NoError(t, err, "second migrate must be a no-op")
	requireGroupDescriptionColumnContract(t, ctx, conn)
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT description FROM groups WHERE id = $1`, oldID).Scan(&desc))
	require.True(t, desc.Valid)
	require.Equal(t, "", desc.String)
}

// requireGroupDescriptionColumnContract asserts the live groups.description
// column is NOT NULL with an empty-string default. PostgreSQL normalises the
// default expression to a cast form ("''::character varying"); the cast suffix
// is stripped so the assertion names the value the spec cares about: ''.
func requireGroupDescriptionColumnContract(t *testing.T, ctx context.Context, conn *pgx.Conn) {
	t.Helper()
	var isNullable, colDefault string
	require.NoError(t, conn.QueryRow(ctx,
		`SELECT is_nullable, coalesce(column_default, '')
		   FROM information_schema.columns
		  WHERE table_schema = 'public' AND table_name = 'groups' AND column_name = 'description'`).
		Scan(&isNullable, &colDefault))
	require.Equal(t, "NO", isNullable, "description must be NOT NULL")
	require.Equal(t, "''", strings.TrimSuffix(colDefault, "::character varying"),
		"description default must be the empty string (raw=%q)", colDefault)
}

// TestPGGroupDescriptionRoundTrip: CreateGroup/GetGroup/ListGroups/UpdateGroup
// carry the description through the real repository (empty string round-trips;
// UpdateGroup overwrites then clears).
func TestPGGroupDescriptionRoundTrip(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()

	// Create with a description.
	g, err := repos.Groups.CreateGroup(ctx, &domain.Group{
		Name: "desc-g", Visibility: domain.GroupVisibilityPublic,
		PriceMultiplier: 10000, Description: "hello-desc",
	})
	require.NoError(t, err)
	require.Equal(t, "hello-desc", g.Description)

	got, err := repos.Groups.GetGroup(ctx, g.ID)
	require.NoError(t, err)
	require.Equal(t, "hello-desc", got.Description)

	// Create without a description → empty string (never NULL).
	g2, err := repos.Groups.CreateGroup(ctx, &domain.Group{
		Name: "nodesc-g", Visibility: domain.GroupVisibilityPublic, PriceMultiplier: 10000,
	})
	require.NoError(t, err)
	require.Equal(t, "", g2.Description)
	got2, err := repos.Groups.GetGroup(ctx, g2.ID)
	require.NoError(t, err)
	require.Equal(t, "", got2.Description)

	// ListGroups surfaces the description.
	rows, _, err := repos.Groups.ListGroups(ctx, repository.ListQuery{})
	require.NoError(t, err)
	byID := map[int64]string{}
	for _, r := range rows {
		byID[r.ID] = r.Description
	}
	require.Equal(t, "hello-desc", byID[g.ID])
	require.Equal(t, "", byID[g2.ID])

	// UpdateGroup overwrites (PUT全量).
	g.Description = "updated-desc"
	updated, err := repos.Groups.UpdateGroup(ctx, g)
	require.NoError(t, err)
	require.Equal(t, "updated-desc", updated.Description)
	got, err = repos.Groups.GetGroup(ctx, g.ID)
	require.NoError(t, err)
	require.Equal(t, "updated-desc", got.Description)

	// UpdateGroup clears.
	g.Description = ""
	updated, err = repos.Groups.UpdateGroup(ctx, g)
	require.NoError(t, err)
	require.Equal(t, "", updated.Description)
	got, err = repos.Groups.GetGroup(ctx, g.ID)
	require.NoError(t, err)
	require.Equal(t, "", got.Description)
}
