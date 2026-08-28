// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/account"
	"github.com/is7qin/c3api/internal/ent/template"
)

const templateWriteLockNamespace int64 = 0x4333415049540000

type lockedAccount struct {
	id         int64
	templateID int64
	baseURL    *string
}

func withWriteTx(ctx context.Context, driver dialect.Driver, fn func(*ent.Client, dialect.Driver) error) error {
	tx, err := driver.Tx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback() // nolint:errcheck // Commit 后回滚仅返回事务已关闭。
	txDriver := &txDriver{tx: tx, drv: driver}
	if err := fn(ent.NewClient(ent.Driver(txDriver)), txDriver); err != nil {
		return err
	}
	return tx.Commit()
}

func lockTemplateWrites(ctx context.Context, driver dialect.Driver, ids []int64) error {
	for _, id := range sortedUniqueIDs(ids) {
		var result sql.Result
		if err := driver.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, []any{templateWriteLockNamespace ^ id}, &result); err != nil {
			return fmt.Errorf("lock template %d: %w", id, err)
		}
	}
	return nil
}

func lockAccountsForUpdate(ctx context.Context, driver dialect.Driver, ids []int64) ([]lockedAccount, error) {
	sortedIDs := sortedUniqueIDs(ids)
	args := make([]any, len(sortedIDs))
	placeholders := make([]string, len(sortedIDs))
	for index, id := range sortedIDs {
		args[index] = id
		placeholders[index] = fmt.Sprintf("$%d", index+1)
	}
	query := `SELECT id, template_id, base_url FROM accounts WHERE id IN (` + strings.Join(placeholders, ",") + `) ORDER BY id FOR UPDATE`
	rows := &entsql.Rows{}
	if err := driver.Query(ctx, query, args, rows); err != nil {
		return nil, err
	}
	defer rows.Close() // nolint:errcheck // Rows.Err reports iteration failures.
	locked := make([]lockedAccount, 0, len(sortedIDs))
	for rows.Next() {
		var row lockedAccount
		var baseURL sql.NullString
		if err := rows.Scan(&row.id, &row.templateID, &baseURL); err != nil {
			return nil, err
		}
		if baseURL.Valid {
			row.baseURL = &baseURL.String
		}
		locked = append(locked, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	existing := make([]int64, 0, len(locked))
	for _, row := range locked {
		existing = append(existing, row.id)
	}
	if err := diffMissing(existing, sortedIDs); err != nil {
		return nil, err
	}
	return locked, nil
}

func loadTemplates(ctx context.Context, client *ent.Client, ids []int64) (map[int64]*domain.Template, error) {
	uniqueIDs := sortedUniqueIDs(ids)
	rows, err := client.Template.Query().Where(template.IDIn(uniqueIDs...)).All(ctx)
	if err != nil {
		return nil, err
	}
	gotIDs := make([]int64, 0, len(rows))
	result := make(map[int64]*domain.Template, len(rows))
	for _, row := range rows {
		gotIDs = append(gotIDs, row.ID)
		result[row.ID] = toDomainTemplate(row)
	}
	if err := diffMissing(gotIDs, uniqueIDs); err != nil {
		return nil, err
	}
	return result, nil
}

func validateCodexAccountBaseURL(tpl *domain.Template, baseURL *string) error {
	if isCodexType(tpl.CredentialType) && baseURL != nil && *baseURL != "" {
		return fmt.Errorf("%w: codex account base_url must be empty", ErrInvalidInput)
	}
	return nil
}

func validateCodexTemplateUpdate(ctx context.Context, client *ent.Client, tpl *domain.Template) error {
	if !isCodexType(tpl.CredentialType) {
		return nil
	}
	if tpl.BaseURL != "" {
		return fmt.Errorf("%w: codex template base_url must be empty", ErrInvalidInput)
	}
	hasOverride, err := client.Account.Query().Where(
		account.TemplateIDEQ(tpl.ID),
		account.BaseURLNotNil(),
		account.BaseURLNEQ(""),
	).Exist(ctx)
	if err != nil {
		return err
	}
	if hasOverride {
		return fmt.Errorf("%w: referenced codex account base_url must be empty", ErrInvalidInput)
	}
	return nil
}

func isCodexType(typ credential.Type) bool {
	return typ == credential.TypeCodexOAuth || typ == credential.TypeCodexPAT
}

func sortedUniqueIDs(ids []int64) []int64 {
	result := slices.Clone(ids)
	slices.Sort(result)
	return slices.Compact(result)
}
