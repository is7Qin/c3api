// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package user

// payment_methods.go 充值渠道列表（spec 支付网关 v2 §5-2/§10-1）：用户钱包页的
// "充多少 USD → 选渠道"入口。
//
// 渠道可见性由 service 的 `ResolveChannel` **唯一**判定（账户 enabled / 协议已注册
// / cfg 可解析 / 渠道 enabled / 币种白名单 / rate>0 / 金额限额 / priority）——本
// 文件**只做投影**，一行过滤都不写：展示面与下单面必须是同一次判定（M26）。
//
// `rate` 是**参考值**（"当前汇率"），不构成报价承诺；下单响应里的 `pay_amount`
// 才是权威值（§5-3）。

import (
	"net/http"

	"github.com/is7qin/c3api/internal/handler/httpface"
)

// GetUserPaymentMethods 可售方式列表（ServerInterface）。
//
// 返回**空数组**（不是 null、不是 404）当支付未启用或没有可用账户：钱包页据此
// 隐藏"立即充值"，而不是把"没有渠道"误报成服务故障。max_usd 是 service 侧的
// **生效**上限（已按该渠道汇率可表示范围收敛），不是配置面裸值（F46）。
func (h *UserAPI) GetUserPaymentMethods(w http.ResponseWriter, r *http.Request) {
	methods, err := h.svc.PaymentMethods(r.Context())
	if err != nil {
		httpface.WriteServiceErr(w, err)
		return
	}
	out := make([]PaymentMethod, 0, len(methods))
	for _, m := range methods {
		out = append(out, PaymentMethod{
			PayType:  m.PayType,
			Provider: string(m.Provider),
			Currency: m.Currency,
			Rate:     httpface.FormatRateMicro(m.RateMicro),
			MinUsd:   m.MinUSD,
			MaxUsd:   m.MaxUSD,
		})
	}
	httpface.WriteJSON(w, http.StatusOK, out)
}
