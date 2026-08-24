// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// imageBillLogFor 图片计费落账行（统一计费模型 spec 2026-08-13：图片 6 专列删
// 除——image token 并入 Input/OutputTokens、张数入 CallCount、每张价入
// PricePerCallMillis；text 分量恒 0；TotalTokens 含 image tokens 不含张数）。
// Cost 按 ImageCost 实参口径（口径不变）：100×800000/1e6 + 50×3000000/1e6 +
// 2×5400 = 11030。命名区别于 C 的 imageLogFor（pg_image_logcols_test.go——
// 本 helper 走游标消费扣减断言，C 走 InsertBatch/QueryUsages 列断言）。
func imageBillLogFor(userID int64, requestID string) *domain.UsageLog {
	i64 := func(v int64) *int64 { return &v }
	l := logFor(userID, requestID)
	l.Format = domain.FormatOpenAIImages
	l.InputTokens = 100
	l.OutputTokens = 50
	l.TotalTokens = 150
	l.CallCount = 2
	l.PricePerCallMillis = i64(5400)
	l.Cost = 11030
	return l
}

// TestPGUsageLogImageColumnsRoundTrip 图片计费落账迁移全链往返（spec 2026-08-13
// 同步点 2/3/4/5，F2 单写点形态）：usage flusher InsertBatch 写入 format=
// openai-images 行（billed=false 出生）→ 分区表落库（usageLogColumnDefs 单一
// 事实源含新列）→ 游标消费按 cost 扣减 → QueryUsages 完整读回。口径不变断言：
// cost/张数/每张价与迁移前一致——仅落账列迁移（call_count=张数、
// price_per_call_millis=每张价、image token 并入 in/out）。任一同步点漏改
// （COPY 列清单/行值/CreateBulk/列定义/查询映射）本测试即红。
func TestPGUsageLogImageColumnsRoundTrip(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	u := seedPGUser(t, repos, "img@example.com")
	require.NoError(t, repos.UpdateUserBalance(ctx, u.ID, 1000000))
	l := imageBillLogFor(u.ID, "img-req")
	seedUnbilled(t, repos, l)

	rows := fetchAllUnbilled(t, repos)
	require.Len(t, rows, 1)
	require.Equal(t, int64(11030), rows[0].Cost, "LedgerRow 投影 cost（ImageCost 口径不变）")

	bal, od, _, err := repos.DeductOnlyAndMark(ctx, u.ID, l.Cost, ledgerRowIDs(rows))
	require.NoError(t, err)
	require.False(t, od)
	require.Equal(t, int64(1000000-l.Cost), bal, "按 image 分量 cost 扣减（口径不变）")

	out, err := repos.QueryUsages(ctx, repository.UsageQuery{UserID: u.ID, From: ptrTime(time.Now().Add(-time.Hour)), To: ptrTime(time.Now().Add(time.Hour))})
	require.NoError(t, err)
	require.Len(t, out, 1)
	got := out[0]
	require.Equal(t, domain.FormatOpenAIImages, got.Format, "format=openai-images 落库")
	require.Equal(t, int64(2), got.CallCount, "张数 → call_count")
	require.Equal(t, int64(100), got.InputTokens, "image input tokens 并入 input_tokens")
	require.Equal(t, int64(50), got.OutputTokens, "image output tokens 并入 output_tokens")
	require.Equal(t, int64(150), got.TotalTokens, "TotalTokens 含 image tokens（口径不变）")
	require.NotNil(t, got.PricePerCallMillis)
	require.Equal(t, int64(5400), *got.PricePerCallMillis, "每张价 → price_per_call_millis（毫分/张）")
	require.Equal(t, int64(11030), got.Cost, "cost 与迁移前一致（ImageCost 口径不变）")
	require.Equal(t, l.RequestID, got.RequestID)
}

func ptrTime(t time.Time) *time.Time { return &t }
