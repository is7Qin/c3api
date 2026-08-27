// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

type settleWarningTestRow struct {
	uid, balance                          int64
	batch, debited, forced, marked, ghost int64
	threshold                             sql.NullInt64
	email                                 sql.NullString
}

type settleWarningTestScanner struct {
	rows  []settleWarningTestRow
	index int
}

func (s *settleWarningTestScanner) Next() bool {
	s.index++
	return s.index < len(s.rows)
}

func (s *settleWarningTestScanner) Scan(dest ...any) error {
	row := s.rows[s.index]
	*dest[0].(*int64) = row.uid
	*dest[1].(*int64) = row.balance
	*dest[2].(*int64) = row.batch
	*dest[3].(*int64) = row.debited
	*dest[4].(*int64) = row.forced
	*dest[5].(*int64) = row.marked
	*dest[6].(*int64) = row.ghost
	*dest[7].(*sql.NullInt64) = row.threshold
	*dest[8].(*sql.NullString) = row.email
	return nil
}

func (s *settleWarningTestScanner) Err() error { return nil }

func TestScanSettleResultCarriesTypedWarningsOnlyForCrossings(t *testing.T) {
	rows := &billingRows{
		rowScanner: &settleWarningTestScanner{index: -1, rows: []settleWarningTestRow{
			{uid: -1, balance: -1, batch: 3, debited: 2, forced: 1, marked: 3},
			{uid: 11, balance: 700},
			{uid: 12, balance: 500, threshold: sql.NullInt64{Int64: 500, Valid: true}, email: sql.NullString{String: "crossed@example.com", Valid: true}},
		}},
		closeFunc: func() {},
	}

	got, err := scanSettleResult(rows)

	require.NoError(t, err)
	require.Equal(t, int64(3), got.BatchRows)
	require.Equal(t, []domain.UserBalance{{UserID: 11, Balance: 700}, {UserID: 12, Balance: 500}}, got.Balances)
	require.Equal(t, []domain.BalanceWarningEvent{{
		EventType:       domain.NotificationBalanceWarningCrossed,
		EntityType:      domain.NotificationUser,
		EntityID:        12,
		BalanceMillis:   500,
		ThresholdMillis: 500,
		Email:           "crossed@example.com",
	}}, got.BalanceWarnings)
}
