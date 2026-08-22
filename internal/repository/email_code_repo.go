// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
package repository

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/emailcode"
)

// EmailCodeRepo 验证码持久化。
type EmailCodeRepo struct {
	client *ent.Client
	driver dialect.Driver
}

func toDomainEmailCode(row *ent.EmailCode) *domain.EmailCode {
	return &domain.EmailCode{
		ID:         row.ID,
		Email:      row.Email,
		Purpose:    domain.EmailCodePurpose(row.Purpose),
		CodeSHA256: row.CodeSha256,
		ExpiresAt:  row.ExpiresAt,
		Attempts:   row.Attempts,
		CreatedAt:  row.CreatedAt,
		UpdatedAt:  row.UpdatedAt,
	}
}

// GetEmailCode 取单行；缺行返回 nil。
func (r *EmailCodeRepo) GetEmailCode(ctx context.Context, email, purpose string) (*domain.EmailCode, error) {
	row, err := r.client.EmailCode.Query().Where(emailcode.EmailEQ(email), emailcode.PurposeEQ(emailcode.Purpose(purpose))).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return toDomainEmailCode(row), nil
}

// UpsertEmailCode 覆盖旧码（天然失效旧码），重置 attempts/expires。
func (r *EmailCodeRepo) UpsertEmailCode(ctx context.Context, email, purpose, sha256 string, expiresAt time.Time) (*domain.EmailCode, error) {
	// 机会式清理替代常驻清扫工（评审 M2）。
	var res sql.Result
	if err := r.driver.Exec(ctx, `DELETE FROM "email_codes" WHERE "expires_at" < now()`, []any{}, &res); err != nil {
		return nil, err
	}
	_, err := r.client.EmailCode.Create().
		SetEmail(email).
		SetPurpose(emailcode.Purpose(purpose)).
		SetCodeSha256(sha256).
		SetExpiresAt(expiresAt).
		SetAttempts(0).
		OnConflictColumns("email", "purpose").
		Update(func(u *ent.EmailCodeUpsert) {
			u.SetCodeSha256(sha256).
				SetExpiresAt(expiresAt).
				SetAttempts(0).
				SetUpdatedAt(time.Now())
		}).ID(ctx)
	if err != nil {
		return nil, err
	}
	return r.GetEmailCode(ctx, email, purpose)
}

// IncrementAttempts 原子自增 attempts 并返回新值。
func (r *EmailCodeRepo) IncrementAttempts(ctx context.Context, email, purpose string) (int, error) {
	const q = `UPDATE "email_codes" SET attempts = attempts + 1 WHERE email = $1 AND purpose = $2 RETURNING "attempts"`
	rows := &entsql.Rows{}
	if err := r.driver.Query(ctx, q, []any{email, purpose}, rows); err != nil {
		return 0, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, err
		}
		return 0, fmt.Errorf("%w: email=%s purpose=%s", ErrNotFound, email, purpose)
	}
	var attempts int
	if err := rows.Scan(&attempts); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return attempts, nil
}

// DeleteEmailCode 删除验证码（消费成功后）。
func (r *EmailCodeRepo) DeleteEmailCode(ctx context.Context, email, purpose string) error {
	n, err := r.client.EmailCode.Delete().Where(emailcode.EmailEQ(email), emailcode.PurposeEQ(emailcode.Purpose(purpose))).Exec(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
