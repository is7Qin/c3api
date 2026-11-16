// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/settingssnap"
	"github.com/is7qin/c3api/pkg/logx"
)

// GetSettings 全部设置（默认值 + DB 覆盖；/api/admin/settings GET）。
func (s *Service) GetSettings(ctx context.Context) ([]*domain.Setting, error) {
	return s.store.GetAllSettings(ctx)
}

// serviceTierPolicyKeys service_tier 转发策略 key → 值域（从注册表
// PolicyValues 枚举域派生，消双处同步——注册表是唯一事实源，新增策略 key 只改
// 注册表一处，此处随派生自动跟随；非法值 → 400，见 UpdateSetting）。
var serviceTierPolicyKeys = func() map[string][]string {
	m := make(map[string][]string, 3)
	for _, d := range domain.DefaultSettings {
		if len(d.PolicyValues) > 0 {
			m[d.Key] = d.PolicyValues
		}
	}
	return m
}()

// UpdateSetting 类型化校验后更新（/api/admin/settings PUT）：
// key ∈ 内置注册表（未知 key → 400）；switch 必须 true/false；number 必须
// 数字且落在注册表 Min/Max 值域内（负值/越界 → 400）；带 PolicyValues 枚举
// 域的条目（service_tier_policy_*）必须命中枚举。更新成功后同步内存快照——
// 注册等读路径即时生效；本地即时重算走统一去抖通道（inv.Settings：auth 快照
// 全量 Reload，gate 预算按新 N 重算）+ NOTIFY 广播其余实例。
func (s *Service) UpdateSetting(ctx context.Context, key, value string) (*domain.Setting, error) {
	def := domain.DefaultSetting(key)
	if def == nil {
		return nil, ErrInvalidInput
	}
	switch def.Type {
	case domain.SettingTypeSwitch:
		if value != "true" && value != "false" {
			return nil, ErrInvalidInput
		}
	case domain.SettingTypeNumber:
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return nil, ErrInvalidInput
		}
		// 值域护栏：注册表 Min/Max 是单一事实源——越界 → 400，
		// 与管理面 CreateUser/UpdateUser 负值拒绝语义一致；消费端零改动。
		if def.Min != nil && n < *def.Min {
			return nil, ErrInvalidInput
		}
		if def.Max != nil && n > *def.Max {
			return nil, ErrInvalidInput
		}
	}
	if vals, ok := serviceTierPolicyKeys[key]; ok && !slices.Contains(vals, value) {
		return nil, ErrInvalidInput
	}
	// mail 依赖约束（fail-fast，无静默联动）：
	// register_verification 开启时要求 enabled=true 且 smtp_host/from_address 非空；
	// 关闭 enabled 时若 verif 仍为开同样拒绝。
	effective := func(k string) string {
		if k == key {
			return value
		}
		return s.settingValue(k)
	}
	if effective("mail.register_verification") == "true" {
		if effective("mail.enabled") != "true" || effective("mail.smtp_host") == "" || effective("mail.from_address") == "" {
			return nil, ErrInvalidInput
		}
	}
	// custom_pages 是全仓唯一一条 JSON 承载的 string 设置：写入前按
	// Parse → Normalize → Validate → Marshal（spec §4.5 唯一顺序）处理，并用
	// 归一化结果覆盖 value。
	//
	// 覆盖必须在 store.SetSetting **之前**：下面三个"同一个值"必须字节一致——
	// ① 本函数返回体 set.Value（管理面保存后直接回显）；
	// ② 库内落的值（后端重启/其它实例重载时读到的）；
	// ③ 随后 reloadSettings 灌进快照的值（读路径 Service.CustomPages 看到的）。
	// 若把校验放到 SetSetting 之后（或干脆只校验不改 value），三者就会分叉：
	// 设置页显示的是归一化串、库里躺的是原始串，下一次保存又拿原始串再归一化，
	// 幂等性与 diff 稳定性都没了。
	if key == domain.SettingCustomPages {
		pages, err := domain.ParseCustomPages(value)
		if err != nil {
			// 带详情的 400（spec §6-4 修订 / §19-3）：httpface.writeInvalidInput 直接拿
			// err.Error() 当 400 body，前端 toast 原样显示——丢掉详情就没法指出"第几条、
			// 哪个字段"，§13 的验收项不成立。errors.Is 仍可判别（%w 包住哨兵）。
			return nil, fmt.Errorf("%w: %s", ErrInvalidInput, err.Error())
		}
		pages = domain.NormalizeCustomPages(pages)
		if err := domain.ValidateCustomPages(pages); err != nil {
			return nil, fmt.Errorf("%w: %s", ErrInvalidInput, err.Error())
		}
		// Marshal 失败是编码层的内部错误（正常输入走不到），没有可定位的管理面语义，
		// 保持泛化文案：给管理员一句"service: invalid input"比暴露编码器错误更有用。
		normalized, err := domain.MarshalCustomPages(pages)
		if err != nil {
			return nil, ErrInvalidInput
		}
		value = normalized
	}
	set, err := s.store.SetSetting(ctx, key, def.Type, value)
	if err != nil {
		return nil, err
	}
	s.reloadSettings(ctx)
	// 本地即时重算走统一去抖通道：settings 快照已由上方 reloadSettings 同步
	// 刷新（新 N 先入快照），KindSettings 触发 auth 快照全量 Reload（gate
	// 预算按新 N 重算；≤200ms 去抖窗口与其余 Kind 一致）。顺序不变量：
	// settings 快照刷新必须先于 auth.Reload。远端实例由 NOTIFY → dispatcher.Apply
	// 同步 ReloadSettings + scope 重载（保持）。
	s.inv.Settings()
	s.publish(ctx, notify.Change{Settings: true}) // 其余实例 settings 快照重载（多实例）
	return set, nil
}

// ensureSnap 字面量 Service 兼容（测试绕过 New 直构 struct）：settings
// 为空时按 New 同语义自建（store 同源，无分叉——生产恒经 New 非空）。
func (s *Service) ensureSnap() *settingssnap.Snapshot {
	if s.settings == nil {
		s.settings = settingssnap.New(s.store, s.log)
	}
	return s.settings
}

// ReloadSettings settings 快照全量重载。供 dispatcher 的本地变更、远端
// NOTIFY 和断线重连 FullRefresh 共用；失败返回错误由 dispatcher/listener 记录。
func (s *Service) ReloadSettings(ctx context.Context) error {
	return s.ensureSnap().Reload(ctx)
}

// reloadSettings 全量重载设置快照（New 初始化 + UpdateSetting 后调用）。
// 失败 fail-safe：仅告警，保留旧快照/空快照继续——读快照缺失
// 按零值处理（与无配置现状行为一致），不阻断服务启动。
func (s *Service) reloadSettings(ctx context.Context) {
	if err := s.ReloadSettings(ctx); err != nil && s.log != nil {
		s.log.Warn("settings snapshot reload failed", logx.Error(err))
	}
}

// settingValue 快照查值：缺失（含快照未初始化）返回空串。
func (s *Service) settingValue(key string) string {
	return s.settings.Value(key)
}

// settingInt 快照数值读取：缺失/解析失败 → 0（UpdateSetting 已做类型化
// 校验，此处仅防御性兜底；解析失败按 0 = 不送/不限语义）。
func (s *Service) settingInt(key string) int64 {
	return s.settings.Int(key)
}
