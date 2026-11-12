// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package paymentadmin 支付账户/渠道的**管理面**（OpenAPI tag: payment-admin）。
//
// 路径由生成的 chi-server 提供（`/payment/accounts…`，**不带** /api/admin 前缀），
// 路由由 server.go 挂在 `/api/admin/*` 的**同一鉴权组**内——账户行里有商户私钥，
// 这个面绝不能露出到无鉴权路径。
//
// 与 notify 面的分工：这里回 JSON 信封 + 常规状态码（400/404/409/500/503），
// 一切"供应商语义"（掩码、capabilities、merchant_ref 只读、软停）都在 service
// 里完成；handler 只做契约转换 + 错误码映射（§10-4）。
package paymentadmin

//go:generate go run github.com/oapi-codegen/oapi-codegen/v2/cmd/oapi-codegen@v2.4.1 -generate types,chi-server -include-tags payment-admin -package paymentadmin -o api.gen.go ../../../openapi/openapi.yaml

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
	"github.com/is7qin/c3api/pkg/logx"
)

// API 管理面账户/渠道处理器（实现生成的 ServerInterface）。
type API struct {
	svc *service.Service
	log *logx.Logger
}

// 编译期断言：契约必须覆盖 §10-3 的七条路由。
var _ ServerInterface = (*API)(nil)

// New 构造管理面处理器。
func New(svc *service.Service, log *logx.Logger) *API {
	return &API{svc: svc, log: log}
}

// Router 返回带 /api/admin 前缀的 chi 路由（挂在 adminAuth 组内）。
func (a *API) Router() http.Handler {
	return HandlerWithOptions(a, ChiServerOptions{
		BaseURL: "/api/admin",
		ErrorHandlerFunc: func(w http.ResponseWriter, _ *http.Request, err error) {
			// 路径参数绑定失败：400 + 契约信封（不是纯文本）。
			httpface.WriteErr(w, http.StatusBadRequest, err.Error())
		},
	})
}

// GetPaymentProviders 协议元数据（§18-2 / §10-3）：只回**字段名与标志**，永不回任何
// 配置值——连掩码后的 `***` 也不回（账户级的值只出现在 GetPaymentAccounts 的 config
// 里，那里按 `secret` 掩码）。
//
// 顺序直接用 service 返回的顺序（= `ProviderRegistry.All()` 在构造期定死的 `name`
// 升序），这里**不再排序**：多一处排序就是在"唯一事实来源"之外再造一条排序规则，
// 注册表顺序与端点输出一旦分叉，测试与前端下拉都不再钉得住（N55/N63）。
//
// 该调用**无错误分支**：`ListPaymentProviders` 是全函数（不查 DB、不解析配置、
// nil 注册表 ≡ 空表 → `{"providers":[]}` 200），所以这里没有 service 错误可映射；
// 其余管理面方法仍走 writeErr/httpface.WriteServiceErr 的常规路径。
func (a *API) GetPaymentProviders(w http.ResponseWriter, r *http.Request) {
	views := a.svc.ListPaymentProviders()
	out := ProviderMetadataList{Providers: make([]ProviderMetadata, 0, len(views))}
	for i := range views {
		out.Providers = append(out.Providers, toAPIProviderMetadata(&views[i]))
	}
	httpface.WriteJSON(w, http.StatusOK, out)
}

// GetPaymentAccounts 账户列表（含渠道 / config_revision / config_error / 掩码 config）。
func (a *API) GetPaymentAccounts(w http.ResponseWriter, r *http.Request) {
	views, err := a.svc.ListPaymentAccounts(r.Context())
	if err != nil {
		a.writeErr(w, err)
		return
	}
	out := make([]PaymentAccount, 0, len(views))
	for i := range views {
		out = append(out, toAPIPaymentAccount(&views[i]))
	}
	httpface.WriteJSON(w, http.StatusOK, out)
}

// PostPaymentAccounts 创建账户：`ParseConfig` 校验（非法 → 400）+ 推导 merchant_ref；
// `(provider, merchant_ref)` 或 key 冲突 → 409。
func (a *API) PostPaymentAccounts(w http.ResponseWriter, r *http.Request) {
	var body PostPaymentAccountsJSONRequestBody
	if err := httpface.Decode(r, &body); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	raw, err := marshalConfig(body.Config)
	if err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "config must be a JSON object: "+err.Error())
		return
	}
	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}
	priority := 0
	if body.Priority != nil {
		priority = *body.Priority
	}
	displayName := ""
	if body.DisplayName != nil {
		displayName = *body.DisplayName
	}
	view, err := a.svc.CreatePaymentAccount(r.Context(), service.PaymentAccountInput{
		Key:         body.Key,
		Provider:    domain.ProviderName(body.Provider),
		DisplayName: displayName,
		Enabled:     enabled,
		Priority:    priority,
		Config:      raw,
	})
	if err != nil {
		a.writeErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusCreated, toAPIPaymentAccount(view))
}

// PutPaymentAccountsKey 更新账户（只改 display_name/priority/enabled/config）。
//
// **四个字段全部可选**（契约：缺省 = 不变）：nil 直接透传为 "不改"，所以只想切
// 启停的前端不必回传 display_name（否则一次误传就把展示名覆盖成空串）。`config`
// 缺省 = 不变；密钥字段 `"***"` = 保持原值；新 config 的 merchant_ref 与现列
// 不符 → **400**（N29：改商户身份请新建账户 + 停用旧账户）。
func (a *API) PutPaymentAccountsKey(w http.ResponseWriter, r *http.Request, key string) {
	var body PutPaymentAccountsKeyJSONRequestBody
	if err := httpface.Decode(r, &body); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	// 生成类型本身就是指针：nil = 键缺省（或显式 null）= 不改，原样透传给 service。
	in := service.PaymentAccountUpdateInput{
		DisplayName: body.DisplayName,
		Priority:    body.Priority,
		Enabled:     body.Enabled,
	}
	if body.Config != nil {
		raw, err := marshalConfig(*body.Config)
		if err != nil {
			httpface.WriteErr(w, http.StatusBadRequest, "config must be a JSON object: "+err.Error())
			return
		}
		in.Config = raw
	}
	view, err := a.svc.UpdatePaymentAccount(r.Context(), key, in)
	if err != nil {
		a.writeErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIPaymentAccount(view))
}

// DeletePaymentAccountsKey **恒为软停**（§10-3/N30）：置 enabled=false，行永不删除，
// 幂等 200（重复 DELETE 仍回账户体——运维看到的是"现在确实停着"）。
func (a *API) DeletePaymentAccountsKey(w http.ResponseWriter, r *http.Request, key string) {
	view, err := a.svc.SoftDeletePaymentAccount(r.Context(), key)
	if err != nil {
		a.writeErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIPaymentAccount(view))
}

// PutPaymentAccountsKeyChannelsPayType upsert 单渠道（`pay_type ∈ capabilities`）。
func (a *API) PutPaymentAccountsKeyChannelsPayType(w http.ResponseWriter, r *http.Request, key string, payType string) {
	var body PutPaymentAccountsKeyChannelsPayTypeJSONRequestBody
	if err := httpface.Decode(r, &body); err != nil {
		httpface.WriteErr(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	view, err := a.svc.UpsertPaymentChannel(r.Context(), key, service.PaymentChannelInput{
		PayType:   payType,
		Enabled:   body.Enabled,
		Currency:  body.Currency,
		RateMicro: body.RateMicro,
		MinUSD:    body.MinUsd,
		MaxUSD:    body.MaxUsd,
	})
	if err != nil {
		a.writeErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, toAPIPaymentAccount(view))
}

// DeletePaymentAccountsKeyChannelsPayType 删除单渠道（幂等 204；账户不存在 → 404）。
func (a *API) DeletePaymentAccountsKeyChannelsPayType(w http.ResponseWriter, r *http.Request, key string, payType string) {
	if err := a.svc.DeletePaymentChannel(r.Context(), key, payType); err != nil {
		a.writeErr(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// PostPaymentAccountsKeyTest 签名/连通性自检（§5-4：上线前对备用账户跑一次，
// 是唯一探活手段）。
//
// 实现口径（spec §10-3 标"可选"）：**只验配置与签名**——用账户 cfg 构造一个
// 形状合法的签名 URL（不发网络请求、不落库、不改状态）。查单探活需要真实订单号，
// 那是 `/orders/{id}/sync` 的职责。
func (a *API) PostPaymentAccountsKeyTest(w http.ResponseWriter, r *http.Request, key string) {
	ctx := r.Context()
	view, err := a.svc.ProbePaymentAccount(ctx, key)
	if err != nil {
		a.writeErr(w, err)
		return
	}
	httpface.WriteJSON(w, http.StatusOK, PaymentAccountTestResponse{
		Ok:    view.OK,
		Error: view.Error,
	})
}

// toAPIPaymentAccount 领域视图 → 契约（config 已由 service 掩码；绝不会在这里
// 看到明文密钥——掩码是 service 的唯一职责，handler 不做"顺手脱敏"）。
func toAPIPaymentAccount(v *service.PaymentAccountView) PaymentAccount {
	out := PaymentAccount{
		Key:            v.Account.Key,
		Provider:       string(v.Account.Provider),
		MerchantRef:    v.Account.MerchantRef,
		DisplayName:    v.Account.DisplayName,
		Enabled:        v.Account.Enabled,
		Priority:       v.Account.Priority,
		Config:         v.Config,
		ConfigRevision: v.Account.ConfigRevision,
		CreatedAt:      v.Account.CreatedAt,
		UpdatedAt:      v.Account.UpdatedAt,
		Channels:       make([]PaymentChannel, 0, len(v.Channels)),
	}
	if out.Config == nil {
		out.Config = map[string]any{}
	}
	if v.ConfigError != "" {
		msg := v.ConfigError
		out.ConfigError = &msg
	}
	for _, ch := range v.Channels {
		out.Channels = append(out.Channels, PaymentChannel{
			PayType:   ch.PayType,
			Enabled:   ch.Enabled,
			Currency:  ch.Currency,
			RateMicro: ch.RateMicro,
			MinUsd:    ch.MinUSD,
			MaxUsd:    ch.MaxUSD,
		})
	}
	return out
}

// toAPIProviderMetadata 领域投影 → 契约（§18-2）。
//
// 三条"不许"都在这里：**不排序**（顺序就是协议声明顺序/注册表顺序）、**不补字段**
// （fields 只来自协议自述的 schema）、**不查 DB**（元数据与账户行无关）。`kind` 原样
// 透传字符串：未知值也透传——它只是呈现提示，降级判定在前端（`secret:true` 一律给
// password 控件），服务端不认识就不猜（N43）。
func toAPIProviderMetadata(v *service.PaymentProviderView) ProviderMetadata {
	fields := make([]ProviderConfigField, 0, len(v.Schema.Fields))
	for _, f := range v.Schema.Fields {
		fields = append(fields, ProviderConfigField{
			Name:     f.Name,
			Kind:     string(f.Kind),
			Secret:   f.Secret,
			Required: f.Required,
			Label:    f.Label,
			Example:  f.Example,
		})
	}
	caps := v.Capabilities
	if caps == nil {
		// null ≠ []：前端把 capabilities 当下拉数据源，"没有能力"必须是空数组，
		// 否则 `caps.map(...)` 会在协议侧漏填时炸在渲染里。
		caps = []string{}
	}
	return ProviderMetadata{
		Name:         string(v.Name),
		Capabilities: caps,
		ConfigSchema: ProviderConfigSchema{Fields: fields},
	}
}

// marshalConfig 契约里的 `map[string]interface{}` → 入库的原始 JSON。
func marshalConfig(cfg map[string]interface{}) (json.RawMessage, error) {
	if cfg == nil {
		return nil, errors.New("config is required")
	}
	return json.Marshal(cfg)
}

// writeErr 管理面错误映射（§10-4）：
//
//	400 参数/金额/pay_type 未知/配置非法/merchant_ref 变更
//	404 账户不存在
//	409 key 或 (provider,merchant_ref) 重复
//	503 支付未启用/无可用账户/账户配置不可解析
//	其余 500（不把内部细节回显给管理台）
func (a *API) writeErr(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, repository.ErrMerchantRefImmutable):
		// N29：改商户身份不是并发冲突，是"你写了错误的配置" → 400（§17-10）。
		httpface.WriteErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, service.ErrNotFound):
		httpface.WriteErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, service.ErrInvalidInput):
		httpface.WriteErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, service.ErrConflict):
		httpface.WriteErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, service.ErrUnavailable):
		httpface.WriteErr(w, http.StatusServiceUnavailable, err.Error())
	default:
		if a.log != nil {
			a.log.Error("paymentadmin: request failed", logx.Error(err))
		}
		httpface.WriteErr(w, http.StatusInternalServerError, "internal error")
	}
}
