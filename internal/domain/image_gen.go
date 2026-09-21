// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

// 生图领域类型（SDK 接入契约——适配层 domain↔codexsdk 转换的网关侧同构面，
// 字段形态对齐 sdk-image-gen-spec：ImageGenParams/ImageRef/ImageResponse/Image/
// ImageUsage/ImageStreamEvent）。网关从 HTTP 请求 JSON body / multipart form
// 解析参数传入，SDK 不做 HTTP 协议解析；响应统一走本组类型口径 → 网关据此
// 计费（data 长 = 张数、usage 提取 image tokens）与序列化转发——与 API-key
// 直连响应同一口径（网关统一计费逻辑）。

// ImageGenParams 生图参数（generations/edits 共参；codex-rs 实证收敛参数集）。
type ImageGenParams struct {
	Model      string     // 生图模型（gpt-image-2 等；必填）
	Prompt     string     // 必填
	N          *int       // nil = 1
	Size       *string    // nil = "auto"
	Quality    *string    // nil = "auto"；枚举 low|medium|high|auto
	Background *string    // nil = "auto"；枚举 transparent|opaque|auto
	Images     []ImageRef // edits 输入图片（≤5）；generations 恒空
}

// ImageRef 单张输入图（edits）。
type ImageRef struct {
	ImageURL *string // 完整 URL 或 base64 data URL
	Raw      []byte  // 原始文件字节（multipart 上传形态）→ SDK 内部转 data URL
}

// ImageResponse 标准 ImagesResponse（对齐 OpenAI /v1/images/* 响应）——网关
// 直接据此计费（Data 长 = 张数、Usage 提取 image_tokens）。
type ImageResponse struct {
	Created      int64
	Background   *string
	Data         []Image
	OutputFormat *string
	Quality      *string
	Size         *string
	Usage        *ImageUsage // 上游未提供 → nil（网关 per-image 分量兜底）
}

// Image 单张生成结果（实证：b64_json 为原始 PNG base64，无 data URL 前缀）。
type Image struct {
	B64JSON *string
}

// ImageUsage 生图 token 用量（上游 input/output_tokens_details.image_tokens
// 提取为平铺四字段；JSON tag 对齐 codex-sdk ImageUsage——completed SSE 帧
// usage 字段映射 = JSON tag 直透，wire 形态定死）。
type ImageUsage struct {
	InputTokens       int64 `json:"input_tokens"`
	InputImageTokens  int64 `json:"input_image_tokens"` // input_tokens_details.image_tokens
	OutputTokens      int64 `json:"output_tokens"`
	OutputImageTokens int64 `json:"output_image_tokens"` // output_tokens_details.image_tokens
}

// ImageStreamEventType 流式事件类型（wire 事件名：上游 SSE 帧 type 字段值与
// codex-sdk 合成流式事件 Type 值同值域）。类型化后 SDK 升级改事件
// 名 → 适配层显式映射未知 Warn + 跳过（不静默透传落账 0 张）；switch 消费侧
// 编译期暴露全部调用点。
type ImageStreamEventType string

// ImageStreamEvent 事件类型常量（GenerateImageStream 合成流式事件 + 直连上游
// SSE 事件名——wire 事件名四处生产字面量收敛于此：image_usage.go:65 /
// image_gen.go:66 / caller_images_stream.go buildCompletedFrame / billing 旁路）。
const (
	// ImageStreamEventKeepalive 保活事件（生成等待期间每 60s 一个；
	// B64JSON/Usage 恒 nil）——网关收到首个事件即发 SSE 响应头，keepalive
	// 保证 CF 120s 响应头超时门槛内必有字节流（524 免疫）。
	ImageStreamEventKeepalive ImageStreamEventType = "keepalive"
	// ImageStreamEventCompleted 每张图一个 completed 事件（带 b64_json；
	// usage 仅最后一个事件携带）。
	ImageStreamEventCompleted ImageStreamEventType = "image_generation.completed"
	// ImageStreamEventEditCompleted edits 端点完成事件（直连上游 SSE 形态；
	// codex-sdk 合成流式不产出——edits 端点也合成 generations.completed）。
	ImageStreamEventEditCompleted ImageStreamEventType = "image_edit.completed"
)

// ImageStreamEvent 流式事件（codex-sdk 合成流式产出——网关零合成逻辑，只做
// SSE 透传：keepalive → ": ping" 注释行；completed → SSE 事件帧）。partial_image
// 不合成（无 wire 来源）。
type ImageStreamEvent struct {
	Type    ImageStreamEventType // 见上常量组
	B64JSON *string              // completed：原始 PNG base64；keepalive 恒 nil
	Usage   *ImageUsage          // 仅最后一个 completed 事件携带；keepalive 恒 nil
}
