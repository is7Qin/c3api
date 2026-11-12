// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

// payment_providers_test.go 协议元数据端点的**契约**单测（spec v2 §18-2 / §10-3）：
//
//   - P46 形状与顺序：`providers` 覆盖注册表全集、`name` 升序、每项 `capabilities`
//     与非空 `config_schema.fields`，且 `fields` **逐位保持协议声明顺序**（不重排）；
//     空注册表 → `{"providers":[]}`（200）。
//   - P48 只回允许的键：递归遍历响应 JSON 的键集合 ⊆ 白名单；`example` 里的探针串
//     只出现在 `example`；响应不含 `config` 键、不含 `***`。
//   - P49 只在管理面：真实 `PaymentAdminHandler` + 真实 `AdminHandler`（不是 marker）
//     跑路径矩阵——带凭据 200、无凭据 401、未知子路径 404、`/api/admin/orders` 仍归
//     管理面、`/api/payment/providers`（公开面）404。
//
// 三条用例都用**真 `domain.ProviderRegistry` + 假 provider**（注册表是具体类型，
// 不可 fake）：元数据的唯一来源必须是协议实现本身，否则"前端镜像"只是换了个位置。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/auth"
	"github.com/is7qin/c3api/internal/domain"
	paymentadminapi "github.com/is7qin/c3api/internal/handler/paymentadmin"
	"github.com/is7qin/c3api/internal/server"
)

// metaProbe 写在 `example` 里的可识别串：它只允许出现在 `example` 位置（P48）。
const metaProbe = "META_PROBE_9f3a"

// metaAllowedKeys P48 的响应结构白名单（逐键枚举 = 契约的"形状"部分）。
// 注意 `config_schema` 是允许的，**`config`（账户配置）永远不许出现**。
var metaAllowedKeys = map[string]bool{
	"providers":     true,
	"name":          true,
	"capabilities":  true,
	"config_schema": true,
	"fields":        true,
	"kind":          true,
	"secret":        true,
	"required":      true,
	"label":         true,
	"example":       true,
}

// metaProvider 元数据用例的假协议：只有 Name/Capabilities/Schema 有意义，其余方法
// 只为满足 domain.PaymentProvider（这些路径不建单、不收回调）。
type metaProvider struct {
	name string
	caps []string
	// fields 的顺序就是**协议声明顺序**：端点必须原样透传（P46 的 `[zeta, alpha]`）。
	fields []domain.ConfigField
}

func newMetaProvider(name string, caps []string, fields ...domain.ConfigField) *metaProvider {
	return &metaProvider{name: name, caps: caps, fields: fields}
}

func (p *metaProvider) Name() domain.ProviderName { return domain.ProviderName(p.name) }

func (p *metaProvider) Capabilities() []string { return append([]string(nil), p.caps...) }

func (p *metaProvider) ConfigSchema() domain.ConfigSchema {
	return domain.ConfigSchema{Fields: p.fields}
}

func (p *metaProvider) ParseConfig([]byte) (domain.AccountConfig, error) {
	return nil, domain.ErrConfigMissing
}

func (p *metaProvider) BuildPayURL(domain.AccountConfig, domain.PayRequest) (string, error) {
	return "", domain.ErrConfigMissing
}

func (p *metaProvider) VerifyNotify(domain.AccountConfig, url.Values) (domain.NotifyEvent, error) {
	return domain.NotifyEvent{}, domain.ErrConfigMissing
}

// 编译期断言：假协议必须满足协议面（接口一变这里先红，而不是用例里散落报错）。
var _ domain.PaymentProvider = (*metaProvider)(nil)

// TestPaymentProvidersMetadataShapeAndOrder P46：形状、全集、升序、声明顺序、空注册表。
func TestPaymentProvidersMetadataShapeAndOrder(t *testing.T) {
	// 两个协议的注册序故意逆字典序（zeta 先注册）：升序只能来自注册表，不能来自插入序。
	zeta := newMetaProvider("zeta_proto", []string{"alipay", "wxpay"},
		domain.ConfigField{Name: "zeta", Kind: domain.ConfigFieldText, Required: true},
		domain.ConfigField{Name: "alpha", Kind: domain.ConfigFieldPassword, Secret: true, Required: true},
	)
	alpha := newMetaProvider("alpha_proto", []string{"alipay"},
		domain.ConfigField{Name: "base_url", Kind: domain.ConfigFieldURL, Example: "https://pay.example.com"},
	)
	p := newPaymentHarnessBare(t, zeta, alpha)

	rec := p.do(p.adminPay, http.MethodGet, "/api/admin/payment/providers", "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var body paymentadminapi.ProviderMetadataList
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Len(t, body.Providers, 2, "providers 必须覆盖注册表全集")

	require.Equal(t, []string{"alpha_proto", "zeta_proto"}, []string{body.Providers[0].Name, body.Providers[1].Name},
		"providers 必须按 name 升序（序由注册表定死，端点不得重排）")

	for i, got := range body.Providers {
		require.NotEmpty(t, got.Capabilities, "providers[%d].capabilities 不得为空（渠道下拉的唯一来源）", i)
		require.NotEmpty(t, got.ConfigSchema.Fields, "providers[%d].config_schema.fields 不得为空（否则表单画不出框）", i)
	}

	// 逐位比对**协议声明顺序**：`[zeta, alpha]`（逆字典序）不得被服务端排成 `[alpha, zeta]`。
	require.Equal(t, []string{"zeta", "alpha"}, metaFieldNames(body.Providers[1].ConfigSchema.Fields),
		"fields 顺序必须逐位等于协议声明顺序（服务端不得重排）")
	require.Equal(t, []string{"alipay", "wxpay"}, body.Providers[1].Capabilities,
		"capabilities 顺序即协议声明顺序")
	require.False(t, body.Providers[1].ConfigSchema.Fields[0].Secret)
	require.True(t, body.Providers[1].ConfigSchema.Fields[1].Secret, "secret 标志原样透传（掩码的唯一声明口）")
	require.Equal(t, "https://pay.example.com", body.Providers[0].ConfigSchema.Fields[0].Example,
		"example 原样透传（前端 placeholder）")

	// 空注册表 → `{"providers":[]}`（**200**，不是 404）：新部署尚未接协议时管理面不能整页红。
	empty := newPaymentHarnessBare(t)
	rec = empty.do(empty.adminPay, http.MethodGet, "/api/admin/payment/providers", "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, `{"providers":[]}`, strings.TrimSpace(rec.Body.String()),
		"空注册表必须回空数组（null 会让前端 caps/fields 遍历炸在渲染里）")
}

// TestPaymentProvidersResponseKeyWhitelist P48：响应结构白名单 + 探针只落在 example。
func TestPaymentProvidersResponseKeyWhitelist(t *testing.T) {
	probe := newMetaProvider("probe_proto", []string{"alipay"},
		domain.ConfigField{
			Name: "token", Kind: domain.ConfigFieldPassword, Secret: true, Required: true,
			Label: "API Token", Example: metaProbe,
		},
		domain.ConfigField{Name: "base_url", Kind: domain.ConfigFieldURL, Example: "https://pay.example.com"},
	)
	// 真注册表 + 假协议：断言的是"投影只搬字段名与标志"，所以探针串只能出现在 example。
	p := newPaymentHarnessBare(t, probe)

	rec := p.do(p.adminPay, http.MethodGet, "/api/admin/payment/providers", "", "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	raw := rec.Body.String()
	require.NotContains(t, raw, "***", "元数据连掩码后的值也不回（账户值只在 /payment/accounts 的 config 里）")
	require.NotContains(t, raw, `"config"`, "账户配置键不得出现：元数据只有 config_schema（形状），没有 config（值）")

	var doc any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &doc))
	metaAssertKeysAllowed(t, doc, "root")
	metaAssertProbeOnlyInExample(t, doc, "root", "")

	// 反向自检：探针确实在了（否则上面两条"只在 example"的断言会空转成恒绿）。
	found := false
	for _, f := range mustMetaProviders(t, doc)[0].ConfigSchema.Fields {
		if f.Example == metaProbe {
			found = true
		}
	}
	require.True(t, found, "探针必须出现在某个字段的 example 上（否则白名单断言是空转）")
}

// TestPaymentProvidersRoutesOnlyOnAdminSurface P49：真实 handler 的路径矩阵。
//
// 这里不用 marker：`p.adminPay` 是 `paymentadmin.New(svc).Router()`、`p.admin` 是
// `handler.New(svc).Router()`，一起挂在**真实** `server.NewServer`（含 adminAuth）上
// ——挂载/鉴权只有一处实现，测试不去复制它。
func TestPaymentProvidersRoutesOnlyOnAdminSurface(t *testing.T) {
	p := newPaymentHarnessBare(t, newMetaProvider("alpha_proto", []string{"alipay"},
		domain.ConfigField{Name: "base_url", Kind: domain.ConfigFieldURL}))

	// 平台管理员（adminAuth 的两条分支都要求快照 role=platform_admin + 状态 active）。
	// Issuer 的 secret 与 newPaymentHarnessBare 内的一致（同一部署只有一个签名密钥）。
	iss := auth.NewIssuer("test-secret")
	admin, err := p.store.CreateUser(t.Context(), &domain.User{
		Email: "admin@example.com", PasswordHash: "x",
		Role: domain.RolePlatformAdmin, Status: domain.UserStatusActive,
	})
	require.NoError(t, err)
	adminToken, err := iss.Issue(admin.ID, admin.Email, string(domain.RolePlatformAdmin), admin.TokenVersion)
	require.NoError(t, err)

	srv := server.NewServer(server.Options{
		JWTIssuer:           iss,
		Auth:                fakeUserStatus{store: p.store},
		AdminHandler:        p.admin,
		PaymentAdminHandler: p.adminPay,
		PaymentHandler:      p.pay,
	})

	cases := []struct {
		name, path, token string
		want              int
		// wantBody 非空 = 该 200 的 body 必须含此片段（订单面 200 是订单信封，不是元数据信封）。
		wantBody string
	}{
		{"管理面 + 凭据 → 200 且 body 含 providers", "/api/admin/payment/providers", adminToken, http.StatusOK, "providers"},
		{"同路径无凭据 → 401（凭据面绝不露出）", "/api/admin/payment/providers", "", http.StatusUnauthorized, ""},
		{"未知子路径 → 404（不误吞同前缀的其它路径）", "/api/admin/payment/unknown", adminToken, http.StatusNotFound, ""},
		{"订单面仍归 AdminHandler", "/api/admin/orders", adminToken, http.StatusOK, ""},
		{"公开面不认元数据路径 → 404（不进管理面 handler）", "/api/payment/providers", "", http.StatusNotFound, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.token != "" {
				req.Header.Set("Authorization", "Bearer "+tc.token)
			}
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			require.Equal(t, tc.want, rec.Code, rec.Body.String())
			if tc.wantBody != "" {
				require.Contains(t, rec.Body.String(), tc.wantBody,
					"200 必须是元数据信封（不是 marker）：订单面 200 回的是订单信封")
			}
		})
	}
}

// metaFieldNames 抽出字段名序列（逐位比较用）。
func metaFieldNames(fields []paymentadminapi.ProviderConfigField) []string {
	out := make([]string, 0, len(fields))
	for _, f := range fields {
		out = append(out, f.Name)
	}
	return out
}

// mustMetaProviders 把响应 JSON 反序列化成契约类型（白名单用例要回头取字段值）。
func mustMetaProviders(t *testing.T, doc any) []paymentadminapi.ProviderMetadata {
	t.Helper()
	raw, err := json.Marshal(doc)
	require.NoError(t, err)
	var list paymentadminapi.ProviderMetadataList
	require.NoError(t, json.Unmarshal(raw, &list))
	require.NotEmpty(t, list.Providers)
	return list.Providers
}

// metaAssertKeysAllowed 递归遍历响应 JSON，键集合必须 ⊆ 白名单：多一个键就等于把
// 未声明的形状暴露给前端（P48 的判据是结构，不是字段值）。
func metaAssertKeysAllowed(t *testing.T, node any, path string) {
	t.Helper()
	switch v := node.(type) {
	case map[string]any:
		for key, child := range v {
			require.True(t, metaAllowedKeys[key], "响应出现白名单外的键 %q（路径 %s）", key, path)
			metaAssertKeysAllowed(t, child, path+"."+key)
		}
	case []any:
		for i, child := range v {
			metaAssertKeysAllowed(t, child, path+"["+strconv.Itoa(i)+"]")
		}
	}
}

// metaAssertProbeOnlyInExample 探针串只允许作为 `example` 的值出现：出现在别的键
// 下就说明某个配置值（或其片段）漏进了元数据。
func metaAssertProbeOnlyInExample(t *testing.T, node any, path, key string) {
	t.Helper()
	switch v := node.(type) {
	case map[string]any:
		for k, child := range v {
			metaAssertProbeOnlyInExample(t, child, path+"."+k, k)
		}
	case []any:
		for i, child := range v {
			metaAssertProbeOnlyInExample(t, child, path+"["+strconv.Itoa(i)+"]", key)
		}
	case string:
		if strings.Contains(v, metaProbe) {
			require.Equal(t, "example", key, "探针只能出现在 example（实际键 %q，路径 %s）", key, path)
		}
	}
}
