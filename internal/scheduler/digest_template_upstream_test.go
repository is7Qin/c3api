// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package scheduler

import (
	"encoding/json"
	"reflect"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
)

// digestTemplateUpstream 的字节门禁（spec custom-request-patch §6/§17.2）。
//
// 判据分三层：
//  1. **KAT 冻结字节**：空摘要、Paths-only 摘要、Patch-only 摘要、Paths+Patch
//     摘要各一枚硬编码 hex（按 8 字节大端长度前缀 + sha256 在 Node 里独立算的，
//     不是把实现抄一遍）——Paths-only 必须与 P1 的 digestUpstreamPaths **逐字节
//     相同**（既有 golden 不动），而 nil 等价于空集。
//  2. **敏感度/顺序无关**：Paths 变 / Patch 变 / 两者都变 ⇒ 摘要变；Paths 与
//     Patch 的 map 遍历序、delete 书写序都不影响摘要（CanonicalBytes 已排序）。
//  3. **不新增摘要槽位**：tplDigests 仍 5 项 [32]byte（Patch 折叠进
//     upstreamPathsKey 这一枚，spec §6 评审 oracle N2）。
//
// 判据故意不含「用写入前 bytes 算摘要」的用例：那份 bytes 在 jsonb 规范化后与
// 读回值不同（§17.2），只在真实 PG 链路上才有意义——见
// request_patch_snapshot_pg_test.go 的读回稳定性用例。

// 以下 hex 是 P1 算法（digestParts 的 8 字节大端长度前缀 + 键值对）对空集/
// 单路径/单 patch 的冻结字节：Paths-only 与 P1 的 digestUpstreamPaths 逐字节一致
// 是本文件的承重断言。
const (
	wantEmptyUpstreamDigest = "af5570f5a1810b7af78caf4bc70a660f0df51e42baf91d4de5b2328de0e83dfc"
	wantPathsOnlyDigest     = "4e8800a2e358d7ac60377498f16b5b69d731fd5c1f547611c8080b19fb1a97e9"
	wantPatchOnlyDigest     = "00827f366b634da0eb4461031184b70c1a070129cb1e782a031b1da64f5445e8"
	wantPathsPatchDigest    = "3a39f024426367af6a98049709b617da46f1d1368c8ef108998808fb5ea2496e"
)

func mustPatch(t *testing.T, wire string) *domain.RequestPatch {
	t.Helper()
	p, err := domain.ParseRequestPatch(json.RawMessage(wire))
	require.NoError(t, err, "wire 必须能过 ParseRequestPatch：%s", wire)
	require.NotNil(t, p, "非空 wire 必须解析出非 nil patch：%s", wire)
	return p
}

// TestDigestTemplateUpstreamKAT 冻结摘要字节：nil/空 = P1 空集摘要；Paths-only =
// P1 digestUpstreamPaths 的字节；Patch 作为一个 length-prefixed part 追加。
func TestDigestTemplateUpstreamKAT(t *testing.T) {
	empty := digestTemplateUpstream(nil)
	require.Equal(t, wantEmptyUpstreamDigest, hex32(empty),
		"nil up 的摘要必须等于 P1 前空 UpstreamPaths map 的摘要（既有 golden 不动）")

	paths := map[string]string{"openai-chat": "v1/chat/completions"}
	require.Equal(t, wantPathsOnlyDigest, hex32(digestTemplateUpstream(&domain.TemplateUpstream{Paths: paths})),
		"Paths-only 摘要必须与 P1 digestUpstreamPaths 逐字节一致（字节口径保留）")

	// nil/空的所有形态收敛到同一枚摘要：nil up、空 TemplateUpstream、空 Paths、
	// nil Patch 都必须是「未配置」。
	for name, up := range map[string]*domain.TemplateUpstream{
		"nil":                nil,
		"empty struct":       {},
		"empty paths map":    {Paths: map[string]string{}},
		"nil paths, nil pch": {Paths: nil, Patch: nil},
	} {
		require.Equal(t, empty, digestTemplateUpstream(up), "%s 必须与 nil 同摘要", name)
	}

	patchOnly := mustPatch(t, `{"headers":{"X-A":"1"}}`)
	require.Equal(t, wantPatchOnlyDigest, hex32(digestTemplateUpstream(&domain.TemplateUpstream{Patch: patchOnly})),
		"Patch-only 摘要：CanonicalBytes 作为一个 part 追加（KAT 冻结）")
	require.NotEqual(t, empty, digestTemplateUpstream(&domain.TemplateUpstream{Patch: patchOnly}),
		"配了 patch 与未配置必须不同（否则改 patch 不换叶）")

	both := &domain.TemplateUpstream{Paths: paths, Patch: patchOnly}
	require.Equal(t, wantPathsPatchDigest, hex32(digestTemplateUpstream(both)),
		"Paths + Patch 同槽摘要（KAT 冻结）")
}

// TestDigestTemplateUpstreamSensitivity 敏感度与顺序无关。
func TestDigestTemplateUpstreamSensitivity(t *testing.T) {
	basePaths := map[string]string{"openai-chat": "v1/chat/completions"}
	basePatch := mustPatch(t, `{"headers":{"X-A":"1"},"formats":{"openai-chat":{"set":{"metadata.k":1}}}}`)

	base := digestTemplateUpstream(&domain.TemplateUpstream{Paths: basePaths, Patch: basePatch})

	// Paths 变 / Patch 变 / 两者都变 ⇒ 摘要变。
	pathsChanged := digestTemplateUpstream(&domain.TemplateUpstream{
		Paths: map[string]string{"openai-chat": "v2/chat/completions"}, Patch: basePatch,
	})
	patchChanged := digestTemplateUpstream(&domain.TemplateUpstream{
		Paths: basePaths, Patch: mustPatch(t, `{"headers":{"X-A":"2"},"formats":{"openai-chat":{"set":{"metadata.k":1}}}}`),
	})
	bothChanged := digestTemplateUpstream(&domain.TemplateUpstream{
		Paths: map[string]string{"openai-chat": "v2/chat/completions"},
		Patch: mustPatch(t, `{"headers":{"X-A":"2"},"formats":{"openai-chat":{"set":{"metadata.k":1}}}}`),
	})
	require.NotEqual(t, base, pathsChanged, "Paths 变化必须改摘要（改路径换新计划）")
	require.NotEqual(t, base, patchChanged, "Patch 变化必须改摘要（改 patch 换新计划）")
	require.NotEqual(t, pathsChanged, patchChanged, "任一维度不同都不得撞摘要")
	require.NotEqual(t, base, bothChanged)

	// Paths 顺序无关（map 遍历序）；同内容不同 map 实例同摘要。
	require.Equal(t, digestTemplateUpstream(&domain.TemplateUpstream{Paths: map[string]string{"a": "1", "b": "2"}}),
		digestTemplateUpstream(&domain.TemplateUpstream{Paths: map[string]string{"b": "2", "a": "1"}}),
		"Paths 的 map 遍历序不得影响摘要")

	// Patch 的书写序无关：header/格式层/set 键序、delete 书写顺序都由 Parse 深归一
	// + CanonicalBytes 排序收敛。
	require.Equal(t,
		digestTemplateUpstream(&domain.TemplateUpstream{Patch: mustPatch(t,
			`{"headers":{"X-B":"2","X-A":"1"},"formats":{"openai-chat":{"delete":["b.path","a.path"],"set":{"z":2,"a":1}}}}`)}),
		digestTemplateUpstream(&domain.TemplateUpstream{Patch: mustPatch(t,
			`{"headers":{"X-A":"1","X-B":"2"},"formats":{"openai-chat":{"set":{"a":1,"z":2},"delete":["a.path","b.path"]}}}`)}),
		"Patch 的 map/delete 书写顺序不得影响摘要（Parse 深归一 + CanonicalBytes 排序）")

	// 同一份 wire 解析两次 ⇒ 同一枚摘要（Parse 幂等是 §17.2 的前提）。
	wire := `{"headers":{"Authorization":"Bearer {api_key}"},"formats":{"openai-chat":{"set":{"big":9007199254740993,"e18":1e18}}}}`
	require.Equal(t, digestTemplateUpstream(&domain.TemplateUpstream{Patch: mustPatch(t, wire)}),
		digestTemplateUpstream(&domain.TemplateUpstream{Patch: mustPatch(t, wire)}),
		"同一 wire 两次 Parse 必须同摘要（深归一幂等）")

	// 键值拼接歧义：与 P1 同一判据（长度前缀），Path 边界不可省。
	require.NotEqual(t,
		digestTemplateUpstream(&domain.TemplateUpstream{Paths: map[string]string{"ab": "c"}}),
		digestTemplateUpstream(&domain.TemplateUpstream{Paths: map[string]string{"a": "bc"}}),
		"Paths 键值边界不可省（拼接歧义）")
}

// TestDigestTemplateUpstreamFoldsIntoPlanKey 证明摘要**真的进键**：newSnapshotStatic
// 的 upstreamPathsKey 随 Paths/Patch 变化，而其余 key 字段不动时整枚 planKey 也随之
// 变化（否则复用旧叶 ⇒ 旧 patch/旧路径继续生效）。
func TestDigestTemplateUpstreamFoldsIntoPlanKey(t *testing.T) {
	tpl := tplWith(domain.FormatOpenAIChat, []string{"m-a"})
	acc := *acc(9, tpl, 4)

	build := func(up *domain.TemplateUpstream) planKey {
		tp := *tpl
		tp.Upstream = up
		return planKeyOf(newSnapshotStatic(acc, &tp, nil, directEgress(), nil))
	}
	paths := map[string]string{"openai-chat": "v1/chat/completions"}
	patch := mustPatch(t, `{"headers":{"X-A":"1"}}`)

	plain := build(nil)
	pathsOnly := build(&domain.TemplateUpstream{Paths: paths})
	patchOnly := build(&domain.TemplateUpstream{Patch: patch})
	both := build(&domain.TemplateUpstream{Paths: paths, Patch: patch})

	require.Equal(t, wantEmptyUpstreamDigest, hex32(plain.upstreamPathsKey),
		"未配置时键里的槽位必须是空集摘要（P1 golden）")
	require.Equal(t, wantPathsOnlyDigest, hex32(pathsOnly.upstreamPathsKey))
	require.Equal(t, wantPatchOnlyDigest, hex32(patchOnly.upstreamPathsKey))
	require.Equal(t, wantPathsPatchDigest, hex32(both.upstreamPathsKey))

	require.NotEqual(t, plain, pathsOnly, "配路径必须改 planKey（否则复用旧叶，路径不生效）")
	require.NotEqual(t, plain, patchOnly, "配 request_patch 必须改 planKey（否则复用旧叶，旧 patch 继续生效）")
	require.NotEqual(t, pathsOnly, patchOnly)
	require.NotEqual(t, pathsOnly, both)

	// 同内容不同对象/实例 ⇒ 同键（摘要按值比较，指针不入键）。
	require.Equal(t, both, build(&domain.TemplateUpstream{
		Paths: map[string]string{"openai-chat": "v1/chat/completions"},
		Patch: mustPatch(t, `{"headers":{"X-A":"1"}}`),
	}), "同内容不同实例必须同键（摘要值语义）")
}

// TestTplDigestsStaysFiveSlots 钉住 spec §6 评审 oracle N2：request_patch 折叠进
// 既有 upstreamPathsKey 槽位，tplDigests 仍 5 项 [32]byte（不新增第 6 枚）。
func TestTplDigestsStaysFiveSlots(t *testing.T) {
	typ := reflect.TypeOf(tplDigests{})
	require.Equal(t, reflect.Struct, typ.Kind())
	require.Equal(t, 5, typ.NumField(), "tplDigests 必须仍 5 项（Patch 并入 upstreamPathsKey，不新增摘要槽）")
	for i := 0; i < typ.NumField(); i++ {
		require.Equal(t, reflect.Array, typ.Field(i).Type.Kind(), "第 %d 项必须是数组摘要", i)
		require.Equal(t, 32, typ.Field(i).Type.Len(), "第 %d 项必须是 [32]byte（既有 32B 槽位）", i)
	}
	require.Equal(t, "upstreamPathsKey", typ.Field(4).Name,
		"custom 上游修饰的承载槽位名保留（语义扩为 TemplateUpstream）")
}

// hex32 把 [32]byte 渲染成小写 hex（KAT 断言用）。
func hex32(v [32]byte) string {
	const hexDigits = "0123456789abcdef"
	out := make([]byte, 64)
	for i, b := range v {
		out[i*2] = hexDigits[b>>4]
		out[i*2+1] = hexDigits[b&0x0f]
	}
	return string(out)
}

// TestPlanKeySizeStays392 钉住 P2 前后 planKey 尺寸不变（spec §6/§19 既有数字档）：
// request_patch 折叠进既有 32B 摘要槽，没有新增键字段（22 项字段集另由
// static_key_test.go 的 declaredPlanKeyFields 钉住）——尺寸不动的机械证据。
func TestPlanKeySizeStays392(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("64 位布局门禁")
	}
	require.Equal(t, uintptr(392), unsafe.Sizeof(planKey{}),
		"P2 不得改变 planKey 尺寸：request_patch 必须折叠进 upstreamPathsKey，不新增字段")
}
