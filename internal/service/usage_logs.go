// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

// usage_logs / err_logs 查询面（消费面改名裁决：log → usage 语义；错误审计
// err_logs 独立查询面——/usage_logs 与 /err_logs API）。

import (
	"context"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// QueryUsages usage_logs 计费明细分页查询（/usage_logs API；错误行含
// abort/failover 半异常标记——error_type 过滤保留）。keyset 游标分页：
// 返回行可能含 limit+1 探测行，next_cursor 组装在 handler。
// 返回 []*domain.UsageLog 直透（不再擦除为 []any——spec 2026-08-17 边界收敛，
// 类型信息保留，handler 断言删除）。
func (s *Service) QueryUsages(ctx context.Context, q repository.UsageQuery) ([]*domain.UsageLog, error) {
	return s.store.QueryUsages(ctx, q)
}

// QueryErrLogs err_logs 错误明细分页查询（/err_logs API：完整错误面——拒绝 +
// 异常双轨，status_code/error_type 全值；行类型同为 *domain.UsageLog——
// err_logs 表复用该领域类型）。keyset 游标分页同 QueryUsages。
func (s *Service) QueryErrLogs(ctx context.Context, q repository.ErrLogQuery) ([]*domain.UsageLog, error) {
	return s.store.QueryErrLogs(ctx, q)
}
