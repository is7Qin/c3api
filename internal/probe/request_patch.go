// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package probe

// request_patch 数据面接线（spec custom-request-patch §7.3/§10 P5）：
//
// probe 是 custom 模板的诊断面，必须与生产 HTTP 面**同一份 wire 语义**，否则
// 探测结果不能作为配置生效的证据。两个入口都是**零逻辑委托**——层选择 / 逐层
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
//     结构性跳过（只注入头；不是失败，不触观测回调）；
//   - 无配置（spec.Upstream == nil / 无 patch）⇒ nil 装饰器 / 原 slice，零分配。
//
// 失败语义（spec §7.4/§8）：JSON 但 sjson 残余失败（保存期已拦；只可能是手工改库
// 的脏值）⇒ 原 body 裸透传、探测不中止、不分类为 probe 失败。可观测面走**构造期
// 注入的 PatchWarn 回调**：组合根把 HTTP/WS 面同一个固定文案限频 WARN 实现注入
// 进来（同一张 template_id+format 限频表），三面同一套语义。probe 包自身仍
// **不写日志、不 import logx、不新增架构边**——本包只把「残余失败发生了」这一
// 事实交给注入的 func。

import (
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
)

// probeHeaderDecorator 构造 probe 的 HTTP 出站头装饰器：与 proxy 完全同一个
// domain 构造（唯一实现），apiKey 固定取 spec.Credential 原值。
func probeHeaderDecorator(spec domain.ProbeSpec, key domain.UpstreamPathKey) func(*http.Request) {
	return domain.RequestPatchHeaderDecorator(spec.Upstream, key, spec.Credential)
}

// applyBodyPatch 是三个静态族探测路径（覆盖 5 个 factory 调用）**唯一**的 body
// patch 入口，在既有请求体合成/变换之后、出站之前应用：
//   - 选择 + 执行走 domain.ApplyRequestPatch（唯一执行器）；
//   - 无该格式层 / 空 patch / 非 JSON 容器（images edits 的 multipart 等）⇒ 原
//     slice：设计性跳过不是失败，不触回调、零分配；
//   - JSON 但 sjson 残余失败 ⇒ 原 body 裸透传、探测不中止，并调用构造期注入的
//     PatchWarn(spec.TemplateID, key) 恰好一次（nil = 不告警）。本包不写日志。
func (p *Prober) applyBodyPatch(spec domain.ProbeSpec, key domain.UpstreamPathKey, body []byte) []byte {
	out, err := domain.ApplyRequestPatch(spec.Upstream, key, body)
	if err == nil {
		return out
	}
	if p != nil && p.patchWarn != nil {
		p.patchWarn(spec.TemplateID, key)
	}
	return body
}
