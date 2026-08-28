// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package billing

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

type recordingBalanceWarningSink struct {
	mu     sync.Mutex
	events []domain.BalanceWarningEvent
	accept bool
}

func (s *recordingBalanceWarningSink) TrySubmit(event domain.BalanceWarningEvent) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.accept {
		return false
	}
	s.events = append(s.events, event)
	return true
}

func (s *recordingBalanceWarningSink) snapshot() []domain.BalanceWarningEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]domain.BalanceWarningEvent(nil), s.events...)
}

func TestSettleLaneParallelHandsOffWarningsOnlyFromSuccessfulCommits(t *testing.T) {
	store := newFakeLedgerStore()
	f := newFlusherWith(store, map[int64]int64{4: 1_000, 5: 1_000})
	sink := &recordingBalanceWarningSink{accept: true}
	f.SetBalanceWarningSink(sink)
	committed := domain.BalanceWarningEvent{EventType: domain.NotificationBalanceWarningCrossed, EntityType: domain.NotificationUser, EntityID: 4, BalanceMillis: 900, ThresholdMillis: 900, Email: "committed@example.com"}
	uncommitted := domain.BalanceWarningEvent{EventType: domain.NotificationBalanceWarningCrossed, EntityType: domain.NotificationUser, EntityID: 5, BalanceMillis: 900, ThresholdMillis: 900, Email: "rolled-back@example.com"}
	settle := func(_ context.Context, _, _, bucket int) (domain.SettlementSummary, error) {
		switch bucket {
		case 0:
			return domain.SettlementSummary{BatchRows: 1, Marked: 1, Balances: []domain.UserBalance{{UserID: 4, Balance: 900}}, BalanceWarnings: []domain.BalanceWarningEvent{committed}}, nil
		case 1:
			return domain.SettlementSummary{BatchRows: 1, Balances: []domain.UserBalance{{UserID: 5, Balance: 900}}, BalanceWarnings: []domain.BalanceWarningEvent{uncommitted}}, errors.New("injected rollback")
		case 2:
			return domain.SettlementSummary{BalanceWarnings: []domain.BalanceWarningEvent{uncommitted}}, errors.New("injected mark-guard failure")
		case 3:
			return domain.SettlementSummary{BalanceWarnings: []domain.BalanceWarningEvent{uncommitted}}, errors.New("injected commit failure")
		}
		return domain.SettlementSummary{}, nil
	}
	failed := [settleParallelism]bool{}

	marked := f.settleLaneParallel(context.Background(), "balance", settle, newBatchController(), &failed)

	require.Equal(t, int64(1), marked)
	require.Equal(t, []domain.BalanceWarningEvent{committed}, sink.snapshot())
	committedBalance, ok := f.bal.BalanceOf(4)
	require.True(t, ok)
	require.Equal(t, int64(900), committedBalance)
	uncommittedBalance, ok := f.bal.BalanceOf(5)
	require.True(t, ok)
	require.Equal(t, int64(1_000), uncommittedBalance)
	require.True(t, failed[1])
	require.True(t, failed[2])
	require.True(t, failed[3])
}

func TestApplySettlementIgnoresWarningSinkDrop(t *testing.T) {
	store := newFakeLedgerStore()
	f := newFlusherWith(store, map[int64]int64{1: 1_000})
	sink := &recordingBalanceWarningSink{}
	f.SetBalanceWarningSink(sink)
	event := domain.BalanceWarningEvent{EventType: domain.NotificationBalanceWarningCrossed, EntityType: domain.NotificationUser, EntityID: 1, BalanceMillis: 900, ThresholdMillis: 900, Email: "drop@example.com"}

	f.applySettlement(domain.SettlementSummary{Balances: []domain.UserBalance{{UserID: 1, Balance: 900}}, BalanceWarnings: []domain.BalanceWarningEvent{event}})

	balance, ok := f.bal.BalanceOf(1)
	require.True(t, ok)
	require.Equal(t, int64(900), balance)
	require.Empty(t, sink.snapshot())
}
