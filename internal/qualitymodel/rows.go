// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package qualitymodel 是 quality 采样域与存储实现之间的**行契约**。
//
// 为什么单独成包（R1-b）：internal/quality（采样/折叠）生产这些行，
// internal/repository（落库）消费它们。契约住在任何一方都会让另一方反向依赖
// 一个它不该知道的实现 —— 于是把"行形状"提到两侧都能依赖的叶子包。
//
// 边界：只依赖 internal/domain 与 stdlib。**不**依赖 repository（否则环回来）、
// **不**依赖 quality（契约不该反向依赖它的生产者）。
package qualitymodel

import (
	"time"

	"github.com/is7qin/c3api/internal/domain"
)

// RoutingQualityRow 是质量事实表 routing_quality_fact（S1）的落库行。
//
// 逐字段原样搬自 repository/routing.go，一个字节都不改：字段名是与 SQL 列名 /
// 落库 args 对齐的契约（UpsertQualityRow 显式按 row.FieldName 绑定），改名即静默
// 改变语义；字段**顺序**不是契约（SQL 列序由唯一索引决定，且全仓无位置字面量）。
type RoutingQualityRow struct {
	IdentityVersion      int16
	RouteClassID         domain.RouteClassIDVal
	QualityClassID       domain.QualityClassIDVal
	CandidateFingerprint domain.CandidateFingerprintVal
	InstanceSrc          string
	BucketMinute         time.Time
	AbsoluteSequence     int64
	Attempts             int64
	Successes            int64
	Count429             int64
	CountOrdinary4xx     int64
	Count5xx             int64
	CountNetwork         int64
	TTFTN                int64
	TTFTSumLogQ32        int64
	TTFTSumSqLogQ32      int64
	TTFTHist             []int64
	InputTokens          int64
	OutputTokens         int64
	CacheReadTokens      int64
	CacheCreateTokens    int64
	Calls                int64
	Images               int64
}

// RoutingFlowRow 是合并流层 routing_flow_fact（S2′）的落库行。
//
// 同上：字段名逐字搬自 repository/routing.go，是与落库 args 对齐的契约。
type RoutingFlowRow struct {
	IdentityVersion   int16
	RouteClassID      domain.RouteClassIDVal
	TerminalMinute    time.Time
	Ordinal           int16
	Lane              string
	AccountID         int64
	PreviousAccountID *int64
	PreviousOutcome   string
	TransitionReason  string
	Outcome           string
	IsTerminal        bool
	// Generation 是单事件代际（写面输入口径）：同边多代际输入由
	// UpsertFlowSnapshot 按新键折叠为 min_generation 后落库。
	Generation       int64
	InstanceSrc      string
	AbsoluteSequence int64
	ChainCount       int64
}
