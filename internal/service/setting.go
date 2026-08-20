// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"context"
	"slices"
	"strconv"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/pkg/logx"
)

// GetSettings 全部设置（默认值 + DB 覆盖；/api/admin/settings GET）。
func (s *Service) GetSettings(ctx context.Context) ([]*domain.Setting, error) {
	return s.store.GetAllSettings(ctx)
}

// serviceTierPolicyKeys service_tier 转发策略 key → 值域（P3-7：从注册表
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
// 注册等读路径即时生效；本地直连分发器按 scope 精确重载（#36 auth gate 预算
// 按新 N 即时重算）+ NOTIFY 广播其余实例。
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
		// 值域护栏（A-P2-11）：注册表 Min/Max 是单一事实源——越界 → 400，
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
	set, err := s.store.SetSetting(ctx, key, def.Type, value)
	if err != nil {
		return nil, err
	}
	s.reloadSettings(ctx)
	// #36 本地实例即时重算（R2 M-1）：自播 NOTIFY 被 Listener Src 跳过，本地
	// settings 变更必须直连分发器——与远端 NOTIFY 同路径（Apply：同步
	// ReloadSettings + 注册表 ScopeSettings 精确重载 auth，gate 预算按新 N
	// 重算）。本地快照已由上方 reloadSettings 刷新，Apply 内 ReloadSettings
	// 是幂等重复（settings 低频路径，可接受；单一分发入口防本地/远端行为
	// 漂移）。30s 超时包裹本地直连链（合后清单：裸 WithoutCancel 无界——DB
	// 悬挂时 admin PUT 永久挂起、处理 goroutine 堆积；超时/请求取消中止本地
	// 收敛，由 NOTIFY/60s 周期兜底刷新补齐）。nil = 未装配 no-op。
	if s.local != nil {
		relCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		s.local.Apply(relCtx, notify.Change{Settings: true})
	}
	s.publish(ctx, notify.Change{Settings: true}) // 其余实例 settings 快照重载（#14 多实例）
	return set, nil
}

// ReloadSettings settings 快照全量重载（invalidate.SettingsReloader 接口实现，
// T3 main 装配注入 invalidate.Config.Settings；供 NOTIFY settings 分支触发全
// 实例重载）。与 UpdateSetting 内部路径同实现（reloadSettings 复用）；失败
// 返回错误由调用方（去抖器 reloadAll）Warn。
func (s *Service) ReloadSettings(ctx context.Context) error {
	rows, err := s.store.GetAllSettings(ctx)
	if err != nil {
		return err
	}
	m := make(map[string]*domain.Setting, len(rows))
	for _, st := range rows {
		m[st.Key] = st
	}
	s.settings.Store(&m)
	return nil
}

// reloadSettings 全量重载设置快照（New 初始化 + UpdateSetting 后调用）。
// 失败 fail-safe（评审 M-1）：仅告警，保留旧快照/空快照继续——读快照缺失
// 按零值处理（与无配置现状行为一致），不阻断服务启动。
func (s *Service) reloadSettings(ctx context.Context) {
	if err := s.ReloadSettings(ctx); err != nil && s.log != nil {
		s.log.Warn("settings snapshot reload failed", logx.Error(err))
	}
}

// settingValue 快照查值：缺失（含快照未初始化）返回空串。
func (s *Service) settingValue(key string) string {
	m := s.settings.Load()
	if m == nil {
		return ""
	}
	if st, ok := (*m)[key]; ok {
		return st.Value
	}
	return ""
}

// settingInt 快照数值读取：缺失/解析失败 → 0（UpdateSetting 已做类型化
// 校验，此处仅防御性兜底；解析失败按 0 = 不送/不限语义）。
func (s *Service) settingInt(key string) int64 {
	v, err := strconv.ParseInt(s.settingValue(key), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// ClusterInstances 集群实例数 N（settings.cluster.instances，多实例预算分摊
// 设计文档 §3.1）：所有实例从 DB settings 读同一 N（config 文件可漂移，DB 是
// 唯一共识源）。快照缺失/非法 → 回退 1（单实例语义；DB 无行即注册表默认）。
// 供 T3 gate/limiter 预算分配读取；N 变更经 settings NOTIFY 传播。
func (s *Service) ClusterInstances() int {
	n := s.settingInt("cluster.instances")
	if n < 1 {
		return 1
	}
	return int(n)
}
