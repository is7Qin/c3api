// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"fmt"

	"github.com/is7qin/c3api/internal/domain"
)

// CustomPages 当前生效的自定义嵌入页配置。
//
// 读快照，不读库：设置快照（settingssnap）是全部读路径的唯一上游，本方法每次
// 被调用都直接拿现值的字符串，解析不缓存——配置条目 ≤20 条，解析+归一+校验是
// 常数级开销，换来的是「UpdateSetting 落库后下一次调用立即可见」，不需要任何
// 失效广播参与。
//
// 读路径也校验（与写入路径同序 Parse → Normalize → Validate，只是不重新
// Marshal）：写入路径只覆盖"我们自己的写"，而库不是只有我们一个写入方——运维
// 手改 DB、迁移脚本、回滚到旧版本都可能写进非法值。校验是 ≤20 项的枚举/长度
// 检查，常数开销，换的是「非法配置不可能被服务出去」：这里宁可返回错误让上层
// 变成 500（固定文案 custom pages config is invalid），也不静默返回空列表——
// 静默等于配置坏了没人知道，管理员只会看到侧栏入口"莫名消失"。
func (s *Service) CustomPages() ([]domain.CustomPage, error) {
	raw := s.settingValue(domain.SettingCustomPages) // 快照，不读库
	pages, err := domain.ParseCustomPages(raw)
	if err != nil {
		return nil, fmt.Errorf("custom pages config is invalid: %w", err)
	}
	pages = domain.NormalizeCustomPages(pages)
	if err := domain.ValidateCustomPages(pages); err != nil {
		return nil, fmt.Errorf("custom pages config is invalid: %w", err)
	}
	return pages, nil
}
