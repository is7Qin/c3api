// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import (
	"reflect"
	"testing"
)

// TestDepsHasNoFuncFields 钉住 P3 的结论：Deps 不再用 func 字段承载协作面。
//
// 为什么 func 字段是有害的：它丢掉了「谁实现」的类型信息（读 Deps 看不出实现方）、
// 无法 mock（只能造闭包）、并且每一个都额外背一条 nil 语义。历史上
// CompileNotify / MailEnqueue / ClearBalanceWarningCooldown 三个字段的注释都写着
// 「func 值注入，无 import 环」——但环并不存在（本包本就 import scheduler，
// scheduler/notification 都不 import 本包），它们只是历史防御。
//
// 新增 func 字段若确有必要（例如纯回调、无接收者），请先确认它是「值」而不是
// 「协作面」，并在 Deps 注释里写明为什么不能用接口。
func TestDepsHasNoFuncFields(t *testing.T) {
	typ := reflect.TypeOf(Deps{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type.Kind() == reflect.Func {
			t.Errorf("Deps.%s 是 func 字段：协作面请定义消费方接口（见 CompileTrigger/MailEnqueuer/CooldownClearer）", f.Name)
		}
	}
}
