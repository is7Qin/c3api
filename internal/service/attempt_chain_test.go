// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// 请求链合并的纯函数表 + 窗口/边界（spec 2026-11-18 §6 S3-9/10 的服务面一半）。
//
// 纯函数断言的是**形状**：去重（attempt 胜且不回填）、nil ordinal 的
// ordinal_unknown、usage 恒最后、乱序输入得同一序列、truncated 原样透出。
// 真实写路径的端到端证据在 internal/proxy（跑一次真 failover）；这里禁止为了
// 「让用例绿」手搓 err_logs 行——输入就是 repo DTO，等价于读面结果。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/repository"
)

var attemptChainBase = time.Date(2026, 11, 18, 10, 0, 0, 0, time.UTC)

func ord(v int16) *int16 { return &v }

func strp(v string) *string { return &v }

// chainRow 造一行（默认是 attempt 腿的形状：有 ordinal、有 lane）。
func chainRow(ordinal int16, at time.Time) repository.AttemptChainRow {
	return repository.AttemptChainRow{
		Ordinal: &ordinal, AccountID: int64(ordinal), Lane: "primary",
		Format: "openai-chat", StatusCode: 500, Outcome: "5xx", ErrorType: "5xx",
		ErrorMessage: strp("boom"), LatencyMS: 12, CreatedAt: at,
	}
}

// errRowNoOrdinal 造一行「循环不可见」的 err_logs 行（本地拒绝/已交付失败）。
func errRowNoOrdinal(at time.Time) repository.AttemptChainRow {
	return repository.AttemptChainRow{
		AccountID: 0, Format: "openai-chat", StatusCode: 401, ErrorType: "auth",
		ErrorMessage: strp("invalid key"), LatencyMS: 1, CreatedAt: at,
	}
}

func usageRow(at time.Time) repository.AttemptChainRow {
	return repository.AttemptChainRow{
		AccountID: 7, Format: "openai-chat", ErrorType: "none",
		CommitState: strp("response_started"), LatencyMS: 900, CreatedAt: at,
	}
}

// TestMergeAttemptChain 表驱动：六种输入形状（含空集合、只有 usage、同 ordinal
// 去重、err 无 ordinal、truncated、乱序）。
func TestMergeAttemptChain(t *testing.T) {
	for _, tc := range []struct {
		name          string
		rows          repository.AttemptChainRows
		wantSources   []string
		wantOrdinals  []*int16
		wantUnknown   []bool
		wantTruncated bool
		// wantErrMessages 逐跳的错误文本（nil = 该跳无文本）。去重场景用它证明
		// 「err 行有、attempt 行没有」的字段**没有**被回填。
		wantErrMessages []*string
	}{
		{
			name: "empty",
			rows: repository.AttemptChainRows{},
		},
		{
			name: "usage only",
			rows: repository.AttemptChainRows{
				Usage: repository.AttemptChainSource{Rows: []repository.AttemptChainRow{usageRow(attemptChainBase)}},
			},
			wantSources:  []string{"usage"},
			wantOrdinals: []*int16{nil},
			wantUnknown:  []bool{false},
		},
		{
			name: "attempt wins over same ordinal err",
			rows: repository.AttemptChainRows{
				Attempt: repository.AttemptChainSource{Rows: []repository.AttemptChainRow{chainRow(2, attemptChainBase)}},
				Err: repository.AttemptChainSource{Rows: []repository.AttemptChainRow{{
					Ordinal: ord(2), AccountID: 2, StatusCode: 502, ErrorType: "5xx",
					// err 行独有的文本：去重时必须丢掉，**不得**搬进 attempt 行。
					ErrorMessage: strp("exhausted after retries"), LatencyMS: 30,
					CreatedAt: attemptChainBase.Add(time.Second),
				}}},
			},
			wantSources:     []string{"attempt"},
			wantOrdinals:    []*int16{ord(2)},
			wantUnknown:     []bool{false},
			wantErrMessages: []*string{strp("boom")},
		},
		{
			name: "err without ordinal is kept and flagged",
			rows: repository.AttemptChainRows{
				Attempt: repository.AttemptChainSource{Rows: []repository.AttemptChainRow{chainRow(1, attemptChainBase.Add(time.Minute))}},
				Err:     repository.AttemptChainSource{Rows: []repository.AttemptChainRow{errRowNoOrdinal(attemptChainBase)}},
			},
			wantSources:     []string{"err", "attempt"},
			wantOrdinals:    []*int16{nil, ord(1)},
			wantUnknown:     []bool{true, false},
			wantErrMessages: []*string{strp("invalid key"), strp("boom")},
		},
		{
			name: "truncated bubbles up from any source",
			rows: repository.AttemptChainRows{
				Attempt: repository.AttemptChainSource{Rows: []repository.AttemptChainRow{chainRow(1, attemptChainBase)}},
				Err:     repository.AttemptChainSource{Truncated: true},
			},
			wantSources:     []string{"attempt"},
			wantOrdinals:    []*int16{ord(1)},
			wantUnknown:     []bool{false},
			wantTruncated:   true,
			wantErrMessages: []*string{strp("boom")},
		},
		{
			// 乱序：attempt 腿按 ordinal 乱给、err 行时间更早、usage 行 created_at
			// 早于 attempt —— 输出仍必须是 [nil(err), 1, 2, usage]。
			name: "unordered input sorts deterministically and pins usage last",
			rows: repository.AttemptChainRows{
				Attempt: repository.AttemptChainSource{Rows: []repository.AttemptChainRow{
					chainRow(2, attemptChainBase.Add(2*time.Minute)),
					chainRow(1, attemptChainBase.Add(time.Minute)),
				}},
				Err:   repository.AttemptChainSource{Rows: []repository.AttemptChainRow{errRowNoOrdinal(attemptChainBase)}},
				Usage: repository.AttemptChainSource{Rows: []repository.AttemptChainRow{usageRow(attemptChainBase.Add(30 * time.Second))}},
			},
			wantSources:     []string{"err", "attempt", "attempt", "usage"},
			wantOrdinals:    []*int16{nil, ord(1), ord(2), nil},
			wantUnknown:     []bool{true, false, false, false},
			wantErrMessages: []*string{strp("invalid key"), strp("boom"), strp("boom"), nil},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hops, truncated := mergeAttemptChain(tc.rows)
			require.Equal(t, tc.wantTruncated, truncated)
			require.Len(t, hops, len(tc.wantSources))
			for i, hop := range hops {
				require.Equal(t, tc.wantSources[i], hop.Source, "hop %d source", i)
				if tc.wantOrdinals[i] == nil {
					require.Nil(t, hop.Ordinal, "hop %d ordinal", i)
				} else {
					require.NotNil(t, hop.Ordinal, "hop %d ordinal", i)
					require.Equal(t, *tc.wantOrdinals[i], *hop.Ordinal, "hop %d ordinal", i)
				}
				require.Equal(t, tc.wantUnknown[i], hop.OrdinalUnknown, "hop %d ordinal_unknown", i)
				if tc.wantErrMessages != nil {
					if tc.wantErrMessages[i] == nil {
						require.Nil(t, hop.ErrorMessage, "hop %d error_message", i)
					} else {
						require.NotNil(t, hop.ErrorMessage, "hop %d error_message", i)
						require.Equal(t, *tc.wantErrMessages[i], *hop.ErrorMessage, "hop %d error_message", i)
					}
				}
			}
		})
	}
}

// TestMergeAttemptChainDuplicateErrRowsSurvive 同一 ordinal 的两条 err 行**不**互相
// 去重（去重只发生在 attempt ↔ err 之间）：多出来的那条是真实数据，静默吞掉它
// 就是在替上游擦掉一次落库异常。
func TestMergeAttemptChainDuplicateErrRowsSurvive(t *testing.T) {
	first := errRowNoOrdinal(attemptChainBase)
	second := errRowNoOrdinal(attemptChainBase.Add(time.Second))
	first.Ordinal, second.Ordinal = ord(3), ord(3)
	hops, _ := mergeAttemptChain(repository.AttemptChainRows{
		Err: repository.AttemptChainSource{Rows: []repository.AttemptChainRow{first, second}},
	})
	require.Len(t, hops, 2, "两条同 ordinal 的 err 行都是真实行，不得互删")
}

// TestMergeAttemptChainKeepsUsageAtSourceCap 尝试腿顶到 128 行（truncated）时，
// usage 终态行**仍在链尾**、总量 129——防「三源合计裁 128」把终态吞掉（那会让
// 链看起来「没有交付结论」，而终态恰恰是最要紧的一跳）。
func TestMergeAttemptChainKeepsUsageAtSourceCap(t *testing.T) {
	const capRows = 128 // 与 repository.attemptChainSourceLimit 同值；repo 侧另有用例钉住该常量
	attempts := make([]repository.AttemptChainRow, 0, capRows)
	for i := 1; i <= capRows; i++ {
		attempts = append(attempts, chainRow(int16(i), attemptChainBase.Add(time.Duration(i)*time.Second)))
	}
	hops, truncated := mergeAttemptChain(repository.AttemptChainRows{
		Attempt: repository.AttemptChainSource{Rows: attempts, Truncated: true},
		Usage:   repository.AttemptChainSource{Rows: []repository.AttemptChainRow{usageRow(attemptChainBase.Add(48 * time.Hour))}},
	})
	require.Len(t, hops, capRows+1, "截断按**每源**判定：usage 不受 attempt 腿的上限影响")
	require.True(t, truncated)
	require.Equal(t, "usage", hops[len(hops)-1].Source, "usage 终态必须在链尾")
}

// TestMergeAttemptChainKeepsBothNullOrdinalErrRows 同一 request 的两条 NULL ordinal
// err 行（两次本地拒绝）都必须出现：去重键只在 attempt↔err 之间生效，NULL 之间
// 不互相去重（否则第二条拒绝会被静默吞掉）。
func TestMergeAttemptChainKeepsBothNullOrdinalErrRows(t *testing.T) {
	first := errRowNoOrdinal(attemptChainBase)
	first.StatusCode = 401
	second := errRowNoOrdinal(attemptChainBase.Add(90 * time.Second))
	second.StatusCode = 402
	second.ErrorType = "billing"
	hops, truncated := mergeAttemptChain(repository.AttemptChainRows{
		Err: repository.AttemptChainSource{Rows: []repository.AttemptChainRow{second, first}},
	})
	require.False(t, truncated)
	require.Len(t, hops, 2, "两条 NULL ordinal 的拒绝行都是真实行，不得互删")
	require.Equal(t, 401, hops[0].StatusCode, "按 created_at 排序：先发生的在前")
	require.Equal(t, 402, hops[1].StatusCode)
	for _, hop := range hops {
		require.Nil(t, hop.Ordinal)
		require.True(t, hop.OrdinalUnknown)
	}
}

// attemptChainStore fake：记录入参并回放固定结果（能力探测走 store 的
// QueryAttemptChain 方法）。
type attemptChainStore struct {
	*fakeStore
	err      error
	rows     repository.AttemptChainRows
	gotID    string
	gotFrom  time.Time
	gotTo    time.Time
	gotCalls int
}

func (s *attemptChainStore) QueryAttemptChain(_ context.Context, requestID string, from, to time.Time) (repository.AttemptChainRows, error) {
	s.gotCalls++
	s.gotID, s.gotFrom, s.gotTo = requestID, from, to
	return s.rows, s.err
}

func newAttemptChainService(store *attemptChainStore, now time.Time) *Service {
	svc := New(Deps{Store: store, Invalidate: NopInvalidator{}, EmailCodeStore: testEmailCodes})
	svc.statsNow = func() time.Time { return now }
	return svc
}

// TestQueryAttemptChainDefaultsWindow 缺省 from/to = now ∓ 24h（spec §4：默认 ±1 天），
// 且回显的是**生效**窗口（不是入参零值）。
func TestQueryAttemptChainDefaultsWindow(t *testing.T) {
	store := &attemptChainStore{fakeStore: newFakeStore()}
	now := attemptChainBase
	svc := newAttemptChainService(store, now)

	got, err := svc.QueryAttemptChain(context.Background(), AttemptChainQuery{RequestID: "req-1"})
	require.NoError(t, err)
	require.Equal(t, now.Add(-24*time.Hour), store.gotFrom)
	require.Equal(t, now.Add(24*time.Hour), store.gotTo)
	require.Equal(t, store.gotFrom, got.From)
	require.Equal(t, store.gotTo, got.To)
	require.Equal(t, "req-1", got.RequestID)
	require.NotNil(t, got.Hops, "空链必须是 []（nil 会序列化成 null）")
	require.Empty(t, got.Hops)
}

// TestQueryAttemptChainExplicitWindow 显式窗口原样下传（UTC 归一）；跨度恰好 7 天
// 必须放行（上限是闭区间语义：> cap 才拒）。
func TestQueryAttemptChainExplicitWindow(t *testing.T) {
	store := &attemptChainStore{fakeStore: newFakeStore()}
	svc := newAttemptChainService(store, attemptChainBase)
	from := attemptChainBase.Add(-48 * time.Hour)
	to := from.Add(attemptChainWindowMax)

	_, err := svc.QueryAttemptChain(context.Background(), AttemptChainQuery{RequestID: "req-2", From: &from, To: &to})
	require.NoError(t, err)
	require.Equal(t, 1, store.gotCalls)
	require.Equal(t, from.UTC(), store.gotFrom)
	require.Equal(t, to.UTC(), store.gotTo)
}

// TestQueryAttemptChainRejectsBadWindows 窗口与 request_id 的边界：空/过长 id、
// from ≥ to、跨度超 7 天全部 ErrInvalidInput（400），且**不触读面**。
func TestQueryAttemptChainRejectsBadWindows(t *testing.T) {
	now := attemptChainBase
	longID := make([]byte, attemptChainRequestIDMax+1)
	for i := range longID {
		longID[i] = 'a'
	}
	badFrom := now
	badTo := now
	tooWideFrom := now.Add(-8 * 24 * time.Hour)
	tooWideTo := now

	for name, q := range map[string]AttemptChainQuery{
		"empty request id":      {RequestID: ""},
		"blank request id":      {RequestID: "   "},
		"request id too long":   {RequestID: string(longID)},
		"from equals to":        {RequestID: "r", From: &badFrom, To: &badTo},
		"window wider than max": {RequestID: "r", From: &tooWideFrom, To: &tooWideTo},
		"from after to":         {RequestID: "r", From: &badTo, To: &tooWideFrom},
	} {
		t.Run(name, func(t *testing.T) {
			store := &attemptChainStore{fakeStore: newFakeStore()}
			svc := newAttemptChainService(store, now)
			_, err := svc.QueryAttemptChain(context.Background(), q)
			require.ErrorIs(t, err, ErrInvalidInput)
			require.Zero(t, store.gotCalls, "非法入参不得打到读面")
		})
	}
}

// TestQueryAttemptChainWideWindowIncludesEarlierAttempt 请求可以跨小时：attempt 行
// 落在 created_at−2h 时，created_at ±24h 的窗口必须仍把它查回来（窗口原样下传，
// 合并不按时间裁；前端 CHAIN_WINDOW_MS 正是 24h）。
func TestQueryAttemptChainWideWindowIncludesEarlierAttempt(t *testing.T) {
	created := attemptChainBase
	earlier := created.Add(-2 * time.Hour)
	store := &attemptChainStore{fakeStore: newFakeStore(), rows: repository.AttemptChainRows{
		Attempt: repository.AttemptChainSource{Rows: []repository.AttemptChainRow{chainRow(1, earlier)}},
	}}
	svc := newAttemptChainService(store, created)
	from := created.Add(-24 * time.Hour)
	to := created.Add(24 * time.Hour)

	got, err := svc.QueryAttemptChain(context.Background(), AttemptChainQuery{
		RequestID: "req-long", From: &from, To: &to,
	})
	require.NoError(t, err)
	require.Equal(t, from.UTC(), store.gotFrom, "±24h 的窗口必须原样下传到读面")
	require.Equal(t, to.UTC(), store.gotTo)
	require.Len(t, got.Hops, 1)
	require.Equal(t, earlier, got.Hops[0].CreatedAt, "早 2h 的那一跳仍要在链上（不能被时间裁掉）")
}

// TestQueryAttemptChainNotWired 读能力未装配 ⇒ 明确错误（不是空链：空链会被读成
// 「这个请求没有任何尝试」）。
func TestQueryAttemptChainNotWired(t *testing.T) {
	svc := newAttemptChainService(&attemptChainStore{fakeStore: newFakeStore()}, attemptChainBase)
	svc.store = newFakeStore() // 去掉 QueryAttemptChain 能力
	_, err := svc.QueryAttemptChain(context.Background(), AttemptChainQuery{RequestID: "r"})
	require.ErrorIs(t, err, errAttemptChainNotWired)
}

// TestQueryAttemptChainPropagatesReaderError 读面错误原样上抛（不折叠成空链）。
func TestQueryAttemptChainPropagatesReaderError(t *testing.T) {
	boom := errors.New("db down")
	store := &attemptChainStore{fakeStore: newFakeStore(), err: boom}
	svc := newAttemptChainService(store, attemptChainBase)
	_, err := svc.QueryAttemptChain(context.Background(), AttemptChainQuery{RequestID: "r"})
	require.ErrorIs(t, err, boom)
}
