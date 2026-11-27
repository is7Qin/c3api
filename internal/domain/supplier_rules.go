// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
	"time"
)

// 供应商付款/冻结/风控的**纯规则与值类型**（spec 2026-10-09 §2.4/§6.5）。
//
// 为什么在 domain 而不是 internal/supplier：这些函数没有状态、没有 I/O、没有
// receiver——它们只回答"给定这些输入，判定是什么"。而调用方有两类：
// 持久化实现（repository 在 claim 事务里算门禁）与 worker 编排。
// 放在 supplier 会让持久化层反向依赖 worker 编排包；放在 domain 则双方都只依赖
// 值类型与纯谓词，依赖方向变干净。supplier 对这些名字保留了别名，
// 既有调用点一字未动。
//
// ⚠ 本文件必须保持纯：不 import pgx/redis/net，不读包级可变量，不取当前时间
// （时间一律由调用方以参数传入，便于表驱动测试）。

// PayoutGateConfig 付款风控门阈值（§6.4/§6.5 四键 + 计费开关）。
type PayoutGateConfig struct {
	// BillingEnabled 计费链开关：false ⇒ 消费不落 usage、探针无意义 ⇒ 拒绝放行。
	BillingEnabled bool
	// MaxBacklogRows billed 健康探针 backlog 上限（行）。
	MaxBacklogRows int
	// MaxBacklogAge 队头最长滞留。
	MaxBacklogAge time.Duration
	// MaxObserveAge 健康观测最大陈旧。
	MaxObserveAge time.Duration
}

// BillingProbe 扣费链健康探针结果（单语句快照）。
type BillingProbe struct {
	// BacklogRows 未扣费且计价为正的行数。
	BacklogRows int64
	// OldestCreatedAt 队头 created_at（backlogRows=0 时为 nil）。
	OldestCreatedAt *time.Time
	// ObservedAt 查询快照采样时刻（DB 时钟）。
	ObservedAt time.Time
}

// ErrPayoutGate 付款风控门拒绝（失败闭合，不置 paying）。
var ErrPayoutGate = errors.New("supplier: payout risk gate rejected")

// EvaluatePayoutGate 放行谓词（§6.5，单语句快照；dbNow = claim 事务 DB 时间）：
//
//	billing_enabled
//	AND backlog_rows <= max_backlog_rows
//	AND (backlog_rows = 0 OR db_now - oldest_created_at <= max_backlog_age)
//	AND 0 <= db_now - observed_at <= max_observe_age
//
// 任一不满足（含探针缺结果 / 负或未来时间 / 过期）⇒ 拒绝。
func EvaluatePayoutGate(cfg PayoutGateConfig, p BillingProbe, dbNow time.Time) error {
	if !cfg.BillingEnabled {
		return fmt.Errorf("%w: billing disabled", ErrPayoutGate)
	}
	if p.BacklogRows < 0 {
		return fmt.Errorf("%w: negative backlog rows", ErrPayoutGate)
	}
	if p.BacklogRows > int64(cfg.MaxBacklogRows) {
		return fmt.Errorf("%w: backlog_rows %d > %d", ErrPayoutGate, p.BacklogRows, cfg.MaxBacklogRows)
	}
	if p.BacklogRows > 0 {
		if p.OldestCreatedAt == nil {
			return fmt.Errorf("%w: backlog_rows %d but oldest_created_at missing", ErrPayoutGate, p.BacklogRows)
		}
		if age := dbNow.Sub(*p.OldestCreatedAt); age < 0 || age > cfg.MaxBacklogAge {
			return fmt.Errorf("%w: backlog head age %s out of [0, %s]", ErrPayoutGate, age, cfg.MaxBacklogAge)
		}
	}
	observeAge := dbNow.Sub(p.ObservedAt)
	if observeAge < 0 || observeAge > cfg.MaxObserveAge {
		return fmt.Errorf("%w: observe age %s out of [0, %s]", ErrPayoutGate, observeAge, cfg.MaxObserveAge)
	}
	return nil
}

// EffectiveFreezeHours 生效冻结小时（全局开关优先，§2.4）：
//
//	生效 = freeze_enabled ? COALESCE(row.freeze_hours, globalDefault) : 0
//
// rowOverride = nil ⇒ 继承 globalDefault；= &0 ⇒ 不冻结；> 0 ⇒ 覆盖。
func EffectiveFreezeHours(freezeEnabled bool, rowOverride *int, globalDefault int) int {
	if !freezeEnabled {
		return 0
	}
	if rowOverride != nil {
		return *rowOverride
	}
	return globalDefault
}

// BucketAvailableAt 桶时刻（写时算定，纯整数，单位统一到秒；§2.4）：
//
//	available_at = ceil_to(credited, g) + H×3600
//	ceil_to(t, g) = (t + g − 1) / g × g   （整数运算；不用 time.Truncate）
//
// credited 取 DB clock_timestamp()（M1：不由各实例时钟决定）。g<=0 ⇒ 兜底 1s。
func BucketAvailableAt(credited time.Time, g time.Duration, freezeHours int) time.Time {
	gs := int64(g / time.Second)
	if gs <= 0 {
		gs = 1
	}
	t := credited.Unix()
	ceil := ((t + gs - 1) / gs) * gs
	return time.Unix(ceil+int64(freezeHours)*3600, 0).UTC()
}

// MaxActiveBuckets 数学紧上界 N(x) = ceil(x/g) − floor((x−H)/g) 的闭式上界
// （§2.4/G7 第一层）：`g ∤ H` 与 `g | H` 均为 `ceil(H/g)+1`。默认 H=24,g=2h ⇒ 13。
// 物理行数（G7 第二层）停摆/多代可超此界，不承诺无条件容量上界。
func MaxActiveBuckets(g time.Duration, freezeHours int) int64 {
	gs := int64(g / time.Second)
	if gs <= 0 {
		gs = 1
	}
	h := max(int64(freezeHours)*3600, 0)
	return (h+gs-1)/gs + 1
}

// ---- 风险核对记录（risk_review）----

// ErrRiskReview risk_review 非法（缺项/未知键/错类型/未来 decided_at/超长或过期
// expires_at/revision 不符等）⇒ 失败闭合（service 归类 400）。
var ErrRiskReview = errors.New("supplier: invalid risk_review record")

// RiskReviewScope 固定 scope（平台信用风险放行决定）。
const RiskReviewScope = "platform_credit_risk"

// RiskReviewDecision 唯一允许的结论（放行平台信用风险）。
const RiskReviewDecision = "approve_credit_risk"

//
// 字段面固定：operator_id / decided_at / scope / evidence / decision /
// expires_at / approved_revision。严格 JSON（存 TEXT）：解析时拒绝未知键、错类型、
// 字段缺失、尾随数据；校验时拒绝任意 operator_id、错 scope、错 decision、缺
// evidence、未来 decided_at、超长/过期 expires_at、revision 不符。

// Evidence 非空结构（证据 reference + 核对摘要）。两字段都不得为空串。
type Evidence struct {
	Reference string `json:"reference"`
	Summary   string `json:"summary"`
}

// RiskReview 结构化风险核对记录（§6.5）。
type RiskReview struct {
	OperatorID       int64     `json:"operator_id"`
	DecidedAt        time.Time `json:"decided_at"`
	Scope            string    `json:"scope"`
	Evidence         Evidence  `json:"evidence"`
	Decision         string    `json:"decision"`
	ExpiresAt        time.Time `json:"expires_at"`
	ApprovedRevision int64     `json:"approved_revision"`
}

// ParseRiskReview 严格反序列化：拒绝未知键（DisallowUnknownFields）、错类型
// （JSON 类型与字段不符）、字段缺失（用指针探测）与尾随数据。
func ParseRiskReview(s string) (RiskReview, error) {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	var raw struct {
		OperatorID       *int64     `json:"operator_id"`
		DecidedAt        *time.Time `json:"decided_at"`
		Scope            *string    `json:"scope"`
		Evidence         *Evidence  `json:"evidence"`
		Decision         *string    `json:"decision"`
		ExpiresAt        *time.Time `json:"expires_at"`
		ApprovedRevision *int64     `json:"approved_revision"`
	}
	if err := dec.Decode(&raw); err != nil {
		return RiskReview{}, fmt.Errorf("%w: %v", ErrRiskReview, err)
	}
	// 尾随数据 ⇒ 拒绝（合法单值 JSON 之后的任何非空白 token）。
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return RiskReview{}, fmt.Errorf("%w: trailing data after risk_review", ErrRiskReview)
	}
	var missing []string
	if raw.OperatorID == nil {
		missing = append(missing, "operator_id")
	}
	if raw.DecidedAt == nil {
		missing = append(missing, "decided_at")
	}
	if raw.Scope == nil {
		missing = append(missing, "scope")
	}
	if raw.Evidence == nil {
		missing = append(missing, "evidence")
	}
	if raw.Decision == nil {
		missing = append(missing, "decision")
	}
	if raw.ExpiresAt == nil {
		missing = append(missing, "expires_at")
	}
	if raw.ApprovedRevision == nil {
		missing = append(missing, "approved_revision")
	}
	if len(missing) > 0 {
		return RiskReview{}, fmt.Errorf("%w: missing fields %s", ErrRiskReview, strings.Join(missing, ","))
	}
	return RiskReview{
		OperatorID:       *raw.OperatorID,
		DecidedAt:        raw.DecidedAt.UTC(),
		Scope:            *raw.Scope,
		Evidence:         *raw.Evidence,
		Decision:         *raw.Decision,
		ExpiresAt:        raw.ExpiresAt.UTC(),
		ApprovedRevision: *raw.ApprovedRevision,
	}, nil
}

// BuildRiskReview 由服务端派生权威字段构造记录：operator_id 取资金 Actor、
// decided_at 取 DB 时刻、expires_at = decided_at + maxAge、scope/decision 固定；
// **客户端仅提供结构化 evidence（reference + summary，均非空）** 与 approved_revision。
// 任意非空结构缺失 ⇒ 拒绝（不得把自由文本当证据自动签为放行）。
func BuildRiskReview(actorID int64, evidence Evidence, approvedRevision int64, decidedAt time.Time, maxAge time.Duration) (RiskReview, error) {
	ref := strings.TrimSpace(evidence.Reference)
	summary := strings.TrimSpace(evidence.Summary)
	if ref == "" || summary == "" {
		return RiskReview{}, fmt.Errorf("%w: evidence reference and summary required", ErrRiskReview)
	}
	if approvedRevision < 1 {
		return RiskReview{}, fmt.Errorf("%w: approved_revision required", ErrRiskReview)
	}
	return RiskReview{
		OperatorID:       actorID,
		DecidedAt:        decidedAt.UTC(),
		Scope:            RiskReviewScope,
		Evidence:         Evidence{Reference: ref, Summary: summary},
		Decision:         RiskReviewDecision,
		ExpiresAt:        decidedAt.Add(maxAge).UTC(),
		ApprovedRevision: approvedRevision,
	}, nil
}

// MarshalRiskReview 序列化记录为紧凑 JSON（落 `risk_review` TEXT）。
func MarshalRiskReview(rr RiskReview) (string, error) {
	b, err := json.Marshal(rr)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrRiskReview, err)
	}
	return string(b), nil
}

// ValidateRiskReview 校验记录字段面（§6.5 拒绝矩阵）：
//   - operator_id 必须 == 资金 Actor（非客户端任意填）；
//   - scope == platform_credit_risk；
//   - decision == approve_credit_risk；
//   - evidence.reference/summary 均非空；
//   - approved_revision == expected_revision；
//   - decided_at 不得在未来（`now` 为权威 DB 时刻，无时钟偏移宽限）；
//   - decided_at 不得早于 now - maxAge（过期）；
//   - expires_at 不得超出 decided_at + maxAge（超长）；
//   - expires_at 不得早于 now（已过期）。
func ValidateRiskReview(rr RiskReview, actorID, expectedRevision int64, now time.Time, maxAge time.Duration) error {
	if rr.OperatorID != actorID {
		return fmt.Errorf("%w: operator_id %d != actor %d", ErrRiskReview, rr.OperatorID, actorID)
	}
	if rr.Scope != RiskReviewScope {
		return fmt.Errorf("%w: scope %q", ErrRiskReview, rr.Scope)
	}
	if rr.Decision != RiskReviewDecision {
		return fmt.Errorf("%w: decision %q", ErrRiskReview, rr.Decision)
	}
	if strings.TrimSpace(rr.Evidence.Reference) == "" || strings.TrimSpace(rr.Evidence.Summary) == "" {
		return fmt.Errorf("%w: evidence incomplete", ErrRiskReview)
	}
	if rr.ApprovedRevision != expectedRevision {
		return fmt.Errorf("%w: approved_revision %d != expected %d", ErrRiskReview, rr.ApprovedRevision, expectedRevision)
	}
	// A22「未来即拒绝」：所有权威时刻取自 DB，不接受实例时钟偏移宽限。
	if rr.DecidedAt.After(now) {
		return fmt.Errorf("%w: decided_at in the future", ErrRiskReview)
	}
	if now.Sub(rr.DecidedAt) > maxAge {
		return fmt.Errorf("%w: risk review expired (decided_at too old)", ErrRiskReview)
	}
	if rr.ExpiresAt.After(rr.DecidedAt.Add(maxAge)) {
		return fmt.Errorf("%w: expires_at exceeds decided_at + max_age", ErrRiskReview)
	}
	if rr.ExpiresAt.Before(now) {
		return fmt.Errorf("%w: risk review expired (expires_at)", ErrRiskReview)
	}
	return nil
}

// ParseAndValidateRiskReview 严格反序列化 + 校验（round-trip 复核持久化记录）。
func ParseAndValidateRiskReview(s string, actorID, expectedRevision int64, now time.Time, maxAge time.Duration) error {
	rr, err := ParseRiskReview(s)
	if err != nil {
		return err
	}
	return ValidateRiskReview(rr, actorID, expectedRevision, now, maxAge)
}

// ---- 记账聚合（守恒 + 溢出检查）----

// - 金额守恒（M2）：Σto_available + Σto_frozen == Σ rows.Earn，不符返回错误；
// - 返回 uid 升序（显式锁序）。
func AggregateBatch(rows []BatchRow, freezeByUID map[int64]int) ([]UIDCredit, []DayRecon, error) {
	seen := make(map[int64]struct{}, len(rows))
	type acc struct{ earn, cost int64 }
	perUID := make(map[int64]*acc)
	perDay := make(map[[2]int64]*DayRecon)
	var rowsEarn int64

	for _, r := range rows {
		if _, dup := seen[r.ID]; dup {
			return nil, nil, fmt.Errorf("supplier: duplicate batch row id %d", r.ID)
		}
		seen[r.ID] = struct{}{}
		if r.Earn < 0 || r.Cost < 0 {
			return nil, nil, fmt.Errorf("supplier: negative earn/cost in row %d", r.ID)
		}
		var err error
		if rowsEarn, err = AddChecked(rowsEarn, r.Earn); err != nil {
			return nil, nil, err
		}
		a := perUID[r.UID]
		if a == nil {
			a = &acc{}
			perUID[r.UID] = a
		}
		if a.earn, err = AddChecked(a.earn, r.Earn); err != nil {
			return nil, nil, err
		}
		if a.cost, err = AddChecked(a.cost, r.Cost); err != nil {
			return nil, nil, err
		}
		day := r.CreatedAt.UTC().Truncate(24 * time.Hour)
		key := [2]int64{r.UID, day.Unix()}
		d := perDay[key]
		if d == nil {
			d = &DayRecon{UID: r.UID, SourceDay: day}
			perDay[key] = d
		}
		if d.GrossCost, err = AddChecked(d.GrossCost, r.Cost); err != nil {
			return nil, nil, err
		}
		if d.Earn, err = AddChecked(d.Earn, r.Earn); err != nil {
			return nil, nil, err
		}
		if d.Rows, err = AddChecked(d.Rows, 1); err != nil {
			return nil, nil, err
		}
	}

	totals := make([]UIDCredit, 0, len(perUID))
	var sumAvail, sumFrozen int64
	for uid, a := range perUID {
		uc := UIDCredit{UID: uid, Earn: a.earn}
		if freezeByUID[uid] > 0 {
			uc.ToFrozen = a.earn
		} else {
			uc.ToAvailable = a.earn
		}
		var err error
		if sumAvail, err = AddChecked(sumAvail, uc.ToAvailable); err != nil {
			return nil, nil, err
		}
		if sumFrozen, err = AddChecked(sumFrozen, uc.ToFrozen); err != nil {
			return nil, nil, err
		}
		totals = append(totals, uc)
	}
	sort.Slice(totals, func(i, j int) bool { return totals[i].UID < totals[j].UID })

	// 金额守恒（M2）：Σto_available + Σto_frozen == Σ rows.Earn。
	combined, err := AddChecked(sumAvail, sumFrozen)
	if err != nil {
		return nil, nil, err
	}
	if combined != rowsEarn {
		return nil, nil, fmt.Errorf("supplier: credit conservation violation: avail+frozen=%d != earn=%d", combined, rowsEarn)
	}

	recon := make([]DayRecon, 0, len(perDay))
	for _, d := range perDay {
		recon = append(recon, *d)
	}
	sort.Slice(recon, func(i, j int) bool {
		if recon[i].UID != recon[j].UID {
			return recon[i].UID < recon[j].UID
		}
		return recon[i].SourceDay.Before(recon[j].SourceDay)
	})
	return totals, recon, nil
}

// AddChecked int64 加法溢出检查（失败闭合，不静默钳制）。导出供记账守恒与解冻聚合
// 前置断言复用（§5.3 M1：UPDATE 前验证非负/无溢出）。
func AddChecked(a, b int64) (int64, error) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, fmt.Errorf("supplier: int64 overflow summing %d + %d", a, b)
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, fmt.Errorf("supplier: int64 underflow summing %d + %d", a, b)
	}
	return a + b, nil
}

// ---- 记账/解冻链的行值（存储契约与编排共用）----

// BatchRow credit 取批单行（usage_logs 未记账正收益子集投影，§5.1/§5.2）。
type BatchRow struct {
	ID        int64
	UID       int64
	Cost      int64
	Earn      int64
	CreatedAt time.Time
}

// UIDCredit 单供应商本批记账增量（§5.2 阶段 C 输入；uid 升序 = 锁序）。
type UIDCredit struct {
	UID         int64
	Earn        int64 // Σ earn（毫分）
	ToFrozen    int64 // 入 chunks 额（freeze_hours>0 时 = Earn；=0 时不写 chunks）
	ToAvailable int64 // 直接入 available 额
}

// DayRecon 源日封账增量（§3.10；维度 = 源行 created_at 的 UTC 日）。
type DayRecon struct {
	UID       int64
	SourceDay time.Time // UTC 日（truncate 到日）
	GrossCost int64
	Earn      int64
	Rows      int64
}

// ThawBucketSnapshot 冻结桶物理观测（spec §7.2/G7）：DAL 直读，零缓存。
type ThawBucketSnapshot struct {
	BucketRows       int64 // supplier_frozen_chunks 总行数
	BucketLagSeconds int64 // 最老未解冻桶 available_at 距 now 秒数（全部未来/空 = 0）
	OverdueRows      int64 // 已到期（available_at <= now）桶行数
	OverdueAmount    int64 // 已到期桶金额合计（millis）
}
