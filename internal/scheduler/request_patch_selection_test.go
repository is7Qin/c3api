// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

// request_patch_selection_test.go — P2 装配点（spec custom-request-patch §6/§10 P2）：
// 唯一的生产 Selection 构造点（reserveOnView）必须把快照的 Template.Upstream
// **指针拷贝**进 Selection：数据面（P3-P5）只读它，不复制 map、不重复判断。

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// TestSelectionCarriesTemplateUpstreamPointer 钉住装配语义：
//   - custom + 配置：Selection.Upstream 与 tpl.Upstream **同一指针**
//     （装配是字段拷贝，0 分配；Patch 也随之可达，数据面不用回查控制面）；
//   - 未配置：Selection.Upstream 为 nil（数据面走 domain.ResolveUpstreamPath
//     的 nil 安全默认分支，零判断）。
func TestSelectionCarriesTemplateUpstreamPointer(t *testing.T) {
	patch := &domain.RequestPatch{Headers: map[string]string{"X-Call-Source": "vibe_code"}}
	up := &domain.TemplateUpstream{
		Paths: map[string]string{string(domain.UpstreamPathChat): "v1/custom/chat?api-version=2024-10-21"},
		Patch: patch,
	}
	customTpl := tpl(1, domain.FormatOpenAIChat, []string{"gpt-4o"})
	customTpl.CredentialType = credential.TypeCustom
	customTpl.Upstream = up

	s := newSched(t, newMemLoader(map[int64][]*domain.Account{10: {acc(1, customTpl, 4)}}))
	sel, err := s.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	require.Same(t, up, sel.Upstream,
		"Selection.Upstream 必须是快照指针的字段拷贝：不复制 map、不新建结构体（0 分配）")
	require.Same(t, patch, sel.Upstream.Patch, "Patch 随 Selection 到达数据面（同源同门）")
	require.Equal(t, "v1/custom/chat?api-version=2024-10-21", sel.Upstream.Paths[string(domain.UpstreamPathChat)],
		"路径与 patch 在同一个 Upstream 上（P3 消费同一份只读对象）")
	sel.Handle.Release()

	// 未配置（api_key / custom 无 ext 行）：nil，数据面走 nil 安全默认分支。
	plain := tpl(2, domain.FormatOpenAIChat, []string{"gpt-4o"})
	s2 := newTestScheduler(t, []*domain.Account{acc(2, plain, 4)})
	sel2, err := s2.Select(10, domain.FormatOpenAIChat, "gpt-4o")
	require.NoError(t, err)
	require.Nil(t, sel2.Upstream, "未配置 ⇒ Selection.Upstream nil（零判断的默认端点路径）")
	sel2.Handle.Release()
}
