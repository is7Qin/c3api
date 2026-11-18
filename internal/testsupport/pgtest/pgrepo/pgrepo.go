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
//   - migrateTemplate: build the template database with the **versioned
//     migrations** (the same code path as `c3api -migrate up`), then the day
//     partitions via the single entry and the price/codex seeds.
//   - ensureCloneDaily: top up a clone's day partitions after CREATE DATABASE
//     ... TEMPLATE, since the template only carries the partitions that existed
//     when it was built.
package pgrepo

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	// Registers the "pgx" driver for the database/sql handle opened below.
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/is7qin/c3api/internal/db/migrate"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/testsupport/pgtest"
)

// partitionHorizon 与生产两个调用点同值（`-migrate up` 退出前、启动在写 worker
// 之前）：模板/克隆的分区窗口与生产一致，测试才不会在日界外"恰好"通过。
const partitionHorizon = 24 * time.Hour

func init() {
	pgtest.RegisterDataLayerHooks(migrateTemplate, ensureCloneDaily)
}

// openRepo opens a data-layer Repository on a one-shot database/sql handle. The
// returned close function releases the handle (and therefore every connection to
// the database it pointed at); callers must invoke it, because pgtest's
// "template has no resident connection" invariant depends on it.
func openRepo(ctx context.Context, dsn string) (*repository.Repository, func(), error) {
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, nil, err
	}
	closeFn := func() { _ = sqlDB.Close() }
	// 不迁移：模板已经跑过版本化迁移，克隆是它的逐字节拷贝。
	repo, err := repository.NewWithoutMigrate(ctx, entsql.OpenDB(dialect.Postgres, sqlDB), nil)
	if err != nil {
		closeFn()
		return nil, nil, err
	}
	return repo, closeFn, nil
}

// migrateTemplate builds a fresh template database the way production does:
//
//  1. `migrate.Up` — the versioned migrations are the single source of schema
//     truth (ent's Schema.Create is no longer on any production path, so it must
//     not be on any test path either);
//  2. the partition single entry, which refuses to create parent tables: a
//     template whose parents are missing means the migrations did not run;
//  3. the price-variant effect CHECK and the codex-search seed — still startup
//     bootstrap (spec §3 defers them to a later batch).
//
// It owns and closes its connection before returning.
func migrateTemplate(ctx context.Context, dsn string) error {
	sqlDB, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer func() { _ = sqlDB.Close() }()

	if _, err := migrate.Up(ctx, sqlDB, migrate.UpOptions{}); err != nil {
		return fmt.Errorf("versioned migrations: %w", err)
	}
	repo, err := repository.NewWithoutMigrate(ctx, entsql.OpenDB(dialect.Postgres, sqlDB), nil)
	if err != nil {
		return err
	}
	if err := ensureBase(ctx, repo); err != nil {
		return err
	}
	if err := repo.EnsurePriceVariantsEffectCheck(ctx); err != nil {
		return err
	}
	return repo.EnsureCodexSearchSeed(ctx)
}

// ensureCloneDaily tops up a clone's day partitions for "now". A template built
// yesterday carries yesterday's partitions; a clone must have today's. Idempotent,
// so re-running is safe. It goes through the same single entry as production, so
// a clone can never carry a different partition set than `-migrate up` builds.
func ensureCloneDaily(ctx context.Context, dsn string) error {
	repo, closeFn, err := openRepo(ctx, dsn)
	if err != nil {
		return err
	}
	defer closeFn()
	return ensureBase(ctx, repo)
}

// ensureBase 补齐全部七组（六张分区父表 + 静态快照表）。
func ensureBase(ctx context.Context, repo *repository.Repository) error {
	now := time.Now()
	return repo.EnsureAllTablePartitions(ctx, now, now.Add(partitionHorizon))
}
