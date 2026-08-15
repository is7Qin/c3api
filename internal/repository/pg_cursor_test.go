// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

// keyset 游标分页（spec 2026-08-11-logs-cursor：/usage_logs + /err_logs 四端点
// 去 Total 改游标）真实 PG 测试：跨分区游标翻页无重复/无遗漏、过滤+时间窗+
// 游标组合、limit+1 探测（末页无探测行）、cursor 缺失/≤0 = 首页、EXPLAIN
// 分区裁剪 + 主键范围（查询成本有界验收）。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// pageWalk 按 keyset 语义翻完整个结果集：cursor = 本页最后一条 id（探测行
// 存在时——rows 恰为 limit+1 说明还有下一页）；返回全部行（不含探测行）。
// 与 handler next_cursor 组装逻辑同构。
func pageWalk(t *testing.T, repos *repository.Repository, q repository.UsageQuery) []*domain.UsageLog {
	t.Helper()
	var got []*domain.UsageLog
	for {
		rows, err := repos.Usages.QueryUsages(context.Background(), q)
		require.NoError(t, err)
		if len(rows) == 0 {
			break
		}
		hasMore := len(rows) > q.Limit
		if hasMore {
			rows = rows[:q.Limit]
		}
		got = append(got, rows...)
		if !hasMore {
			break
		}
		q.Cursor = rows[len(rows)-1].ID
	}
	return got
}

// TestPGCursorCrossPartition 跨分区游标翻页（相邻页边界）：12 行跨两个日分区
// （7 今日 + 5 明日），limit=3 翻页——id 全局单调，跨分区自然有序；
// 结果无重复、无遗漏、严格降序（id 序 ≈ 时间序）。
func TestPGCursorCrossPartition(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	tomorrow := today.AddDate(0, 0, 1)

	for i := 0; i < 7; i++ {
		require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{usageLogFor(fmt.Sprintf("cp-%d", i), today.Add(time.Duration(i)*time.Minute))}))
	}
	for i := 0; i < 5; i++ {
		require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{usageLogFor(fmt.Sprintf("cp-t-%d", i), tomorrow.Add(time.Duration(i)*time.Minute))}))
	}

	from := today.Add(-time.Hour)
	to := tomorrow.Add(24 * time.Hour)
	got := pageWalk(t, repos, repository.UsageQuery{From: &from, To: &to, Limit: 3})
	require.Len(t, got, 12, "12 行全部翻出（无遗漏）")
	seen := map[int64]bool{}
	for i, r := range got {
		require.False(t, seen[r.ID], "无重复 id=%d", r.ID)
		seen[r.ID] = true
		if i > 0 {
			require.Less(t, got[i].ID, got[i-1].ID, "严格 id 降序（跨分区有序）")
		}
	}
	// 与全量集合一致：翻页覆盖全部 12 个 id
	all, err := repos.Usages.QueryUsages(ctx, repository.UsageQuery{From: &from, To: &to, Limit: 100})
	require.NoError(t, err)
	require.Len(t, all, 12)
	for _, r := range all {
		require.True(t, seen[r.ID], "翻页结果覆盖全量 id=%d", r.ID)
	}
}

// TestPGCursorFilterWindowCombined 过滤（model + user）+ 时间窗 + 游标组合：
// 三条件交集上翻页，窗外行/他过滤值行不得混入。
func TestPGCursorFilterWindowCombined(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	now := time.Now().UTC()
	base := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)

	for i := 0; i < 6; i++ {
		l := usageLogFor(fmt.Sprintf("f-%d", i), base.Add(time.Duration(i)*time.Minute))
		if i%2 == 0 {
			l.Model = "gpt-4o"
			l.UserID = 42
		} else {
			l.Model = "o3"
			l.UserID = 7
		}
		require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{l}))
	}

	from := base.Add(-time.Hour)
	to := base.Add(2*time.Minute + 30*time.Second) // 只覆盖 f-0/f-1/f-2（f-3 起窗外）
	got := pageWalk(t, repos, repository.UsageQuery{Model: "gpt-4o", UserID: 42, From: &from, To: &to, Limit: 1})
	require.Len(t, got, 2, "窗内 gpt-4o/用户 42 = f-0/f-2 两行")
	require.Equal(t, "f-2", got[0].RequestID, "首页首行 = 窗内最新")
	require.Equal(t, "f-0", got[1].RequestID)
	for _, r := range got {
		require.Equal(t, "gpt-4o", r.Model)
		require.Equal(t, int64(42), r.UserID)
	}
}

// TestPGFormatPredicate format 谓词（UsageQuery.Format / ErrLogQuery.Format）
// 真实 PG：种子不同 format 行 → 筛选返回正确子集；空串 = 不过滤；与 model
// 组合 AND；无效值自然查空（与 model 同语义，契约不校验值域）。
func TestPGFormatPredicate(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	now := time.Now().UTC()
	base := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)

	// usage_logs：2 chat + 2 responses + 1 images（images 为 usage 侧独有枚举值）
	chat1 := usageLogFor("fmt-u-chat-1", base)
	chat2 := usageLogFor("fmt-u-chat-2", base.Add(time.Minute))
	resp1 := usageLogFor("fmt-u-resp-1", base.Add(2*time.Minute))
	resp1.Format = domain.FormatOpenAIResponses
	resp2 := usageLogFor("fmt-u-resp-2", base.Add(3*time.Minute))
	resp2.Format = domain.FormatOpenAIResponses
	img1 := usageLogFor("fmt-u-img-1", base.Add(4*time.Minute))
	img1.Format = domain.FormatOpenAIImages
	require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{chat1, chat2, resp1, resp2, img1}))

	// 空串 = 不过滤（全量）
	all, err := repos.Usages.QueryUsages(ctx, repository.UsageQuery{Limit: 100})
	require.NoError(t, err)
	require.Len(t, all, 5, "空串 format 不过滤")

	// format=openai-chat → 恰 2 行且全部命中
	chats, err := repos.Usages.QueryUsages(ctx, repository.UsageQuery{Format: "openai-chat", Limit: 100})
	require.NoError(t, err)
	require.Len(t, chats, 2, "usage format=openai-chat → 2 行")
	for _, r := range chats {
		require.Equal(t, domain.FormatOpenAIChat, r.Format)
	}

	// format + model 组合（AND 交集）：responses 行均为默认 model "m"
	resps, err := repos.Usages.QueryUsages(ctx, repository.UsageQuery{Format: "openai-responses", Model: "m", Limit: 100})
	require.NoError(t, err)
	require.Len(t, resps, 2, "format+model 组合 → 2 行")
	for _, r := range resps {
		require.Equal(t, domain.FormatOpenAIResponses, r.Format)
		require.Equal(t, "m", r.Model)
	}

	// 无效 format 值（契约不校验值域）→ 自然查空
	none, err := repos.Usages.QueryUsages(ctx, repository.UsageQuery{Format: "no-such-format", Limit: 100})
	require.NoError(t, err)
	require.Empty(t, none, "无效 format 值自然查空")

	// err_logs：2 chat + 1 responses（errlog 枚举无 openai-images——filter 恒空语义）
	eChat1 := errLogFor("fmt-e-chat-1", base)
	eChat2 := errLogFor("fmt-e-chat-2", base.Add(time.Minute))
	eResp1 := errLogFor("fmt-e-resp-1", base.Add(2*time.Minute))
	eResp1.Format = domain.FormatOpenAIResponses
	require.NoError(t, repos.InsertErrLogBatch(ctx, []*domain.UsageLog{eChat1, eChat2, eResp1}))

	// format=openai-responses → 恰 1 行
	eresps, err := repos.QueryErrLogs(ctx, repository.ErrLogQuery{Format: "openai-responses", Limit: 100})
	require.NoError(t, err)
	require.Len(t, eresps, 1, "err format=openai-responses → 1 行")
	require.Equal(t, "fmt-e-resp-1", eresps[0].RequestID)
	require.Equal(t, domain.FormatOpenAIResponses, eresps[0].Format)

	// errlog 枚举缺 openai-images：err_logs 侧 filter=openai-images 恒空（语义正确，
	// search 错误行落该值时 COPY 校验失败，本就不会出现——spec 注记）
	enone, err := repos.QueryErrLogs(ctx, repository.ErrLogQuery{Format: "openai-images", Limit: 100})
	require.NoError(t, err)
	require.Empty(t, enone, "err_logs format=openai-images 恒空（枚举缺该值）")

	// err_logs 空串 = 不过滤
	eall, err := repos.QueryErrLogs(ctx, repository.ErrLogQuery{Limit: 100})
	require.NoError(t, err)
	require.Len(t, eall, 3, "err_logs 空串 format 不过滤")
}

// TestPGCursorProbeLimitPlusOne limit+1 探测语义：非末页返回 limit+1 行
// （多取 1 探测行）；末页返回 ≤ limit 行（探测行缺省）；cursor 缺失/≤0 =
// 首页（无 id 谓词，与 0 等价）。
func TestPGCursorProbeLimitPlusOne(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	now := time.Now().UTC()
	base := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		require.NoError(t, repos.Usages.InsertBatch(ctx, []*domain.UsageLog{usageLogFor(fmt.Sprintf("pr-%d", i), base.Add(time.Duration(i)*time.Minute))}))
	}
	from := base.Add(-time.Hour)
	to := base.Add(24 * time.Hour)

	// 首页 limit=3：5 行 → 3 页行 + 1 探测行 = 4 行返回
	page1, err := repos.Usages.QueryUsages(ctx, repository.UsageQuery{From: &from, To: &to, Limit: 3})
	require.NoError(t, err)
	require.Len(t, page1, 4, "非末页 limit+1 探测")
	require.Equal(t, "pr-4", page1[0].RequestID, "首页首行 = 最新")

	// 以本页最后一条 id 为游标 → 第二页 [pr-1, pr-0] = 2 行 ≤ limit（末页无探测）
	page2, err := repos.Usages.QueryUsages(ctx, repository.UsageQuery{From: &from, To: &to, Cursor: page1[2].ID, Limit: 3})
	require.NoError(t, err)
	require.Len(t, page2, 2, "末页 ≤ limit 行（无探测行）")
	require.Equal(t, "pr-1", page2[0].RequestID)
	require.Equal(t, "pr-0", page2[1].RequestID)

	// cursor 缺失 vs ≤0 等价（首页）：相同首行
	pageNeg, err := repos.Usages.QueryUsages(ctx, repository.UsageQuery{From: &from, To: &to, Cursor: -9, Limit: 3})
	require.NoError(t, err)
	require.Len(t, pageNeg, 4)
	require.Equal(t, page1[0].ID, pageNeg[0].ID, "cursor ≤0 无 id 谓词 = 首页")
}

// errPageWalk err_logs keyset 翻页走查（与 pageWalk 同构——cursor = 本页最后一
// 条 id，rows 恰为 limit+1 说明还有下一页）；返回全部行（不含探测行）。
// 评审 L3：QueryErrLogs cursor 分支真实 PG 专项——此前仅 usage 侧有专项走查。
func errPageWalk(t *testing.T, repos *repository.Repository, q repository.ErrLogQuery) []*domain.UsageLog {
	t.Helper()
	var got []*domain.UsageLog
	for {
		rows, err := repos.QueryErrLogs(context.Background(), q)
		require.NoError(t, err)
		if len(rows) == 0 {
			break
		}
		hasMore := len(rows) > q.Limit
		if hasMore {
			rows = rows[:q.Limit]
		}
		got = append(got, rows...)
		if !hasMore {
			break
		}
		q.Cursor = rows[len(rows)-1].ID
	}
	return got
}

// TestPGCursorErrLogsCrossPartition err_logs 跨分区游标翻页专项（评审 L3）：
// 与 usage 侧同语义——跨两个日分区翻页无重复、无遗漏、严格 id 降序、与全量
// 集合一致；status_code 过滤与游标组合只翻出命中行。
func TestPGCursorErrLogsCrossPartition(t *testing.T) {
	repos := newPGRepos(t)
	ctx := context.Background()
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	tomorrow := today.AddDate(0, 0, 1)

	for i := 0; i < 7; i++ {
		l := errLogFor(fmt.Sprintf("ce-%d", i), today.Add(time.Duration(i)*time.Minute))
		if i%2 == 0 {
			l.StatusCode = 429
			l.ErrorType = domain.Err429
		} else {
			l.StatusCode = 402
			l.ErrorType = domain.ErrBilling
		}
		require.NoError(t, repos.InsertErrLogBatch(ctx, []*domain.UsageLog{l}))
	}
	for i := 0; i < 5; i++ {
		l := errLogFor(fmt.Sprintf("ce-t-%d", i), tomorrow.Add(time.Duration(i)*time.Minute))
		if i%2 == 0 {
			l.StatusCode = 429
			l.ErrorType = domain.Err429
		} else {
			l.StatusCode = 402
			l.ErrorType = domain.ErrBilling
		}
		require.NoError(t, repos.InsertErrLogBatch(ctx, []*domain.UsageLog{l}))
	}

	from := today.Add(-time.Hour)
	to := tomorrow.Add(24 * time.Hour)
	// 全量翻页：12 行无重复/无遗漏 + 严格降序
	got := errPageWalk(t, repos, repository.ErrLogQuery{From: &from, To: &to, Limit: 3})
	require.Len(t, got, 12, "err_logs 12 行全部翻出（无遗漏）")
	seen := map[int64]bool{}
	for i, r := range got {
		require.False(t, seen[r.ID], "无重复 id=%d", r.ID)
		seen[r.ID] = true
		if i > 0 {
			require.Less(t, got[i].ID, got[i-1].ID, "严格 id 降序（跨分区有序）")
		}
	}
	all, err := repos.QueryErrLogs(ctx, repository.ErrLogQuery{From: &from, To: &to, Limit: 100})
	require.NoError(t, err)
	require.Len(t, all, 12)
	for _, r := range all {
		require.True(t, seen[r.ID], "翻页结果覆盖全量 id=%d", r.ID)
	}

	// status_code=429 过滤 + 游标组合：4 今日 + 3 明日 = 7 行，仅命中行
	got429 := errPageWalk(t, repos, repository.ErrLogQuery{From: &from, To: &to, StatusCode: 429, Limit: 2})
	require.Len(t, got429, 7, "429 过滤翻页 = 7 行")
	for _, r := range got429 {
		require.Equal(t, 429, r.StatusCode, "翻页行 status_code 恒 429")
	}
}

// planNode EXPLAIN JSON 树节点（本测试只需 Node Type / 表名 / 索引 / 谓词）。
type planNode struct {
	NodeType      string      `json:"Node Type"`
	RelationName  string      `json:"Relation Name"`
	IndexName     string      `json:"Index Name"`
	ScanDirection string      `json:"Scan Direction"`
	IndexCond     string      `json:"Index Cond"`
	Filter        string      `json:"Filter"`
	Plans         []*planNode `json:"Plans"`
}

// walkPlan 递归收集节点（断言 Seq Scan 缺席 + 谓词存在性）。
func walkPlan(n *planNode, visit func(*planNode)) {
	if n == nil {
		return
	}
	visit(n)
	for _, c := range n.Plans {
		walkPlan(c, visit)
	}
}

// TestPGCursorExplainBoundedCost 查询成本有界（验收）：带 from/to + cursor 的
// 查询 EXPLAIN 无 Seq Scan——RANGE 分区裁剪（仅命中当日分区）+ id 范围谓词
// （Index Cond 或 Filter，随 planner 访问路径选择；主键 id 天然有序零排序）。
// usage_logs/err_logs 双表。单分区命中时 planner 折叠 Append 为直接 Index
// Scan——按"Relation Name = 命中日分区"断言裁剪，不绑定 Append 形态。
// 种子（评审 M2）：1 行表无统计信息时 planner 可能选 Seq Scan——计划形态断言
// 是 flake。每表 5000 行（当日分区内时间散布）+ ANALYZE 后计划锁定
// （LIMIT 21 + ORDER BY id DESC → 索引提前终止路径恒优于 Seq Scan+Sort）。
func TestPGCursorExplainBoundedCost(t *testing.T) {
	_ = newPGRepos(t) // 建 schema + 分区 bootstrap（种子经 pool 直插，无需 repo）
	ctx := context.Background()
	pool := pgTestPool(t)

	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct{ table, reqPrefix string }{
		{"usage_logs", "ex-u-"},
		{"err_logs", "ex-e-"},
	} {
		// 5000 行 × 500ms 间隔 = 41.7min，全部落在当日分区且 < 窗下界 1h 内
		pgExec(t, pool, fmt.Sprintf(
			`INSERT INTO %s (request_id, format, created_at)
			 SELECT '%s' || g, 'openai-chat', $1::timestamptz - g * interval '500 milliseconds'
			 FROM generate_series(1, 5000) g`, tc.table, tc.reqPrefix), today)
		pgExec(t, pool, `ANALYZE `+tc.table)
	}
	from := today.Add(-time.Hour).Format(time.RFC3339)
	to := today.Add(2 * time.Hour).Format(time.RFC3339)

	for _, tc := range []struct{ name, table string }{
		{"usage_logs", "usage_logs"},
		{"err_logs", "err_logs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var planJSON string
			err := pool.QueryRow(ctx, `
				EXPLAIN (FORMAT JSON) SELECT id, created_at FROM `+tc.table+`
				WHERE created_at >= $1 AND created_at <= $2 AND id < $3
				ORDER BY id DESC LIMIT 21`, from, to, 999999).Scan(&planJSON)
			require.NoError(t, err)
			t.Logf("%s plan: %s", tc.table, planJSON)
			var plan []struct {
				Plan *planNode `json:"Plan"`
			}
			require.NoError(t, json.Unmarshal([]byte(planJSON), &plan))
			require.Len(t, plan, 1)

			var seqScan, idPred bool
			relations := map[string]bool{}
			walkPlan(plan[0].Plan, func(n *planNode) {
				if n.NodeType == "Seq Scan" {
					seqScan = true
				}
				if containsIDRange(n.IndexCond) || containsIDRange(n.Filter) {
					idPred = true
				}
				if n.RelationName != "" {
					relations[n.RelationName] = true
				}
			})
			require.False(t, seqScan, "%s 无全分区 Seq Scan", tc.table)
			require.True(t, idPred, "%s 计划含 id < cursor 范围谓词", tc.table)
			// 分区裁剪：命中关系仅为当日分区（子分区）
			require.Len(t, relations, 1, "%s 时间窗裁剪到 1 个分区", tc.table)
			require.True(t, relations[tc.table+"_"+today.Format("20060102")],
				"%s 命中分区 = 当日分区 %s_%s", tc.table, tc.table, today.Format("20060102"))
		})
	}
}

// containsIDRange 谓词文本含 "id <" 即视为 id 范围（Index Cond 或 Filter）。
func containsIDRange(s string) bool {
	return strings.Contains(s, "id <")
}
