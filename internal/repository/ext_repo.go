// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"fmt"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/templateext"
)

// TemplateExtRepo 模板类型化扩展（template_ext 1:1 边缘表；通用框架——codex
// 专属账号 ext 见 ext_codex_repo.go）：credential_type ∈ {responses-special,
// codex-oauth, codex-pat} 的模板才有 ext 行。模板是共享配置面：只存类型声明 +
// strip_image_tools 公共开关——凭据列组（oauth/pat）一律在 account_ext。
// 类型一致性由 service 校验；FK 由 ent 边缘表保证（父行缺失 → 约束错误，
// 调用方先查父行）。
type TemplateExtRepo struct{ client *ent.Client }

// UpsertTemplateExt 幂等写入（Create/Update 合一）：template_id 冲突 → 全列
// 更新（含 NULL 清空——调用方显式传 nil 即清列），无冲突 → 插入。
// 注意 ent 语义：SetNillableX(nil) 是 no-op（不进 INSERT 列），NULL 清空必须在
// 冲突 UPDATE 里显式 ClearX（与 pricing repo 同模式）。
// 返回落库后的完整行（upsert 路径 RETURNING 仅 id，重查读全列）。
func (r *TemplateExtRepo) UpsertTemplateExt(ctx context.Context, e *domain.TemplateExt) (*domain.TemplateExt, error) {
	_, err := r.client.TemplateExt.Create().
		SetTemplateID(e.TemplateID).
		SetCredentialType(string(e.CredentialType)).
		SetNillableStripImageTools(e.StripImageTools).                 // nil = no-op（插入路径缺列 → NULL）
		SetUpstreamPaths(e.UpstreamPaths).                             // nil → NULL（JSON 列零值即 nil）
		SetCodexResponsesTransport(string(e.CodexResponsesTransport)). // 非可空列：零值 "" 即"未配置 = 默认 http"
		OnConflictColumns(templateext.FieldTemplateID).
		Update(func(u *ent.TemplateExtUpsert) {
			u.SetCredentialType(string(e.CredentialType))
			// 非可空列：恒 Set（不 Clear——空串本身就是默认面的完整表达）
			u.SetCodexResponsesTransport(string(e.CodexResponsesTransport))
			// 可空列：非 nil → Set 覆盖；nil → Clear（NULL 清空）
			if e.StripImageTools != nil {
				u.SetStripImageTools(*e.StripImageTools)
			} else {
				u.ClearStripImageTools()
			}
			if e.UpstreamPaths != nil {
				u.SetUpstreamPaths(e.UpstreamPaths)
			} else {
				u.ClearUpstreamPaths()
			}
		}).
		ID(ctx)
	if err != nil {
		return nil, err
	}
	return r.GetTemplateExt(ctx, e.TemplateID)
}

// GetTemplateExt 按模板 id 取扩展行；无行 → ErrNotFound（template_id 缺失——
// 父模板不存在时 FK 保证必无行，404 语义一致）。
func (r *TemplateExtRepo) GetTemplateExt(ctx context.Context, templateID int64) (*domain.TemplateExt, error) {
	row, err := r.client.TemplateExt.Query().Where(templateext.TemplateIDEQ(templateID)).Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, fmt.Errorf("%w: template_id=%d missing", ErrNotFound, templateID)
		}
		return nil, err
	}
	return toDomainTemplateExt(row), nil
}
