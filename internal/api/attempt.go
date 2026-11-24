// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package api

import (
	"fmt"

	"github.com/is7qin/c3api/internal/domain"
)

// 一次派发的**值契约**（P3c II）：AttemptPlanIdentity / AttemptLane / Attempt 的
// canonical 定义在本包。控制面（internal/scheduler）只留
// `type X = api.X` 别名与三条 lane 常量值别名，不再持真定义
// （机械判据见 type_uniqueness_gate_test.go）。
//
// 为什么归契约层：这四个类型的产生方在控制面（选号、预留、计划推进），消费方在
// 数据面（落日志、判重试、折叠 lane 观测），两头都要看见 ⇒ owner 只能是中立层。
//
// 归属代价，写在这里免得后人当"契约层自由发挥"：Validate 的链式规则
// （ordinal == 1 ⇒ PreviousAttemptID 为 nil；ordinal > 1 ⇒ 必须非 nil）是**调度
// 策略**，不是纯形状检查。策略变化（例如允许探针尝试 ordinal 0）必须改本文件，
// 而且那是**调度语义变更**，不是"清理契约"。本类型不变式的主人仍是 internal/scheduler。
//
// 依赖纪律：本文件只 import fmt + domain，且 domain 用到的是**请求词汇**
// （CallerKind / OperationTag 的闭集合法性），不是路由或调度决策词汇
// （FormatOpenAIChat / RouteClassIDHex 之类）。api 对 domain 是整包白名单，
// 没有符号级门禁 ⇒ 这条是**约定**，破坏它不会变红，只能在评审里守。

type AttemptPlanIdentity struct {
	RequestID         string
	UserID            int64
	RouteClassID      string
	RequestedModel    string
	RoutingGeneration uint64
	// MaxAttempts bounds successful dispatches (1..8). 0 means unset and
	// falls back to the control plane's fixed dedup array bound
	// (domain.MaxFailoverAttempts == 8; the constant itself lives in
	// internal/scheduler, which this package must not import); values >8
	// clamp to 8. The proxy stamps its normalized proxy.failover_attempts here.
	MaxAttempts       uint8
	ApplyModelMapping bool
	AffinityHash      uint64
	HasAffinity       bool
	// Conversation 请求带对话标识且会话簿已装配：预留**不认领**槽位（身份由 SDK
	// 会话簿按对话合成），该位原样写进 Selection.Conversation。零值 = 槽位路径。
	Conversation bool
}

type AttemptLane string

const (
	AttemptLanePrimary  AttemptLane = "primary"
	AttemptLaneExplore  AttemptLane = "explore"
	AttemptLaneDegraded AttemptLane = "degraded"
)

func (l AttemptLane) Valid() bool {
	switch l {
	case AttemptLanePrimary, AttemptLaneExplore, AttemptLaneDegraded:
		return true
	}
	return false
}

type Attempt struct {
	AttemptID            string
	RouteClassID         string
	QualityClassID       string
	CandidateFingerprint string
	TemplateID           int64
	AccountID            int64
	RequestedModel       string
	MappedModel          string
	Lane                 AttemptLane
	Ordinal              uint8
	RoutingGeneration    uint64
	IdentityRevision     int64
	PreviousAttemptID    *string
	PreviousAccountID    *int64
	CallerCategory       string
	OperationTag         string
}

func (a Attempt) Validate() error {
	if a.AttemptID == "" {
		return fmt.Errorf("AttemptID required")
	}
	if a.RouteClassID == "" {
		return fmt.Errorf("RouteClassID required")
	}
	if a.QualityClassID == "" {
		return fmt.Errorf("QualityClassID required")
	}
	if a.CandidateFingerprint == "" {
		return fmt.Errorf("CandidateFingerprint required")
	}
	if a.TemplateID == 0 {
		return fmt.Errorf("TemplateID required")
	}
	if a.AccountID == 0 {
		return fmt.Errorf("AccountID required")
	}
	if a.RequestedModel == "" {
		return fmt.Errorf("RequestedModel required")
	}
	if a.MappedModel == "" {
		return fmt.Errorf("MappedModel required")
	}
	if !a.Lane.Valid() {
		return fmt.Errorf("Lane must be valid")
	}
	if a.Ordinal == 0 {
		return fmt.Errorf("Ordinal must be >0")
	}
	if a.RoutingGeneration == 0 {
		return fmt.Errorf("RoutingGeneration must be >0")
	}
	if a.IdentityRevision <= 0 {
		return fmt.Errorf("IdentityRevision must be >0")
	}
	if a.CallerCategory == "" {
		return fmt.Errorf("CallerCategory required")
	}
	if c := domain.CallerKind(a.CallerCategory); !c.Valid() {
		return fmt.Errorf("CallerCategory invalid %q", a.CallerCategory)
	}
	if a.OperationTag == "" {
		return fmt.Errorf("OperationTag required")
	}
	if op := domain.OperationTag(a.OperationTag); !op.Valid() {
		return fmt.Errorf("OperationTag invalid %q", a.OperationTag)
	}
	if a.Ordinal == 1 && a.PreviousAttemptID != nil {
		return fmt.Errorf("PreviousAttemptID must be nil for ordinal 1")
	}
	if a.Ordinal > 1 {
		if a.PreviousAttemptID == nil || *a.PreviousAttemptID == "" {
			return fmt.Errorf("PreviousAttemptID required for ordinal >1")
		}
	}
	return nil
}
