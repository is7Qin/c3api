// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.
package pgtest_test

import (
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/testsupport/pgtest"
)

// TestRequirePGGateIsFatal 用子进程钉住 pgtest 的统一闸门：require-PG 开关为 1
// 且 PG 测试 DSN 缺失时，任何 PG 入口（这里用 DSN）都必须让**进程**非零退出，
// 而不是只打一行 skip；没有该开关时保持 skip（退出 0）。
//
// 本文件同样受源码门约束：不得书写那个 DSN 环境变量名（拼接也不行）。所以子进程
// 按前缀丢弃父进程的全部 TEST_* 开关，并且只断言失败文案里操作者可见的导出提示
// ——「名字只出现在 pgtest.go」本身就是门的判据。
func TestRequirePGGateIsFatal(t *testing.T) {
	if os.Getenv("PGTEST_GATE_PROBE") == "1" {
		pgtest.DSN(t) // 闸门缺失时这里会 skip，子进程退出 0 → 外层用例红
		return
	}

	run := func(env []string) (string, error) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestRequirePGGateIsFatal$", "-test.v")
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	out, err := run(gateProbeEnv("PGTEST_GATE_PROBE=1", "TEST_REQUIRE_PG=1"))
	require.Error(t, err, "missing DSN + TEST_REQUIRE_PG=1 must fail the test process: %s", out)
	require.Contains(t, out, "TEST_REQUIRE_PG=1")
	require.Contains(t, out, "is empty: export ")
	require.Contains(t, out, "sslmode=disable")

	out, err = run(gateProbeEnv("PGTEST_GATE_PROBE=1"))
	require.NoError(t, err, "without TEST_REQUIRE_PG a missing DSN must still skip: %s", out)
	require.Contains(t, out, "SKIP")
}

// gateProbeEnv 复制当前环境但丢弃全部 TEST_* 开关，再追加 extra。按前缀过滤而不是
// 按名字过滤：本文件不许书写 DSN 环境变量名。key 里不含 '='，前缀匹配不会误伤值。
func gateProbeEnv(extra ...string) []string {
	env := make([]string, 0, len(os.Environ())+len(extra))
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "TEST_") {
			continue
		}
		env = append(env, kv)
	}
	return append(env, extra...)
}
