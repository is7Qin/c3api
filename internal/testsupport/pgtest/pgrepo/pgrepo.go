// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package pgrepo wires the c3api data layer into pgtest. It exists as its own
// package (importing both pgtest and repository) so that pgtest itself stays
// free of the repository import — otherwise internal (package repository) tests
// importing pgtest would hit "import cycle not allowed in test". External test
// packages blank-import pgrepo for its init side effect, which registers the two
// repository-dependent steps pgtest needs:
//
//   - migrateTemplate: create the schema (ent migrate, minus the partitioned
//     tables) plus the day partitions and the price/codex seeds for a fresh
//     template database.
//   - ensureCloneDaily: top up a clone's day partitions after CREATE DATABASE
//     ... TEMPLATE, since the template only carries the partitions that existed
//     when it was built.
package pgrepo

import (
	"context"
	"database/sql"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	// Registers the "pgx" driver for the database/sql handle opened below.
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/testsupport/pgtest"
)

func init() {
	pgtest.RegisterDataLayerHooks(migrateTemplate, ensureCloneDaily)
}

// openRepo opens a data-layer Repository on a one-shot database/sql handle. The
// returned close function releases the handle (and therefore every connection to
// the database it pointed at); callers must invoke it, because pgtest's
// "template has no resident connection" invariant depends on it.
func openRepo(ctx context.Context, dsn string, migrate bool) (*repository.Repository, func(), error) {
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, nil, err
	}
	closeFn := func() { _ = sqlDB.Close() }
	repo, err := repository.NewWithPG(ctx, entsql.OpenDB(dialect.Postgres, sqlDB), migrate, nil)
	if err != nil {
		closeFn()
		return nil, nil, err
	}
	return repo, closeFn, nil
}

// migrateTemplate builds a fresh template database: ent migrate (the migration
// hook excludes the partitioned tables), the four bootstrap partition sets, the
// price-variant effect CHECK and the codex-search seed. It owns and closes its
// connection before returning.
func migrateTemplate(ctx context.Context, dsn string) error {
	repo, closeFn, err := openRepo(ctx, dsn, true)
	if err != nil {
		return err
	}
	defer closeFn()
	if err := ensureBase(ctx, repo); err != nil {
		return err
	}
	if err := repo.EnsurePriceVariantsEffectCheck(ctx); err != nil {
		return err
	}
	return repo.EnsureCodexSearchSeed(ctx)
}

// ensureCloneDaily tops up a clone's four day-partition sets for "now". A
// template built yesterday carries yesterday's partitions; a clone must have
// today's. Idempotent, so re-running is safe.
func ensureCloneDaily(ctx context.Context, dsn string) error {
	repo, closeFn, err := openRepo(ctx, dsn, false)
	if err != nil {
		return err
	}
	defer closeFn()
	return ensureBase(ctx, repo)
}

func ensureBase(ctx context.Context, repo *repository.Repository) error {
	now := time.Now()
	if err := repo.EnsureUsageLogPartitioned(ctx, now); err != nil {
		return err
	}
	if err := repo.EnsureErrLogPartitioned(ctx, now); err != nil {
		return err
	}
	if err := repo.EnsureUsageStatsPartitioned(ctx, now); err != nil {
		return err
	}
	return repo.EnsureUsageEntityStatsPartitioned(ctx, now)
}
