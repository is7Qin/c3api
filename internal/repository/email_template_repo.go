// SPDX-License-Identifier: AGPL-3.0-or-later
package repository

import (
	"context"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/emailtemplate"
)

// EmailTemplateRepo 邮件模板持久化。
type EmailTemplateRepo struct{ client *ent.Client }

func toDomainEmailTemplate(row *ent.EmailTemplate) *domain.EmailTemplate {
	return &domain.EmailTemplate{
		Purpose:   domain.EmailTemplatePurpose(row.Purpose),
		Subject:   row.Subject,
		BodyText:  row.BodyText,
		UpdatedAt: row.UpdatedAt,
	}
}

// GetEmailTemplate 单个模板；缺行返回 nil（调用方回退默认）。
func (r *EmailTemplateRepo) GetEmailTemplate(ctx context.Context, purpose string) (*domain.EmailTemplate, error) {
	row, err := r.client.EmailTemplate.Query().Where(emailtemplate.PurposeEQ(emailtemplate.Purpose(purpose))).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return toDomainEmailTemplate(row), nil
}

// ListEmailTemplates 全量 DB 模板（不含默认合成，合成由 service 层完成）。
func (r *EmailTemplateRepo) ListEmailTemplates(ctx context.Context) ([]*domain.EmailTemplate, error) {
	rows, err := r.client.EmailTemplate.Query().Order(ent.Asc(emailtemplate.FieldPurpose)).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.EmailTemplate, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainEmailTemplate(row))
	}
	return out, nil
}

// UpsertEmailTemplate 创建或更新模板。
func (r *EmailTemplateRepo) UpsertEmailTemplate(ctx context.Context, purpose, subject, bodyText string) (*domain.EmailTemplate, error) {
	_, err := r.client.EmailTemplate.Create().
		SetPurpose(emailtemplate.Purpose(purpose)).
		SetSubject(subject).
		SetBodyText(bodyText).
		OnConflictColumns("purpose").
		Update(func(u *ent.EmailTemplateUpsert) {
			u.SetSubject(subject).
				SetBodyText(bodyText).
				SetUpdatedAt(time.Now())
		}).ID(ctx)
	if err != nil {
		return nil, err
	}
	return r.GetEmailTemplate(ctx, purpose)
}

// DeleteEmailTemplate 删除模板行（还原默认）。
func (r *EmailTemplateRepo) DeleteEmailTemplate(ctx context.Context, purpose string) error {
	n, err := r.client.EmailTemplate.Delete().Where(emailtemplate.PurposeEQ(emailtemplate.Purpose(purpose))).Exec(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
