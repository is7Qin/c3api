// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

// 内置设置注册表（类型化配置）：key/type/value 默认值。管理面 PUT 落库覆盖；
// DB 无行即默认（Get 读路径免初始化）。新增内置项 = 在此追加 + 管理面允许列表
// 同步（service.ValidateSetting 用）。
// 数值条目 Min/Max 域（nil = 无限制）：管理面 UpdateSetting 越界 → 400 拒绝
// （A-P2-11 护栏前置，消费端零改动；仅注册表承载，不落库）。PolicyValues 枚举
// 域：字符串条目合法值清单（service 校验从注册表派生，消双处同步，P3-7）。
var DefaultSettings = []Setting{
	{Key: "signup_enabled", Type: SettingTypeSwitch, Value: "true"},
	// 新用户初始资源：公开注册路径应用；管理面 CreateUser 不套默认（显式传值）。
	{Key: "default_user_max_concurrency", Type: SettingTypeNumber, Value: "0", Min: i64p(0)}, // 0 = 不限
	{Key: "default_user_balance", Type: SettingTypeNumber, Value: "0", Min: i64p(0)},         // 最小单位
	{Key: "default_user_temp_balance", Type: SettingTypeNumber, Value: "0", Min: i64p(0)},    // 0 = 不送
	{Key: "default_user_temp_balance_ttl_days", Type: SettingTypeNumber, Value: "30", Min: i64p(0)},
	// litellm 模型价格同步（Phase 5 计费价格来源）：worker 定期拉取 + 管理端手动设价。
	{Key: "price_source_url", Type: SettingTypeString,
		Value: "https://raw.githubusercontent.com/BerriAI/litellm/main/model_prices_and_context_window.json"},
	{Key: "price_sync_cron", Type: SettingTypeString, Value: "0 3 * * *"}, // cron 表达式（gronx 解析）
	// service_tier 转发策略（Phase 5 计费）：priority/flex/fast 请求分别按对应 key
	// 处理转发体——passthrough（默认，原样转发）/ strip（删除该字段）/ reject
	// （400 拒绝，不转发）；auto/空恒透传。PolicyValues 枚举域是值域单一事实源
	// （service.UpdateSetting 校验从注册表派生，见 setting.go）。
	{Key: "service_tier_policy_priority", Type: SettingTypeString, Value: "passthrough",
		PolicyValues: []string{"passthrough", "strip", "reject"}},
	{Key: "service_tier_policy_flex", Type: SettingTypeString, Value: "passthrough",
		PolicyValues: []string{"passthrough", "strip", "reject"}},
	{Key: "service_tier_policy_fast", Type: SettingTypeString, Value: "passthrough",
		PolicyValues: []string{"passthrough", "strip", "reject"}},
	// 集群实例数 N（#14 多实例预算分摊，设计文档 §3.1）：存 DB settings——
	// 所有实例必须读到同一 N（config 文件各实例可漂移，DB 是唯一共识源）；
	// N 变更走 settings NOTIFY 天然传播。service.ClusterInstances 读取，
	// 缺失/非法回退 1（单实例语义）。Min=1：PUT 0 从"接受回退 1"强化为 400
	// 拒绝（护栏前置；除零安全面本就由消费端双守卫覆盖，见 spec 评审）。
	{Key: "cluster.instances", Type: SettingTypeNumber, Value: "1", Min: i64p(1)},
	// 邮件服务（email service）：全部走运行时设置，非 config.toml。
	{Key: "mail.enabled", Type: SettingTypeSwitch, Value: "false"},
	{Key: "mail.register_verification", Type: SettingTypeSwitch, Value: "false"},
	{Key: "mail.smtp_host", Type: SettingTypeString, Value: ""},
	{Key: "mail.smtp_port", Type: SettingTypeNumber, Value: "587", Min: i64p(1), Max: i64p(65535)},
	{Key: "mail.smtp_username", Type: SettingTypeString, Value: ""},
	{Key: "mail.smtp_password", Type: SettingTypeString, Value: ""},
	{Key: "mail.from_address", Type: SettingTypeString, Value: ""},
	{Key: "mail.tls", Type: SettingTypeString, Value: "starttls", PolicyValues: []string{"starttls", "implicit", "none"}},
}

// i64p 注册表数值值域指针辅助（Min/Max 域字面量；nil = 无限制，见 Setting 注释）。
func i64p(v int64) *int64 { return &v }

// DefaultSetting 返回内置 key 的默认设置；未知 key 返回 nil。
func DefaultSetting(key string) *Setting {
	for i := range DefaultSettings {
		if DefaultSettings[i].Key == key {
			s := DefaultSettings[i]
			return &s
		}
	}
	return nil
}
