// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package supplier

// risk_test.go A22：risk_review 严格反序列化拒绝矩阵 + 建立/校验良构。

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func riskJSON(fields string) string {
	return "{" + fields + "}"
}

func TestRiskReviewStrictRejection(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	maxAge := 24 * time.Hour
	good := fmt.Sprintf(`{"operator_id":7,"decided_at":%q,"scope":"platform_credit_risk","evidence":{"reference":"bank-ref-1","summary":"ok"},"decision":"approve_credit_risk","expires_at":%q,"approved_revision":3}`,
		now.Format(time.RFC3339), now.Add(maxAge).Format(time.RFC3339))

	// 良构：解析 + 校验通过。
	rr, err := ParseRiskReview(good)
	require.NoError(t, err)
	require.NoError(t, ValidateRiskReview(rr, 7, 3, now, maxAge))

	cases := []struct {
		name string
		raw  string
	}{
		{"unknown key", riskJSON(`"operator_id":7,"decided_at":"` + now.Format(time.RFC3339) + `","scope":"platform_credit_risk","evidence":{"reference":"r","summary":"s"},"decision":"approve_credit_risk","expires_at":"` + now.Add(maxAge).Format(time.RFC3339) + `","approved_revision":3,"bogus":1`)},
		{"wrong type operator_id", riskJSON(`"operator_id":"seven","decided_at":"` + now.Format(time.RFC3339) + `","scope":"platform_credit_risk","evidence":{"reference":"r","summary":"s"},"decision":"approve_credit_risk","expires_at":"` + now.Add(maxAge).Format(time.RFC3339) + `","approved_revision":3`)},
		{"wrong type decided_at", riskJSON(`"operator_id":7,"decided_at":1234567890,"scope":"platform_credit_risk","evidence":{"reference":"r","summary":"s"},"decision":"approve_credit_risk","expires_at":"` + now.Add(maxAge).Format(time.RFC3339) + `","approved_revision":3`)},
		{"wrong type evidence", riskJSON(`"operator_id":7,"decided_at":"` + now.Format(time.RFC3339) + `","scope":"platform_credit_risk","evidence":"plain","decision":"approve_credit_risk","expires_at":"` + now.Add(maxAge).Format(time.RFC3339) + `","approved_revision":3`)},
		{"missing operator_id", riskJSON(`"decided_at":"` + now.Format(time.RFC3339) + `","scope":"platform_credit_risk","evidence":{"reference":"r","summary":"s"},"decision":"approve_credit_risk","expires_at":"` + now.Add(maxAge).Format(time.RFC3339) + `","approved_revision":3`)},
		{"missing evidence", riskJSON(`"operator_id":7,"decided_at":"` + now.Format(time.RFC3339) + `","scope":"platform_credit_risk","decision":"approve_credit_risk","expires_at":"` + now.Add(maxAge).Format(time.RFC3339) + `","approved_revision":3`)},
		{"missing expires_at", riskJSON(`"operator_id":7,"decided_at":"` + now.Format(time.RFC3339) + `","scope":"platform_credit_risk","evidence":{"reference":"r","summary":"s"},"decision":"approve_credit_risk","approved_revision":3`)},
		{"trailing data", good + ` {"x":1}`},
		{"not json", `not-json`},
	}
	for _, c := range cases {
		t.Run("parse/"+c.name, func(t *testing.T) {
			_, err := ParseRiskReview(c.raw)
			require.ErrorIs(t, err, ErrRiskReview, "must reject: %s", c.name)
		})
	}
}

func TestRiskReviewValidateRejections(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	maxAge := 24 * time.Hour
	base := RiskReview{
		OperatorID:       7,
		DecidedAt:        now,
		Scope:            RiskReviewScope,
		Evidence:         Evidence{Reference: "ref", Summary: "summary"},
		Decision:         RiskReviewDecision,
		ExpiresAt:        now.Add(maxAge),
		ApprovedRevision: 3,
	}
	// 良构基线。
	require.NoError(t, ValidateRiskReview(base, 7, 3, now, maxAge))

	t.Run("arbitrary operator_id", func(t *testing.T) {
		bad := base
		bad.OperatorID = 999
		require.ErrorIs(t, ValidateRiskReview(bad, 7, 3, now, maxAge), ErrRiskReview)
	})
	t.Run("wrong scope", func(t *testing.T) {
		bad := base
		bad.Scope = "something_else"
		require.ErrorIs(t, ValidateRiskReview(bad, 7, 3, now, maxAge), ErrRiskReview)
	})
	t.Run("wrong decision", func(t *testing.T) {
		bad := base
		bad.Decision = "deny"
		require.ErrorIs(t, ValidateRiskReview(bad, 7, 3, now, maxAge), ErrRiskReview)
	})
	t.Run("empty evidence reference", func(t *testing.T) {
		bad := base
		bad.Evidence = Evidence{Reference: "  ", Summary: "s"}
		require.ErrorIs(t, ValidateRiskReview(bad, 7, 3, now, maxAge), ErrRiskReview)
	})
	t.Run("empty evidence summary", func(t *testing.T) {
		bad := base
		bad.Evidence = Evidence{Reference: "r", Summary: ""}
		require.ErrorIs(t, ValidateRiskReview(bad, 7, 3, now, maxAge), ErrRiskReview)
	})
	t.Run("revision mismatch", func(t *testing.T) {
		bad := base
		bad.ApprovedRevision = 4
		require.ErrorIs(t, ValidateRiskReview(bad, 7, 3, now, maxAge), ErrRiskReview)
	})
	t.Run("future decided_at", func(t *testing.T) {
		bad := base
		bad.DecidedAt = now.Add(time.Hour)
		bad.ExpiresAt = bad.DecidedAt.Add(maxAge)
		require.ErrorIs(t, ValidateRiskReview(bad, 7, 3, now, maxAge), ErrRiskReview)
	})
	t.Run("expired decided_at", func(t *testing.T) {
		bad := base
		bad.DecidedAt = now.Add(-48 * time.Hour)
		bad.ExpiresAt = bad.DecidedAt.Add(maxAge)
		require.ErrorIs(t, ValidateRiskReview(bad, 7, 3, now, maxAge), ErrRiskReview)
	})
	t.Run("overlong expires_at", func(t *testing.T) {
		bad := base
		bad.ExpiresAt = base.DecidedAt.Add(maxAge + time.Minute)
		require.ErrorIs(t, ValidateRiskReview(bad, 7, 3, now, maxAge), ErrRiskReview)
	})
	t.Run("expires_at in the past", func(t *testing.T) {
		bad := base
		bad.DecidedAt = now.Add(-2 * time.Hour)
		bad.ExpiresAt = now.Add(-time.Minute)
		require.ErrorIs(t, ValidateRiskReview(bad, 7, 3, now, maxAge), ErrRiskReview)
	})
}

func TestRiskReviewBuildRoundTrip(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	maxAge := 24 * time.Hour

	// 空 evidence reference ⇒ 拒绝。
	_, err := BuildRiskReview(7, "   ", 3, now, maxAge)
	require.ErrorIs(t, err, ErrRiskReview)

	rr, err := BuildRiskReview(7, "bank-ref-1", 3, now, maxAge)
	require.NoError(t, err)
	raw, err := MarshalRiskReview(rr)
	require.NoError(t, err)
	// round-trip：严格反序列化 + 校验（生产 claim 路径同款）。
	require.NoError(t, ParseAndValidateRiskReview(raw, 7, 3, now, maxAge))
	// 解析回来的字段一致。
	back, err := ParseRiskReview(raw)
	require.NoError(t, err)
	require.Equal(t, rr, back)

	// 篡改为未来 decided_at ⇒ round-trip 校验拒绝。
	future, err := MarshalRiskReview(func() RiskReview {
		x := rr
		x.DecidedAt = now.Add(time.Hour)
		x.ExpiresAt = now.Add(time.Hour + maxAge)
		return x
	}())
	require.NoError(t, err)
	require.ErrorIs(t, ParseAndValidateRiskReview(future, 7, 3, now, maxAge), ErrRiskReview)

	// errors.Is 归一：ErrRiskReview 应可从包装错误识别。
	_, err = ParseRiskReview("{")
	require.True(t, errors.Is(err, ErrRiskReview))
}
