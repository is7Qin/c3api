// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

// 本文件不带 e2e build tag：env 隔离/动态端口/uuid 是纯 helper，普通 test job 直接
// 单测（见 harness_process_test.go），e2e harness 与场景共用同一实现。
//
// 为什么必须过滤子进程 env：internal/config 的 env provider 会把**所有** C3API_ 前缀
// env 映射成配置键，且 ErrorUnused=true（未知键启动即 fatal）；harness 自己的
// E2E_* 旋钮（E2E_LOG_DIR / E2E_REDIS_DB）同样不得成为被测进程的行为输入。因此
// server/fakeupstream 子进程一律 os.Environ() 经 deny-list 过滤后使用（保留
// PATH/SystemRoot/TEMP/HOME 等 OS 变量，不用严格白名单——白名单会在新 OS/新工具
// 依赖上静默破测）。
package e2e

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"strings"
)

// childEnvDenyPrefixes 不得继承给被测子进程的 env 前缀（C3API_ 配置命名空间 +
// harness 自身 E2E_ 测试旋钮）。
var childEnvDenyPrefixes = []string{"C3API_", "E2E_"}

// subprocessEnv 被测子进程（server / fakeupstream / migrate）的 env：os.Environ()
// 经 deny-list 过滤。**go build 不走这里**——构建子进程必须继承完整 env（私有模块
// token / GOPRIVATE / insteadOf 有效）。
func subprocessEnv() []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		blocked := false
		for _, prefix := range childEnvDenyPrefixes {
			if strings.HasPrefix(kv, prefix) {
				blocked = true
				break
			}
		}
		if !blocked {
			out = append(out, kv)
		}
	}
	return out
}

// envOr 读环境变量，缺省回落 fallback（harness 读取 base 资源地址的唯一入口）。
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// newRunUUID 生成 32 hex 的 run 唯一标识（registry 文件名 / Redis 锁 owner / Job 名
// / temp+log 目录后缀共用；crypto/rand 防并发撞车）。
func newRunUUID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("run uuid: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// allocLoopbackAddr 动态端口：监听 127.0.0.1:0 拿到内核分配的空闲端口后立即关闭。
// 调用方负责在 bind 竞争（极少见）时用新地址重试；e2e 禁止固定端口字面量。
func allocLoopbackAddr() (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("alloc loopback addr: %w", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		return "", fmt.Errorf("alloc loopback addr close: %w", err)
	}
	return addr, nil
}

// sanitizeFileName 名字 -> 安全文件名词（log/config 落盘用）。
func sanitizeFileName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if out == "" {
		out = "unnamed"
	}
	return out
}

// harnessFileExists 文件存在判定（registry/A-V 断言用；非 e2e build tag 也可用）。
func harnessFileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
