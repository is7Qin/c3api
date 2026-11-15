// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/sjson"
)

// 帧路径两处预筛的**等价性门禁**（T0，2026-11-16）。两者都是零语义损失的性能改动，
// 但"零损失"必须是被证明的，不是被相信的：
//
//	W1 模型改写 **加**了字节预筛（`model` 键的裸字节是真超集）
//	W2 终态帧判定 **删**了 memmem 预筛（`isResponsesCompleted` 已蕴含它）
//
// 门禁形态：把改动前的实现原样冻结在测试里当"参照实现"，对同一批语料逐字节对照。
// 冻结是有意的——它不是生产代码的副本，而是**改动点那一刻的语义快照**；生产实现将来
// 再怎么演进，这两条断言仍回答"当初这一步有没有改变行为"。

// rewriteResponseModelJSONPreT0 是 T0 之前 `rewriteResponseModelJSON` 的逐字冻结
// （无条件 json.Valid + 三条 scanKeyValue，无字节预筛）。
func rewriteResponseModelJSONPreT0(body []byte, model string) []byte {
	if model == "" || !json.Valid(body) {
		return body
	}
	original := body
	paths := [...]struct {
		outer []byte
		path  string
	}{
		{path: "model"},
		{outer: responseKeyBytes, path: "response.model"},
		{outer: messageKeyBytes, path: "message.model"},
	}
	for _, target := range paths {
		scope := body
		if target.outer != nil {
			start, end, ok := scanKeyValue(body, target.outer)
			if !ok {
				continue
			}
			scope = body[start:end]
		}
		start, end, ok := scanKeyValue(scope, modelKeyBytes)
		if !ok {
			continue
		}
		current := scope[start:end]
		if len(current) < 2 || current[0] != '"' || current[len(current)-1] != '"' || modelStringEquals(current, model) {
			continue
		}
		var err error
		body, err = sjson.SetBytes(body, target.path, model)
		if err != nil {
			return original
		}
	}
	return body
}

// prefilterCorpus 覆盖预筛可能误杀/误放的全部形态：转义键、值里出现 `model` 字样、
// 深层同名键、顶层非对象、损坏 JSON、BOM、CRLF、重复键、空体。
func prefilterCorpus() []string {
	pad := bytes.Repeat([]byte("x"), 4<<10)
	return []string{
		``,
		` `,
		`{}`,
		`null`,
		`[]`,
		`"model"`,
		`{"type":"response.output_text.delta","delta":"no key here"}`,
		// 键名带 \u 转义：裸键字节是 `\u006dodel`，与 `model` 不相等 ⇒ 两侧都不得改写。
		`{"\u006dodel":"upstream"}`,
		`{"response":{"\u006dodel":"upstream"}}`,
		// 值里出现 `model` 字样：预筛命中、原路径也 miss ⇒ 输出必须一致。
		`{"type":"x","delta":"the word model appears in prose"}`,
		`{"type":"x","delta":"\"model\":\"fake\""}`,
		// 三条真实路径 + 一条不得触碰的深层同名键。
		`{"keep":9007199254740993,"message":{"model":"message-upstream"},"model":"root-upstream","response":{"model":"response-upstream"},"other":{"model":"untouched"}}`,
		`{"a":{"b":{"model":"deep"}}}`,
		// 已经等于目标值（含 `\/` 与 `\u` 转义形态）：必须原样复用入参。
		`{"model":"client\/model","response":{"model":"\u0063lient/model"},"message":{"model":"client/model"}}`,
		// 非字符串值：null / number / object / array。
		`{"model":null}`,
		`{"model":1}`,
		`{"response":{"model":{}}}`,
		`{"message":{"model":[]}}`,
		// 损坏 / 非对象根。
		`{"model":"upstream"`,
		`[{"model":"upstream"}]`,
		`{"model":"upstream"}}`,
		// BOM + CRLF + 重复键。
		"\xef\xbb\xbf{\"model\":\"upstream\"}",
		"{\r\n\"model\":\"upstream\"\r\n}",
		`{"model":"a","model":"b"}`,
		// 大帧（内容帧 + 带键大帧）：预筛/全扫的分歧只可能在大帧上暴露。
		`{"type":"response.output_text.delta","delta":"` + string(pad) + `"}`,
		`{"type":"response.created","response":{"model":"upstream","pad":"` + string(pad) + `"}}`,
	}
}

// TestRewriteModelPrefilterEquivalence W1：加预筛前后输出**逐字节相同**。
func TestRewriteModelPrefilterEquivalence(t *testing.T) {
	models := []string{"", "client-model", "client/model"}
	for _, body := range prefilterCorpus() {
		for _, m := range models {
			in := []byte(body)
			want := rewriteResponseModelJSONPreT0(in, m)
			got := rewriteResponseModelJSON(in, m)
			require.Equal(t, string(want), string(got), "body=%q model=%q", body, m)
			// 未改写时必须**复用入参**（零拷贝契约，同 TestResponseModelRewriteJSONNoOpsReuseInputWithoutAllocations）。
			if len(in) > 0 && len(want) == len(body) && bytes.Equal(want, in) {
				require.Same(t, &in[0], &got[0], "未改写必须返回入参本身：body=%q model=%q", body, m)
			}
		}
	}
}

// TestRewriteModelPrefilterReusesLargeFrameWithoutModel 大帧（1 MiB、不含 `model`
// 字节）必须原样复用入参：这既是零拷贝契约，也是 W1 快路径的**可观察**前提——若预筛
// 被误删，本断言仍会通过（语义没变），但 BenchmarkFrameScanModelRewrite 的
// `prefilter-miss` 子项会立刻掉到 `old-path` 量级；性能回归由那条基准承担，行为回归
// 由 TestRewriteModelPrefilterEquivalence 承担。两条各管一半，不重叠。
func TestRewriteModelPrefilterReusesLargeFrameWithoutModel(t *testing.T) {
	body := benchFrame(1 << 20)
	require.False(t, bytes.Contains(body, modelKeyBytes), "内容帧不应含 model 键字节")

	got := rewriteResponseModelJSON(body, "client-model")

	require.Same(t, &body[0], &got[0])
	allocs := testing.AllocsPerRun(20, func() {
		modelRewriteSink = rewriteResponseModelJSON(body, "client-model")
	})
	require.Zero(t, allocs)
}

// TestCompletedPrefilterRedundant W2：删掉 memmem 预筛前后判定**同值**。
//
// 断言的是蕴含关系本身：`isResponsesCompleted(f) ⇒ bytes.Contains(f, completedTypeBytes)`。
// 只要它成立，`A && B ≡ B`。
func TestCompletedPrefilterRedundant(t *testing.T) {
	pad := string(bytes.Repeat([]byte("z"), 8<<10))
	cases := []string{
		``,
		`{}`,
		`null`,
		`[]`,
		`{"type":"response.completed"}`,
		`{"type":"response.completed","response":{"usage":{"total_tokens":1}}}`,
		`{"type":"response.created"}`,
		`{"type":"response.output_text.delta","delta":"` + pad + `"}`,
		// type 不靠前：预筛会命中、精确判定仍须 true。
		`{"response":{"pad":"` + pad + `"},"type":"response.completed"}`,
		// 值里出现 response.completed 字样但 type 不是它。
		`{"type":"response.output_text.delta","delta":"response.completed"}`,
		// 转义形态的 type 值：裸字节不等 ⇒ 两侧都必须 false。
		`{"type":"response\u002ecompleted"}`,
		`{"type":"RESPONSE.COMPLETED"}`,
		`{"type":1}`,
		`{"type":null}`,
		`{"type":"response.completed"`,
		// 大小写与空白变体。
		"{\"type\" : \"response.completed\"}",
	}
	for _, f := range cases {
		data := []byte(f)
		prefilter := bytes.Contains(data, completedTypeBytes)
		exact := isResponsesCompleted(data)
		require.Equal(t, prefilter && exact, exact, "frame=%q", f)
		// 蕴含本身：精确判定为真时，子串必然出现。
		if exact {
			require.True(t, prefilter, "精确判定为真但子串未出现：frame=%q", f)
		}
	}
}
