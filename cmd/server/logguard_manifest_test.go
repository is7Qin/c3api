// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// logGuardAllowlist 是「允许保留 `log != nil` 守卫」的冻结清单（spec §3.3）。
//
// 为什么要有这道门：空 logger（nil 与零值）已经静默，所以任何新增的
// `if x.log != nil` 都只可能来自两种误解——要么以为空 logger 会 panic，要么
// 把「日志参数里含副作用调用」当成了条件（那才是真需要 guard 的罕见情形）。
// 两种情况都该在 review 时被看见，而不是悄悄长回来。
//
// 键是「相对路径|去掉缩进的整行」，值是出现次数。改这一行必须同时改本表。
var logGuardAllowlist = map[string]int{
	// 日志参数含 partitionDateRange/fields 组装（纯计算，但删了会让它在
	// nil-log 路径无条件执行）。
	"internal/usage/usage.go|if r.log != nil {": 2,
	// 日志参数含 w.now() / w.settings.PriceSyncCron()——有状态读，删 guard
	// 会让测试夹具（seqSettings、递增时钟）多消费一格序列。
	"internal/pricing/worker.go|if w.log != nil {": 2,
}

// TestLogGuardManifest 扫全仓非测试源码，要求 `log != nil` 守卫的多重集与
// 冻结表**完全一致**：多了必红（有人又加了一层“空 logger 会 panic”的旧认知），
// 少了也必红（守卫被清掉但表项没删——不清理会让表逐渐变成“历史残留”。）。
func TestLogGuardManifest(t *testing.T) {
	root := repoRoot(t)
	got := map[string]int{}

	for _, dir := range []string{"internal", "cmd"} {
		base := filepath.Join(root, dir)
		err := filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case "ent", "node_modules", "testdata", ".git":
					return filepath.SkipDir
				}
				return nil
			}
			name := d.Name()
			if !strings.HasSuffix(name, ".go") ||
				strings.HasSuffix(name, "_test.go") ||
				strings.HasSuffix(name, ".gen.go") {
				return nil
			}
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			for _, line := range strings.Split(string(b), "\n") {
				if !hasLogNilCmp(line) {
					continue
				}
				got[rel+"|"+strings.TrimSpace(line)]++
			}
			return nil
		})
		require.NoError(t, err)
	}

	var drift []string
	// 多了 / 数量不符。
	for k, n := range got {
		if logGuardAllowlist[k] != n {
			drift = append(drift, k+"  got="+itoa(n)+" want="+itoa(logGuardAllowlist[k]))
		}
	}
	// 少了（白名单残留）：删了源码守卫却没删表项。不查这一向，表会越积越脏，
	// 下一个人会以为那些守卫还在。
	for k, want := range logGuardAllowlist {
		if got[k] == 0 {
			drift = append(drift, k+"  got=0 want="+itoa(want)+"（守卫已删，请同步删表项）")
		}
	}
	sort.Strings(drift)
	require.Empty(t, drift,
		"`log != nil` 守卫与冻结表不一致：空 logger 已静默，新增守卫请先确认它不是“怕 panic”；若确实因为日志参数含副作用调用而必须保留，把该行登记进 logGuardAllowlist 并写明理由")
}

// repoRoot 从当前测试目录向上找到含 go.mod 的目录，不依赖写死的 ../..。
// go test 的 cwd 就是包目录，但「相对层级」会随包移动而失效；向上找根更稳。
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("找不到仓库根（从 %s 向上未遇 go.mod）", dir)
		}
		dir = parent
	}
}

// hasLogNilCmp 判断一行是否含 `\blog\b != nil`（小写 log，排除 errlog 一类后缀）。
// 允许 `s.log` / `w.log` / 裸 `log`，不接受 `cfg.Log`（大写形式不在本门范围）。
func hasLogNilCmp(line string) bool {
	for i := 0; i+3 <= len(line); i++ {
		if line[i:i+3] != "log" {
			continue
		}
		if i > 0 && isIdentChar(line[i-1]) { // errlog / catalog …
			continue
		}
		j := i + 3
		for j < len(line) && (line[j] == ' ' || line[j] == '\t') {
			j++
		}
		if j+1 < len(line) && line[j] == '!' && line[j+1] == '=' {
			k := j + 2
			for k < len(line) && (line[k] == ' ' || line[k] == '\t') {
				k++
			}
			if strings.HasPrefix(line[k:], "nil") {
				return true
			}
		}
	}
	return false
}

func isIdentChar(b byte) bool {
	return b == '_' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
