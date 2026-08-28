// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/pkg/logx"
)

func (s *Service) CreateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error) {
	if err := validateTemplate(t); err != nil {
		return nil, err
	}
	created, err := s.store.CreateTemplate(ctx, t)
	if err != nil {
		return nil, mapRepoErr(err) // name 唯一冲突 → ErrConflict（409）
	}
	s.inv.Templates()
	s.publish(ctx, notify.Change{Templates: true})
	if s.log != nil {
		s.log.Info("template created", logx.Int64("id", created.ID), logx.String("name", created.Name))
	}
	return created, nil
}

func (s *Service) GetTemplate(ctx context.Context, id int64) (*domain.Template, error) {
	t, err := s.store.GetTemplate(ctx, id)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	return t, nil
}

func (s *Service) ListTemplates(ctx context.Context, q repository.ListQuery) ([]*domain.Template, int64, error) {
	if err := validateListQuery(q, listSortFields["templates"]); err != nil {
		return nil, 0, err
	}
	return s.store.ListTemplates(ctx, q)
}

func (s *Service) UpdateTemplate(ctx context.Context, t *domain.Template) (*domain.Template, error) {
	if err := validateTemplate(t); err != nil {
		return nil, err
	}
	updated, err := s.store.UpdateTemplate(ctx, t)
	if err != nil {
		return nil, mapRepoErr(err) // 改名撞已有 name → ErrConflict（409）
	}
	s.inv.Templates()
	s.publish(ctx, notify.Change{Templates: true})
	return updated, nil
}

func (s *Service) DeleteTemplate(ctx context.Context, id int64) error {
	if err := mapRepoErr(s.store.DeleteTemplate(ctx, id)); err != nil {
		return err // 404 缺 id（与批量语义对齐）
	}
	s.inv.Templates()
	s.publish(ctx, notify.Change{Templates: true})
	return nil
}

func (s *Service) DeleteTemplatesBatch(ctx context.Context, ids []int64) error {
	if err := validateIDs(ids); err != nil {
		return err
	}
	if err := mapRepoErr(s.store.DeleteTemplatesBatch(ctx, ids)); err != nil {
		return err
	}
	s.inv.Templates()
	s.publish(ctx, notify.Change{Templates: true})
	return nil
}

func (s *Service) UpdateTemplatesBatch(ctx context.Context, ids []int64, p repository.TemplatePatch) error {
	if err := validateIDs(ids); err != nil {
		return err
	}
	if err := validateTemplatePatch(p); err != nil {
		return err
	}
	// W1 类型-格式约束：批量 patch 不含 credential_type，supported_formats 变更
	// 时按既有行类型校验（special/oauth/pat 模板只能改成 resp/resp-ws/
	// openai-images/openai-search——Task B/D 扩展：images 直连两类型同支持，
	// codex 走 SDK 生图；search 四类型分派全可达）。一次 IN 批量取模板（替代
	// 逐 id GetTemplate 的 N+1）；缺失任一目标 id → 404（与逐 id 语义一致，
	// 先于任何更新）。
	if p.SupportedFormats != nil {
		tpls, err := s.store.GetTemplatesByIDs(ctx, ids)
		if err != nil {
			return mapRepoErr(err)
		}
		if len(tpls) != len(ids) {
			return ErrNotFound // 缺 id（validateIDs 已去重，数量可精确对比）
		}
		for _, t := range tpls {
			if t.CredentialType == credential.TypeAPIKey {
				continue
			}
			for _, f := range *p.SupportedFormats {
				if f != domain.FormatOpenAIResponses && f != domain.FormatOpenAIResponsesWS && f != domain.FormatOpenAIImages && f != domain.FormatOpenAISearch {
					return ErrInvalidInput
				}
			}
		}
	}
	if p.BaseURL != nil && *p.BaseURL != "" {
		tpls, err := s.store.GetTemplatesByIDs(ctx, ids)
		if err != nil {
			return mapRepoErr(err)
		}
		if len(tpls) != len(ids) {
			return ErrNotFound
		}
		for _, tpl := range tpls {
			if isCodexCredentialType(tpl.CredentialType) {
				return ErrInvalidInput
			}
		}
	}
	if err := mapRepoErr(s.store.UpdateTemplatesBatch(ctx, ids, p)); err != nil {
		return err
	}
	s.inv.Templates()
	s.publish(ctx, notify.Change{Templates: true})
	return nil
}
