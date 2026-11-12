// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// payment_registry_nil_test.go P54：**nil registry ≡ 空注册表**（spec v2 §4 / N61）。
//
// 为什么要钉这条：`PaymentRegistry` 从 `[]PaymentProvider` 换成 `*domain.ProviderRegistry`
// 之后，"支付未装配"（methods 空、建单 503）是**受支持状态**，不是装配错误。基线里
// 大量 `&Service{...}` 字面量不带协议，一旦 nil 变成解引用，这些路径会从"安全返回
// 未命中"变成 panic（而且是运行期、在请求线程里）。
//
// 断言分两半：
//   - `service.New` 收到 nil 不 panic，且三种出口与"空注册表"**逐项相同**；
//   - `domain.ProviderRegistry` 的 nil 接收者安全（Get → (nil,false)、All → nil），
//     含显式 `&ProviderRegistry{}` 零值（空 map/空切片）——那是"空注册表"的另一个形态。

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/settingssnap"
)

// registryNilPayType 已种渠道行的支付方式：有了它，选路的失败原因才是
// `ErrNoEligibleAccount`（503），而不是"任何账户都没有该 pay_type"（400）。
const registryNilPayType = "alipay"

// newRegistryNilService 用真实 `service.New(Deps{...})` 装配：只有注册表不同
// （nil vs 空注册表），其余依赖同一套替身——两个实例因此可以直接等价比较。
//
// 账户行**已种好**且带一条启用渠道：它证明"空注册表下 methods 为空、建单 503"
// 来自**协议未注册**（选路过滤 ②），而不是"根本没有账户行"这种更弱的形状。
func newRegistryNilService(t *testing.T, registry *domain.ProviderRegistry) *Service {
	t.Helper()
	ctx := context.Background()
	fs := newFakeStore()
	// 支付开关打开：methods 才不会被 §5-1b 的"销售面关闭"提前短路成空列表。
	_, err := fs.SetSetting(ctx, domain.SettingPaymentEnabled, domain.SettingTypeSwitch, "true")
	require.NoError(t, err)
	_, err = fs.SetSetting(ctx, domain.SettingNotifyBaseURL, domain.SettingTypeString, "https://api.example.com")
	require.NoError(t, err)
	snap := settingssnap.New(fs, nil)
	require.NoError(t, snap.Load(ctx))

	store := &paymentAccountStore{
		fakeStore: fs,
		accounts: []*domain.PaymentAccount{{
			ID: 1, Key: "ghost_account",
			// 这个协议名**不在**任何注册表里（尤其不在空注册表里）。
			Provider: "ghost_proto", MerchantRef: "merchant-1",
			DisplayName: "ghost", Enabled: true, Priority: 0,
			Config: json.RawMessage(`{"token":"tok"}`), ConfigRevision: 1,
		}},
		channels: map[int64][]*domain.PaymentChannel{
			1: {{AccountID: 1, PayType: registryNilPayType, Enabled: true,
				Currency: "CNY", RateMicro: 7_200_000, MinUSD: 1, MaxUSD: 1000}},
		},
	}
	// Scheduler / Auth 不装配：支付读路径不触达它们（nil 在 Deps 里是受支持状态）。
	return New(Deps{
		Store: store, Invalidate: NopInvalidator{},
		EmailCodeStore: fs, SettingsSnapshot: snap,
		PaymentRegistry: registry,
	})
}

// TestNilPaymentRegistryEqualsEmptyRegistry P54：nil 注册表不 panic，且三种出口与
// 空注册表逐项相同；domain 侧的 nil 接收者与零值注册表同样安全。
func TestNilPaymentRegistryEqualsEmptyRegistry(t *testing.T) {
	ctx := context.Background()

	emptyRegistry, err := domain.NewProviderRegistry(nil)
	require.NoError(t, err, "NewProviderRegistry(nil) 必须是**合法**空注册表（N58/N61）")
	require.NotNil(t, emptyRegistry)

	// 构造本身就是第一条断言：service.New 收到 nil 不 panic（否则本行之前就炸）。
	nilSvc := newRegistryNilService(t, nil)
	emptySvc := newRegistryNilService(t, emptyRegistry)

	// 空注册表下协议清单必须为空（不是 nil：管理面要回 {"providers":[]}）。
	require.NotNil(t, nilSvc.ListPaymentProviders())
	require.Empty(t, nilSvc.ListPaymentProviders())
	require.Equal(t, nilSvc.ListPaymentProviders(), emptySvc.ListPaymentProviders(),
		"nil registry 与空注册表必须是同一语义")

	// methods → 空 []（不是 nil：前端会遍历它）。
	methods, err := nilSvc.PaymentMethods(ctx)
	require.NoError(t, err)
	require.NotNil(t, methods, "空可售集必须是空切片（nil 会让前端渲染路径炸在遍历里）")
	require.Empty(t, methods)
	emptyMethods, err := emptySvc.PaymentMethods(ctx)
	require.NoError(t, err)
	require.Equal(t, methods, emptyMethods)

	// 建单 → 503 语义（ErrUnavailable）：协议未注册 ⇒ 选路过滤掉唯一账户。
	_, nilErr := nilSvc.CreateOrder(ctx, 1, CreateOrderInput{PayType: registryNilPayType, AmountUSD: "10"})
	require.ErrorIs(t, nilErr, ErrUnavailable, "空注册表下建单必须是 503 语义（不是 400/500）")
	_, emptyErr := emptySvc.CreateOrder(ctx, 1, CreateOrderInput{PayType: registryNilPayType, AmountUSD: "10"})
	require.Equal(t, nilErr.Error(), emptyErr.Error(), "nil 与空注册表的失败原因必须一致")

	// domain 侧：nil 接收者安全。
	var nilRegistry *domain.ProviderRegistry
	p, ok := nilRegistry.Get("anything")
	require.False(t, ok)
	require.Nil(t, p)
	require.Nil(t, nilRegistry.All(), "nil 接收者的 All() 返回 nil（不是 panic）")

	// 显式零值（&ProviderRegistry{}）：空 map / 空切片，同样 nil-safe。
	zero := &domain.ProviderRegistry{}
	p, ok = zero.Get("anything")
	require.False(t, ok)
	require.Nil(t, p)
	require.Nil(t, zero.All())
}
