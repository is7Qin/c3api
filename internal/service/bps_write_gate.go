// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"

	"github.com/is7qin/c3api/internal/domain"
)

// rejectBPSAccountWrite keeps management writes fail-closed until the BPS
// profile persistence and handler/API projection are wired. Reads remain
// allowed; only a write whose current/final template is BPS is rejected.
func (s *Service) rejectBPSAccountWrite(ctx context.Context, ids []int64, finalTemplateID *int64) error {
	for _, id := range ids {
		acct, err := s.store.GetAccountWithTemplate(ctx, id)
		if err != nil {
			return mapRepoErr(err)
		}
		if acct.Template != nil && acct.Template.CredentialType.IsBPS() {
			return ErrUnavailable
		}
		if finalTemplateID != nil && *finalTemplateID != acct.TemplateID {
			tpl, err := s.store.GetTemplate(ctx, *finalTemplateID)
			if err != nil {
				return mapRepoErr(err)
			}
			if tpl.CredentialType.IsBPS() {
				return ErrUnavailable
			}
		}
	}
	return nil
}

func rejectBPSTemplate(t *domain.Template) error {
	if t == nil {
		return ErrInvalidInput
	}
	if t.CredentialType.IsBPS() {
		return ErrUnavailable
	}
	return nil
}

func rejectBPSTemplateExt(e *domain.TemplateExt) error {
	if e == nil {
		return ErrInvalidInput
	}
	if e.CredentialType.IsBPS() {
		return ErrUnavailable
	}
	return nil
}

func rejectBPSAccountExt(e *domain.AccountExt) error {
	if e == nil {
		return ErrInvalidInput
	}
	if e.CredentialType.IsBPS() {
		return ErrUnavailable
	}
	return nil
}
