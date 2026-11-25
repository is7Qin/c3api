// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// routing_attempts.go — 请求链端点（GET /api/admin/ops/attempts/{request_id}，
// spec 2026-11-18 retry-observability §4/§4bis）。
//
// 纯委托层：路径/查询参数解析由生成的 chi wrapper 完成，窗口语义（缺省 ±1d、
// from < to、跨度 ≤ 7d）与三源合并全在 service；错误走 httpface.WriteServiceErr
// 唯一映射表。这里只做两件事：取值 + service → 契约类型的投影。
//
// sampling：注入的是**函数值**（`OpsOptions.AttemptSampling func() int64`），
// nil = 未装配 ⇒ 响应省略 sampling 字段——绝不伪造 dropped_total: 0（那会让
// 「没接计数」看起来像「零丢弃」）。

import (
	"net/http"

	"github.com/is7qin/c3api/internal/httpface"
	"github.com/is7qin/c3api/internal/service"
)

// GetAttemptChain 请求链 — ServerInterface。
func (h *AdminAPI) GetAttemptChain(w http.ResponseWriter, r *http.Request, requestID string, params GetAttemptChainParams) {
	res, err := h.svc.QueryAttemptChain(r.Context(), service.AttemptChainQuery{
		RequestID: requestID,
		From:      params.From,
		To:        params.To,
	})
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIAttemptChain(res, h.attemptChainSampling()))
}

// attemptChainSampling 采样段的读取面：未装配（零值 nil 函数值）⇒ nil（省略字段）。
func (h *AdminAPI) attemptChainSampling() *AttemptChainSampling {
	if h.ops.AttemptSampling == nil {
		return nil
	}
	return &AttemptChainSampling{DroppedTotal: h.ops.AttemptSampling()}
}

// toAPIAttemptChain 请求链投影（service → 契约类型）。hops 恒非 nil（空链 = []）；
// 每跳字段原样透传：ordinal nil 即 JSON null，service 不补 0，投影层也不。
func toAPIAttemptChain(res *service.AttemptChain, sampling *AttemptChainSampling) AttemptChainResponse {
	out := AttemptChainResponse{
		RequestId: res.RequestID,
		From:      res.From,
		To:        res.To,
		Hops:      make([]AttemptChainHop, 0, len(res.Hops)),
		Truncated: res.Truncated,
		Sampling:  sampling,
	}
	for _, hop := range res.Hops {
		// ordinal 是 int16（scheduler.Attempt.Ordinal 的宽度）→ 契约的 int；
		// nil 保持 nil（**不要**折叠成 0：0 是「序号 0」这个不存在的形状）。
		var ordinal *int
		if hop.Ordinal != nil {
			v := int(*hop.Ordinal)
			ordinal = &v
		}
		out.Hops = append(out.Hops, AttemptChainHop{
			Source:         AttemptChainSource(hop.Source),
			Ordinal:        ordinal,
			OrdinalUnknown: hop.OrdinalUnknown,
			AccountId:      hop.AccountID,
			Lane:           hop.Lane,
			Format:         hop.Format,
			StatusCode:     hop.StatusCode,
			Outcome:        hop.Outcome,
			ErrorType:      hop.ErrorType,
			CommitState:    hop.CommitState,
			ErrorMessage:   hop.ErrorMessage,
			LatencyMs:      hop.LatencyMS,
			CreatedAt:      hop.CreatedAt,
		})
	}
	return out
}
