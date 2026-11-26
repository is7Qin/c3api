// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package probe

// request_patch 数据面接线（spec custom-request-patch §7.3/§10 P5）：
//
// probe 是 custom 模板的诊断面，必须与生产 HTTP 面**同一份 wire 语义**，否则
// 探测结果不能作为配置生效的证据。两个 helper 都是**零逻辑委托**——层选择 / 逐层
// 应用（两层 / Host 三态 / Del / 运行期跳 Accept-Encoding / {api_key} 展开）与
// body 执行器全部住在 internal/domain，和 proxy 共用同一份实现（禁止在 probe
// 复制判据）：
//
//   - 格式键与 URL 路径**同一把键**（spec §7.3 定稿）：调用点用的 key 就是
//     domain.ResolveUpstreamPath 用的 UpstreamPathKey——chat→openai-chat、
//     responses→openai-responses、responses-ws→openai-responses-ws（probe 的 WS
//     格式实际走 HTTP responses 语义，键仍与 path 一致）、anthropic→anthropic、
//     search→openai-search、images 按 ImageMode→openai-images.generations/edits；
//   - {api_key} 用 spec.Credential（**原始裸 key**，与 proxy 同一来源语义；禁止从
//     已格式化的 Authorization/x-api-key 反解）；
//   - images edits 的 multipart 体在 domain.ApplyRequestPatch 里按非 JSON 容器
//     结构性跳过（只注入头，不 WARN）；
//   - 无配置（spec.Upstream == nil / 无 patch）⇒ nil 装饰器 / 原 slice，零分配。
//
// 失败语义（spec §7.4）：JSON 但 sjson 残余失败（保存期已拦；只可能是手工改库的
// 脏值）⇒ 原 body 裸透传、探测不中止、不分类为 probe 失败。观测面差异：probe 包
// **不写日志**（包注释不变量：日志由 service 统一记一条 account_probe），所以本面
// 不记 WARN——这是与 proxy 面的唯一差异（proxy 有固定文案限频 WARN）。

import (
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
)

// probeHeaderDecorator 构造 probe 的 HTTP 出站头装饰器：与 proxy 完全同一个
// domain 构造（唯一实现），apiKey 固定取 spec.Credential 原值。
func probeHeaderDecorator(spec domain.ProbeSpec, key domain.UpstreamPathKey) func(*http.Request) {
	return domain.RequestPatchHeaderDecorator(spec.Upstream, key, spec.Credential)
}

// probeBodyPatch 在既有请求体合成/变换之后、出站之前应用 body patch：
// 选择 + 执行走 domain.ApplyRequestPatch（唯一执行器）；残余失败原样透传。
func probeBodyPatch(spec domain.ProbeSpec, key domain.UpstreamPathKey, body []byte) []byte {
	out, err := domain.ApplyRequestPatch(spec.Upstream, key, body)
	if err != nil {
		return body
	}
	return out
}
