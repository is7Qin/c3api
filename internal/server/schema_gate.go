// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
package server

import (
	"net/http"
	"strings"

	"github.com/is7qin/c3api/internal/handler/httpface"
)

// schema 启动门的 HTTP 面（spec §2.5/§2.6）。两个出口，一个判据：
//
//   - /readyz：唯一摘流信号（200/503 + `{schema:{…}}`），liveness 不经过它；
//   - 业务面闸门（/api/* 与 /v1/*）：不可服务时**在鉴权之前**拒成 503，这样
//     「写不了库」不会先变成一次成功鉴权 + 一次 500。
//
// 判断数据源只有一个：Options.Ready（由 cmd/server 在启动期用 migrate.Verify +
// migrate.Decide 算好后闭包进进程）。本包不 import internal/db/migrate：路由层
// 只需要水位数字，不需要知道迁移怎么算出来的。

// SchemaStatus 是启动门水位的线缆投影（spec §2.6 的 `{schema:{…}}`）。
//
// 键名是运维契约（值班脚本与 runbook 会读它）：改名/删键等于改一个对外接口。
// db / db_contract 是库里已应用的最高 expand / contract 版本，binary 是本二进制
// 内嵌迁移的最高版本，mode ∈ {serve, readonly}。
type SchemaStatus struct {
	DB         int64  `json:"db"`
	DBContract int64  `json:"db_contract"`
	Binary     int64  `json:"binary"`
	Mode       string `json:"mode"`
}

// ReadyFunc 返回当前 schema 水位与「本二进制能不能服务这个库」。
type ReadyFunc func() (SchemaStatus, bool)

// schemaReadiness 归一 nil Ready（无 DB 的路由用例）：恒可服务，水位全零。
func schemaReadiness(ready ReadyFunc) (SchemaStatus, bool) {
	if ready == nil {
		return SchemaStatus{Mode: "serve"}, true
	}
	return ready()
}

// isBusinessPath 判定「业务面」：所有 API 与前代理解析面。
//
// /healthz 与 /readyz 必须留在外面（探针要能答），静态资源与 SPA 也留在外面
// （控制台在 schema 修好之前仍可加载——它只会看到一堆 503，但页面本身不该 404）。
func isBusinessPath(p string) bool {
	return strings.HasPrefix(p, "/api/") || strings.HasPrefix(p, "/v1")
}

// schemaGuard 是业务面的唯一 HTTP 收口（spec §2.6：admin 路由中间件 + 代理入口
// 合成一处，避免"三个面各自实现一遍，漏一个没人发现"）。不可服务时统一 503 +
// 机读水位 + Retry-After，鉴权在被拒之后才轮到。
func schemaGuard(ready ReadyFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ready == nil || !isBusinessPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			status, ok := ready()
			if ok {
				next.ServeHTTP(w, r)
				return
			}
			w.Header().Set("Retry-After", "5")
			httpface.WriteJSON(w, http.StatusServiceUnavailable, map[string]any{
				"error":  "database schema is not usable by this binary; see /readyz",
				"schema": status,
			})
		})
	}
}
