// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"fmt"
	"time"

	"entgo.io/ent/dialect"
	"entgo.io/ent/dialect/sql/sqlgraph"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/template"
)

type TemplateRepo struct {
	client *ent.Client
	driver dialect.Driver
}

func (r *TemplateRepo) CreateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error) {
	if isCodexType(t.CredentialType) && t.BaseURL != "" {
		return nil, fmt.Errorf("%w: codex template base_url must be empty", ErrInvalidInput)
	}
	row, err := r.client.Template.Create().
		SetName(t.Name).SetBaseURL(t.BaseURL).
		// 全字段 Set（含 credential_type）：空串兜底在 service 层（M-1，防默认值被绕过）
		SetCredentialType(string(t.CredentialType)).
		SetSupportedFormats(formatsToStrings(t.SupportedFormats)).
		SetModels(t.Models).
		SetFormatModels(formatModelsToStrings(t.FormatModels)).
		SetModelMapping(t.ModelMapping).
		Save(ctx)
	if err != nil {
		if sqlgraph.IsUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: name=%q", ErrConflict, t.Name)
		}
		return nil, err
	}
	return toDomainTemplate(row), nil
}

func (r *TemplateRepo) GetTemplate(ctx context.Context, id int64) (*domain.Template, error) {
	row, err := r.client.Template.Get(ctx, id)
	if err != nil {
		return nil, errMissingID(err, id)
	}
	return toDomainTemplate(row), nil
}

// GetTemplatesByIDs 批量取模板（id IN 一次查询，替代逐 id GetTemplate 的 N+1；
// 软删除语义同 GetTemplate——不过滤 deleted_at）。缺失的 id 不报错（返回数量
// < 请求数），调用方按需对比；用于 UpdateTemplatesBatch 的类型-格式约束校验。
func (r *TemplateRepo) GetTemplatesByIDs(ctx context.Context, ids []int64) ([]*domain.Template, error) {
	rows, err := r.client.Template.Query().Where(template.IDIn(ids...)).All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.Template, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainTemplate(row))
	}
	return out, nil
}

func (r *TemplateRepo) ListTemplates(ctx context.Context, q ListQuery) ([]*domain.Template, int64, error) {
	// 软删除：列表默认过滤已删（count 同谓词——pred 复用）；GET 单个不过滤。
	pred := r.client.Template.Query().Where(template.DeletedAtIsNil())
	if q.Name != "" {
		pred = pred.Where(template.NameContainsFold(q.Name))
	}
	total, err := pred.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	order, err := q.sortOrder(templateSortFields)
	if err != nil {
		return nil, 0, err
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	rows, err := pred.Order(order).Offset(q.Offset).Limit(q.Limit).All(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*domain.Template, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainTemplate(row))
	}
	return out, int64(total), nil
}

func (r *TemplateRepo) UpdateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error) {
	var row *ent.Template
	err := withWriteTx(ctx, r.driver, func(client *ent.Client, driver dialect.Driver) error {
		if err := lockTemplateWrites(ctx, driver, []int64{t.ID}); err != nil {
			return err
		}
		if _, err := client.Template.Get(ctx, t.ID); err != nil {
			return errMissingID(err, t.ID)
		}
		if err := validateCodexTemplateUpdate(ctx, client, t); err != nil {
			return err
		}
		var saveErr error
		row, saveErr = client.Template.UpdateOneID(t.ID).
			SetName(t.Name).SetBaseURL(t.BaseURL).
			SetCredentialType(string(t.CredentialType)).
			SetSupportedFormats(formatsToStrings(t.SupportedFormats)).
			SetModels(t.Models).
			SetFormatModels(formatModelsToStrings(t.FormatModels)).
			SetModelMapping(t.ModelMapping).
			Save(ctx)
		return saveErr
	})
	if err != nil {
		if sqlgraph.IsUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: name=%q", ErrConflict, t.Name)
		}
		return nil, err
	}
	return toDomainTemplate(row), nil
}

// DeleteTemplate 软删除：deleted_at 置值（行保留留审计；列表/消费路径按
// deleted_at IS NULL 过滤，GET 单个仍可查已删项）。bulk Update（无 re-SELECT）
// 单语句；0 行命中 = 缺 id → ErrNotFound（与 errMissingID 同格式）。
func (r *TemplateRepo) DeleteTemplate(ctx context.Context, id int64) error {
	n, err := r.client.Template.Update().Where(template.IDEQ(id)).SetDeletedAt(time.Now()).Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: id=%d missing", ErrNotFound, id)
	}
	return nil
}

// formatsToStrings 领域格式数组 → ent 字符串数组。
func formatsToStrings(fs []domain.RequestFormat) []string {
	out := make([]string, 0, len(fs))
	for _, f := range fs {
		out = append(out, string(f))
	}
	return out
}

// formatModelsToStrings 领域 map（键为 RequestFormat）→ ent map（键为 string）。
func formatModelsToStrings(m map[domain.RequestFormat][]string) map[string][]string {
	out := make(map[string][]string, len(m))
	for k, v := range m {
		out[string(k)] = v
	}
	return out
}
