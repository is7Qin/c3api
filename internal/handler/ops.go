// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"net/http"
	"time"

	"github.com/is7qin/c3api/internal/httpface"
	"github.com/is7qin/c3api/pkg/logx"
)

// GET /api/admin/ops/workers 运维观测端点（spec 2026-08-11；并入管理面）：
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
//
// 口径补充（scheduler 的 quota 队列）：关闭排空会把剩余条目从 enqueued 转入
// dropped，故 enqueued + dropped 恒等于 Enqueue 调用次数（两者之和守恒）。
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
	// 游标消费者 lag 族观测直读（ledger-cursor）。nil = 未装配 → alerts 全零）。
	BillingAlerts func() BillingAlerts
	// UsageSnap codex 额度快照数据源（/api/admin/accounts/usage 的
	// upstream 栏经构造直调（*sdkbridge.Codex 满足）；nil = 未装配 → codex
	// 账号 null 快照，与旧 service nil-setter 降级语义一致）。
	UsageSnap CodexUsageProber
	// PricingSync 价格手动同步/预览编排面（POST /pricing/sync 与
	// /pricing/sync/preview 经构造直调；接口在本包声明，pricing_port.go 的
	// PricingSync——*pricing.SyncWorker 方法集是超集，满足它）；nil =
	// 未装配 → 端点 500，与旧 service nil-fetcher 降级语义一致）。
	PricingSync PricingSync
	// AttemptSampling 尝试明细采样丢弃计数面（/api/admin/ops/attempts/{request_id}
	// 的 sampling 段；实现 = (*usage.AttemptLogWorker).Dropped 的方法值）。handler
	// 只需要丢弃总数，不要整个 worker（Enqueue/Close 属于写路径）。
	//
	// **函数值接缝**：零值 nil = 未装配 → 响应省略 sampling 字段（不伪造
	// dropped_total: 0）；非 nil 即可安全直调。typed-nil 在这里不可表达——函数值
	// 没有动态类型，判空即判「有没有人装配」；唯一的 nil 指针风险是取 w.Dropped
	// 那一行，而装配处（cmd/server 的 attemptDropCounter）已用 w == nil 挡在前面。
	AttemptSampling func() int64
	// Log fan-out 未知上游错误 Warn（nil = 静默；生产由组合根注入）。
	Log *logx.Logger
}

// BillingAlerts overview.alerts 数据（billing 游标积压 lag 族三真值原样直出
// ——每周期收尾 refreshLag 原子写，见 billing.FlusherStats）。
type BillingAlerts struct {
	LagMs           int64
	UnbilledRows    int64
	QuarantinedRows int64
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
