// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/is7qin/c3api/internal/credential"
	"github.com/is7qin/c3api/pkg/egress"
)

// account-model-test（单账号单模型探测）的共享数据形状与硬上限。探测本身是
// 只读诊断观测：不落库、不计费、不进调度/规则/失败统计。service 负责组装与
// 校验，internal/probe 负责执行，internal/sdkbridge 只承载 codex SDK 机制。
const (
	// ProbeMaxBodyBytes 上游响应体硬上限（2 MiB）。transport 层对非 2xx 截断到
	// 这里；2xx 允许读取到 +1 字节以发现溢出（见 internal/probe 的 capTransport
	// 与 codex 扫描器）。文本/SSE/search 使用本上限。
	ProbeMaxBodyBytes = 2 << 20
	// ProbeMaxImageResponseBytes images 生图响应体上限（8 MiB）。请求固定
	// response_format=b64_json，4 MiB 图片 base64 后约 5.33 MiB；2 MiB 会把正常
	// 生图结果误判为 response exceeds 2 MiB。8 MiB 覆盖单张 4 MiB 图片 + JSON
	// 包装，同时仍为并发探测保留硬上限。
	ProbeMaxImageResponseBytes = 8 << 20
	// ProbeTimeout 单次探测总期限（常量，不开配置键）。只由 service 在探测入口
	// 创建，prober 不再自行包一层。
	ProbeTimeout = 30 * time.Second
	// ProbeMaxConcurrency 进程内探测执行槽上限（service 准入；多实例不协调）。
	ProbeMaxConcurrency = 4
	// ProbeMaxWaiters 等待执行槽的请求上限：超出立即返回 probe busy，避免
	// 无限 waiter 各自保留请求体/模型串造成内存放大。
	ProbeMaxWaiters = 16
	// ProbeMaxModelLen 请求模型名最大字节数：通配模板允许自由输入，必须在上游
	// 组装/排队前限制长度（否则 1 MiB 模型串可被大量请求保留）。
	ProbeMaxModelLen = 256
	// ProbeErrorMaxLen / ProbeDetailMaxLen 结果文本截断上限（分类/错误码收敛后截断）。
	ProbeErrorMaxLen  = 1024
	ProbeDetailMaxLen = 2048
	// ProbeMaxJSONDepth 上游 JSON 深度上限（spec §5.2）。gjson.ValidBytes/
	// GetBytes 对 `{`/`[` 按层递归，2 MiB 的深层嵌套可耗尽数百 MB 内存；任何
	// 上游 body / SSE 事件在交给 gjson 前必须先经 ShallowJSON 线性扫描。
	ProbeMaxJSONDepth = 128
	// ProbeMaxInputLen 探测自定输入的 UTF-8 **字节**上限（schema maxLength 同值、
	// 运行时按字节而非 rune 校验）。
	ProbeMaxInputLen = 8192
	// ProbeMaxRequestBytes 探测端点请求体上限（8 MiB；其他管理端点仍走 1 MiB）。
	ProbeMaxRequestBytes = 8 << 20
	// ProbeMaxImageSourceBytes image_url 字符串的 UTF-8 字节上限（6 MiB）。
	ProbeMaxImageSourceBytes = 6 << 20
	// ProbeMaxImageBytes edits 取图原始字节上限与图片预览合计上限（4 MiB；
	// 单张预览解码上限复用 ProbeMaxBodyBytes=2 MiB）。
	ProbeMaxImageBytes = 4 << 20
	// ProbePreviewMaxLen output_preview 截断上限（64 KiB，UTF-8 边界）。
	ProbePreviewMaxLen = 64 << 10
	// ProbeMaxImagePreviews image_previews 最多张数（4）。
	ProbeMaxImagePreviews = 4
)

// 探测 image mode（openai-images 的 wire 取值）。service 校验后写入 ProbeSpec；
// 其他格式恒为空。
const (
	ProbeImageModeGenerations = "generations"
	ProbeImageModeEdits       = "edits"
)

// 探测缺省输入：input 缺省或显式 null 时按格式（images 再按 mode）取默认值。
const (
	ProbeDefaultInputText   = "hi"
	ProbeDefaultInputImages = "A simple red apple on a white table"
	ProbeDefaultInputEdits  = "Turn this image into a watercolor painting"
	ProbeDefaultInputSearch = "OpenAI"
)

// 探测结果的固定分类文案。service（准入/期限/凭据早退）与 internal/probe
// （传输/上游/流式）共用这一份词表；结果与日志不得出现表外动态文本。
const (
	ProbeErrorRequestFailed     = "probe request failed"
	ProbeErrorTimedOut          = "probe timed out"
	ProbeErrorCanceled          = "probe canceled"
	ProbeErrorBusy              = "probe busy"
	ProbeErrorBlockedRedirect   = "probe blocked cross-origin redirect"
	ProbeErrorUpstreamRedirect  = "upstream redirect rejected"
	ProbeErrorUpstreamAuth      = "upstream rejected credentials"
	ProbeErrorUpstreamNotFound  = "upstream endpoint or model not found"
	ProbeErrorUpstreamRateLimit = "upstream rate limited"
	ProbeErrorUpstreamRejected  = "upstream rejected request"
	ProbeErrorUpstream          = "upstream error"
	ProbeErrorNoUsableOutput    = "upstream response missing usable output"
	ProbeErrorStreamFailed      = "upstream stream failed"
	ProbeErrorBodyTooLarge      = "response exceeds 2 MiB"
	ProbeErrorImageBodyTooLarge = "response exceeds 8 MiB"
	ProbeErrorCodexTokenExpired = "codex access token expired; re-run device login"
	// 图片取图/输入校验固定文案（不得拼入 image_url/原始值——错误路径绝不回显
	// 请求字段）。取图失败时 status_code=0。
	ProbeErrorImageFetchFailed    = "probe image fetch failed"
	ProbeErrorImageInvalid        = "probe image invalid"
	ProbeErrorImageTooLarge       = "probe image exceeds 4 MiB"
	ProbeErrorInputEmpty          = "probe input is empty"
	ProbeErrorInputTooLarge       = "probe input exceeds 8192 bytes"
	ProbeErrorImageModeInvalid    = "probe image mode invalid"
	ProbeErrorImageModeUnexpected = "probe image mode unexpected"
	ProbeErrorImageURLRequired    = "probe image url required"
	ProbeErrorImageURLInvalid     = "probe image url invalid"
	ProbeErrorImageURLTooLarge    = "probe image url exceeds 6 MiB"
)

// ValidProbeImageMIME 校验会被写入 multipart part 头的图片 MIME：必须是
// image/<subtype>，且不得含 CR/LF 或其他 C0/DEL 控制字符。mime/multipart 会把
// Content-Type 原样写进 part 头，data URL 元数据里的 CRLF 可注入额外头。
// 允许 `;` 参数（调用方通常再经 mediaType 归一）。
func ValidProbeImageMIME(mime string) bool {
	if !strings.HasPrefix(strings.ToLower(mime), "image/") || len(mime) <= len("image/") {
		return false
	}
	for i := 0; i < len(mime); i++ {
		if mime[i] < 0x20 || mime[i] == 0x7f {
			return false
		}
	}
	return true
}

// ProbeContextError 把 ctx 错误归一为固定文案；其余错误按请求失败处理。
// 调用方必须先用它处理 context.Canceled/DeadlineExceeded，再走各自的传输
// 分类（例如 origin 阻止），避免 busy/上游分类掩盖取消。
func ProbeContextError(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return ProbeErrorTimedOut
	case errors.Is(err, context.Canceled):
		return ProbeErrorCanceled
	default:
		return ProbeErrorRequestFailed
	}
}

// ProbeSpec 是 service **组装并校验完成后**交给探测执行器的全部事实：出站目标
// 只来自已存储的账号/模板配置（请求体与 query 不携带 URL/host/路径）。
type ProbeSpec struct {
	AccountID int64
	// TemplateID 只用于 aiclient Factory 的 URL 缓存键（模板 × base_url × path）。
	TemplateID     int64
	CredentialType credential.Type
	// Format 原样回显请求值。codex 族恒走 HTTP responses 语义（与 format 无关）。
	Format         RequestFormat
	RequestedModel string
	UpstreamModel  string
	// BaseURL 有效 base_url：模板值，账号非空 BaseURL 覆盖（静态族使用；
	// codex 族由 SDK 固定官方端点，不读它）。
	BaseURL string
	// UpstreamPaths 仅 custom 模板携带（template_ext.upstream_paths）；nil/空 =
	// 全部回落默认端点。ext 行缺失在 service 已归为 nil。
	UpstreamPaths map[string]string
	// Credential 本次探测使用的静态 key 或 codex 访问令牌/PAT（只用于出站鉴权，
	// 不进入结果/日志；结果字段只有分类白名单与有限错误码）。
	Credential string
	// CodexAccountID codex 账号标识（ChatGPT-Account-ID 头；空 = 不发）。
	CodexAccountID string
	// Egress 已由 service 按 fail-closed 规则解析的出口规格（零值 = 直连）。
	Egress egress.Spec
	// Input 已 trim、已按字节校验的自定输入（缺省值由 service 填好；prober 不再
	// 取默认值，避免第二份默认表）。
	Input string
	// ImageMode/ImageURL 仅 openai-images 使用：service 已按 mode 完成三态与
	// scheme/长度校验（generations 恒空 URL；edits 恒非空 URL）。
	ImageMode string
	ImageURL  string
}

// ProbeResult 是探测结论（HTTP 200 恒返回本对象；success=false 不映射为
// 4xx/5xx）。Error/Detail 只含固定分类文案与有限公开错误码，并已截断。
type ProbeResult struct {
	Success    bool
	LatencyMS  int64
	StatusCode int
	Format     RequestFormat
	// RequestedModel 客户端模型名；UpstreamModel 映射后的上游模型名。
	RequestedModel string
	UpstreamModel  string
	Error          string
	Detail         string
	// OutputPreview 仅 200 成功响应按格式抽取的文本投影（已脱敏、≤64 KiB）；
	// 错误路径、非 200、图片格式恒为空。绝不含原始 JSON/SSE/envelope。
	OutputPreview string
	// ImagePreviews 仅图片格式 200 成功响应中通过 base64+魔数校验的
	// data:image/<mime>;base64,... 列表（最多 4 张，恒非 nil 返回给 handler）。
	ImagePreviews []string
}

// ShallowJSON 线性、零分配地判断 b 的 `{`/`[` 嵌套深度是否不超过 maxDepth。
// 它正确跳过字符串字面量与转义（`"..."` 内的括号不计数），且不校验 JSON 是否
// 合法——调用方在 false 时必须按「不可用/非终态」处理，绝不能把上游可控的深层
// 嵌套交给 gjson 等递归解析器（2 MiB 的 `[` 可耗尽数百 MB 内存）。
func ShallowJSON(b []byte, maxDepth int) bool {
	depth := 0
	inString := false
	escaped := false
	for _, c := range b {
		if inString {
			switch {
			case escaped:
				escaped = false
			case c == '\\':
				escaped = true
			case c == '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			depth++
			if depth > maxDepth {
				return false
			}
		case '}', ']':
			if depth > 0 {
				depth--
			}
		}
	}
	return true
}
