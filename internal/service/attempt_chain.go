// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// 请求链（spec 2026-11-18 retry-observability §4/§4bis）：把三张日志表的行合并
// 成一条按 ordinal 排序的链。合并是**纯函数**（零 IO、零时钟）：输入就是 repo 的
// 三个 DTO 组，输出可序列化的 hops + truncated。窗口缺省/上限与读面装配在
// QueryAttemptChain。
//
// 三源的语义边界（诚实版，不在本层补造数据）：
//   - attempt_logs：failover 循环**可见**的失败尝试，一次 dispatch 一行；
//   - err_logs：终态错误（已交付失败/上游错误）与本地拒绝（401/额度/402 预检）；
//   - usage_logs：请求级终态（成功 / 已交付）。占位在最后，永远保留。
//
// 去重键是 (request_id, attempt_ordinal)：request_id 恒等（整条链按它查），故实现
// 里只剩 ordinal。**attempt 行优先**：同 ordinal 的 err 行丢弃——两表列集不同，
// 丢弃时不回填任何字段（err 行独有的文本不搬进 attempt 行），否则链上会出现
// 「这一跳既像 attempt 又像 err」的缝合行。err 行 ordinal 为 NULL 时不参与去重
// （它没有 attempt 语义：本地拒绝/已交付失败），按时间排并标 ordinal_unknown。
//
// **不提供 in_progress 之类的「进行中」字段**（评审裁决，理由写死在这里）：耗尽
// 与本地拒绝本来就**没有** usage 行，任何「按有无 usage 反推进行中」的字段都会在
// err_logs 行被背压丢弃（worker 有界队列）时说谎——链会显示「还在跑」，而请求早已
// 结束。诚实的表述只有一句：没有 usage 行 ≠ 失败，请求可能仍在进行中，或明细已被
// 采样丢弃（读 sampling.dropped_total）。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/is7qin/c3api/internal/repository"
)

// 链行来源（线格式取值，openapi 的 AttemptChainSource enum 与本处同源）。
const (
	attemptChainSourceAttempt = "attempt"
	attemptChainSourceErr     = "err"
	attemptChainSourceUsage   = "usage"
)

const (
	// attemptChainRequestIDMax request_id 长度上限。真实值 = 连接级 UUID（36）或
	// 各轮派生的 "<base>#rK"；128 足够宽，同时挡住把整段文本塞进路径的滥用。
	attemptChainRequestIDMax = 128
	// attemptChainDefaultSpan 缺省窗口 = now ∓ 24h（spec §4：默认 ±1 天）。
	attemptChainDefaultSpan = 24 * time.Hour
	// attemptChainWindowMax 窗口上限 = 明细保留期（7 天）。超限直接 400，
	// **不**静默裁剪：裁剪后的链看起来完整，实则少了最早那几跳。
	attemptChainWindowMax = 7 * 24 * time.Hour
)

// errAttemptChainNotWired 读面未装配 ⇒ 明确错误（不是空链：空链会被读成
// 「这个请求没有任何尝试」）。
var errAttemptChainNotWired = errors.New("service: attempt chain read surface not wired")

// AttemptChainReader 三源链读能力（实现 = *repository.Repository，经 s.store
// 能力探测；与 RoutingFactReader 同款接缝，不进 Store 复合面）。
type AttemptChainReader interface {
	QueryAttemptChain(ctx context.Context, requestID string, from, to time.Time) (repository.AttemptChainRows, error)
}

// AttemptChainQuery 请求链入参。From/To 为 nil = 缺省（now ∓ 24h）。
type AttemptChainQuery struct {
	RequestID string
	From      *time.Time
	To        *time.Time
}

// AttemptChainHop 链上的一跳。Ordinal 为 nil = 无序号；OrdinalUnknown 只对
// err 行的「循环不可见」形态为 true（usage 终态行按设计没有序号，标它只是噪音）。
type AttemptChainHop struct {
	Source         string
	Ordinal        *int16
	OrdinalUnknown bool
	AccountID      int64
	Lane           string
	Format         string
	StatusCode     int
	Outcome        string
	ErrorType      string
	CommitState    *string
	ErrorMessage   *string
	LatencyMS      int64
	CreatedAt      time.Time
}

// AttemptChain 请求链结果。From/To 是**生效**窗口（缺省填充后），前端据此显示
// 「实际查了哪一段」；Hops 恒非 nil（空链 = []）。
type AttemptChain struct {
	RequestID string
	From      time.Time
	To        time.Time
	Hops      []AttemptChainHop
	Truncated bool
}

// QueryAttemptChain 合并一个 request_id 的三源链行。窗口规则（spec §4bis）：
// 缺省 ±1 天、from < to、跨度 ≤ 7 天（= 明细保留期）；越界一律 ErrInvalidInput
// （400），不静默裁剪。窗口外的 request_id 返回空链（200），不是 404——「查不到
// 明细」与「这个请求不存在」在本面上不可区分，假装 404 只会让运维去翻错方向。
func (s *Service) QueryAttemptChain(ctx context.Context, q AttemptChainQuery) (*AttemptChain, error) {
	if strings.TrimSpace(q.RequestID) == "" || len(q.RequestID) > attemptChainRequestIDMax {
		return nil, fmt.Errorf("service: request_id must be 1-%d characters: %w", attemptChainRequestIDMax, ErrInvalidInput)
	}
	reader, ok := s.store.(AttemptChainReader)
	if !ok {
		return nil, errAttemptChainNotWired
	}
	now := s.statsClock().UTC()
	from, to := now.Add(-attemptChainDefaultSpan), now.Add(attemptChainDefaultSpan)
	if q.From != nil {
		from = q.From.UTC()
	}
	if q.To != nil {
		to = q.To.UTC()
	}
	if !from.Before(to) {
		return nil, fmt.Errorf("service: attempt chain window is empty: from (%s) must be before to (%s): %w",
			from.Format(time.RFC3339), to.Format(time.RFC3339), ErrInvalidInput)
	}
	if span := to.Sub(from); span > attemptChainWindowMax {
		return nil, fmt.Errorf("service: attempt chain window %s exceeds the %s retention cap: %w",
			span, attemptChainWindowMax, ErrInvalidInput)
	}
	rows, err := reader.QueryAttemptChain(ctx, q.RequestID, from, to)
	if err != nil {
		return nil, err
	}
	hops, truncated := mergeAttemptChain(rows)
	return &AttemptChain{RequestID: q.RequestID, From: from, To: to, Hops: hops, Truncated: truncated}, nil
}

// mergeAttemptChain 纯合并：去重 → 排序 → 拼接。行序无关（输入是三个已排序的
// 源，输出由本函数重排，同一行集恒得同一序列）。
//
// 排序：无序号行（nil）排在有序号行之前（它们是本地拒绝/已交付失败，通常发生在
// 第一次 dispatch 之前），各自按 created_at（并列由 stable 保持输入序：attempt 先
// 于 err）；usage 行是请求级终态，恒排最后（即使它的 created_at 更早）。
func mergeAttemptChain(rows repository.AttemptChainRows) ([]AttemptChainHop, bool) {
	// 去重集合：attempt 的 ordinal。容量按 attempt 腿估——它是唯一必然带
	// ordinal 的源。
	seen := make(map[int16]struct{}, len(rows.Attempt.Rows))
	ordered := make([]AttemptChainHop, 0, len(rows.Attempt.Rows)+len(rows.Err.Rows))
	for _, row := range rows.Attempt.Rows {
		if row.Ordinal != nil {
			seen[*row.Ordinal] = struct{}{}
		}
		ordered = append(ordered, attemptChainHopOf(attemptChainSourceAttempt, row))
	}
	for _, row := range rows.Err.Rows {
		if row.Ordinal != nil {
			if _, dup := seen[*row.Ordinal]; dup {
				// 同一 attempt 已在 attempt_logs 里：丢弃 err 行，**不回填**它的
				// 独有字段（回填就是编数据，见文件头）。
				continue
			}
		}
		hop := attemptChainHopOf(attemptChainSourceErr, row)
		hop.OrdinalUnknown = hop.Ordinal == nil
		ordered = append(ordered, hop)
	}
	terminal := make([]AttemptChainHop, 0, len(rows.Usage.Rows))
	for _, row := range rows.Usage.Rows {
		terminal = append(terminal, attemptChainHopOf(attemptChainSourceUsage, row))
	}
	// 并列时 stable 保持输入序（attempt 先于 err），与链语义一致。
	sort.SliceStable(ordered, func(i, j int) bool { return attemptChainHopLess(ordered[i], ordered[j]) })
	sort.SliceStable(terminal, func(i, j int) bool { return attemptChainHopLess(terminal[i], terminal[j]) })
	hops := make([]AttemptChainHop, 0, len(ordered)+len(terminal))
	hops = append(hops, ordered...)
	hops = append(hops, terminal...)
	return hops, rows.Attempt.Truncated || rows.Err.Truncated || rows.Usage.Truncated
}

// attemptChainHopOf 单行 → 一跳（source 由调用方给，字段原样拷贝，不补默认值）。
func attemptChainHopOf(source string, row repository.AttemptChainRow) AttemptChainHop {
	return AttemptChainHop{
		Source:       source,
		Ordinal:      row.Ordinal,
		AccountID:    row.AccountID,
		Lane:         row.Lane,
		Format:       row.Format,
		StatusCode:   row.StatusCode,
		Outcome:      row.Outcome,
		ErrorType:    row.ErrorType,
		CommitState:  row.CommitState,
		ErrorMessage: row.ErrorMessage,
		LatencyMS:    row.LatencyMS,
		CreatedAt:    row.CreatedAt,
	}
}

// attemptChainHopLess 链行全序：ordinal（nil 最前）→ created_at。并列（两者都相等）
// 由 sort.SliceStable 保持输入序——attempt 行先于 err 行入列，与链语义一致。
func attemptChainHopLess(a, b AttemptChainHop) bool {
	switch {
	case a.Ordinal == nil && b.Ordinal == nil:
	case a.Ordinal == nil:
		return true
	case b.Ordinal == nil:
		return false
	case *a.Ordinal != *b.Ordinal:
		return *a.Ordinal < *b.Ordinal
	}
	return a.CreatedAt.Before(b.CreatedAt)
}
