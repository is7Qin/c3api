// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

//go:build e2e

// host 级 e2e 套件锁入口（spec §16，P1 fix3）：**单 test 二进制（= 一个 e2e 套件）
// 在启动时持有唯一 host 级锁**，无论 -run 过滤了哪些 Test（单 Test 也持锁）。
//
//   - 抢不到锁（owner 活/不确定/坏记录）=> 打印清晰错误 + os.Exit(2)，绝不等待/重试/
//     静默 skip（fast-fail 必须可诊断）；
//   - owner 确定死亡 => 保守接管（creation-time 校验，见 harness_suitelock.go）；
//   - m.Run 结束后释放；panic 也通过 defer 释放（os.Exit 前一定先释放）。
//
// 不带 e2e tag 的普通 test job（gate/单测）没有 TestMain => 不受套件锁影响。
// 并发口径收窄为「单套件 × host」：e2e 与全量单测可并发，套件内场景仍串行（t.Parallel
// 禁令不变）；不支持同 host 两个 e2e 套件并发（spec §16 AV-γ 改为第二个套件明确
// fast-fail、不污染第一个）。
package e2e

import (
	"fmt"
	"os"
	"testing"
)

// TestMain e2e 套件锁的获取/释放生命周期（只在 -tags e2e 下编译）。
func TestMain(m *testing.M) {
	lock, err := acquireE2ESuiteLock()
	if err != nil {
		// fast-fail：明确打印，exit 2（不等待/不重试/不静默 skip）。stdout+stderr 双写，
		// 保证 go test 失败输出与 CI artifact 都能看到。
		msg := fmt.Sprintf("\n[e2e suite lock] e2e 套件无法启动：%v\n", err)
		fmt.Fprint(os.Stderr, msg)
		fmt.Fprint(os.Stdout, msg)
		os.Exit(2)
	}
	if lock.tookOver {
		fmt.Fprintf(os.Stderr,
			"[e2e suite lock] 已保守接管陈旧套件锁：原 owner pid=%d host=%s start=%s（进程已确定死亡）\n",
			lock.staleOwner.PID, lock.staleOwner.Hostname, lock.staleOwner.Start.Format("2006-01-02T15:04:05Z07:00"))
	}

	exitCode := 0
	func() {
		defer func() {
			if r := recover(); r != nil {
				// panic：先释放锁（避免一次测试 panic 永久阻塞后续套件），再让 go test
				// 照常报告 panic。
				if rerr := lock.Release(); rerr != nil {
					fmt.Fprintf(os.Stderr, "[e2e suite lock] panic 路径释放套件锁失败：%v\n", rerr)
				}
				panic(r)
			}
		}()
		exitCode = m.Run()
		if rerr := lock.Release(); rerr != nil {
			fmt.Fprintf(os.Stderr, "[e2e suite lock] 释放套件锁失败（不影响本次测试结论，但下次可能要人工清理）：%v\n", rerr)
		}
	}()
	os.Exit(exitCode)
}
