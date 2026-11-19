// SPDX-License-Identifier: AGPL-3.0-or-later
package repository

// 请求链读面的**常量契约**（不碰 DB）：每源上限是 truncated 判定的语义本体，
// 改它必须同时改这里并说清为什么——否则「128 行」会变成一个没人负责的魔数。

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAttemptChainSourceLimitPinned 钉住每源行数上限。改这个数字 = 改契约
// （truncated 的阈值、以及「一屏能看多少跳」），必须显式改测试。
func TestAttemptChainSourceLimitPinned(t *testing.T) {
	require.EqualValues(t, 128, attemptChainSourceLimit)
}
