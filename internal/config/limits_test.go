// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFailoverAttemptsBound：proxy.failover_attempts 的合法域 1..8 由 validate
// 成对锁定，且上下界都取自 domain 常量——改常量则本条必红。
//
// 必须用 defaults() 起手：裸 &Config{...} 会先被别的字段下限打掉，测不到
// failover 分支。反过来 defaults() 缺 auth.jwt_secret/db.dsn 等必填项，所以
// 「8 合法」只能断言「错误与 failover_attempts 无关」，不能断言「无错」。
//
// 三种取值拆成子用例（不是并行）：上界被改小时，「8 被拒」与「9 的文案变成
// <= 改后值」两个信号各自独立可见，不会被前一个 require 的 FailNow 掩盖。
func TestFailoverAttemptsBound(t *testing.T) {
	t.Run("8 accepted", func(t *testing.T) {
		cfg := defaults()
		cfg.Proxy.FailoverAttempts = 8
		err := validate(cfg)
		require.True(t, err == nil || !strings.Contains(err.Error(), "failover_attempts"),
			"8 必须通过 failover 上界（其余必填项报错不算）: %v", err)
	})

	t.Run("0 rejected", func(t *testing.T) {
		cfg := defaults()
		cfg.Proxy.FailoverAttempts = 0
		err := validate(cfg)
		require.ErrorContains(t, err, "proxy.failover_attempts must be >= 1 (got 0)",
			"0 会零次执行 failover 循环并泄漏首次选号的并发槽，启动即拒绝")
	})

	t.Run("9 rejected", func(t *testing.T) {
		cfg := defaults()
		cfg.Proxy.FailoverAttempts = 9
		err := validate(cfg)
		require.ErrorContains(t, err, "proxy.failover_attempts must be <= 8 (got 9)",
			"9 超过 attempted-ID 去重槽位，启动即拒绝（文案逐字钉死）")
	})
}
