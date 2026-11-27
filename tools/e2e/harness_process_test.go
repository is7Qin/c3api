// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 本文件不带 e2e build tag：平台进程 helper 与子进程 env deny-list 的单测在**普通
// test job** 跑（spec §10.7：Windows 本机手工验证，CI Linux 覆盖 Unix 路径）。
//
// 本文件在 boundary gate 的显式白名单内：平台 helper 测试必须能 spawn 短命/长命
// 子进程并构造 cmd.Env（这正是被 gate 禁止的场景行为，只有 harness 自身可用）。
package e2e

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// procHelperEnv 子进程载体标记：让测试二进制以「长命 helper」形态运行。
const procHelperEnv = "E2E_PROC_TEST_CHILD"

// TestProcHelperChild 仅作为被测子进程载体（正常跑时 Skip；不参与断言）。
func TestProcHelperChild(t *testing.T) {
	if os.Getenv(procHelperEnv) != "1" {
		t.Skip("helper only: 由 TestProcCreationTimeAndForceKill 作为子进程启动")
	}
	time.Sleep(60 * time.Second)
}

// TestSubprocessEnvFiltersHarnessPrefixes AV-ε（env 双面）最小验证：
// harness 自己的 E2E_* 旋钮与 C3API_* 配置命名空间都不得出现在被测子进程 env 里；
// 普通 OS 变量必须保留（deny-list 而非严格白名单）。
func TestSubprocessEnvFiltersHarnessPrefixes(t *testing.T) {
	t.Setenv("C3API_HARNESS_SELFTEST_LEAK", "c3api-leak")
	t.Setenv("E2E_HARNESS_SELFTEST_LEAK", "e2e-leak")
	t.Setenv("HARNESS_KEEP_OS_VAR", "keep-me")

	env := subprocessEnv()
	require.NotEmpty(t, env, "过滤后 env 不得为空（OS 变量保留）")
	for _, kv := range env {
		require.False(t, strings.HasPrefix(kv, "C3API_"), "子进程 env 不得含 C3API_*：%s", kv)
		require.False(t, strings.HasPrefix(kv, "E2E_"), "子进程 env 不得含 E2E_*：%s", kv)
	}
	require.Contains(t, env, "HARNESS_KEEP_OS_VAR=keep-me", "普通 OS env 必须保留")

	// 真子进程验证（不是只看切片）：dump 子进程 env 后断言泄漏与保留面。
	var cmd *exec.Cmd
	if os.PathSeparator == '\\' {
		cmd = exec.Command("cmd", "/c", "set")
	} else {
		cmd = exec.Command("env")
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "dump 子进程 env: %s", out)
	dumped := string(out)
	require.NotContains(t, dumped, "C3API_HARNESS_SELFTEST_LEAK", "C3API_* 不得泄漏给子进程")
	require.NotContains(t, dumped, "E2E_HARNESS_SELFTEST_LEAK", "E2E_* 不得泄漏给子进程")
	require.Contains(t, dumped, "HARNESS_KEEP_OS_VAR", "OS env 必须正常继承")
}

// startHelperChild 以测试二进制自身为被测长命子进程（跨平台、不依赖外部命令）。
func startHelperChild(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestProcHelperChild", "-test.v=false")
	cmd.Env = append(os.Environ(), procHelperEnv+"=1")
	require.NoError(t, cmd.Start(), "启动 helper 子进程")
	return cmd
}

// waitGone 有界轮询 PID 确定死亡（强杀是异步的；不用固定 sleep 断言）。
func waitGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		alive, certain := procAlive(pid)
		if certain && !alive {
			return
		}
		if time.Now().After(deadline) {
			alive, certain = procAlive(pid)
			t.Fatalf("pid=%d 未在 10s 内确认死亡（alive=%v certain=%v）", pid, alive, certain)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestProcCreationTimeAndForceKill 平台 helper 契约（spec §9.3/§10.4）：
// creation_time 可读 => 强杀前校验（不匹配拒绝）；匹配才杀；杀掉后确定死亡。
func TestProcCreationTimeAndForceKill(t *testing.T) {
	cmd := startHelperChild(t)
	pid := cmd.Process.Pid
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	alive, certain := procAlive(pid)
	require.True(t, alive, "helper 子进程应存活")
	require.True(t, certain, "本进程子进程的存活状态必须确定")
	ct, ok := procCreationTime(pid)
	require.True(t, ok, "creation_time 必须可读")
	require.NotZero(t, ct)

	// creation_time 不匹配 => 拒绝强杀（PID 复用安全），进程仍存活。
	err := forceKillTree(childProcess{PID: pid, CreationTime: ct + 1, Kind: "helper"})
	require.Error(t, err, "creation_time 不匹配必须拒绝强杀")
	alive, _ = procAlive(pid)
	require.True(t, alive, "拒绝强杀后子进程仍存活")

	// 匹配 => 强杀成功且确认死亡。
	require.NoError(t, forceKillTree(childProcess{PID: pid, CreationTime: ct, Kind: "helper"}))
	waitGone(t, pid)
	// 强杀后 Wait 返回非 nil（被信号/非 0 退出）是预期：helper 子进程不是优雅退出。
	_ = cmd.Wait()
}

// TestAllocLoopbackAddr 动态端口契约：127.0.0.1:0 -> 非 0 端口且互异。
func TestAllocLoopbackAddr(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		addr, err := allocLoopbackAddr()
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(addr, "127.0.0.1:"), "必须回环地址：%s", addr)
		require.False(t, strings.HasSuffix(addr, ":0"), "必须拿到内核分配的非 0 端口：%s", addr)
		require.False(t, seen[addr], "动态端口不得重复：%s", addr)
		seen[addr] = true
	}
}

// TestSanitizeFileName 日志/配置文件名归一（路径分隔符/冒号等不得进文件名）。
func TestSanitizeFileName(t *testing.T) {
	require.Equal(t, "gateway-1", sanitizeFileName("gateway:1"))
	require.Equal(t, "a-b", sanitizeFileName("a/b"))
	require.Equal(t, "unnamed", sanitizeFileName(""))
}
