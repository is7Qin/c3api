// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// TestProbeLimiterExecutionAndWaiters：执行槽与等待位是两段容量；4 个执行
// 期间第 5 个只能排队，16 个等待位满后第 21 个才 busy；晋升后等待位立即释放。
func TestProbeLimiterExecutionAndWaiters(t *testing.T) {
	l := newProbeLimiter()
	held := make([]func(), 0, domain.ProbeMaxConcurrency)
	for range domain.ProbeMaxConcurrency {
		release, err := l.acquire(context.Background())
		require.NoError(t, err)
		held = append(held, release)
	}

	type acquired struct {
		release func()
		err     error
	}
	waiting := make(chan acquired, domain.ProbeMaxWaiters)
	for range domain.ProbeMaxWaiters {
		go func() {
			release, err := l.acquire(context.Background())
			waiting <- acquired{release: release, err: err}
		}()
	}
	require.Eventually(t, func() bool {
		return len(l.waiters) == domain.ProbeMaxWaiters
	}, time.Second, time.Millisecond, "16 个等待位应被占满")

	_, err := l.acquire(context.Background())
	require.ErrorIs(t, err, errProbeBusy, "第 21 个准入必须立即 busy")

	held[0]()
	first := <-waiting
	require.NoError(t, first.err)
	require.NotNil(t, first.release)
	require.Eventually(t, func() bool {
		return len(l.waiters) == domain.ProbeMaxWaiters-1
	}, time.Second, time.Millisecond, "晋升后必须释放等待位")
	first.release()

	for _, release := range held[1:] {
		release()
	}
	for range domain.ProbeMaxWaiters - 1 {
		got := <-waiting
		require.NoError(t, got.err)
		got.release()
	}
}

// TestProbeLimiterCanceledWaiterReleases：排队中取消必须返回 ctx 错误并释放
// 等待位，不能被队列满掩盖为 busy。
func TestProbeLimiterCanceledWaiterReleases(t *testing.T) {
	l := newProbeLimiter()
	var held []func()
	for range domain.ProbeMaxConcurrency {
		release, err := l.acquire(context.Background())
		require.NoError(t, err)
		held = append(held, release)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := l.acquire(ctx)
		errCh <- err
	}()
	require.Eventually(t, func() bool { return len(l.waiters) == 1 }, time.Second, time.Millisecond)
	cancel()
	require.ErrorIs(t, <-errCh, context.Canceled)
	require.Eventually(t, func() bool { return len(l.waiters) == 0 }, time.Second, time.Millisecond)
	for _, release := range held {
		release()
	}
}
