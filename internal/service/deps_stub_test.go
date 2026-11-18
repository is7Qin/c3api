// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package service

import "context"

// P3 把 Deps 的三个 func 字段还原成了消费方接口。测试里大量用闭包字面量
// （`MailEnqueue: func(t MailSendTask) error { return nil }`）表达「只记数/只
// 返回固定值」的桩——这些适配器让它们继续一行写完，不必为每个用例造结构体。
//
// 只放测试文件：生产侧传的是真实接收者（*MailWorker / *notification.Cooldown /
// *scheduler.Scheduler），由 wiring_billing.go 的编译期断言兜住。

type mailEnqueuerFunc func(MailSendTask) error

func (f mailEnqueuerFunc) Enqueue(t MailSendTask) error { return f(t) }

type cooldownClearerFunc func(context.Context, int64, int64) error

func (f cooldownClearerFunc) Clear(ctx context.Context, userID, thresholdMillis int64) error {
	return f(ctx, userID, thresholdMillis)
}

type compileTriggerFunc func()

func (f compileTriggerFunc) RequestCompile() { f() }
