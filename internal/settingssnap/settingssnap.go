// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// Package settingssnap 是 settings 全量内存快照的唯一事实源（B3 根因重开）：
// Service 与 MailWorker 同源共享单个 *Snapshot（单指针，无双快照分叉）。
// 叶子包：仅依赖 domain + logx + 标准库，不 import internal/repository（防环——
// SettingsLoader 窄接口由 repos/fakeStore 结构性满足）。
package settingssnap

import (
	"context"
	"strconv"
	"sync/atomic"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// SettingsLoader 窄接口：快照唯一上游。repos 与测试 fake 均已满足。
type SettingsLoader interface {
	GetAllSettings(ctx context.Context) ([]*domain.Setting, error)
}

// Snapshot settings 全量内存快照（默认值 + DB 覆盖）：公开读路径零 DB 直读；
// 仅管理面 UpdateSetting / NOTIFY dispatcher 低频重载（无锁，atomic 指针交换）。
type Snapshot struct {
	store SettingsLoader
	log   *logx.Logger
	ptr   atomic.Pointer[map[string]*domain.Setting]
}

// New 构造快照（不加载；调用方 Load 首载，失败 Warn 语义由调用方保持）。
func New(store SettingsLoader, log *logx.Logger) *Snapshot {
	return &Snapshot{store: store, log: log}
}

// Load 首载（New 后调用一次）。
func (s *Snapshot) Load(ctx context.Context) error {
	return s.Reload(ctx)
}

// Reload 全量重载（UpdateSetting / dispatcher Apply-FullRefresh 共用）。
func (s *Snapshot) Reload(ctx context.Context) error {
	if s == nil || s.store == nil {
		return nil
	}
	rows, err := s.store.GetAllSettings(ctx)
	if err != nil {
		return err
	}
	m := make(map[string]*domain.Setting, len(rows))
	for _, st := range rows {
		m[st.Key] = st
	}
	s.ptr.Store(&m)
	return nil
}

// Value 快照查值：缺失（含快照未初始化）返回空串。nil 接收者安全
// （字面量 Service 测试绕过 New——读路径零值，与旧原子指针 nil 语义一致）。
func (s *Snapshot) Value(key string) string {
	if s == nil {
		return ""
	}
	m := s.ptr.Load()
	if m == nil {
		return ""
	}
	if st, ok := (*m)[key]; ok {
		return st.Value
	}
	return ""
}

// Int 快照数值读取：缺失/解析失败 → 0（UpdateSetting 已做类型化校验，
// 此处仅防御性兜底；解析失败按 0 = 不送/不限语义）。
func (s *Snapshot) Int(key string) int64 {
	v, err := strconv.ParseInt(s.Value(key), 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// MailConfig 邮件通道配置（7 返回；语义与原 service.mailConfig 逐行一致）。
// nil 接收者 → !ok（未配置降级，与旧空快照语义一致）。
func (s *Snapshot) MailConfig() (host string, port int, username, password, fromAddr, tlsPolicy string, ok bool) {
	if s == nil {
		return "", 0, "", "", "", "", false
	}
	host = s.Value("mail.smtp_host")
	fromAddr = s.Value("mail.from_address")
	if s.Value("mail.enabled") != "true" || host == "" || fromAddr == "" {
		return "", 0, "", "", "", "", false
	}
	port64, err := strconv.Atoi(s.Value("mail.smtp_port"))
	if err != nil || port64 < 1 || port64 > 65535 {
		return "", 0, "", "", "", "", false
	}
	return host, port64, s.Value("mail.smtp_username"), s.Value("mail.smtp_password"), fromAddr, s.Value("mail.tls"), true
}

// Log 日志面（MailWorker 构造前已存在，透出只读）。
func (s *Snapshot) Log() *logx.Logger { return s.log }
