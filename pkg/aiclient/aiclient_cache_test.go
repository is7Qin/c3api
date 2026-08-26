// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package aiclient

import (
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

func TestFactoryURLCacheIdentityAndInvalidate(t *testing.T) {
	f := NewFactory(&http.Client{}, Config{})
	u1, err := f.fullURLOf(1, "https://api.example.com", "chat/completions")
	require.NoError(t, err)
	require.Equal(t, "/v1/chat/completions", u1.Path)

	// 同键命中返回同一指针（零分配快路径）
	u2, err := f.fullURLOf(1, "https://api.example.com", "chat/completions")
	require.NoError(t, err)
	require.Same(t, u1, u2)

	// 不同键不同 URL：v1 前缀路径不重复补 /v1
	u3, err := f.fullURLOf(1, "https://api.example.com", "v1/messages")
	require.NoError(t, err)
	require.Equal(t, "/v1/messages", u3.Path)

	// 失效后同键重新解析（新代号新对象）
	f.InvalidateAll()
	u4, err := f.fullURLOf(1, "https://api.example.com", "chat/completions")
	require.NoError(t, err)
	require.NotSame(t, u1, u4)
	require.Equal(t, u1.String(), u4.String())
}

func TestFactoryByTMergePreservesSiblingFields(t *testing.T) {
	// 回归：并发构建三格式时，字段级合并不得抹掉同模板先建好的兄弟客户端
	// （整条目覆盖会把 {chat} 覆写成 {responses}）。
	f := NewFactory(&http.Client{}, Config{})
	tpl := &domain.Template{ID: 7, BaseURL: "https://api.example.com"}

	c1 := f.chat(tpl)
	require.NotNil(t, c1)

	c2 := f.responses(tpl)
	require.NotNil(t, c2)

	snap := f.cc.Load()
	tc := snap.byT[tpl.ID]
	require.NotNil(t, tc)
	require.NotNil(t, tc.chat, "responses 构建不得抹掉已建的 chat")
	require.NotNil(t, tc.responses)

	c3 := f.anthropic(tpl)
	require.NotNil(t, c3)

	snap = f.cc.Load()
	tc = snap.byT[tpl.ID]
	require.NotNil(t, tc.chat)
	require.NotNil(t, tc.responses)
	require.NotNil(t, tc.anthropic)

	// 稳定命中：再取同指针
	require.Same(t, c1, f.chat(tpl))
	require.Same(t, c3, f.anthropic(tpl))
}

// TestFactoryConcurrentBuildAndInvalidate 并发锤：多模板 × 三格式懒构建 +
// InvalidateAll 风暴混跑。CI 无 -race，本地验证须 `go test -race`。
// 终态断言：每模板三字段同时非 nil（合并不丢字段）；URL 键解析恒成功。
func TestFactoryConcurrentBuildAndInvalidate(t *testing.T) {
	f := NewFactory(&http.Client{}, Config{})
	const templates = 8
	tpls := make([]*domain.Template, templates)
	for i := range tpls {
		tpls[i] = &domain.Template{ID: int64(i + 1), BaseURL: "https://api.example.com"}
	}

	var wg sync.WaitGroup
	wg.Add(4 * templates)
	for _, tpl := range tpls {
		go func(tp *domain.Template) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_, err := f.fullURLOf(tp.ID, tp.BaseURL, "chat/completions")
				require.NoError(t, err)
				require.NotNil(t, f.chat(tp))
			}
		}(tpl)
		go func(tp *domain.Template) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_, err := f.fullURLOf(tp.ID, tp.BaseURL, "v1/messages")
				require.NoError(t, err)
				require.NotNil(t, f.responses(tp))
			}
		}(tpl)
		go func(tp *domain.Template) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				require.NotNil(t, f.anthropic(tp))
			}
		}(tpl)
		go func(tp *domain.Template) {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				f.InvalidateAll()
			}
		}(tpl)
	}
	wg.Wait()

	// 终态：失效风暴停止后再各构建一次，三字段必须共存于同一快照条目
	for _, tpl := range tpls {
		require.NotNil(t, f.chat(tpl))
		require.NotNil(t, f.responses(tpl))
		require.NotNil(t, f.anthropic(tpl))
		snap := f.cc.Load()
		tc := snap.byT[tpl.ID]
		require.NotNil(t, tc)
		require.NotNil(t, tc.chat)
		require.NotNil(t, tc.responses)
		require.NotNil(t, tc.anthropic)
	}
}
