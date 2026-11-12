// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestDefaultSettingPricingKeys litellm 价格同步 2 key 内置注册表默认值命中。
func TestDefaultSettingPricingKeys(t *testing.T) {
	src := DefaultSetting("price_source_url")
	require.NotNil(t, src, "price_source_url 内置注册")
	require.Equal(t, SettingTypeString, src.Type)
	require.Equal(t, "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json", src.Value)

	cron := DefaultSetting("price_sync_cron")
	require.NotNil(t, cron, "price_sync_cron 内置注册")
	require.Equal(t, SettingTypeString, cron.Type)
	require.Equal(t, "0 3 * * *", cron.Value)
}

// TestPricingSourceValid PricingSource 合法性校验。
func TestPricingSourceValid(t *testing.T) {
	for _, s := range []PricingSource{PricingSourceLitellm, PricingSourceManual} {
		require.True(t, s.Valid(), "source %s 合法", s)
	}
	require.False(t, PricingSource("scheduled").Valid())
}

// TestMailSettingsRegistry mail.* 8 键内置注册表默认值与类型契约。
func TestMailSettingsRegistry(t *testing.T) {
	cases := []struct {
		key   string
		typ   SettingType
		value string
	}{
		{"mail.enabled", SettingTypeSwitch, "false"},
		{"mail.register_verification", SettingTypeSwitch, "false"},
		{"mail.smtp_host", SettingTypeString, ""},
		{"mail.smtp_port", SettingTypeNumber, "465"},
		{"mail.smtp_username", SettingTypeString, ""},
		{"mail.smtp_password", SettingTypeString, ""},
		{"mail.from_address", SettingTypeString, ""},
		{"mail.tls", SettingTypeString, "implicit"},
	}
	for _, c := range cases {
		s := DefaultSetting(c.key)
		require.NotNil(t, s, "%s 内置注册", c.key)
		require.Equal(t, c.typ, s.Type, "%s 类型", c.key)
		require.Equal(t, c.value, s.Value, "%s 默认值", c.key)
	}
	port := DefaultSetting("mail.smtp_port")
	require.NotNil(t, port.Min)
	require.Equal(t, int64(1), *port.Min)
	require.NotNil(t, port.Max)
	require.Equal(t, int64(65535), *port.Max)
	tls := DefaultSetting("mail.tls")
	require.ElementsMatch(t, []string{"starttls", "implicit", "none"}, tls.PolicyValues)
}

// TestPaymentSettingsRegistry 支付网关注册表（spec 支付网关 §6）：17 个键的类型与
// 默认值必须与 spec 一字不差。值域断言针对三处**会造成资损/免费下单**的地方：
func TestPaymentSettingsRegistry(t *testing.T) {
	// spec §15-7：v2 只留 4 键（删 13、留 4）。键面收敛是"账户是数据"的落地形状：
	// 凭据/汇率/限额搬进 payment_accounts + payment_channels 后，settings 里再出现
	// 这些键就说明有代码在走 v1 的旁路。
	cases := []struct {
		key   string
		typ   SettingType
		value string
	}{
		{SettingPaymentEnabled, SettingTypeSwitch, "false"},
		{SettingNotifyBaseURL, SettingTypeString, ""},
		{SettingPortalBaseURL, SettingTypeString, ""},
		{SettingOrderTTLMinutes, SettingTypeNumber, "30"},
	}
	for _, c := range cases {
		s := DefaultSetting(c.key)
		require.NotNil(t, s, "%s 必须在册", c.key)
		require.Equal(t, c.typ, s.Type, "%s 类型", c.key)
		require.Equal(t, c.value, s.Value, "%s 默认值", c.key)
	}

	// TTL 必须有上下界（0 分钟 = 立即过期，等于把建单功能关掉）。
	ttl := DefaultSetting(SettingOrderTTLMinutes)
	require.NotNil(t, ttl.Min)
	require.Equal(t, int64(1), *ttl.Min)
	require.NotNil(t, ttl.Max)
	require.Equal(t, int64(1440), *ttl.Max)

	// v1 的 13 个键必须**不在**注册表里（否则 spec §15-7 的删除没有落地）。
	for _, key := range []string{
		"payment.epay.enabled", "payment.epay_base_url", "payment.epay_pid",
		"payment.epay_private_key", "payment.epay_public_key",
		"payment.min_usd", "payment.max_usd",
		"payment.alipay.enabled", "payment.alipay.currency", "payment.alipay.rate_micro",
		"payment.wxpay.enabled", "payment.wxpay.currency", "payment.wxpay.rate_micro",
	} {
		require.Nil(t, DefaultSetting(key), "v1 键 %s 必须已从注册表删除", key)
	}
}
