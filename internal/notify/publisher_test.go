// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package notify

import (
	"context"
	"errors"
	"testing"

	"github.com/pashagolub/pgxmock/v5"
	"github.com/stretchr/testify/require"
)

// TestPublisherPublish 发布调用 pg_notify，载荷 = Marshal(Change)（Publisher
// 自动填 src）。
func TestPublisherPublish(t *testing.T) {
	pool, err := pgxmock.NewPool()
	require.NoError(t, err)
	pool.ExpectExec("(?i)pg_notify").
		WithArgs(`{"v":1,"users":true,"src":"i-1"}`).
		WillReturnResult(pgxmock.NewResult("SELECT", 0))

	p := NewPublisher(pool, "i-1", nil)
	require.NoError(t, p.Publish(context.Background(), Change{V: 1, Users: true}))
	require.NoError(t, pool.ExpectationsWereMet())
}

// TestPublisherPublishGuard 超限载荷经守卫降级后再发送（groups 丢弃 +
// templates=true）。
func TestPublisherPublishGuard(t *testing.T) {
	pool, err := pgxmock.NewPool()
	require.NoError(t, err)
	groups := make([]int64, 1200)
	for i := range groups {
		groups[i] = int64(10000000 + i)
	}
	want := string(Marshal(Change{V: 1, Templates: true, Src: "i-1"})) // 降级后的期望载荷
	pool.ExpectExec("(?i)pg_notify").
		WithArgs(want).
		WillReturnResult(pgxmock.NewResult("SELECT", 0))

	p := NewPublisher(pool, "i-1", nil)
	require.NoError(t, p.Publish(context.Background(), Change{V: 1, Groups: groups}))
	require.NoError(t, pool.ExpectationsWereMet())
}

// TestPublisherError 发布失败 → 错误透传（NOTIFY 丢一条由 60s 兜底收敛）。
func TestPublisherError(t *testing.T) {
	pool, err := pgxmock.NewPool()
	require.NoError(t, err)
	pool.ExpectExec("(?i)pg_notify").
		WithArgs(pgxmock.AnyArg()).
		WillReturnError(errors.New("connection lost"))

	p := NewPublisher(pool, "", nil)
	require.Error(t, p.Publish(context.Background(), Change{V: 1, Users: true}))
}

// TestPublisherNilPool 未注入 pool → 显式错误（装配缺失不静默 no-op）。
func TestPublisherNilPool(t *testing.T) {
	p := NewPublisher(nil, "", nil)
	require.Error(t, p.Publish(context.Background(), Change{Users: true}))
}

// TestPublisherEmptySrc 无实例 ID（单实例部署）→ 载荷不带 src 字段。
func TestPublisherEmptySrc(t *testing.T) {
	pool, err := pgxmock.NewPool()
	require.NoError(t, err)
	pool.ExpectExec("(?i)pg_notify").
		WithArgs(`{"v":1,"users":true}`).
		WillReturnResult(pgxmock.NewResult("SELECT", 0))

	p := NewPublisher(pool, "", nil)
	require.NoError(t, p.Publish(context.Background(), Change{V: 1, Users: true}))
	require.NoError(t, pool.ExpectationsWereMet())
}
