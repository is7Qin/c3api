// SPDX-License-Identifier: AGPL-3.0-or-later
package repository

import (
	"context"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/emailcode"
)

// EmailCodeRepo 验证码持久化。
type EmailCodeRepo struct{ client *ent.Client }

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
	row, err := r.client.EmailCode.Query().Where(emailcode.EmailEQ(email), emailcode.PurposeEQ(emailcode.Purpose(purpose))).Only(ctx)
	if err != nil {
		return 0, err
	}
	updated, err := r.client.EmailCode.UpdateOneID(row.ID).SetAttempts(row.Attempts + 1).Save(ctx)
	if err != nil {
		return 0, err
	}
	return updated.Attempts, nil
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
