// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// schema 启动门（spec §2.5「按 contract 水位判，不按 head 判」+ §2.6「不可服务 =
// NotReady，不是 CrashLoop」）。
//
// 这里只有两件事：把 migrate.Verify 的结果冻成进程级状态，以及把它投影成
// server.ReadyFunc。/readyz、业务面 503、写 worker 是否启动全部读这一份状态，
// 因此「能不能服务」在本进程只有一个真相。
package main

import (
	"github.com/is7qin/c3api/internal/db/migrate"
	"github.com/is7qin/c3api/internal/server"
)

// schemaGate 是启动门的进程级状态：水位（对外可见）+ 判定（能不能服务）。
//
// 为什么在启动期算一次、而不是每次探针重算：判定决定的是「这个进程构不构建业务
// 图」（worker、快照、代理面都已按它武装/不武装），运行期改主意等于半装配进程。
// 运维修好 schema 后的动作是重启 Pod，不是等探针自己变绿。
type schemaGate struct {
	verdict  migrate.Verdict
	decision migrate.Decision
}

func newSchemaGate(verdict migrate.Verdict) *schemaGate {
	return &schemaGate{verdict: verdict, decision: migrate.Decide(verdict)}
}

// ready 表示本进程可以服务业务流量。
func (g *schemaGate) ready() bool { return g == nil || g.decision.Ready() }

// status 是 /readyz 的读点（nil gate = 恒可服务，供无 DB 的装配用例）。
func (g *schemaGate) status() (server.SchemaStatus, bool) {
	if g == nil {
		return server.SchemaStatus{Mode: migrate.ModeServe}, true
	}
	return server.SchemaStatus{
		DB:         g.verdict.DBHead,
		DBContract: g.verdict.DBContract,
		Binary:     g.verdict.BinaryHead,
		Mode:       g.decision.Mode,
	}, g.decision.Ready()
}
