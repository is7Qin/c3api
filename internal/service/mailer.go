// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math/big"
	"strconv"
	"strings"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// RenderTemplate 渲染模板：缺行走编译内置默认；仅替换 {{code}}/{{ttl_minutes}}/{{app_name}}。
func (s *Service) RenderTemplate(ctx context.Context, purpose domain.EmailTemplatePurpose, vars map[string]string) (string, string, error) {
	var tmpl domain.EmailTemplate
	row, err := s.store.GetEmailTemplate(ctx, string(purpose))
	if err != nil {
		return "", "", err
	}
	if row != nil {
		tmpl = *row
	} else {
		tmpl = domain.DefaultEmailTemplate(purpose)
	}
	repl := strings.NewReplacer(
		"{{code}}", vars["code"],
		"{{ttl_minutes}}", vars["ttl_minutes"],
		"{{app_name}}", vars["app_name"],
	)
	return repl.Replace(tmpl.Subject), repl.Replace(tmpl.BodyText), nil
}

// ListMailTemplates 管理面列表（DB 行与默认合成，缺行用默认回填）。
func (s *Service) ListMailTemplates(ctx context.Context) ([]*domain.EmailTemplate, error) {
	rows, err := s.store.ListEmailTemplates(ctx)
	if err != nil {
		return nil, err
	}
	byPurpose := make(map[string]*domain.EmailTemplate, len(rows))
	for _, r := range rows {
		byPurpose[string(r.Purpose)] = r
	}
	out := make([]*domain.EmailTemplate, 0, 2)
	for _, p := range []domain.EmailTemplatePurpose{domain.EmailTemplateRegisterCode, domain.EmailTemplateResetCode} {
		if v, ok := byPurpose[string(p)]; ok {
			out = append(out, v)
		} else {
			d := domain.DefaultEmailTemplate(p)
			// 合成默认项的 UpdatedAt 留零值（无 DB 行）。
			out = append(out, &d)
		}
	}
	return out, nil
}

// UpdateMailTemplate 管理面更新；空 bodyText 删除行=还原默认。
func (s *Service) UpdateMailTemplate(ctx context.Context, purpose, subject, bodyText string) (*domain.EmailTemplate, error) {
	p := domain.EmailTemplatePurpose(purpose)
	if !p.Valid() {
		return nil, ErrInvalidInput
	}
	if bodyText == "" {
		// 还原默认：删行，返回默认
		if err := s.store.DeleteEmailTemplate(ctx, purpose); err != nil {
			if !errors.Is(err, repository.ErrNotFound) {
				return nil, err
			}
		}
		d := domain.DefaultEmailTemplate(p)
		return &d, nil
	}
	if subject == "" {
		return nil, ErrInvalidInput
	}
	return s.store.UpsertEmailTemplate(ctx, purpose, subject, bodyText)
}

func (s *Service) mailEnabled() bool {
	return s.settingValue("mail.enabled") == "true"
}

func (s *Service) mailConfig() (host string, port int, username, password, fromAddr, tlsPolicy string, ok bool) {
	host = s.settingValue("mail.smtp_host")
	fromAddr = s.settingValue("mail.from_address")
	if s.settingValue("mail.enabled") != "true" || host == "" || fromAddr == "" {
		return "", 0, "", "", "", "", false
	}
	port64, err := strconv.Atoi(s.settingValue("mail.smtp_port"))
	if err != nil || port64 < 1 || port64 > 65535 {
		return "", 0, "", "", "", "", false
	}
	return host, port64, s.settingValue("mail.smtp_username"), s.settingValue("mail.smtp_password"), fromAddr, s.settingValue("mail.tls"), true
}

// generateCode 生成 6 位数字验证码及其 sha256 hex。
func generateCode() (plain string, shaHex string, err error) {
	n, err := rand.Int(rand.Reader, big.NewInt(900000))
	if err != nil {
		return "", "", err
	}
	num := n.Int64() + 100000
	plain = strconv.FormatInt(num, 10)
	h := sha256.Sum256([]byte(plain))
	shaHex = hex.EncodeToString(h[:])
	return plain, shaHex, nil
}

func hashCode(plain string) string {
	h := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(h[:])
}
