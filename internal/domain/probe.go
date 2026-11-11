// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package domain

import (
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
	// 与 sdkbridge.ProbeResponses）。
	ProbeMaxBodyBytes = 2 << 20
	// ProbeTimeout 单次探测硬上限（常量，不开配置键）。
	ProbeTimeout = 30 * time.Second
	// ProbeMaxConcurrency 进程内探测并发上限（信号量；多实例不协调）。
	ProbeMaxConcurrency = 4
	// ProbeMaxWaiters 等待并发槽的请求上限：超出立即返回 probe busy，避免
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
)

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
