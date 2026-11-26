// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package main

import (
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAccountProbePatchWarnWiring 钉住 request_patch 残余失败可观测性的**唯一**
// 装配点（spec custom-request-patch §7.4/§8，P5-fix1）：
//
//	probe 包不写日志、不 import logx（架构矩阵钉死），所以「注入被删」不会有编译
//	错误——只会静默丢掉 probe 面的限频 WARN。组合根必须把 HTTP/WS 面同一份实现
//	（proxy.PatchWarnFunc）经构造期 Options 注入探测执行器；单一实现 ⇒ 三面共用
//	同一张 template_id+format 限频表。
func TestAccountProbePatchWarnWiring(t *testing.T) {
	billing, err := os.ReadFile("wiring_billing.go")
	require.NoError(t, err)
	src := string(billing)
	require.Contains(t, src, "accountprobe.Options{",
		"探测执行器必须经构造期 Options 注入观测 seam（否则 probe 面残余失败重新静默）")
	require.Contains(t, src, "PatchWarn: proxy.PatchWarnFunc(log)",
		"残余失败 WARN 必须复用 proxy 的同一实现（禁止在 service/main 复制限频表/文案）")

	// 整包唯一性：探测执行器只在组合根构造一次（防复制构造/第二个注入点）。
	count := 0
	for _, name := range pkgFileNames(t) {
		src, err := os.ReadFile(name)
		require.NoError(t, err)
		count += strings.Count(string(src), "accountprobe.New(")
	}
	require.Equal(t, 1, count, "accountprobe.New 只允许出现一次（billing 阶段装配）")
}
