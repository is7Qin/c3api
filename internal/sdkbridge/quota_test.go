// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	codexsdk "github.com/is7Qin/codex-sdk"
)

// TestExhaustedUntil 卫生公式的表驱动用例（纯函数：不建服务、不碰缓存）。
//
// 每个条件至少一条红用例——「接受」与「拒绝」两侧都要有，否则公式里某一行的
// 比较方向写反了也不会现红。
func TestExhaustedUntil(t *testing.T) {
	now := time.Date(2026, time.November, 16, 12, 0, 0, 0, time.UTC)
	const (
		minute = time.Minute
		hour   = time.Hour
		day    = 24 * time.Hour
	)

	// win 构造一扇窗口事实：limit 为窗口长度（0 = 上游未给），resetIn 为
	// ResetAt 相对 now 的剩余，resetAfter 为上游给的相对秒数。
	win := func(reached bool, limit, resetIn, resetAfter time.Duration) codexsdk.QuotaWindow {
		used := 100.0
		if !reached {
			used = 99.9
		}
		return codexsdk.QuotaWindow{
			Source:      "rate_limit/secondary",
			Limit:       limit,
			UsedPercent: used,
			Reached:     reached,
			ResetAt:     now.Add(resetIn),
			ResetAfter:  resetAfter,
		}
	}

	// rawWin 直接给绝对时刻的"打满"窗口：年尺度的 reset 无法用 `now.Add(d)` 表达
	// （时间差超出 time.Duration 的表达范围，`Sub` 会夹成上限），只能给出时刻。
	rawWin := func(resetAt time.Time, limit time.Duration) codexsdk.QuotaWindow {
		return codexsdk.QuotaWindow{
			Source:      "rate_limit/primary",
			Limit:       limit,
			UsedPercent: 100,
			Reached:     true,
			ResetAt:     resetAt,
		}
	}

	cases := []struct {
		name     string
		windows  []codexsdk.QuotaWindow
		wantOK   bool
		wantFrom time.Duration // 期望截止（相对 now）
	}{
		{
			// 主场景：7d 窗口打满、reset 在 6 天后 ⇒ 冷却到那个时刻。
			name:     "单窗口 7d 打满 reset 在 6 天后",
			windows:  []codexsdk.QuotaWindow{win(true, 7*day, 6*day, 6*day)},
			wantOK:   true,
			wantFrom: 6 * day,
		},
		{
			// 取最晚：短窗口（1h）先解封，长窗口（7d）更晚 ⇒ 用后者。
			name: "两个打满窗口取最晚",
			windows: []codexsdk.QuotaWindow{
				win(true, hour, 50*minute, 50*minute),
				win(true, 7*day, 6*day, 6*day),
			},
			wantOK:   true,
			wantFrom: 6 * day,
		},
		{
			// 互校容差 10s 的内侧：差 9s 仍接受（实测上游差 1s，抖动余量）。
			name:     "互校差 9s 接受",
			windows:  []codexsdk.QuotaWindow{win(true, 7*day, 6*day, 6*day+9*time.Second)},
			wantOK:   true,
			wantFrom: 6 * day,
		},
		{
			// 互校容差 10s 的外侧：差 11s 拒绝（相对值偏大方向）。
			name:    "互校差 11s 拒绝",
			windows: []codexsdk.QuotaWindow{win(true, 7*day, 6*day, 6*day+11*time.Second)},
			wantOK:  false,
		},
		{
			// 同一容差的另一侧（相对值偏小），确认用的是绝对值而不是单向差。
			name:    "互校差 11s 拒绝（相对值偏小）",
			windows: []codexsdk.QuotaWindow{win(true, 7*day, 6*day, 6*day-11*time.Second)},
			wantOK:  false,
		},
		{
			// Limit==0（上游未给窗口长度）：只能靠绝对上界兜底，31d 内接受。
			name:     "Limit 为 0 且 reset 在 30 天内接受",
			windows:  []codexsdk.QuotaWindow{win(true, 0, 30*day, 0)},
			wantOK:   true,
			wantFrom: 30 * day,
		},
		{
			// 上界内侧的边界值：恰好 31d 仍接受（条件是「超过」才剔除）。
			name:     "Limit 为 0 且 reset 恰好 31 天接受",
			windows:  []codexsdk.QuotaWindow{win(true, 0, 31*day, 0)},
			wantOK:   true,
			wantFrom: 31 * day,
		},
		{
			// 上界外侧：超过 31d 的 reset 不是「本窗口的重置点」，剔除。
			name:    "Limit 为 0 且 reset 超过 31 天拒绝",
			windows: []codexsdk.QuotaWindow{win(true, 0, 32*day, 0)},
			wantOK:  false,
		},
		{
			// 有窗口长度时的自校：剩余超过 1.25×Limit ⇒ 时刻与窗口不匹配。
			name:    "reset 超过 1.25 倍窗口长度拒绝",
			windows: []codexsdk.QuotaWindow{win(true, hour, 2*hour, 2*hour)},
			wantOK:  false,
		},
		{
			// 1.25× 恰好相等的边界：条件是「大于」才剔除，等于仍接受。
			name:     "reset 恰好 1.25 倍窗口长度接受",
			windows:  []codexsdk.QuotaWindow{win(true, 4*hour, 5*hour, 5*hour)},
			wantOK:   true,
			wantFrom: 5 * hour,
		},
		{
			// ResetAfter 未给（0）：跳过互校，其余校验照做 ⇒ 接受。
			name:     "ResetAfter 为 0 但绝对时刻合法接受",
			windows:  []codexsdk.QuotaWindow{win(true, 7*day, 6*day, 0)},
			wantOK:   true,
			wantFrom: 6 * day,
		},
		{
			// ResetAfter 为非正值（上游发了负数，SDK 归 0 之前也可能直接给负）：
			// 同样只跳过互校这一条，不因此整窗作废。
			name:     "ResetAfter 为负但绝对时刻合法接受",
			windows:  []codexsdk.QuotaWindow{win(true, 7*day, 6*day, -5*minute)},
			wantOK:   true,
			wantFrom: 6 * day,
		},
		{
			// 未打满（used_percent=99.9 ⇒ Reached=false）：不参与冷却判决。
			name:    "未打满不参与",
			windows: []codexsdk.QuotaWindow{win(false, 7*day, 6*day, 6*day)},
			wantOK:  false,
		},
		{
			// 已过期：窗口翻转过的那次观测不能把冷却延长到现在。
			name:    "reset_at 在过去不参与",
			windows: []codexsdk.QuotaWindow{win(true, 7*day, -hour, 0)},
			wantOK:  false,
		},
		{
			// 正好到点（ResetAt == now）：`After` 语义下同样剔除。
			name:    "reset_at 正好等于 now 不参与",
			windows: []codexsdk.QuotaWindow{win(true, 7*day, 0, 0)},
			wantOK:  false,
		},
		{
			// 绝对时刻缺失（零值）：相对秒数无法变成时刻。
			name: "reset_at 为零值不参与",
			windows: []codexsdk.QuotaWindow{{
				Source: "rate_limit/primary", Limit: 7 * day, UsedPercent: 100, Reached: true,
			}},
			wantOK: false,
		},
		{
			// 坏窗口不污染好窗口：一扇超界的窗口被剔除，另一扇照常给出截止。
			name: "超界窗口不污染合法窗口",
			windows: []codexsdk.QuotaWindow{
				win(true, 7*day, 6*day, 6*day),
				win(true, 0, 40*day, 0),
			},
			wantOK:   true,
			wantFrom: 6 * day,
		},
		{
			// 空集 ⇒ (zero, false)：调用方据此保持原冷却时长。
			name:    "空切片返回 false",
			windows: nil,
			wantOK:  false,
		},
		{
			// 回归源（N6）：一条被写坏的巨大 Limit 不得把绝对上界作废。
			// reset 在 9e18 秒（远超任何窗口），剩余被 `Sub` 夹成 time.Duration
			// 上限——旧实现只做「剩余 > 1.25×Limit」的乘积自校，而 Limit 取
			// math.MaxInt64 时剩余永远不"超过"它 ⇒ 照单全收，`until.UnixMilli`
			// 溢出成负数。
			name:    "reset 在 9e18 秒且 Limit 巨大仍然拒绝",
			windows: []codexsdk.QuotaWindow{rawWin(time.Unix(9e18, 0), time.Duration(math.MaxInt64))},
			wantOK:  false,
		},
		{
			// 既有行为的防退化：无人给窗口长度时，年尺度的 reset 一律剔除。
			name:    "reset 在 9999 年且 Limit 为 0 拒绝",
			windows: []codexsdk.QuotaWindow{rawWin(time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC), 0)},
			wantOK:  false,
		},
		{
			// 合法长窗口不得被绝对上界误杀：30d 窗口打满、reset 正好 30 天后。
			name:     "30d 窗口打满 reset 在 30 天后接受",
			windows:  []codexsdk.QuotaWindow{win(true, 30*day, 30*day, 0)},
			wantOK:   true,
			wantFrom: 30 * day,
		},
		{
			// 绝对上界内侧的最后 1s：31d - 1s 仍接受（条件是「超过」才剔除）。
			name:     "31d 窗口打满 reset 在 31 天差 1 秒接受",
			windows:  []codexsdk.QuotaWindow{win(true, 31*day, 31*day-time.Second, 0)},
			wantOK:   true,
			wantFrom: 31*day - time.Second,
		},
		{
			// 边界外：31d + 1ns 剔除（绝对上界先于窗口长度自校求值，两者都不放行）。
			name:    "31d 窗口打满 reset 超过绝对上界 1 纳秒拒绝",
			windows: []codexsdk.QuotaWindow{win(true, 31*day, 31*day+1, 0)},
			wantOK:  false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			until, ok := ExhaustedUntil(c.windows, now)
			require.Equal(t, c.wantOK, ok, "参与判定")
			if !c.wantOK {
				require.True(t, until.IsZero(), "拒绝时必须是零值时刻，got %v", until)
				return
			}
			require.Truef(t, until.Equal(now.Add(c.wantFrom)),
				"截止 = %v, 期望 %v", until, now.Add(c.wantFrom))
		})
	}
}

// usageWindowBody 构造上游 usage 响应：单窗口，用量百分比与 reset 时刻由参数给。
// resetAfterSeconds 是上游的相对秒数（绝对/相对互校的输入）。
func usageWindowBody(usedPercent float64, limitSeconds, resetAt, resetAfterSeconds int64) string {
	return fmt.Sprintf(`{
  "plan_type": "chatgpt-plus",
  "rate_limit": {
    "allowed": false,
    "limit_reached": true,
    "primary_window": {
      "used_percent": %v,
      "limit_window_seconds": %d,
      "reset_after_seconds": %d,
      "reset_at": %d
    }
  }
}`, usedPercent, limitSeconds, resetAfterSeconds, resetAt)
}

// TestCodexQuotaDeadlineExhausted 端到端：7d 窗口打满 ⇒ 截止就是该窗口的 reset_at，
// 且**只拉一次上游**（第二次命中 5min TTL——补拉上界的第一条：TTL 内 0 次上游）。
func TestCodexQuotaDeadlineExhausted(t *testing.T) {
	const (
		week = 7 * 24 * time.Hour
		// reset 在 6 天后（落在 1.25×7d 的卫生上界内），上游相对秒数与之一致。
		left = 6 * 24 * time.Hour
	)
	resetAt := time.Now().Add(left).Truncate(time.Second)
	srv, c := newUsageUpstream(t, codexUpstreamStep{status: 200, body: usageWindowBody(
		100, int64(week/time.Second), resetAt.Unix(), int64(left/time.Second),
	)})
	a := NewCodex(nil, testEgressPool(newOfficialRewriteTransport(t, srv.URL)), nil, RotationDeps{})
	cred := usageCred(1, srv.URL+"/codex/responses")
	ctx := context.Background()

	until, ok, err := a.QuotaDeadline(ctx, cred)
	require.NoError(t, err)
	require.True(t, ok, "7d 窗口打满 ⇒ 有可信截止")
	// SDK 把 Unix 秒还原成 UTC 时刻；Equal 按瞬时比较（不比较 Location/单调钟）。
	require.Truef(t, until.Equal(resetAt), "截止 = %v, 期望 %v", until, resetAt)
	require.Equal(t, 1, c.callsN())

	// 第二次命中快照缓存：零上游，且窗口事实**不被命中路径重写**（同一截止）。
	until2, ok2, err := a.QuotaDeadline(ctx, cred)
	require.NoError(t, err)
	require.True(t, ok2)
	require.Truef(t, until2.Equal(until), "缓存命中后截止不变: %v → %v", until, until2)
	require.Equal(t, 1, c.callsN(), "TTL 内第二次调用零上游")
}

// TestCodexQuotaDeadlineNotExhausted 未打满（4%）：不报错、无截止 ⇒ 调用方保持
// 原有冷却时长（非额度 429 不会因为"有窗口"就被延长）。
func TestCodexQuotaDeadlineNotExhausted(t *testing.T) {
	const week = 7 * 24 * time.Hour
	resetAt := time.Now().Add(6 * 24 * time.Hour).Truncate(time.Second)
	srv, c := newUsageUpstream(t, codexUpstreamStep{status: 200, body: usageWindowBody(
		4, int64(week/time.Second), resetAt.Unix(), int64(6*24*3600),
	)})
	a := NewCodex(nil, testEgressPool(newOfficialRewriteTransport(t, srv.URL)), nil, RotationDeps{})
	cred := usageCred(1, srv.URL+"/codex/responses")

	until, ok, err := a.QuotaDeadline(context.Background(), cred)
	require.NoError(t, err, "窗口没打满不是错误")
	require.False(t, ok, "未打满 ⇒ 无截止")
	require.True(t, until.IsZero(), "无截止时必须是零值时刻")
	require.Equal(t, 1, c.callsN())
}

// TestCodexQuotaDeadlineKeepsWindowsOnFailure 拉取失败不清已观测的窗口事实
// （失败只写冷却，不伪造"没有窗口"）：错误优先，但 entry 里的窗口不被抹掉。
func TestCodexQuotaDeadlineKeepsWindowsOnFailure(t *testing.T) {
	const week = 7 * 24 * time.Hour
	resetAt := time.Now().Add(6 * 24 * time.Hour).Truncate(time.Second)
	srv, c := newUsageUpstream(t,
		codexUpstreamStep{status: 200, body: usageWindowBody(
			100, int64(week/time.Second), resetAt.Unix(), int64(6*24*3600),
		)},
		codexUpstreamStep{status: 500, body: `{"error":{"message":"boom"}}`},
	)
	a := NewCodex(nil, testEgressPool(newOfficialRewriteTransport(t, srv.URL)), nil, RotationDeps{})
	cred := usageCred(1, srv.URL+"/codex/responses")
	ctx := context.Background()

	_, ok, err := a.QuotaDeadline(ctx, cred)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, 1, c.callsN())

	// 时间注入越 TTL（禁 sleep）→ 第二次真打上游，得到 500。
	e, err := a.entryFor(cred)
	require.NoError(t, err)
	e.usageMu.Lock()
	e.usageAt = time.Now().Add(-a.usageTTL - time.Second)
	e.usageMu.Unlock()

	until, ok, err := a.QuotaDeadline(ctx, cred)
	require.ErrorIs(t, err, ErrUpstream, "上游失败优先于窗口事实")
	require.False(t, ok)
	require.True(t, until.IsZero())
	require.Equal(t, 2, c.callsN())

	e.usageMu.Lock()
	kept := len(e.usageWindows)
	e.usageMu.Unlock()
	require.NotZero(t, kept, "失败不清已观测的窗口事实（清掉就等于伪造「没打满」）")
}

// TestCodexQuotaDeadlineUpstreamError 上游 500：错误原样透传，且失败冷却生效
// （60s 内第二次调用不再打上游——补拉失败不会退化成重试风暴）。
func TestCodexQuotaDeadlineUpstreamError(t *testing.T) {
	srv, c := newUsageUpstream(t, codexUpstreamStep{status: 500, body: `{"error":{"message":"boom"}}`})
	a := NewCodex(nil, testEgressPool(newOfficialRewriteTransport(t, srv.URL)), nil, RotationDeps{})
	cred := usageCred(1, srv.URL+"/codex/responses")
	ctx := context.Background()

	until, ok, err := a.QuotaDeadline(ctx, cred)
	require.ErrorIs(t, err, ErrUpstream, "500 → ErrUpstream 分类（错误面透传）")
	require.False(t, ok)
	require.True(t, until.IsZero())
	require.Equal(t, 1, c.callsN())

	until, ok, err = a.QuotaDeadline(ctx, cred)
	require.ErrorIs(t, err, ErrUpstream, "冷却内返回分类哨兵")
	require.False(t, ok)
	require.True(t, until.IsZero())
	require.Equal(t, 1, c.callsN(), "失败冷却内第二次调用零上游")
}
