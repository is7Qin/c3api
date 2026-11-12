// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package payment 实现支付网关的**公开回调面**（OpenAPI tag: payment 的独立
// ServerInterface）：GET /api/payment/{account}/notify 与 .../return。
//
// 这个面没有身份鉴权（网关不会带 JWT），所以它是全仓唯一由陌生方直接驱动的写
// 入口。设计上只有三条允许的来源：账户行（路径即账户）、注册表里的协议实现
// （真伪判定）、service 的 CompleteOrder（唯一入账路径）——加上本文件里的
// **独立错误映射表**（spec §8）。
//
// 三条与其它面不同的纪律：
//  1. **不复用** httpface.WriteServiceErr：那张表把 ErrConflict 映射成 409、
//     ErrNotFound 映射成 404，而网关只对 5xx 重试——用错表 = 收了钱不再重试 =
//     资损（F39）。
//  2. 响应体是裸文本 success/fail，**不是** JSON 信封（网关按文本判定）。
//  3. 可判定输入错误（验签/重复键/必填缺失/pid/未知单号）回 200 fail 且**不写**
//     fail_reason：单号还没被证明属于我们，用它落库会让伪造签名污染任意订单。
package payment

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/service"
	"github.com/is7qin/c3api/pkg/logx"
)

// 网关响应文本（§5-4/§8）：只有这两个字面量，没有第三个。
const (
	notifySuccess = "success"
	notifyFail    = "fail"
)

// API 公开回调面处理器（实现生成的 ServerInterface）。
type API struct {
	svc *service.Service
	// log 面日志（nil = 静默）。只有 5xx 与「钱已收但没入账」类信号值得落日志。
	log *logx.Logger
}

// New 构造公开回调面处理器（协议与账户都由 service 提供：v2 的协议注册表与
// 配置缓存都在 service 内，handler 不再持有第二份装配——两份装配一定会分叉）。
func New(svc *service.Service, log *logx.Logger) *API {
	return &API{svc: svc, log: log}
}

// Router 组装 /api/payment/* 路由（spec 路径自带 /api/payment 前缀，故无
// BaseURL；**不挂鉴权中间件**——网关无 JWT，由 server.go 挂在非 admin 组）。
func Router(svc *service.Service, log *logx.Logger) http.Handler {
	return Mount(New(svc, log))
}

// Mount 把已构造的 API 挂上生成路由（导出：测试可先构造 API 再挂载，
// 与 user.Mount / handler.Router 同款分层）。
func Mount(api *API) http.Handler {
	return HandlerWithOptions(api, ChiServerOptions{
		ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			// 路径参数绑定失败（account 段永远是 string，实际不可达）也不许回
			// JSON：网关只认 success/fail。
			if api.log != nil {
				api.log.Warn("payment: bind request param", logx.Error(err))
			}
			writeText(w, http.StatusBadRequest, notifyFail)
		},
	})
}

// PaymentNotify 异步通知——**10 步契约**（spec §6-2，顺序即规范）：
//
//	 1 按路径 account key 加载账户（不存在 → 404）
//	 2 解析/取缓存配置（失败 → 503）
//	 3 取协议实现（未注册 → 503 + ERROR）
//	 4 用该账户 cfg 验签（失败 → 200 fail，不写审计）
//	 5 重复键/值字符/必填存在性（失败 → 200 fail，不写审计）
//	 6 提取 out_trade_no / money / trade_status
//	 7 锁订单（FOR UPDATE；不存在 → 200 fail，不写审计）
//	 8 校验 订单.account_key == 路径 account（不符 → 200 fail + fail_reason=account）
//	 9 校验 订单.provider == 账户.provider（不符 → 200 fail + fail_reason=provider）
//	10 幂等分支（paid → success）→ 状态/金额/币种 → 入账
//
// 4/5 由协议实现一次完成（`VerifyNotify` 拿到的就是原始 url.Values，任何一步失败
// 都不返回半成品事件）；7–10 由 `CompleteOrder` 在**同一个事务**里完成。
// 8/9 在 10 的幂等分支之前——否则已 paid 的订单收到错账户的通知会被幂等分支
// 吞掉、回 success（M23 的资损形态）。
func (h *API) PaymentNotify(w http.ResponseWriter, r *http.Request, account string) {
	ctx := r.Context()

	// ① 账户：不存在 → 404（这个 URL 从来不属于任何账户）。
	acct, err := h.svc.GetPaymentAccount(ctx, account)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) || errors.Is(err, service.ErrInvalidInput) {
			if h.log != nil {
				h.log.Warn("payment: notify for unknown account", logx.String("account", account))
			}
			writeText(w, http.StatusNotFound, notifyFail)
			return
		}
		// DB 故障不是"这个账户不存在"：503 让网关重试（404 会让它放弃）。
		if h.log != nil {
			h.log.Error("payment: load payment account",
				logx.String("account", account), logx.Error(err))
		}
		writeText(w, http.StatusServiceUnavailable, notifyFail)
		return
	}

	// ② 配置（enabled=false **照常处理**：钱可能已经收了；只有 cfg 坏才 503）。
	cfg, err := h.svc.GetPaymentAccountConfig(ctx, acct)
	if err != nil {
		if h.log != nil {
			// 协议未注册属于第 ③ 步的语义，但第 ② 步在无协议时无法解析配置——
			// 两者都是 503，日志里区分开便于排障。
			h.log.Error("payment: notify config unavailable",
				logx.String("account", account), logx.String("provider", string(acct.Provider)),
				logx.Error(err))
		}
		writeText(w, http.StatusServiceUnavailable, notifyFail)
		return
	}

	// ③ 协议：未注册 → 503 + ERROR（下线协议只允许 enabled=false；实现删了历史
	// 迟到回调就永远验不了签，所以这里按"可修复的装配问题"处理）。
	p, ok := h.svc.PaymentProvider(acct.Provider)
	if !ok {
		if h.log != nil {
			h.log.Error("payment: notify for unregistered provider",
				logx.String("account", account), logx.String("provider", string(acct.Provider)))
		}
		writeText(w, http.StatusServiceUnavailable, notifyFail)
		return
	}

	// ④⑤ 重复键 / 必填存在性 / sign_type 硬校验 / 验签 / pid / 状态 / 金额解析。
	params := r.URL.Query()
	ev, err := p.VerifyNotify(cfg, params)
	if err != nil {
		h.verifyErr(w, ctx, account, ev, err)
		return
	}

	// ⑥ 提取字段（ev 已归一：OutTradeNo / TradeNo / PaidMinor / Status / Raw）。
	// ⑦–⑩ 锁订单 → 账户/协议一致 → 幂等/金额/入账（单事务）。
	if _, err := h.svc.CompleteOrder(ctx, service.CompleteInput{
		Provider:     acct.Provider, // ← 账户行，绝不从订单取（M18）
		AccountKey:   acct.Key,      // ← 路径段（第 ⑧ 步的比对基准）
		OutTradeNo:   ev.OutTradeNo,
		TradeNo:      ev.TradeNo,
		MoneyMinor:   ev.PaidMinor,
		PaidCurrency: ev.PaidCurrency, // 空 = 上游没说，service 用订单快照
		Status:       ev.Status,
		Timestamp:    ev.Timestamp,
		RawNotify:    service.SanitizeRawNotify(ev.Raw),
	}); err != nil {
		h.completeErr(w, ctx, ev, err)
		return
	}
	writeText(w, http.StatusOK, notifySuccess)
}

// verifyErr 验签族的错误映射（§8）：
//
//	cfg 不可用/跨协议误用 → 503 fail（让网关重试；不写审计）
//	验签通过后的失败（如 money 不可解析，ev 已回填单号）→ 200 fail + 审计
//	其余（重复键/值字符/必填缺失/sign_type/验签失败/pid 不符）→ 200 fail，不写审计
func (h *API) verifyErr(w http.ResponseWriter, ctx context.Context, account string, ev domain.NotifyEvent, err error) {
	switch {
	case errors.Is(err, domain.ErrConfigMissing), errors.Is(err, domain.ErrConfigProviderMismatch):
		if h.log != nil {
			h.log.Error("payment: notify verify config unusable",
				logx.String("account", account), logx.Error(err))
		}
		writeText(w, http.StatusServiceUnavailable, notifyFail)
		return
	}
	if ev.OutTradeNo != "" {
		// 单号已被验签 + pid 证明属于我们：此刻的失败**必须**留审计（§16/L8）。
		h.rejectNotify(w, ctx, ev, postVerifyReason(err), http.StatusOK)
		return
	}
	if h.log != nil {
		h.log.Debug("payment: notify rejected",
			logx.String("account", account), logx.Error(err))
	}
	writeText(w, http.StatusOK, notifyFail)
}

// PaymentReturn 同步跳转回门户（§6-1）：只校验单号**形状**与账户存在性，不落账、
// 不改状态、不查订单（跳转不是凭据，真伪由 notify 决定）。
func (h *API) PaymentReturn(w http.ResponseWriter, r *http.Request, account string) {
	ctx := r.Context()
	// 账户不存在 → 404：与 notify 同形（这个 URL 从来不属于任何账户）。
	if _, err := h.svc.GetPaymentAccount(ctx, account); err != nil {
		if errors.Is(err, service.ErrNotFound) || errors.Is(err, service.ErrInvalidInput) {
			writeText(w, http.StatusNotFound, notifyFail)
			return
		}
		writeText(w, http.StatusServiceUnavailable, notifyFail)
		return
	}
	order := r.URL.Query().Get("order")
	if !domain.ValidOutTradeNo(order) {
		// 形状不合法 = 不是本系统产生的单号：拒绝跳转，别把用户带回一个假单页。
		writeText(w, http.StatusBadRequest, "invalid order")
		return
	}
	target := "/user/wallet?order=" + url.QueryEscape(order)
	if base := h.svc.PortalBaseURL(); base != "" {
		target = base + target
	}
	http.Redirect(w, r, target, http.StatusFound)
}

// completeErr 完成订单失败的**独立**映射（spec §8 表；不复用
// httpface.WriteServiceErr）。只有两个出口族：
//
//	可判定/资损信号 → 200 fail + fail_reason（网关不重试，人工处置）
//	内部错误       → 500 fail + fail_reason=internal（网关重试）
func (h *API) completeErr(w http.ResponseWriter, ctx context.Context, ev domain.NotifyEvent, err error) {
	switch {
	case errors.Is(err, domain.ErrOrderNotFound):
		// 未知单号：200 fail，**不写** fail_reason（记录它等于让伪造签名污染任意订单）。
		if h.log != nil {
			h.log.Warn("payment: notify for unknown out_trade_no",
				logx.String("out_trade_no", ev.OutTradeNo))
		}
		writeText(w, http.StatusOK, notifyFail)
		return
	case errors.Is(err, domain.ErrAccountMismatch):
		// 第 ⑧ 步：等额同币也可能来自另一家账户 → 只落审计，不入账（P32/B42）。
		if h.log != nil {
			h.log.Warn("payment: notify account mismatch",
				logx.String("out_trade_no", ev.OutTradeNo), logx.Error(err))
		}
		h.rejectNotify(w, ctx, ev, domain.OrderFailReasonAccount, http.StatusOK)
		return
	case errors.Is(err, service.ErrProviderMismatch):
		// 第 ⑨ 步：回调账户的协议与订单快照不符。
		h.rejectNotify(w, ctx, ev, domain.OrderFailReasonProvider, http.StatusOK)
		return
	case errors.Is(err, service.ErrAmountMismatch):
		h.rejectNotify(w, ctx, ev, domain.OrderFailReasonAmount, http.StatusOK)
		return
	case errors.Is(err, service.ErrStatusNotPaid):
		h.rejectNotify(w, ctx, ev, domain.OrderFailReasonStatus, http.StatusOK)
		return
	case errors.Is(err, service.ErrOrderClosed):
		// 已关单却收到成功通知：资损信号，ERROR 日志 + fail_reason=closed（P10/B11）。
		if h.log != nil {
			h.log.Error("payment: paid notify for closed order (manual follow-up required)",
				logx.String("out_trade_no", ev.OutTradeNo), logx.String("trade_no", ev.TradeNo))
		}
		h.rejectNotify(w, ctx, ev, domain.OrderFailReasonClosed, http.StatusOK)
		return
	case errors.Is(err, domain.ErrUserUnavailable):
		// 用户被禁/被删：钱已收、不入账 → 必须留信号（解禁后补码并 resolve）。
		if h.log != nil {
			h.log.Warn("payment: notify for unavailable user",
				logx.String("out_trade_no", ev.OutTradeNo), logx.Error(err))
		}
		h.rejectNotify(w, ctx, ev, domain.OrderFailReasonUser, http.StatusOK)
		return
	}

	// 其余一律按 service 的分类表落信号：internal（码冲突/死锁重试耗尽/DB/未知
	// 状态）是 500 fail——**唯一**让网关重试的出口。未归类错误 fail-closed 同样
	// 按 internal 处理（绝不 200 吞掉）。
	reason := service.OrderFailReason(err)
	if reason == "" || reason == domain.OrderFailReasonInternal {
		if h.log != nil {
			h.log.Error("payment: notify internal error",
				logx.String("out_trade_no", ev.OutTradeNo), logx.Error(err))
		}
		h.rejectNotify(w, ctx, ev, domain.OrderFailReasonInternal, http.StatusInternalServerError)
		return
	}
	h.rejectNotify(w, ctx, ev, reason, http.StatusOK)
}

// postVerifyReason 验签通过后失败的 fail_reason（§8）：协议实现通过
// `FailReason() string` 声明（当前是 amount；见 epay 包的 notifyRejection）。
//
// 用**消费者侧接口**而不是 import 具体协议：回调面只依赖 domain.PaymentProvider，
// 换协议（§14）不需要动这里。未声明原因时按唯一的回填类兜底——能走到这条分支
// 说明单号已验签，而 §5-4 第 8 步（金额）是当前唯一可能失败的步骤。
func postVerifyReason(err error) string {
	var fr interface{ FailReason() string }
	if errors.As(err, &fr) {
		if reason := fr.FailReason(); reason != "" {
			return reason
		}
	}
	return domain.OrderFailReasonAmount
}

// rejectNotify 落一次失败审计并回 fail（审计失败只告警，不影响响应语义）。
func (h *API) rejectNotify(w http.ResponseWriter, ctx context.Context, ev domain.NotifyEvent, reason string, status int) {
	h.svc.RecordNotifyFailure(ctx, ev.OutTradeNo, reason)
	if h.log != nil && reason != domain.OrderFailReasonClosed {
		// closed 已在调用点打过 ERROR；其余在这里留一条 Warn 便于串联审计。
		h.log.Warn("payment: notify failed",
			logx.String("out_trade_no", ev.OutTradeNo),
			logx.String("reason", reason),
			logx.Int("status", status))
	}
	writeText(w, status, notifyFail)
}

// writeText 写裸文本响应（网关只认文本，不认 JSON 信封）。
func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// 编译期断言：生成的契约必须有这两条路由，少一条都说明 openapi tag 掉了。
var _ ServerInterface = (*API)(nil)
