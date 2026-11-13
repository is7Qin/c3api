// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/usage"
)

// --- 计费判据（spec-stream-failure-truth §5.2/§5.4）---
//
// 判据 = `error_type == 'none' OR commit_state IN ('sent_ambiguous',
// 'response_started')`；NULL = **未知** ⇒ 不计费（只此一条
// 保守判定，不回填不重判）。这里断言的是**哪几行**进了哪张表，不是"非空"。

func commitOf(c CommitState) *string {
	s := c.String()
	return &s
}

// TestCommitStateBillable 纯判据：两值可计费、其余（含 nil 未知与非法值）不可计费。
func TestCommitStateBillable(t *testing.T) {
	require.False(t, commitStateBillable(nil), "NULL = 未知 ⇒ 不计费（§5.4）")
	require.False(t, commitStateBillable(commitOf(CommitNotSent)), "没发 ⇒ 不计费")
	require.False(t, commitStateBillable(commitOf(CommitUpstreamResponded)), "发了没答 ⇒ 不计费")
	require.True(t, commitStateBillable(commitOf(CommitSentAmbiguous)), "写了业务帧但去向不确定 ⇒ 计费")
	require.True(t, commitStateBillable(commitOf(CommitResponseStarted)), "已开始下发 ⇒ 计费")
	require.False(t, commitStateBillable(strPtrT("garbage")), "未知取值不得计费")
	require.False(t, commitStateBillable(strPtrT("")), "空串不得计费")
}

// TestRouteLogBillableMatrix routeLog 分表成员资格矩阵（精确行集断言）：
//   - usage_logs = error_type none ∨ commit_state 可计费（已交付的失败双轨）；
//   - err_logs   = error_type != none（全部错误明细）；
//   - commit_state NULL 只落在「不计费」这一条保守判定上。
func TestRouteLogBillableMatrix(t *testing.T) {
	usageStore := &captureLogStore{}
	errStore := &captureLogStore{}
	p := newTestProxyFormatLogs(t, "http://127.0.0.1:1", domain.FormatOpenAIChat, usageStore)
	// 两张表各用独立捕获 store：断言「哪几行」而不是「有几行」。
	p.errlog = usage.NewErrLogWorker(usage.ErrLogConfig{QueueSize: 4096, FlushInterval: time.Hour}, errStore, nil)

	type row struct {
		id     string
		et     domain.ErrorType
		commit *string
	}
	rows := []row{
		{"success", domain.ErrNone, nil},
		{"success-with-commit", domain.ErrNone, commitOf(CommitResponseStarted)},
		{"abort-committed", domain.ErrAbort, commitOf(CommitResponseStarted)},
		{"5xx-sent-ambiguous", domain.Err5xx, commitOf(CommitSentAmbiguous)},
		{"5xx-response-started", domain.Err5xx, commitOf(CommitResponseStarted)},
		{"5xx-unknown", domain.Err5xx, nil},                      // NULL = 未知
		{"5xx-not-sent", domain.Err5xx, commitOf(CommitNotSent)}, // 没发
		{"5xx-upstream-responded", domain.Err5xx, commitOf(CommitUpstreamResponded)},
		{"network-committed", domain.ErrNetwork, commitOf(CommitResponseStarted)},
	}
	for _, r := range rows {
		p.routeLog(&domain.UsageLog{
			RequestID: r.id, ErrorType: r.et, CommitState: r.commit, CreatedAt: time.Now(),
		})
	}
	require.NoError(t, p.rec.Close(context.Background()))
	require.NoError(t, p.errlog.Close(context.Background()))

	require.Equal(t, []string{
		"5xx-response-started", "5xx-sent-ambiguous",
		"abort-committed", "network-committed", "success", "success-with-commit",
	}, requestIDs(usageStore), "usage_logs = 成功 ∨ 已交付（commit_state 可计费）")
	require.Equal(t, []string{
		"5xx-not-sent", "5xx-response-started",
		"5xx-sent-ambiguous", "5xx-unknown", "5xx-upstream-responded",
		"abort-committed", "network-committed",
	}, requestIDs(errStore), "err_logs = error_type != none（含已交付的失败双轨）")
}

// requestIDs 取捕获 store 的全部 request_id（排序——断言精确集合）。
func requestIDs(store *captureLogStore) []string {
	store.mu.Lock()
	defer store.mu.Unlock()
	out := make([]string, 0, len(store.logs))
	for _, l := range store.logs {
		out = append(out, l.RequestID)
	}
	sort.Strings(out)
	return out
}
