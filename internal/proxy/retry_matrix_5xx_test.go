// SPDX-License-Identifier: AGPL-3.0-or-later
package proxy

// retry_matrix_5xx_test.go 钉住「5xx 换号重试的可重试集合」规格
// （.omo/plans/spec-retry-5xx-2026-11-14.md）：
//   - 白名单 IsRetryable5xx 的唯一事实源（含 501/505 永久排除）；
//   - 规格 §测试矩阵 的 22 条 status × commit × frame × hard × terminal 组合；
//   - retryOutcomeForAttempt 的 terminal 计算（failover_plan 的新增分支）；
//   - hard continuation 的 5xx 永不换号（overlay 与矩阵两层）。

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/scheduler"
)

func TestIsRetryable5xxWhitelist(t *testing.T) {
	retryable := []AttemptStatus{500, 502, 503, 504, 520, 521, 522, 523, 524, 525, 526, 527, 529, 530}
	for _, s := range retryable {
		require.True(t, IsRetryable5xx(s), "%d 必须可换号", s)
	}
	// 501 未实现 / 505 协议版本：永久不兼容，换号也不会成功。
	// 506/507/508/510/511 及非 5xx：不做未经验证的换号。
	notRetryable := []AttemptStatus{0, 200, 400, 429, 499, 501, 505, 506, 507, 508, 510, 511, 528, 531, 599}
	for _, s := range notRetryable {
		require.False(t, IsRetryable5xx(s), "%d 不得进可重试集合", s)
	}
}

// TestRetryMatrixRetryable5xx 是规格 §测试矩阵 的 22 条穷举（编号即规格行号）。
// valid 断言该形状能否通过类型层契约（AttemptOutcome.Validate）：矩阵的
// 「no」有时由类型层先挡住（非法形状），有时由 5xx 分支/fallback 挡住——
// 两者都必须是 no，但分层要写清楚，避免把「形状非法」误读成「矩阵判过」。
func TestRetryMatrixRetryable5xx(t *testing.T) {
	type row struct {
		spec  string
		name  string
		apply func(o *AttemptOutcome)
		retry bool
		valid bool
	}
	rows := []row{
		{"#1", "500 upstream_responded 零字节", func(o *AttemptOutcome) { o.HTTPStatus = 500 }, true, true},
		{"#2", "502 upstream_responded 零字节", func(o *AttemptOutcome) { o.HTTPStatus = 502 }, true, true},
		{"#3", "503 upstream_responded 零字节", func(o *AttemptOutcome) { o.HTTPStatus = 503 }, true, true},
		{"#4", "504 upstream_responded 零字节", func(o *AttemptOutcome) { o.HTTPStatus = 504 }, true, true},
		{"#5", "520 upstream_responded 零字节", func(o *AttemptOutcome) { o.HTTPStatus = 520 }, true, true},
		{"#6", "527 upstream_responded 零字节", func(o *AttemptOutcome) { o.HTTPStatus = 527 }, true, true},
		{"#7", "529 upstream_responded 零字节", func(o *AttemptOutcome) { o.HTTPStatus = 529 }, true, true},
		{"#8", "530 upstream_responded 零字节", func(o *AttemptOutcome) { o.HTTPStatus = 530 }, true, true},
		{"#9", "501 未实现", func(o *AttemptOutcome) { o.HTTPStatus = 501 }, false, false},
		{"#10", "505 协议版本", func(o *AttemptOutcome) { o.HTTPStatus = 505 }, false, false},
		{"#11", "506 非白名单", func(o *AttemptOutcome) { o.HTTPStatus = 506 }, false, false},
		{"#12", "500 已写业务帧", func(o *AttemptOutcome) { o.HTTPStatus = 500; o.BusinessFrameSent = true }, false, false},
		{"#13", "500 sent_ambiguous", func(o *AttemptOutcome) { o.HTTPStatus = 500; o.Commit = CommitSentAmbiguous }, false, false},
		{"#14", "500 response_started", func(o *AttemptOutcome) { o.HTTPStatus = 500; o.Commit = CommitResponseStarted }, false, false},
		{"#15", "500 hard continuation", func(o *AttemptOutcome) { o.HTTPStatus = 500; o.HardContinuation = true }, false, true},
		{"#16", "500 terminal 短路", func(o *AttemptOutcome) { o.HTTPStatus = 500; o.Terminal = true }, false, true},
		{"#17", "500 not_sent", func(o *AttemptOutcome) { o.HTTPStatus = 500; o.Commit = CommitNotSent }, false, false},
		{"#18", "429 现状仍可换号", func(o *AttemptOutcome) { o.HTTPStatus = 429 }, true, true},
		{"#19", "429 hard 仍不换号", func(o *AttemptOutcome) {
			o.HTTPStatus = 429
			o.HardContinuation = true
			o.Terminal = true
		}, false, true},
		{"#20", "网络 not_sent 现状仍可换号", func(o *AttemptOutcome) { o.Commit = CommitNotSent; o.HTTPStatus = 0 }, true, true},
		{"#21", "400 不换号", func(o *AttemptOutcome) { o.HTTPStatus = 400; o.Terminal = true }, false, true},
		{"#22", "499 客户端取消不换号", func(o *AttemptOutcome) {
			// 规格列：status 499 / not_sent / frame false / hard false / terminal false。
			// 类型层把 client_cancel 钉成「status 0 + terminal」（生产侧的 499 由
			// code 0 + ctx 取消派生，见 pipeline 的 client-closed-request 分支），
			// 因此本行形状非法——「no」既来自 ResultClientCancel 的前置排除，
			// 也来自契约本身。合法 client_cancel 形状的 no 由既有用例覆盖。
			o.Commit = CommitNotSent
			o.Result = ResultClientCancel
			o.HTTPStatus = 499
			o.Terminal = false
		}, false, false},

		// 附加守卫（不属于 22 条，专门证明「非白名单 5xx 的 fallback 分支」与
		// 「已写字节的 5xx 连类型层都不成立」）。
		{"+#23", "501 终态走 5xx fallback", func(o *AttemptOutcome) { o.HTTPStatus = 501; o.Terminal = true }, false, true},
		{"+#24", "505 终态走 5xx fallback", func(o *AttemptOutcome) { o.HTTPStatus = 505; o.Terminal = true }, false, true},
		{"+#25", "506 终态走 5xx fallback", func(o *AttemptOutcome) { o.HTTPStatus = 506; o.Terminal = true }, false, true},
		{"+#26", "529 终态仍不换号", func(o *AttemptOutcome) { o.HTTPStatus = 529; o.Terminal = true }, false, true},
		{"+#27", "500 已写字节 + 终态：形状非法", func(o *AttemptOutcome) {
			o.HTTPStatus = 500
			o.Commit = CommitResponseStarted
			o.BusinessFrameSent = true
			o.Terminal = true
		}, false, false},
		{"+#28", "502 已写字节 + 终态：形状非法", func(o *AttemptOutcome) {
			o.HTTPStatus = 502
			o.Commit = CommitSentAmbiguous
			o.BusinessFrameSent = true
			o.Terminal = true
		}, false, false},
	}
	require.Len(t, rows, 28)
	specRows := 0
	for _, r := range rows {
		if r.spec[0] == '#' {
			specRows++
		}
	}
	require.Equal(t, 22, specRows, "规格 §测试矩阵 22 条一条不能少")

	for _, r := range rows {
		t.Run(r.spec+" "+r.name, func(t *testing.T) {
			o := baseOutcome()
			// 矩阵基形：上游已响应 5xx/4xx、零业务字节、非 hard、非终态。
			o.Commit = CommitUpstreamResponded
			o.Result = ResultFailed
			o.HTTPStatus = 500
			o.BusinessFrameSent = false
			o.HardContinuation = false
			o.Terminal = false
			r.apply(&o)

			err := o.Validate()
			require.Equal(t, r.valid, err == nil, "形状合法性（err=%v）", err)
			require.Equal(t, r.retry, CanRetry(CallerChat, o),
				"expect=%v cat=%v commit=%v status=%d frame=%v hard=%v terminal=%v err=%v",
				r.retry, o.CallerCategory, o.Commit, o.HTTPStatus, o.BusinessFrameSent, o.HardContinuation, o.Terminal, err)
		})
	}
}

// TestRetryMatrixRetryable5xxAllCallers 十类 caller 共用同一集合（矩阵不按
// caller 分叉）：零字节 5xx 全可换号，hard continuation 全不可。
func TestRetryMatrixRetryable5xxAllCallers(t *testing.T) {
	for _, cat := range AllCallerCategories() {
		o := baseOutcome()
		o.CallerCategory = cat
		o.Commit = CommitUpstreamResponded
		o.Result = ResultFailed
		o.HTTPStatus = 502
		require.NoError(t, o.Validate())
		require.True(t, CanRetry(cat, o), "%s 零字节 502 必须可换号", cat)

		o.HardContinuation = true
		require.False(t, CanRetry(cat, o), "%s hard 502 不得换号", cat)

		// outcomeForKind 的 "5xx" 分支（规格 §5）：零字节形状合法且可换号；
		// hard 形状仍被矩阵挡住。
		helper := outcomeForKind("5xx", cat, false)
		require.False(t, helper.Terminal, "零字节 5xx 不得被 Terminal 短路")
		require.NoError(t, helper.Validate(), "helper 5xx 形状必须合法")
		require.True(t, CanRetry(cat, helper), "%s helper 5xx 必须可换号", cat)
		helperHard := outcomeForKind("5xx", cat, true)
		require.False(t, CanRetry(cat, helperHard), "%s helper hard 5xx 不得换号", cat)
	}
}

func attemptOpt() scheduler.Attempt {
	return scheduler.Attempt{
		AttemptID: "req-1:1", RouteClassID: "rc", QualityClassID: "qc", CandidateFingerprint: "fp",
		TemplateID: 1, AccountID: 1, RequestedModel: "gpt-4o", MappedModel: "gpt-4o",
		Lane: scheduler.AttemptLanePrimary, Ordinal: 1, RoutingGeneration: 1,
		IdentityRevision: 1, CallerCategory: string(CallerChat), OperationTag: "chat_completions",
	}
}

// TestRetryOutcomeForAttemptRetryable5xx 钉 failover_plan 的 terminal 计算：
// 白名单内的 5xx 必须不再被 Terminal 短路，其余 5xx 保持终态（逐字对齐旧行为）。
func TestRetryOutcomeForAttemptRetryable5xx(t *testing.T) {
	cases := []struct {
		name     string
		code     int
		callErr  error
		terminal bool
		retry    bool
		valid    bool
	}{
		{"500", 500, nil, false, true, true},
		{"502", 502, nil, false, true, true},
		{"503", 503, nil, false, true, true},
		{"504", 504, nil, false, true, true},
		{"520", 520, nil, false, true, true},
		{"529", 529, nil, false, true, true},
		{"530", 530, nil, false, true, true},
		{"501 未实现", 501, nil, true, false, true},
		{"505 协议版本", 505, nil, true, false, true},
		{"506 非白名单", 506, nil, true, false, true},
		{"429 现状不变", 429, nil, false, true, true},
		{"400 4xx 终态", 400, nil, true, false, true},
		{"0 网络错误", 0, errors.New("eof"), false, true, true},
		// code 0 无 callErr 是既有退化形状（terminal not_sent 本就不合法）：
		// terminal 保持 true ⇒ 不换号，行为与改动前一致。
		{"0 无错误信息", 0, nil, true, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := retryOutcomeForAttempt(attemptOpt(), c.code, c.callErr, context.Background())
			require.Equal(t, c.terminal, o.Terminal, "terminal 计算")
			require.Equal(t, c.valid, o.Validate() == nil, "terminal 形状合法性")
			require.Equal(t, c.retry, CanRetry(CallerChat, o), "矩阵判定")
		})
	}
}

// TestShouldRetryWithPlanRetryable5xx 钉 overlay：hard continuation 的 5xx 由
// shouldRetryWithPlan 置位后仍不换号（矩阵与 overlay 语义一致，不靠单点）。
func TestShouldRetryWithPlanRetryable5xx(t *testing.T) {
	p := &Proxy{}
	ctx := context.Background()
	require.True(t, p.shouldRetryWithPlan(ctx, 502, nil, attemptOpt(), false), "零字节 502 可换号")
	require.False(t, p.shouldRetryWithPlan(ctx, 502, nil, attemptOpt(), true), "hard 502 不得换号")
	require.False(t, p.shouldRetryWithPlan(ctx, 501, nil, attemptOpt(), false), "501 不得换号")
}
