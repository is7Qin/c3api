// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestRenderTemplate_BalanceThresholdPreservedForAuth(t *testing.T) {
	fs := newFakeStore()
	svc := newMailService(t, fs)
	ctx := context.Background()

	_, err := fs.UpsertEmailTemplate(ctx, string(domain.EmailTemplateRegisterCode), "subj {{balance}} {{threshold}} {{code}} {{app_name}}", "body {{balance}} {{threshold}} code={{code}} app={{app_name}}")
	require.NoError(t, err)
	subj, body, err := svc.RenderTemplate(ctx, domain.EmailTemplateRegisterCode, map[string]string{
		"code": "123", "app_name": "c3api",
	})
	require.NoError(t, err)
	require.Contains(t, subj, "{{balance}}", "register_code template must preserve literal {{balance}}")
	require.Contains(t, subj, "{{threshold}}", "register_code template must preserve literal {{threshold}}")
	require.Contains(t, body, "{{balance}}")
	require.Contains(t, body, "{{threshold}}")
	require.Contains(t, body, "123")
	require.Contains(t, subj, "c3api")
	require.NotContains(t, subj, "code") // sanity: code replaced, not leaked as placeholder

	// Same for reset_code
	_, err = fs.UpsertEmailTemplate(ctx, string(domain.EmailTemplateResetCode), "reset {{balance}}", "reset body {{threshold}} {{code}}")
	require.NoError(t, err)
	subj, body, err = svc.RenderTemplate(ctx, domain.EmailTemplateResetCode, map[string]string{
		"code": "999", "app_name": "c3api",
	})
	require.NoError(t, err)
	require.Contains(t, subj, "{{balance}}")
	require.Contains(t, body, "{{threshold}}")
	require.Contains(t, body, "999")

	// Balance warning must still replace
	_, err = fs.UpsertEmailTemplate(ctx, string(domain.EmailTemplateBalanceWarning), "warn {{balance}} {{threshold}} {{app_name}}", "bal {{balance}} thr {{threshold}}")
	require.NoError(t, err)
	subj, body, err = svc.RenderTemplate(ctx, domain.EmailTemplateBalanceWarning, map[string]string{
		"balance": "1.00", "threshold": "2.00", "app_name": "c3api",
	})
	require.NoError(t, err)
	require.Equal(t, "warn 1.00 2.00 c3api", subj)
	require.Equal(t, "bal 1.00 thr 2.00", body)
	require.NotContains(t, subj, "{{balance}}")
	require.NotContains(t, body, "{{threshold}}")
}
