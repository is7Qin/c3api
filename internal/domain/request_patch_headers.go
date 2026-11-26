// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

// request_patch 的**数据面执行核心**（spec custom-request-patch §2.4/§2.5/§7.1-
// §7.4）：头的两层应用与 body 的格式层选择各只有一份判据，HTTP（proxy/probe）、
// WS 握手（proxy）三个出站面共用。
//
// 为什么住在 domain：三个消费面唯一共同依赖的是本包的值类型（TemplateUpstream /
// RequestPatch），而 probe 不能 import proxy、proxy 也不该为 probe 反向依赖；
// 判据留在任一执行面都会迫使另一面复制一份"看起来等价"的实现——那正是本 feature
// 最危险的漂移点（配置对某个上游静默失效，没有任何行为测试会红）。本文件只做纯
// 数据变换（不 IO、不加锁、不读全局、不写日志），与 ApplyRequestBodyPatch 同性质。

import (
	"net/http"
	"strings"
)

// apiKeyPlaceholder 是 request_patch 头值里唯一保留的占位符（spec §2.4）：
// 运行期用当次 attempt 的账号上游 key 原值展开；body 值不做替换（字面量），
// 避免 key 被写进提示/日志面。
const apiKeyPlaceholder = "{api_key}"

// RequestPatchHeaderLayers 取该出站格式键的两层头（全局层、格式层）。这是
// 「哪些头层属于本次请求」的**唯一判据**：
//   - up == nil / Patch == nil（未配置）⇒ (nil, nil)（调用方据此走 nil 快路径）；
//   - 只有全局层命中 ⇒ (global, nil)；只有格式层命中 ⇒ (nil, format)；
//   - 两层都无头（empty headers，解析期已归一并删除空层）⇒ (nil, nil)。
//
// 返回的是快照内 map 的**只读引用**：不复制、不排序（头应用逐键 Set/Del，与
// map 迭代序无关）；调用方不得写。
func RequestPatchHeaderLayers(up *TemplateUpstream, key UpstreamPathKey) (global, format map[string]string) {
	if up != nil && up.Patch != nil {
		global = up.Patch.Headers
		if fp, ok := up.Patch.Formats[key]; ok {
			format = fp.Headers
		}
	}
	if len(global) == 0 && len(format) == 0 {
		return nil, nil
	}
	return global, format
}

// ApplyRequestPatchHeaderLayers 应用**两层**头（全局层 → 格式层，后者逐键覆盖）：
//   - 每层内非空值 Set、空串 "" Del；
//   - Host 特例：写 *host（HTTP 的 req.Host / WS 的 DialOptions.Host），**按层
//     三态**——非空覆盖、空值清除 ⇒ 出站回退 URL.Host；绝不写 header map
//     （net/http 忽略 Header["Host"]，coder/websocket 只在 DialOptions.Host 非空
//     时覆盖）；
//   - Accept-Encoding 运行期永不应用（保存期已 400 的响应编码不变量兜底；这里防御
//     手工改库的脏值）；
//   - 头值里的 {api_key} 用 apiKey 原值展开（可多次；无占位符时 strings.ReplaceAll
//     零分配返回原串）。
//
// 头名在保存期已归一为 http.CanonicalHeaderKey（ParseRequestPatch），此处直接
// 按规范形比较，不再二次归一。
func ApplyRequestPatchHeaderLayers(header http.Header, host *string, global, format map[string]string, apiKey string) {
	applyRequestPatchHeaderLayer(header, host, global, apiKey)
	applyRequestPatchHeaderLayer(header, host, format, apiKey)
}

// applyRequestPatchHeaderLayer 应用**一层**头语义（全局层/格式层共用；层内逐键）。
func applyRequestPatchHeaderLayer(header http.Header, host *string, headers map[string]string, apiKey string) {
	for name, value := range headers {
		switch name {
		case "Host":
			*host = expandAPIKey(value, apiKey)
		case "Accept-Encoding":
			continue
		default:
			if value == "" {
				header.Del(name)
				continue
			}
			header.Set(name, expandAPIKey(value, apiKey))
		}
	}
}

// RequestPatchHeaderDecorator 构造一次 HTTP 出站请求的头装饰器（spec §7.1）：
// **proxy 的 15 个装饰器接线位点（11 个 aiclient 出站调用 + 4 个 rawJSON 第 8 参）
// 中 HTTP 侧的 14 处与 probe 的 5 个 factory 调用共用这一个构造**——其中 13 处直接
// 构造（9 个 aiclient HTTP 出站调用 + 4 个 rawJSON 第 8 参），rawJSON 内部的 RawPost
// 只原样透传该装饰器；WS 拨号的 1 处改用 wsHeaderDecorator（层判据仍是同一份）。
//   - 无头配置 ⇒ nil（aiclient 快路径，零分配）；
//   - 否则返回闭包：两层头应用到 req.Header / req.Host（判据见
//     ApplyRequestPatchHeaderLayers）。
//
// apiKey 必须来自当次 attempt 的**原始凭据**（proxy 的 credentialFor(sel) /
// probe 的 spec.Credential），不得从 Authorization / "Bearer "+key 反解。
// 调用时机由 aiclient 保证：网关注入完自己的头（Content-Type/auth/Accept/
// Anthropic-Version）**之后**才调装饰器（后写覆盖赢，可覆盖/删除网关默认头）。
func RequestPatchHeaderDecorator(up *TemplateUpstream, key UpstreamPathKey, apiKey string) func(*http.Request) {
	global, format := RequestPatchHeaderLayers(up, key)
	if len(global) == 0 && len(format) == 0 {
		return nil
	}
	return func(req *http.Request) {
		ApplyRequestPatchHeaderLayers(req.Header, &req.Host, global, format, apiKey)
	}
}

// RequestPatchFormat 取该出站格式的格式层 patch（值拷贝；不存在 ⇒ ok=false）。
// 返回**值**而不是指针：FormatPatch 只有两个 map + 一个 slice（24B 头），值传递
// 避免取 map 元素地址造成的堆逃逸；执行器按值读。
func RequestPatchFormat(up *TemplateUpstream, key UpstreamPathKey) (FormatPatch, bool) {
	if up == nil || up.Patch == nil || len(up.Patch.Formats) == 0 {
		return FormatPatch{}, false
	}
	fp, ok := up.Patch.Formats[key]
	return fp, ok
}

// ApplyRequestPatch 是 body 面的**选择 + 执行**唯一入口（域内执行器仍是
// ApplyRequestBodyPatch）：proxy（HTTP/WS 帧经 bodyPatch）与 probe（合成体）共用，
// 保证"哪个格式层 + 空 patch ⇒ 原切片 + 设计性跳过 + 残余失败返回原 body+error"
// 只有一份选择逻辑。
//
// 返回值语义：
//   - 无该格式层 / 空 patch / body 非 JSON 容器（multipart edits、二进制）⇒
//     (body, nil)：**设计性跳过**，不是失败，调用方不得 WARN；
//   - JSON 但 sjson 残余失败 ⇒ (body, err)：调用方裸透传 + 按 §7.4 的观测语义处理
//     （三面共用一个固定文案限频 WARN 实现：HTTP/WS 面直接调用，probe 面经构造期
//     注入的 PatchWarn 回调——probe 包自身不写日志），**不中止请求**。
func ApplyRequestPatch(up *TemplateUpstream, key UpstreamPathKey, body []byte) ([]byte, error) {
	fp, ok := RequestPatchFormat(up, key)
	if !ok {
		return body, nil
	}
	return ApplyRequestBodyPatch(body, &fp)
}

// expandAPIKey 展开头值中的 {api_key} 占位符（可多次）；无占位符时零分配
// （strings.ReplaceAll 内部 count==0 直接返回原串）。
func expandAPIKey(value, apiKey string) string {
	return strings.ReplaceAll(value, apiKeyPlaceholder, apiKey)
}
