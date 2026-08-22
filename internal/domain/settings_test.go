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
		{"mail.smtp_port", SettingTypeNumber, "587"},
		{"mail.smtp_username", SettingTypeString, ""},
		{"mail.smtp_password", SettingTypeString, ""},
		{"mail.from_address", SettingTypeString, ""},
		{"mail.tls", SettingTypeString, "starttls"},
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
