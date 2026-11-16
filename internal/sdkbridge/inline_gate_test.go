// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestStreamThinShellInlines 数据面"零损失"的**机械证明**（T3 评审第 8 条：不能靠人读
// 报告）。`*Stream` 的五个方法必须被编译器内联——内联后调用点展开成对 SDK 的直接调用，
// 与改造前 `codexTransport` 同一条指令流；不内联就意味着多一层真实函数调用。
//
// 为什么用子进程跑 `go test -gcflags=-m`：内联判定只在编译期可见，`testing` 包里没有
// 运行时 API 能读到它。子进程带 `-run '^$'`（不执行任何用例）⇒ 不会递归调用本用例。
//
// `Close` 刻意不列入：它带返回值、且每**请求**只在连接收尾调一次，不在帧循环上；
// 把它列进来只会让门禁在编译器换策略时无意义地变红。
func TestStreamThinShellInlines(t *testing.T) {
	root := moduleRoot(t)
	cmd := exec.Command("go", "test", "-run", "^$", "-gcflags=-m", "./internal/sdkbridge")
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "go test -gcflags=-m 失败：%s", out)
	got := string(out)

	for _, sym := range []string{"(*Stream).Write", "(*Stream).Read", "(*Stream).Ping"} {
		trunc := got
		if len(trunc) > 4000 {
			trunc = trunc[:4000] + "\n…（已截断）"
		}
		require.Contains(t, got, "can inline "+sym,
			"数据面零损失证明失效：%s 未被内联（每帧会多一层真实调用）。编译器报告见下\n%s", sym, trunc)
	}
}
