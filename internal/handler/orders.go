// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// orders.go 支付订单管理面（spec 支付网关 §7-3，tag orders；paths **不带**
// /api/admin 前缀，路由由生成的 chi-server 提供）。
//
// 与 notify 面的关键区别：这里**绝不回 200 fail**。管理台要的是"这次动作成没成"，
// 所以按 §7-4 回 400/404/409/500/502/503；「已付款未入账」这类资损信号额外用
// 502 + `fail_reason` 机读字段表达（§8：sync/close 只与 notify **共用副作用**，
// 不共用网关语义）。

import (
	"bytes"
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
	"github.com/is7qin/c3api/pkg/logx"
)

// GetOrders 订单列表（全量；user_id/status/out_trade_no/created 区间筛选）。
// limit 默认 20、上限 100（service 侧归一，与 §7-3 一致）。
func (h *AdminAPI) GetOrders(w http.ResponseWriter, r *http.Request, params GetOrdersParams) {
	q := repository.OrderQuery{
		UserID:      httpface.Deref(params.UserId),
		OutTradeNo:  httpface.Deref(params.OutTradeNo),
		CreatedFrom: params.CreatedFrom,
		CreatedTo:   params.CreatedTo,
		Limit:       int(httpface.Deref(params.Limit)),
		Offset:      int(httpface.Deref(params.Offset)),
	}
	if params.Status != nil {
		st := domain.OrderStatus(*params.Status)
		if !st.Valid() {
			// 生成器不校验 enum：非法状态必须 400，不能静默当成"不过滤"。
			httpface.WriteErr(w, http.StatusBadRequest, "invalid status")
			return
		}
		q.Status = &st
	}
	rows, total, err := h.svc.ListOrders(r.Context(), q)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]OrderDetail, 0, len(rows))
	for _, o := range rows {
		out = append(out, toOrderDetail(o))
	}
	httpface.WriteJSON(w, http.StatusOK, OrderListResponse{Total: total, Rows: out})
}

// GetOrdersId 订单详情（唯一能看到 raw_notify / fail_* / resolved_at 的面）。
func (h *AdminAPI) GetOrdersId(w http.ResponseWriter, r *http.Request, id int64) {
	o, err := h.svc.GetOrder(r.Context(), id)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toOrderDetail(o))
}

// PostOrdersIdSync 手动查单（§7-3 sync 流程）：非 pending / 无查单能力 → 409；
// 查单故障或入账失败 → 502（订单不变，可人工处置）。
func (h *AdminAPI) PostOrdersIdSync(w http.ResponseWriter, r *http.Request, id int64) {
	res, err := h.svc.SyncOrder(r.Context(), id)
	if err != nil {
		h.writeOrderErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, OrderSyncResponse{
		Status:      OrderStatus(res.Order.Status),
		Credited:    res.Credited,
		AlreadyPaid: res.AlreadyPaid,
		Order:       toOrderDetail(res.Order),
	})
}

// PostOrdersIdClose 关单（§4-2 三态，force 的优先级写死在 service）：
//
//	paid + 入账成功 → 200 {status: paid, credited: true}
//	paid + 入账失败 → 502 + fail_reason（**不关单**，订单保持 pending）
//	非 paid        → 200 {status: closed}（0 行 → 409）
func (h *AdminAPI) PostOrdersIdClose(w http.ResponseWriter, r *http.Request, id int64) {
	force, err := decodeCloseRequest(r)
	if err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	res, err := h.svc.CloseOrder(r.Context(), id, force)
	if err != nil {
		h.writeOrderErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, OrderCloseResponse{
		Status:   OrderStatus(res.Status),
		Credited: res.Credited,
		Order:    toOrderDetail(res.Order),
	})
}

// PostOrdersIdResolve 标记人工处置完成（§7-3 / F48）：写 resolved_at，防止同一
// 订单被补两次（已标记 → 409，B41）。补码本身仍走兑换码生成（created_by=admin）。
func (h *AdminAPI) PostOrdersIdResolve(w http.ResponseWriter, r *http.Request, id int64) {
	o, err := h.svc.ResolveOrder(r.Context(), id)
	if err != nil {
		h.writeOrderErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, OrderResolveResponse{Resolved: true, Order: toOrderDetail(o)})
}

// decodeCloseRequest 解析**可选**请求体（空体 = 不强制）。生成器只对必填体注入
// 参数（可选体连解码都不生成），故这里自己读——DecodeRaw 会把 body 复原给
// httpface.Decode 的严格解码（未知字段 → 400）。
func decodeCloseRequest(r *http.Request) (bool, error) {
	raw, err := httpface.DecodeRaw(r)
	if err != nil {
		return false, err
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return false, nil
	}
	var in OrderCloseRequest
	if err := httpface.Decode(r, &in); err != nil {
		return false, err
	}
	return httpface.Deref(in.Force), nil
}

// writeOrderErr 管理面订单端点的错误映射（§7-4 + §8 的共用副作用）：
//
//	fail_reason ∈ {provider, amount, status, closed, user} → 502 + fail_reason
//	   ——「钱已收/可疑，但这次没入账」，管理台据此提示改用 sync 或人工补账；
//	其余 → 统一表 httpface.WriteServiceErr（400/404/409/500/502/503）。
//
// 顺序不能反：ErrConflict 在 service.OrderFailReason 里归为 internal（因为它对
// notify 面意味着 5xx），但在管理面它必须先是 409（状态冲突）。
func (h *AdminAPI) writeOrderErr(w http.ResponseWriter, err error) {
	reason := service.OrderFailReason(err)
	if !isOrderSignalReason(reason) {
		httpface.WriteServiceErr(w, err)
		return
	}
	if reason == domain.OrderFailReasonClosed && h.log != nil {
		// closed 分支的拒绝必须留 ERROR 日志（§4-2/B11）：这是"已付款未入账"。
		h.log.Error("orders: paid but order is closed (manual follow-up required)",
			logx.String("reason", reason), logx.Error(err))
	}
	httpface.WriteJSON(w, http.StatusBadGateway, map[string]any{
		"error":       err.Error(),
		"fail_reason": reason,
	})
}

// isOrderSignalReason 报告该 fail_reason 是否属于"可判定但没入账"的信号族
// （internal 不算：它要么已由 ErrConflict/ErrUpstream 映射成 409/502，要么是
// 真内部错误 → 500）。
func isOrderSignalReason(reason string) bool {
	switch reason {
	case domain.OrderFailReasonProvider,
		domain.OrderFailReasonAmount,
		domain.OrderFailReasonStatus,
		domain.OrderFailReasonClosed,
		domain.OrderFailReasonUser:
		return true
	}
	return false
}

// toOrderDetail 领域订单 → 管理面契约（金额全部走定点字符串，整数账本字段并存）。
func toOrderDetail(o *domain.Order) OrderDetail {
	return OrderDetail{
		Id:               o.ID,
		OrderNo:          o.OutTradeNo,
		UserId:           o.UserID,
		Provider:         string(o.Provider),
		AccountKey:       o.AccountKey,
		PayType:          o.PayType,
		Status:           OrderStatus(o.Status),
		AmountUsd:        httpface.FormatUSDFromMilli(o.CreditMilli),
		PayAmount:        httpface.FormatMinor(o.PayMinor),
		Currency:         o.Currency,
		Rate:             httpface.FormatRateMicro(o.RateMicro),
		CreditMilli:      o.CreditMilli,
		PayMinor:         o.PayMinor,
		TradeNo:          o.TradeNo,
		RedemptionCodeId: o.RedemptionCodeID,
		RawNotify:        o.RawNotify,
		FailReason:       o.FailReason,
		FailCount:        o.FailCount,
		LastNotifyAt:     o.LastNotifyAt,
		ResolvedAt:       o.ResolvedAt,
		CreatedAt:        o.CreatedAt,
		PaidAt:           o.PaidAt,
		ExpiresAt:        o.ExpiresAt,
	}
}
