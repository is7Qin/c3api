// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSupplierPayeeSnapshotNormalizeAndValidate §6.5 C4：结构化收款目标 trim + 非空。
func TestSupplierPayeeSnapshotNormalizeAndValidate(t *testing.T) {
	got, err := (SupplierPayeeSnapshot{PayeeName: "  张三 ", Account: " 6222 ", Unit: " 某行 "}).NormalizeAndValidate()
	require.NoError(t, err)
	require.Equal(t, "张三", got.PayeeName)
	require.Equal(t, "6222", got.Account)
	require.Equal(t, "某行", got.Unit)

	for _, bad := range []SupplierPayeeSnapshot{
		{PayeeName: "", Account: "a", Unit: "u"},
		{PayeeName: "p", Account: "  ", Unit: "u"},
		{PayeeName: "p", Account: "a", Unit: ""},
	} {
		_, err := bad.NormalizeAndValidate()
		require.ErrorIs(t, err, ErrSupplierInputInvalid)
	}
}

// TestSupplierRiskEvidenceNormalizeAndValidate §6.5 C1/I8：reference/summary 非空 +
// revision 绑定。
func TestSupplierRiskEvidenceNormalizeAndValidate(t *testing.T) {
	got, err := (SupplierRiskEvidence{Reference: " r ", Summary: " s ", ApprovedRevision: 7}).NormalizeAndValidate(7)
	require.NoError(t, err)
	require.Equal(t, "r", got.Reference)
	require.Equal(t, "s", got.Summary)

	_, err = (SupplierRiskEvidence{Reference: "r", Summary: "s", ApprovedRevision: 7}).NormalizeAndValidate(8)
	require.ErrorIs(t, err, ErrSupplierInputInvalid)

	_, err = (SupplierRiskEvidence{Reference: " ", Summary: "s", ApprovedRevision: 7}).NormalizeAndValidate(7)
	require.ErrorIs(t, err, ErrSupplierInputInvalid)
}

// TestSupplierClaimCommandNormalizeAndValidate 认领命令：金额 + 收款目标 + 风险证据。
func TestSupplierClaimCommandNormalizeAndValidate(t *testing.T) {
	valid := SupplierClaimCommand{
		AmountMillis: 100,
		Revision:     3,
		Payee:        SupplierPayeeSnapshot{PayeeName: "p", Account: "a", Unit: "u"},
		Risk:         SupplierRiskEvidence{Reference: "r", Summary: "s", ApprovedRevision: 3},
	}
	got, err := valid.NormalizeAndValidate()
	require.NoError(t, err)
	require.Equal(t, int64(100), got.AmountMillis)

	bad := valid
	bad.AmountMillis = 0
	_, err = bad.NormalizeAndValidate()
	require.ErrorIs(t, err, ErrSupplierInputInvalid)
}

// TestSupplierPayoutFailureConfirmationValidate §6.5 C4：任一缺失/false ⇒ 失败闭合。
func TestSupplierPayoutFailureConfirmationValidate(t *testing.T) {
	got, err := (SupplierPayoutFailureConfirmation{
		Reason: " r ", Evidence: " e ", ConfirmedNotPaid: true, OldExecutionStopped: true,
	}).Validate()
	require.NoError(t, err)
	require.Equal(t, "r", got.Reason)
	require.Equal(t, "e", got.Evidence)

	_, err = (SupplierPayoutFailureConfirmation{
		Reason: "r", Evidence: "e", ConfirmedNotPaid: false, OldExecutionStopped: true,
	}).Validate()
	require.True(t, errors.Is(err, ErrSupplierInputInvalid))
}

// TestNormalizeExternalRef §6.3：付款凭证 trim 后非空。
func TestNormalizeExternalRef(t *testing.T) {
	got, err := NormalizeExternalRef("  TX-1  ")
	require.NoError(t, err)
	require.Equal(t, "TX-1", got)

	_, err = NormalizeExternalRef("   ")
	require.ErrorIs(t, err, ErrSupplierInputInvalid)
}
