// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"net/http"

	"github.com/is7qin/c3api/internal/scheduler"
)

// planReadyGate 冷启动就绪门（plan-only 选号契约的启动面收口）：编译计划
// （DecisionView）发布前 AI 流量一律 503 拒绝——选号面已无计划外车道，冷窗口
// 内放行只会把请求打成 404，503+Retry-After 才是诚实的"尚未就绪"。管理面/
// 用户面/healthz 不经过本门（空库无账号时编译产物为空决策但照常发布，管理
// 控制台必须可达才能配账号）。判定 = 调度器原子视图根读（零锁零 DB 零 Redis），
// 请求路径成本与选号面同源。
func planReadyGate(sched *scheduler.Scheduler, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		v := sched.View()
		if v == nil || v.DecisionView() == nil {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"routing plan not ready","type":"gateway_error"}}`))
			return
		}
		next.ServeHTTP(w, r)
	})
}
