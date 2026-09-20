// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
)

// —— 模板类型化扩展（template_ext 1:1；通用框架：表结构/CRUD 骨架/类型枚举
// 校验。W1 数据层 CRUD + 契约，消费接线 W3/W4/W6；codex 专属类型/列组见
// ext_codex.go，未来 claude oauth 等新类型 → 新增 ext_claude.go 同构） ——

// validateTemplateExt 校验模板 ext 行：credential_type ∈ {responses-special,
// codex-oauth, codex-pat}（api_key 主列类型无 ext 行；类型一致性——ext 行类型
// 必须 == 父模板类型——由 UpsertTemplateExt 校验）。模板是共享配置面：唯一
// 可空列 strip_image_tools（三类型公共能力开关，nil = 未配置 = 关闭）。
func validateTemplateExt(e *domain.TemplateExt) error {
	if e.TemplateID <= 0 {
		return ErrInvalidInput
	}
	if !e.CredentialType.ValidTemplateExt() {
		return ErrInvalidInput
	}
	return nil
}

// GetTemplateExt 模板 ext 行（编辑回显）。模板缺 id → 404。
func (s *Service) GetTemplateExt(ctx context.Context, templateID int64) (*domain.TemplateExt, error) {
	if _, err := s.store.GetTemplate(ctx, templateID); err != nil {
		return nil, mapRepoErr(err)
	}
	e, err := s.store.GetTemplateExt(ctx, templateID)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return e, nil
}

// UpsertTemplateExt 幂等写入模板 ext 行（Create/Update 合一；update 全列更新
// 含 NULL 清空）。模板缺 id → 404（FK 由仓库保证）。
// 类型一致性：ext 行 credential_type 必须与父模板的 credential_type 一致
// （api_key 模板无 ext 行；special/oauth/pat 模板只能挂同类型行）——不一致 → 400。
// ext 行是模板快照的静态原料之一（strip_image_tools 参与路由属性推导），
// 故写入后必须与模板其余写面同规失效：Templates() 全量重载调度快照 + 失效
// 客户端工厂，并广播 NOTIFY 使多实例收敛。
func (s *Service) UpsertTemplateExt(ctx context.Context, e *domain.TemplateExt) (*domain.TemplateExt, error) {
	tpl, err := s.store.GetTemplate(ctx, e.TemplateID)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if tpl.CredentialType != e.CredentialType {
		return nil, ErrInvalidInput // 父模板类型与 ext 行类型必须一致
	}
	if err := validateTemplateExt(e); err != nil {
		return nil, err
	}
	saved, err := s.store.UpsertTemplateExt(ctx, e)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	s.inv.Templates()
	s.publish(ctx, notify.Change{Templates: true})
	return saved, nil
}
