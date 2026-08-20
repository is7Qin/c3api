// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"
	"time"

	"github.com/is7qin/c3api/internal/handler/httpface"
)

// GET /api/admin/ops/workers 运维观测端点（spec 2026-08-11；用户裁决并入管理面）：
// 按需聚合各 worker 可观测状态。采集纪律：原子读既有计数器 + len(channel)
// （零锁零分配，O(1)）；不做持续采集/推送/存储；鉴权走 /admin 组 adminAuth。
//
// 装配链路（三处）：
//   1. 接口实现面：各 worker 模块 Stats() any（billing/invalidate/notify/pricing/
//      rule/scheduler/usage + cmd/server/auth_sync_stats.go；原子读组装，不锁
//      模块内部）——StatsProvider 是独立契约，不改 worker.Worker（Manager 无枚举）
//   2. 装配点：cmd/server/main.go——类型断言逐个入列（缺失 Warn 一次），
//      New(svc, OpsOptions{Workers, Snapshots}) 组合根注入
//   3. 转换点：cmd/server/snapshots.go——snapshot.Status → SnapshotState
//      （LastError error 接口 → 字符串；Scopes 指针化）
// 响应类型 WorkerStatus/SnapshotState/WorkersResponse 由契约生成
// （openapi.yaml ops tag → api.gen.go；stats 字段清单见契约 description）。
// 本文件只实现装配与直出（Stats any 原样序列化，零转换）。

// StatsProvider worker 可观测状态提供面。
//
// 同步义务：改 Stats() 返回的字段（增删改名）必须同步
// openapi.yaml WorkerStatus.stats 的字段清单（description），否则契约
// 文档过期且无人知——清单是消费方（前端/运维脚本）的唯一依据。
type StatsProvider interface {
	Name() string
	Stats() any
}

// OpsOptions GET /api/admin/ops/workers 装配参数（New 变参注入；零值 = 端点返回空）。
type OpsOptions struct {
	Workers   []StatsProvider
	Snapshots func() []SnapshotState // nil = 快照区返回空
	// InFlightUsers 实时在途并发快照提供面（/api/admin/users-top；实现 =
	// proxy.Auth.InFlightUsers——门禁快照只读访问器，零锁冷面。nil = 未装配 →
	// 端点返回空列表）。
	InFlightUsers func() map[int64]int64
	// BillingAlerts 计费告警面（/api/admin/overview alerts 段；实现 = billing
	// flusher 直读 PendingLogs/PendingWaterline/Warned。nil = 未装配 →
	// alerts 全零）。
	BillingAlerts func() BillingAlerts
}

// BillingAlerts overview.alerts 数据（毫分水位原样输出——pending_waterline
// 为 billing 包级水线常量直读；warned = 水线告警边沿是否置位）。
type BillingAlerts struct {
	Pending          int64
	PendingWaterline int64
	Warned           bool
}

// GetOpsWorkers 契约实现（api.gen.go ServerInterface）：按需组装，无缓存。
// stats 直出各 worker 原类型（契约自由 schema，JSON 序列化时原样编码，零转换）。
func (h *AdminAPI) GetOpsWorkers(w http.ResponseWriter, r *http.Request) {
	ws := make([]WorkerStatus, 0, len(h.ops.Workers))
	for _, p := range h.ops.Workers {
		ws = append(ws, WorkerStatus{Name: p.Name(), Stats: p.Stats()})
	}
	var snaps []SnapshotState
	if h.ops.Snapshots != nil {
		snaps = h.ops.Snapshots()
	} else {
		snaps = []SnapshotState{} // 未装配快照区 → JSON [] 非 null
	}
	httpface.WriteJSON(w, http.StatusOK, WorkersResponse{
		Workers:     ws,
		Snapshots:   snaps,
		GeneratedAt: time.Now().UTC(),
	})
}
