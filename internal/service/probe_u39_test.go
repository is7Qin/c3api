// SPDX-License-Identifier: AGPL-3.0-or-later
package service

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// TestProbeCredentialTypeMismatchIsAccountUnavailable 钉住 U39 J2 的错误面：
// 模板与 ext 行的 BPS 位不一致 ⇒ 该账号不可用（ErrInvalidInput / 400），
// **不许**退化成未分类 500，也不许继续拿 ext 列的类型去探测。
func TestProbeCredentialTypeMismatchIsAccountUnavailable(t *testing.T) {
	mismatch := fmt.Errorf("%w: account_id=31: template=bps ext=codex-pat",
		repository.ErrCredentialTypeMismatch)

	t.Run("codex", func(t *testing.T) {
		tpl := &domain.Template{
			ID:               6,
			CredentialType:   credential.TypeCodexPAT,
			SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIResponses},
			Models:           []string{"m"},
		}
		acc := &domain.Account{ID: 31, TemplateID: tpl.ID, Template: tpl, Enabled: true}
		prober := &fakeAccountProber{result: domain.ProbeResult{Success: true}}
		svc := newProbeSvc(&probeReadOnlyStore{acc: acc, credErr: mismatch}, prober, nil)

		res, err := svc.ProbeAccount(context.Background(), acc.ID,
			AccountProbeRequest{Model: "m", Format: domain.FormatOpenAIResponses})
		require.Nil(t, res)
		require.ErrorIs(t, err, ErrInvalidInput, "必须分流成'该账号不可用'，不是未分类错误")
		require.ErrorIs(t, err, repository.ErrCredentialTypeMismatch, "原始 sentinel 仍可 errors.Is")
		require.Equal(t, 0, prober.calls, "损坏账号不得发网络请求")
	})

	t.Run("bps", func(t *testing.T) {
		tpl := bpsProbeTemplate()
		store := bpsProbeStore(t, tpl)
		store.credErr = mismatch
		bps := &fakeBPSProbe{}
		svc := newBPSProbeSvc(store, bps, nil)

		res, err := svc.ProbeAccount(context.Background(), 11,
			AccountProbeRequest{Model: "gpt-5.6-sol", Format: domain.FormatOpenAIResponses})
		require.Nil(t, res)
		require.ErrorIs(t, err, ErrInvalidInput)
		require.ErrorIs(t, err, repository.ErrCredentialTypeMismatch)
		require.Equal(t, 0, bps.calls, "损坏账号不得发网络请求")
	})
}
