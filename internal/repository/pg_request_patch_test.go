// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/internal/domain"
)

// TestTemplateExtRequestPatchPG template_ext.request_patch 的真实 PG 往返
// （spec custom-request-patch §5.3/§5.4）：
//   - 写路径 domain.RequestPatch → CanonicalBytes() → jsonb；读路径 jsonb →
//     domain.ParseRequestPatch，Get 回读逐字一致（嵌套 / 大整数 / 空值删除 /
//     多格式）；
//   - nil 清列 ⇒ NULL，回读 nil；
//   - 存量脏值（DB 直写、jsonb 语法合法但 wire 语义非法/形状不对）⇒ 读路径视为
//     未配置：不报错、不 panic（spec §2.6 读路径纪律）。
func TestTemplateExtRequestPatchPG(t *testing.T) {
	repos := newPGReposShared(t)
	ctx := context.Background()
	tpl := seedPGTemplate(t, repos)

	// 完整形状：全局头（含 Del 空值、{api_key} 占位符）+ 两个格式层
	// （零层为 null 被删）+ 大整数/1e18/嵌套 set + delete。
	raw := `{
	  "headers": {"Authorization": "Bearer {api_key}", "X-Call-Source": "vibe_code", "X-Drop": ""},
	  "formats": {
	    "openai-responses": {
	      "headers": {"Openai-Beta": "responses=v2"},
	      "set": {
	        "max_output_tokens": 4096,
	        "big": 9007199254740993,
	        "fraction": 1.50,
	        "metadata": {"nested": {"n": 9007199254740993}, "arr": [1, 2]},
	        "null_value": null
	      },
	      "delete": ["metadata.client_field", "a.b"]
	    },
	    "anthropic": {"set": {"metadata.user_id": "vibe_code"}},
	    "openai-chat": null
	  }
	}`
	patch, err := domain.ParseRequestPatch(json.RawMessage(raw))
	require.NoError(t, err)
	require.NotNil(t, patch)

	saved := seedTemplateExt(t, repos, &domain.TemplateExt{
		TemplateID:     tpl.ID,
		CredentialType: credential.TypeCustom,
		RequestPatch:   patch,
	})
	require.NotNil(t, saved.RequestPatch, "写路径回读必须带回 request_patch")
	require.Equal(t, string(patch.CanonicalBytes()), string(saved.RequestPatch.CanonicalBytes()))

	got, err := repos.TemplateExts.GetTemplateExt(ctx, tpl.ID)
	require.NoError(t, err)
	require.NotNil(t, got.RequestPatch)
	require.Equal(t, string(patch.CanonicalBytes()), string(got.RequestPatch.CanonicalBytes()),
		"读路径必须逐字还原规范形（大整数/1e18 不丢精度）")
	require.Contains(t, string(got.RequestPatch.CanonicalBytes()), "9007199254740993", "大整数不得经 float64 丢精度")
	require.Contains(t, string(got.RequestPatch.CanonicalBytes()), `"fraction":1.50`, "小数位不得被 jsonb 归一掉")
	// 空串头 = Del 是有效条目，不得被当成空层删掉。
	require.Equal(t, "", got.RequestPatch.Headers["X-Drop"])
	require.Equal(t, "Bearer {api_key}", got.RequestPatch.Headers["Authorization"])

	// nil 清列：全列替换语义（NULL 落库），回读 nil。
	saved = seedTemplateExt(t, repos, &domain.TemplateExt{
		TemplateID:     tpl.ID,
		CredentialType: credential.TypeCustom,
		RequestPatch:   nil,
	})
	require.Nil(t, saved.RequestPatch)
	got, err = repos.TemplateExts.GetTemplateExt(ctx, tpl.ID)
	require.NoError(t, err)
	require.Nil(t, got.RequestPatch, "nil ⇒ NULL ⇒ 回读 nil（清空语义）")

	// 存量脏值：jsonb 列本身只保证 JSON 语法合法，wire 语义非法只能来自绕过唯一
	// 写入口的 DB 直改（"非法 JSON" 在 jsonb 上物理不可写）。读路径必须静默当作
	// 未配置，而不是 500/panic。
	pool := pgSharedPool(t)
	for _, dirty := range []string{
		`"garbage"`,                              // 形状不对（JSON 标量）
		`{"headers":{"Bad Name":"v"}}`,           // wire 非法头名
		`{"formats":{"openai-chatx":{}}}`,        // 未知格式键
		`{"headers":{"Accept-Encoding":"gzip"}}`, // 违反响应编码不变量
	} {
		_, err := pool.Exec(ctx, `UPDATE template_exts SET request_patch = $1::jsonb WHERE template_id = $2`, dirty, tpl.ID)
		require.NoError(t, err)
		got, err = repos.TemplateExts.GetTemplateExt(ctx, tpl.ID)
		require.NoError(t, err, "dirty %s 不得让读路径报错", dirty)
		require.Nil(t, got.RequestPatch, "dirty %s 必须被忽略为未配置", dirty)
	}

	// 脏值不阻断其余列：同一行的 upstream_paths 照常回读。
	_, err = pool.Exec(ctx, `UPDATE template_exts SET request_patch = '"garbage"'::jsonb, upstream_paths = '{"openai-chat":"v1/chat/completions"}'::jsonb WHERE template_id = $1`, tpl.ID)
	require.NoError(t, err)
	got, err = repos.TemplateExts.GetTemplateExt(ctx, tpl.ID)
	require.NoError(t, err)
	require.Nil(t, got.RequestPatch)
	require.Equal(t, "v1/chat/completions", got.UpstreamPaths["openai-chat"])

	// jsonb 的数字规范化（已知边界，非丢精度）：jsonb 用 numeric 存数字，指数写法
	// 在 PG 侧展开成十进制（1e18 → 1000000000000000000）；值与原精度都保留，
	// 只是字面量拼写被 PG 归一。端点级 UseNumber 保证这个过程中 float64 不介入。
	expPatch, err := domain.ParseRequestPatch(json.RawMessage(`{"formats":{"anthropic":{"set":{"e18":1e18,"neg":-9223372036854775808}}}}`))
	require.NoError(t, err)
	seedTemplateExt(t, repos, &domain.TemplateExt{
		TemplateID: tpl.ID, CredentialType: credential.TypeCustom, RequestPatch: expPatch,
	})
	got, err = repos.TemplateExts.GetTemplateExt(ctx, tpl.ID)
	require.NoError(t, err)
	require.NotNil(t, got.RequestPatch)
	set := got.RequestPatch.Formats[domain.UpstreamPathAnthropic].Set
	require.Equal(t, "1000000000000000000", string(set["e18"]))
	require.Equal(t, "-9223372036854775808", string(set["neg"]))
}
