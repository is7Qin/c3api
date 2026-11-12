// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

// orders.go 充值订单用户面（spec 支付网关 §7-1）：创建 → 跳网关；列表/详情。
//
// 两条越权与泄露纪律：
//  1. **归属在 handler 判**（service 不猜身份）：列表强制 q.UserID = 当前用户，
//     详情非本人一律 404（不泄露"这个 id 存在"）。
//  2. fail_reason / raw_notify / credit_milli 等运维字段**绝不出现在用户面**：
//     用户只需要"付多少 / 到没到账 / 还能不能付"。

import (
	"context"
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
	"github.com/is7qin/c3api/pkg/logx"
)

// PostUserOrders 创建充值订单（§4-8 六步全在 service）：400 参数/金额非法、
// 503 支付未启用或渠道未配置。
func (h *UserAPI) PostUserOrders(w http.ResponseWriter, r *http.Request) {
	var in CreateOrderRequest
	if err := httpface.Decode(r, &in); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	res, err := h.svc.CreateOrder(r.Context(), currentUserID(r), service.CreateOrderInput{
		AmountUSD: in.AmountUsd,
		PayType:   in.PayType,
	})
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, CreateOrderResponse{
		OrderNo:   res.Order.OutTradeNo,
		PayUrl:    res.PayURL,
		Account:   res.Order.AccountKey,
		AmountUsd: httpface.FormatUSDFromMilli(res.Order.CreditMilli),
		PayAmount: httpface.FormatMinor(res.Order.PayMinor),
		Currency:  res.Order.Currency,
		Rate:      httpface.FormatRateMicro(res.Order.RateMicro),
		ExpiresAt: res.Order.ExpiresAt,
	})
}

// GetUserOrders 我的订单（分页 + status；强制 user_id = 当前用户）。
func (h *UserAPI) GetUserOrders(w http.ResponseWriter, r *http.Request, params GetUserOrdersParams) {
	lq, err := pageToQuery(params.Page, params.PageSize)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	q := repository.OrderQuery{
		UserID: currentUserID(r), // 越权的唯一防线：不从请求参数取 user_id
		Limit:  lq.Limit,
		Offset: lq.Offset,
	}
	if params.Status != nil {
		st := domain.OrderStatus(*params.Status)
		if !st.Valid() {
			// 枚举未命中（生成器不校验 enum）：400 而不是静默忽略筛选条件。
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
	out := make([]UserOrder, 0, len(rows))
	for _, o := range rows {
		out = append(out, h.userOrder(r.Context(), o, true))
	}
	httpface.WriteJSON(w, http.StatusOK, UserOrderListResponse{Total: total, Rows: out})
}

// GetUserOrdersId 我的订单详情（非本人 → 404；pending 且未过期时返回实时重签的
// pay_url）。
func (h *UserAPI) GetUserOrdersId(w http.ResponseWriter, r *http.Request, id int64) {
	o, err := h.svc.GetOrder(r.Context(), id)
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	if o.UserID != currentUserID(r) {
		// 别人的订单一律 404（与"不存在"同形，不泄露存在性）。
		httpface.WriteErr(w, http.StatusNotFound, "order not found")
		return
	}
	httpface.WriteJSON(w, http.StatusOK, h.userOrder(r.Context(), o, false))
}

// userOrder 领域订单 → 用户面契约。pay_url 只在 pending 且未过期时**实时重签**
// （订单表没有 pay_url 列）；签名不可用（配置缺失等）→ null 并 Warn：读面不该
// 因为"暂时不能付"整页 5xx——列表与详情仍然有价值。
//
// withAccount：列表返回 account（用户选"我刚付给哪家"），详情**不返回**
// （spec §10-1：用户详情不外泄 account_key）。
func (h *UserAPI) userOrder(ctx context.Context, o *domain.Order, withAccount bool) UserOrder {
	out := UserOrder{
		Id:        o.ID,
		OrderNo:   o.OutTradeNo,
		Status:    OrderStatus(o.Status),
		AmountUsd: httpface.FormatUSDFromMilli(o.CreditMilli),
		PayAmount: httpface.FormatMinor(o.PayMinor),
		Currency:  o.Currency,
		Rate:      httpface.FormatRateMicro(o.RateMicro),
		PayType:   o.PayType,
		Provider:  string(o.Provider),
		CreatedAt: o.CreatedAt,
		ExpiresAt: o.ExpiresAt,
		PaidAt:    o.PaidAt,
	}
	if withAccount {
		key := o.AccountKey
		out.Account = &key
	}
	if o.Status != domain.OrderStatusPending || !h.now().Before(o.ExpiresAt) {
		return out
	}
	payURL, err := h.svc.OrderPayURL(ctx, o)
	if err != nil {
		if h.log != nil {
			h.log.Warn("user: re-sign order pay url",
				logx.String("out_trade_no", o.OutTradeNo), logx.Error(err))
		}
		return out
	}
	out.PayUrl = &payURL
	return out
}
