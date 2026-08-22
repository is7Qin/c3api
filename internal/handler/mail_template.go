// SPDX-License-Identifier: AGPL-3.0-or-later
package handler

import (
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
)

// GetMailTemplates 邮件模板列表（缺行自动回退内置默认）。
func (h *AdminAPI) GetMailTemplates(w http.ResponseWriter, r *http.Request) {
	rows, err := h.svc.ListMailTemplates(r.Context())
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]MailTemplate, 0, len(rows))
	for _, t := range rows {
		out = append(out, toAPIMailTemplate(t))
	}
	httpface.WriteJSON(w, http.StatusOK, out)
}

// PutMailTemplate 更新邮件模板（空 body_text 还原默认=删行）。
func (h *AdminAPI) PutMailTemplate(w http.ResponseWriter, r *http.Request, purpose string) {
	var in MailTemplateUpdate
	if err := decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	updated, err := h.svc.UpdateMailTemplate(r.Context(), purpose, in.Subject, in.BodyText)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIMailTemplate(updated))
}

func toAPIMailTemplate(t *domain.EmailTemplate) MailTemplate {
	return MailTemplate{
		Purpose:   MailTemplatePurpose(t.Purpose),
		Subject:   t.Subject,
		BodyText:  t.BodyText,
		UpdatedAt: &t.UpdatedAt,
	}
}
