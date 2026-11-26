// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

// request_patch 数据面接线（spec custom-request-patch §7.1/§7.4/§8，P3）：
//   - patchOf / headerDecorator / bodyPatch 三个 helper——无配置一律 nil / 原切片
//     （零闭包、零 map 复制、零解析），有配置才分配（闭包每 attempt 至多一枚）；
//   - 执行顺序：既有变换（setModel / stripTier / 转换）全部之后、真正出网之前
//     （bodyPatch 是 body 的最后一步；headerDecorator 是 aiclient 头的最后一步）；
//   - {api_key} 由**调用点闭包**捕获当次 attempt 的原始 cred 展开（禁止从
//     Authorization 反解，spec 评审 momus C1）；failover 每 attempt 重算；
//   - body sjson 残余失败：裸透传 + 固定文案限频 WARN（按 template_id+format，
//     进程内 sync.Map；只在失败路径访问，不打印 path/value/body，无计数器）；
//   - 日志面脱敏：上游回显的账号 key 在 err_logs/attempt_logs 落库前精确替换为
//     [redacted]（返回客户端的 4xx 原文面保持既有契约，不脱敏）。

import (
	"bytes"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/is7qin/c3api/internal/api"
	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/pkg/logx"
)

// patchKey* 是 request_patch 消费的七个格式键（typed；值与 upstream_paths.go
// 的 string 别名同源——upstreamPath 仍收 string 的既有签名不动，装饰器/bodyPatch
// 收 typed 键以避免调用点散落类型转换）。
const (
	patchKeyChat        = domain.UpstreamPathChat
	patchKeyResponses   = domain.UpstreamPathResponses
	patchKeyResponsesWS = domain.UpstreamPathResponsesWS
	patchKeyAnthropic   = domain.UpstreamPathAnthropic
	patchKeySearch      = domain.UpstreamPathSearch
	patchKeyImagesGen   = domain.UpstreamPathImagesGen
	patchKeyImagesEdits = domain.UpstreamPathImagesEdits
)

// apiKeyPlaceholder 是 request_patch 头值里唯一保留的占位符（spec §2.4）：
// 运行期用当次 attempt 的账号上游 key 原值展开；body 值不做替换（字面量）。
const apiKeyPlaceholder = "{api_key}"

// redactedPatchKey 是日志面脱敏后的替换文本（spec §8：精确字符串替换）。
const redactedPatchKey = "[redacted]"

// patchOf 取当次尝试的 request_patch（nil = 未配置）。只读快照指针——绝不写、
// 不复制（P2 交接：Selection.Upstream.Patch 是只读共享指针）。
func patchOf(sel *api.Selection) *domain.RequestPatch {
	if sel == nil || sel.Upstream == nil {
		return nil
	}
	return sel.Upstream.Patch
}

// formatPatchFor 取该出站格式的格式层 patch（不存在 ⇒ ok=false）。
//
// 返回**值拷贝**而不是指针：domain.FormatPatch 只有两个 map + 一个 slice（24B
// 头），值传递避免取 map 元素地址造成的堆逃逸；执行器按值读。
func formatPatchFor(sel *api.Selection, key domain.UpstreamPathKey) (domain.FormatPatch, bool) {
	patch := patchOf(sel)
	if patch == nil || len(patch.Formats) == 0 {
		return domain.FormatPatch{}, false
	}
	fp, ok := patch.Formats[key]
	return fp, ok
}

// headerDecorator 构造 aiclient 出站头装饰器（spec §7.1）：
//   - 无全局头且无该格式层头 ⇒ nil（aiclient 快路径，零分配）；
//   - 否则返回闭包：先应用全局层，再应用格式层（后者逐键覆盖/删除）；
//   - 每层：非空值 Set、"" Del；Host 特例走 req.Host，**按层三态**（非空 =
//     展开后覆盖，"" = 清除 ⇒ 出站回落 URL.Host——因此格式层 "" 能覆盖全局层
//     Set）；Accept-Encoding 规范形比较后**运行期跳过**（保存期已 400，这里防御
//     脏库值）；
//   - 头值里的 {api_key} 用闭包捕获的 apiKey 原值展开（strings.ReplaceAll 仅在
//     命中占位符时才分配；Host 值同样展开）。
//
// apiKey 必须来自当次 attempt 的**原始 cred**（credentialFor(sel) / caller 的
// cred 参数），不得从 auth / "Bearer "+key 反解。
func headerDecorator(sel *api.Selection, key domain.UpstreamPathKey, apiKey string) func(*http.Request) {
	patch := patchOf(sel)
	if patch == nil {
		return nil
	}
	formatHeaders, hasFormat := patch.Formats[key]
	if len(patch.Headers) == 0 && (!hasFormat || len(formatHeaders.Headers) == 0) {
		return nil
	}
	return func(req *http.Request) {
		applyPatchHeaders(req.Header, &req.Host, patch.Headers, apiKey)
		if hasFormat {
			applyPatchHeaders(req.Header, &req.Host, formatHeaders.Headers, apiKey)
		}
	}
}

// applyPatchHeaders 应用**一层**头语义——HTTP（req.Header / req.Host）与 WS 握手
// （DialOptions.HTTPHeader / DialOptions.Host）的**唯一判据**：落点只有「头 map +
// Host 字符串指针」两处差异，判据本身不复制第二份。
//
// 头名在保存期已归一为规范形（domain.ParseRequestPatch），此处直接比较，不再
// CanonicalHeaderKey。每层：非空 Set、"" Del。
//
// Host 是**按层三态**（spec §2.4/§2.6/§7.1；P3 评审 oracle I1，P4 WS 同语义）：
// 非空值 ⇒ *host = 展开后的值；空值（Del）⇒ *host = ""（显式清除上一层/网关
// 默认）。HTTP 上 req.Host 空 = 出站回退 URL.Host；WS 上 DialOptions.Host 空 =
// 握手回落 URL.Host（coder/websocket 仅在非空时覆盖）——因此格式层 Host:""
// 能覆盖全局层 Host:Set，与普通头的逐层 Del 覆盖语义一致。
//
// Accept-Encoding 运行期永不应用（保存期已 400，防御脏库值；响应编码不变量）。
// 头值里的 {api_key} 用当次 attempt 原始 cred 展开。
func applyPatchHeaders(header http.Header, host *string, headers map[string]string, apiKey string) {
	for name, value := range headers {
		switch name {
		case "Host":
			// Host 特例：只改 Host 头，不改连接目标/TLS SNI（HTTP 与 WS 同一落法：
			// 各自语言层的 Host 字段，绝不写 Header map）。
			*host = expandAPIKey(value, apiKey)
		case "Accept-Encoding":
			// 运行期永不应用（响应编码不变量兜底；保存期已拒绝非空值）。
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

// wsHeaderDecorator 构造 ResponsesWSDial 的**握手**装饰器（spec §7.2/§10 P4）：
//   - 无全局头且无该格式层头 ⇒ nil（aiclient 快路径，零分配）；
//   - 否则返回闭包：全局层 → 格式层逐层应用（共用 applyPatchHeaders 的同一份
//     判据；Host 落 DialOptions.Host，{api_key} 用闭包捕获的原值展开）。
//
// 调用时机由 aiclient.ResponsesWSDial 保证：网关的 Authorization /
// Responses-Websockets 已写入 DialOptions.HTTPHeader **之后**才调本装饰器（后写
// 覆盖赢，可覆盖/删除网关默认头）。apiKey 必须来自当次 attempt 的原始 cred
// （credentialFor(sel)），不得从 Authorization 反解。
func wsHeaderDecorator(sel *api.Selection, key domain.UpstreamPathKey, apiKey string) func(*websocket.DialOptions) {
	patch := patchOf(sel)
	if patch == nil {
		return nil
	}
	formatHeaders, hasFormat := patch.Formats[key]
	if len(patch.Headers) == 0 && (!hasFormat || len(formatHeaders.Headers) == 0) {
		return nil
	}
	return func(opts *websocket.DialOptions) {
		if opts.HTTPHeader == nil {
			// 防御：aiclient 恒先建好 HTTPHeader；独立调用（测试/未来入口）也不 panic。
			opts.HTTPHeader = make(http.Header)
		}
		applyPatchHeaders(opts.HTTPHeader, &opts.Host, patch.Headers, apiKey)
		if hasFormat {
			applyPatchHeaders(opts.HTTPHeader, &opts.Host, formatHeaders.Headers, apiKey)
		}
	}
}

// expandAPIKey 展开头值中的 {api_key} 占位符（可多次）；无占位符时零分配
// （strings.ReplaceAll 内部 count==0 直接返回原串）。
func expandAPIKey(value, apiKey string) string {
	return strings.ReplaceAll(value, apiKeyPlaceholder, apiKey)
}

// bodyPatch 对出站 JSON body 应用格式层 patch（spec §7.1/§7.4）：
//   - 无该格式层 / 空 patch ⇒ 原 slice 返回（零解析、零分配）；
//   - 非 JSON / 空 body ⇒ 设计性跳过（原 slice；不 WARN）；
//   - JSON 但 sjson 残余失败 ⇒ 原 body 裸透传 + 固定文案限频 WARN。
//
// 必须在该格式既有 body 变换（setModel/转换）之后、真正出网之前调用：它是
// body 的最后一步（操作员权威可覆盖 model/stream 等任何字段）。
func (p *Proxy) bodyPatch(sel *api.Selection, key domain.UpstreamPathKey, body []byte) []byte {
	fp, ok := formatPatchFor(sel, key)
	if !ok {
		return body
	}
	out, err := domain.ApplyRequestBodyPatch(body, &fp)
	if err != nil {
		p.warnBodyPatchFailure(sel, key)
		return body
	}
	return out
}

// responseCreateTypeBytes 是 WS 客户端请求帧的顶层 type（唯一 body patch 目标；
// response.cancel 等控制帧永不改写，spec §3 矩阵/§7.2）。
var responseCreateTypeBytes = []byte("response.create")

// wsClientFrameHook 构造 relayWS 的**客户端→上游**帧 hook（spec §7.2/§7.4/§10 P4）：
//   - 无该格式层或 body 面为空（无 set/delete）⇒ nil（relayWS 快路径，零成本）；
//   - 返回闭包后逐帧先做**零分配首键字节扫描**（scanKeyValue，与
//     isResponsesCompleted/isResponsesCreated 同款）判顶层 type == "response.create"；
//     未命中（response.cancel / 其他控制帧 / 非 JSON）返回 nil = 原样转发，**字节
//     不变、不复制帧、不解析帧**；
//   - 命中才执行 body patch——**复用 bodyPatch**（domain.ApplyRequestBodyPatch 是
//     唯一执行器：delete 排序→set 排序、set 胜、非 JSON 结构性跳过；sjson 残余
//     失败 ⇒ bodyPatch 返回原 slice + 固定文案限频 WARN，此处随之裸透传，不中止
//     会话、不改写帧）；
//   - {api_key} 只用于握手头，body 值是字面量（spec §2.4）。
//
// 生命周期：每次 attempt 构造一次（有配置才分配 1 枚闭包），client-loop 单
// goroutine 调用，无锁无全局查询；成功路径零日志。
func (p *Proxy) wsClientFrameHook(sel *api.Selection) func([]byte) []byte {
	fp, ok := formatPatchFor(sel, patchKeyResponsesWS)
	if !ok || (len(fp.Set) == 0 && len(fp.Delete) == 0) {
		return nil
	}
	return func(frame []byte) []byte {
		if !isResponseCreateFrame(frame) {
			return nil // 原样转发：response.cancel 等控制帧字节不变
		}
		return p.bodyPatch(sel, patchKeyResponsesWS, frame)
	}
}

// isResponseCreateFrame 判定一帧是否是客户端 response.create 请求帧（顶层 type
// 的裸字节精确匹配）。零分配、不反序列化：scanKeyValue 单遍字节扫描（type 通常
// 为首键 ⇒ 与帧长无关的常数成本；非对象/结构非法返回 false）。
func isResponseCreateFrame(frame []byte) bool {
	start, end, ok := scanKeyValue(frame, typeKeyBytes)
	if !ok || start >= len(frame) || frame[start] != '"' {
		return false
	}
	return bytes.Equal(frame[start+1:end-1], responseCreateTypeBytes)
}

// bodyPatchWarnGate 是 sjson 残余失败的进程内限频表：键 = template_id+format，
// 值 = 上次 WARN 的 UnixNano。只在失败路径读写（spec §7.4 评审 momus M-i/M-j：
// 成功路径零状态、零锁、零分配；只 WARN、不加计数器）。
var bodyPatchWarnGate sync.Map // map[bodyPatchWarnKey]int64

// bodyPatchWarnKey 限频键（template_id + 出站格式键）。
type bodyPatchWarnKey struct {
	templateID int64
	format     domain.UpstreamPathKey
}

// bodyPatchWarnInterval 单个 (template,format) 的 WARN 最小间隔。
const bodyPatchWarnInterval = time.Minute

// warnBodyPatchFailure 固定文案限频 WARN：**不打印任何 path/value/body**
// （配置值可能是密钥；spec §2.4/§8）。模板共用时不同账号命中同一键 → 限频按
// 模板+格式而不是账号，避免每请求一条刷屏。
func (p *Proxy) warnBodyPatchFailure(sel *api.Selection, key domain.UpstreamPathKey) {
	if p == nil || p.log == nil {
		return
	}
	k := bodyPatchWarnKey{templateID: sel.TemplateID, format: key}
	now := time.Now().UnixNano()
	if last, ok := bodyPatchWarnGate.Load(k); ok && now-last.(int64) < int64(bodyPatchWarnInterval) {
		return
	}
	bodyPatchWarnGate.Store(k, now)
	p.log.Warn("request patch body apply failed; forwarding unmodified body",
		logx.Int64("template_id", sel.TemplateID),
		logx.String("format", string(key)))
}

// upstreamKeyOf 取当次尝试的静态账号 key（nil 安全；非静态通道的空串 =
// 无需脱敏）。这是 Selection 上的只读值拷贝，零分配。
func upstreamKeyOf(sel *api.Selection) string {
	if sel == nil {
		return ""
	}
	return sel.UpstreamKey
}

// redactPatchKey 把日志面文本里出现的账号上游 key 精确替换为 [redacted]
// （spec §8「上游回显防泄漏」只保日志面）。空 key/空文本/未出现 ⇒ 原串零分配。
func redactPatchKey(text, key string) string {
	if text == "" || key == "" || !strings.Contains(text, key) {
		return text
	}
	return strings.ReplaceAll(text, key, redactedPatchKey)
}

// redactLogKey 对一条日志行的 ErrorMessage 做 key 脱敏（唯一实现；调用点见
// recordAttemptLog / round.settle / writeFailoverExhausted / recordFailedDispatch）。
// 上游回显的 key 只可能来自该尝试自己请求头里注入的 key，故行级脱敏用**该行的
// attempt key**即可（failover 每行一 attempt；耗尽行取最后一次 attempt）。
func redactLogKey(msg *string, key string) *string {
	if msg == nil || *msg == "" || key == "" {
		return msg
	}
	redacted := redactPatchKey(*msg, key)
	if redacted == *msg {
		return msg
	}
	return &redacted
}
