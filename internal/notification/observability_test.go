// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package notification

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

type notificationStatsView struct {
	CooldownSuppressed int64  `json:"cooldown_suppressed"`
	ClaimFailedTotal   int64  `json:"claim_failed_total"`
	ReleaseFailedTotal int64  `json:"release_failed_total"`
	FailedTotal        int64  `json:"failed_total"`
	LastError          string `json:"last_error"`
}

func readNotificationStats(t *testing.T, worker *Worker) notificationStatsView {
	t.Helper()
	payload, err := json.Marshal(worker.Stats())
	require.NoError(t, err)
	var stats notificationStatsView
	require.NoError(t, json.Unmarshal(payload, &stats))
	return stats
}

func TestWorkerStatsSanitizesMailFailureCategories(t *testing.T) {
	tests := []struct {
		name     string
		enqueue  WarningMailEnqueue
		category string
	}{
		{
			name: "delivery callback",
			enqueue: func(_ domain.BalanceWarningEvent, complete func(error)) error {
				complete(errors.New("private callback detail"))
				return nil
			},
			category: "mail_delivery_failed",
		},
		{
			name: "enqueue return",
			enqueue: func(domain.BalanceWarningEvent, func(error)) error {
				return errors.New("private enqueue detail")
			},
			category: "mail_enqueue_failed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client, _ := newTestRedis(t)
			failed := make(chan struct{}, 1)
			worker := newTestWorker(t, client, enabledTrue(), test.enqueue)
			worker.failedHook = func() { failed <- struct{}{} }
			require.NoError(t, worker.Start(context.Background()))
			t.Cleanup(func() { require.NoError(t, worker.Close(context.Background())) })

			require.True(t, worker.TrySubmit(warningEvent(1, 100_000, "privacy@example.com")))
			waitForHook(t, failed, time.Second)

			stats := readNotificationStats(t, worker)
			require.Equal(t, int64(1), stats.FailedTotal)
			require.Equal(t, test.category, stats.LastError)
			require.NotContains(t, stats.LastError, "private")
		})
	}
}

func TestWorkerRedisClaimErrorCountsFailureNotCooldownSuppression(t *testing.T) {
	client, redisServer := newTestRedis(t)
	processed := make(chan struct{}, 1)
	worker := newTestWorker(t, client, enabledTrue(), func(domain.BalanceWarningEvent, func(error)) error {
		return nil
	})
	worker.processedHook = func() { processed <- struct{}{} }
	require.NoError(t, worker.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, worker.Close(context.Background())) })
	redisServer.SetError("ERR private redis claim detail")

	require.True(t, worker.TrySubmit(warningEvent(2, 100_000, "claim@example.com")))
	waitForHook(t, processed, time.Second)
	redisServer.SetError("")

	stats := readNotificationStats(t, worker)
	require.Equal(t, int64(1), stats.FailedTotal)
	require.Equal(t, int64(1), stats.ClaimFailedTotal)
	require.Zero(t, stats.CooldownSuppressed)
	require.Equal(t, "cooldown_claim_failed", stats.LastError)
	require.NotContains(t, stats.LastError, "private")
}

func TestWorkerRedisDuplicateCountsCooldownSuppressionOnly(t *testing.T) {
	client, _ := newTestRedis(t)
	event := warningEvent(3, 100_000, "duplicate@example.com")
	_, claimed, err := NewCooldown(client).TryClaim(context.Background(), event)
	require.NoError(t, err)
	require.True(t, claimed)
	processed := make(chan struct{}, 1)
	worker := newTestWorker(t, client, enabledTrue(), func(domain.BalanceWarningEvent, func(error)) error {
		return nil
	})
	worker.processedHook = func() { processed <- struct{}{} }
	require.NoError(t, worker.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, worker.Close(context.Background())) })

	require.True(t, worker.TrySubmit(event))
	waitForHook(t, processed, time.Second)

	stats := readNotificationStats(t, worker)
	require.Zero(t, stats.FailedTotal)
	require.Zero(t, stats.ClaimFailedTotal)
	require.Equal(t, int64(1), stats.CooldownSuppressed)
	require.Empty(t, stats.LastError)
}

func TestWorkerRedisReleaseErrorStaysObservable(t *testing.T) {
	client, redisServer := newTestRedis(t)
	enqueueEntered := make(chan struct{})
	allowFailure := make(chan struct{})
	failed := make(chan struct{}, 1)
	worker := newTestWorker(t, client, enabledTrue(), func(_ domain.BalanceWarningEvent, complete func(error)) error {
		close(enqueueEntered)
		<-allowFailure
		complete(errors.New("private delivery detail"))
		return nil
	})
	worker.failedHook = func() { failed <- struct{}{} }
	require.NoError(t, worker.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, worker.Close(context.Background())) })
	event := warningEvent(4, 100_000, "release@example.com")

	require.True(t, worker.TrySubmit(event))
	waitForHook(t, enqueueEntered, time.Second)
	redisServer.SetError("ERR private redis release detail")
	close(allowFailure)
	waitForHook(t, failed, time.Second)
	redisServer.SetError("")

	stats := readNotificationStats(t, worker)
	require.Equal(t, int64(1), stats.FailedTotal)
	require.Equal(t, int64(1), stats.ReleaseFailedTotal)
	require.Equal(t, "cooldown_release_failed", stats.LastError)
	require.NotContains(t, stats.LastError, "private")
	require.True(t, redisServer.Exists(cooldownKey(event)))
}

func TestWorkerStatsPublishesRedisFailureCounters(t *testing.T) {
	client, _ := newTestRedis(t)
	worker := newTestWorker(t, client, enabledTrue(), func(domain.BalanceWarningEvent, func(error)) error {
		return nil
	})
	payload, err := json.Marshal(worker.Stats())
	require.NoError(t, err)
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(payload, &fields))

	require.Contains(t, fields, "claim_failed_total")
	require.Contains(t, fields, "release_failed_total")
}
