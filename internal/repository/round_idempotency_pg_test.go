// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// G16a（spec-commit-state-and-round-billing §8.3）：轮结算幂等锚在**既有**唯一索引
// 上——同一 `(request_id, created_at)` 连续 InsertBatch 两次 ⇒ `DO NOTHING`，
// usage_logs 仍 1 行。必须是真库：`ON CONFLICT ... DO NOTHING` 的语义只在 PG 上
// 成立（fake store 不执行 SQL，分区表裁剪/冲突目标也不可见）。
//
// 为什么这条门禁重要：每轮结算复用 `usagelog_request_id_created_at`
// （`partition.go` 的 `CREATE UNIQUE INDEX`）+ `usage_repo.go` 的
// `OnConflictColumns(FieldRequestID, FieldCreatedAt).DoNothing()`——本提交**没有**
// 新增任何表/列/索引/迁移，幂等性全部由这条既有索引承担。所以这里的断言直接钉住
// 「同轮重复结算 = 同一键 = 被吞」。

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent/usagelog"
	"github.com/is7qin/c3api/internal/repository"
)

// roundSeedTime 固定日内时刻（分区按日预建，与 cursorSeedTime 同款）。
var roundSeedTime = time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)

// roundLog 一轮的结算行（request_id 按 §6.2 派生；created_at = 轮起点）。
func roundLog(base string, round int, at time.Time) *domain.UsageLog {
	id := base
	if round >= 2 {
		id = fmt.Sprintf("%s#r%d", base, round)
	}
	return &domain.UsageLog{
		RequestID:    id,
		Model:        "gpt-4o",
		Format:       domain.FormatOpenAIResponsesWS,
		ErrorType:    domain.ErrNone,
		LatencyMS:    int64(round) * 10,
		InputTokens:  int64(round),
		OutputTokens: int64(round),
		TotalTokens:  int64(round) * 2,
		Cost:         int64(round) * 100,
		BillingTier:  "auto",
		CreatedAt:    at,
	}
}

func TestPGUsageRoundSettlementIdempotent(t *testing.T) {
	repos := newPGReposShared(t)
	require.NoError(t, repos.EnsureUsageLogPartitioned(context.Background(), roundSeedTime))
	ctx := context.Background()

	base := "round-idem-" + time.Now().Format("150405.000000")
	rows := []*domain.UsageLog{
		roundLog(base, 1, roundSeedTime),
		roundLog(base, 2, roundSeedTime.Add(time.Second)),
		roundLog(base, 3, roundSeedTime.Add(2*time.Second)),
	}

	// 第一次：三轮各一行。
	require.NoError(t, repos.Usages.InsertBatch(ctx, rows))
	require.Equal(t, 3, countRoundRows(t, repos, base))

	// 第二次（同 request_id + 同 created_at 重放，= 同轮重复结算）：全部 DO NOTHING。
	replay := []*domain.UsageLog{
		roundLog(base, 1, roundSeedTime),
		roundLog(base, 2, roundSeedTime.Add(time.Second)),
		roundLog(base, 3, roundSeedTime.Add(2*time.Second)),
	}
	require.NoError(t, repos.Usages.InsertBatch(ctx, replay))
	require.Equal(t, 3, countRoundRows(t, repos, base), "同键重放不得多行（DO NOTHING）")

	// 单轮再放一次（更贴近 G16a 的字面表述）：仍然 1 行。
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{roundLog(base, 2, roundSeedTime.Add(time.Second))}))
	require.Equal(t, 3, countRoundRows(t, repos, base))

	// 键的另一半也必须是轮起点：若 created_at 取落库时刻，同轮两次结算会得到两个
	// 不同的键 ⇒ 下面这次（同 request_id、不同 created_at）**会**多一行。
	// 这正是「created_at 必须取轮起点」的物理证据。
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{
		roundLog(base, 2, roundSeedTime.Add(3*time.Second)),
	}))
	require.Equal(t, 4, countRoundRows(t, repos, base),
		"不同 created_at 是不同键——轮起点口径是幂等成立的前提")
}

// countRoundRows 该连接（base 前缀）的 usage_logs 行数。
func countRoundRows(t *testing.T, repos *repository.Repository, base string) int {
	t.Helper()
	n, err := repos.Client.UsageLog.Query().
		Where(usagelog.RequestIDHasPrefix(base)).
		Count(context.Background())
	require.NoError(t, err)
	return n
}
