// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	codexsdk "github.com/is7Qin/codex-sdk"
	"github.com/tidwall/gjson"

	"github.com/is7qin/c3api/internal/domain"
)

// probeResponsesPayload 是 codex 探测的生产请求体：`input` 必须实际存在且非空
// （SDK 白名单保留 input；只写 model/stream 会发出 `"input":null`）。无输出
// token 上限——SDK 白名单会剔除 max_output_tokens 等非白名单键（spec §5.1）。
type probeResponsesPayload struct {
	Model  string              `json:"model"`
	Input  []probeInputContent `json:"input"`
	Stream bool                `json:"stream"`
}

type probeInputContent struct {
	Role    string           `json:"role"`
	Content []probeInputText `json:"content"`
}

type probeInputText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// ProbeResponsesPayload 构造 codex 探测的生产请求体（service/probe 共用同一份，
// 测试直接断言捕获到的上游请求体，防止回退成 `"input":null`）。
func ProbeResponsesPayload(model string) []byte {
	b, err := json.Marshal(probeResponsesPayload{
		Model: model,
		Input: []probeInputContent{{
			Role:    "user",
			Content: []probeInputText{{Type: "input_text", Text: "hi"}},
		}},
		Stream: true,
	})
	if err != nil {
		return nil
	}
	return b
}

// ProbeResponses 用 codex SDK 的 HTTP arm（StreamBody）发一次静态探测请求。
//
// 这是 account-model-test 的**一次性探测机制**，刻意不复用生产 sdkbridge.Codex：
//   - 不取 ClientTable 条目、不构造账号级 Auth 缓存（每次调用一条临时 SDK client）;
//   - 不传 WithPATOnAuthFatal（无 fatal 回调、不写 failed_at/account_ext）；
//   - 不存在 OAuth 轮转（PAT 不是 rotationTrigger；401/403 原样作为结论）；
//   - 传输守卫（origin 锁定 + 响应体硬上限）由调用方 internal/probe 构造后传入，
//     本函数只负责 SDK 机制，不重复实现守卫。
//
// 返回约定：
//   - 200：status=200，body 读取至多 2 MiB+1 字节；在**真正空行事件边界**（或
//     EOF）判定 response.completed / response.failed / error 终态并停止读取、
//     关闭（上游不关流不得拖到超时）；body 恰好关闭一次，len > 2 MiB 由调用方判
//     溢出失败；
//   - 非 200：*codexsdk.HTTPError → 其 StatusCode/Raw（Raw 经调用方 capTransport
//     截断到 ≤2 MiB；201/204 也按 HTTP 响应保留真实状态）；
//   - 传输失败（拨号/超时/守卫拒绝/200 body 读取失败）：status=0，err 为底层错误。
func ProbeResponses(ctx context.Context, token, accountID string, transport http.RoundTripper, payload []byte, timeout time.Duration) (status int, body []byte, err error) {
	if transport == nil {
		return 0, nil, errors.New("sdkbridge: probe transport is nil")
	}
	client := codexsdk.NewHTTPClient(
		codexsdk.PAT(token, codexsdk.WithPATAccountID(accountID)),
		codexsdk.WithTransport(transport),
		codexsdk.WithTimeout(timeout),
	)
	resp, callErr := client.StreamBody(ctx, payload, codexsdk.Identity{})
	if callErr != nil {
		var revoked *codexsdk.AuthPermanentlyRevokedError
		if errors.As(callErr, &revoked) {
			return http.StatusUnauthorized, revoked.Raw, callErr
		}
		var httpErr *codexsdk.HTTPError
		if errors.As(callErr, &httpErr) {
			return httpErr.StatusCode, httpErr.Raw, callErr
		}
		return 0, nil, callErr
	}
	if resp == nil {
		return 0, nil, fmt.Errorf("sdkbridge: probe got nil response")
	}
	// 200 分支：SDK 不读 body（HTTP arm 只做状态检查），由本函数增量扫描并
	// **恰好关闭一次**。读取上限 = 2 MiB+1，多一个字节用于调用方判溢出。
	// body 读取失败是传输失败（status=0）：SDK 只对非 200 返回 HTTPError，200
	// 头已到但 body 读不完不属于「HTTP 响应」结论。
	body, readErr := readProbeBody(resp.Body)
	if readErr != nil {
		return 0, body, readErr
	}
	return resp.StatusCode, body, nil
}

// readProbeBody 增量读取 200 body，在**真正空行事件边界**（或 EOF）判定终态：
// response.completed / response.failed / error 时立即返回；总量上限 2 MiB+1
// （到限即停，由调用方判溢出）。扫描用索引循环（bytes.IndexByte）而不是
// bytes.Split/ReadBytes：2 MiB 纯换行 body 不能产生百万级行头/行缓冲分配。
// 事件在 buf 内以 [eventStart, lineEnd) 表示，只在边界解析一次——既不会把多行
// 事件的首行误当完整事件截断，也避免逐行重解析的二次放大。body 恰好关闭一次。
func readProbeBody(rc io.ReadCloser) ([]byte, error) {
	defer rc.Close()

	limit := int64(domain.ProbeMaxBodyBytes) + 1
	var buf bytes.Buffer
	chunk := make([]byte, 32*1024)
	eventStart, lineStart := 0, 0
	for {
		room := limit - int64(buf.Len())
		if room <= 0 {
			return buf.Bytes(), nil
		}
		readBuf := chunk
		if int64(len(readBuf)) > room {
			readBuf = readBuf[:room]
		}
		n, readErr := rc.Read(readBuf)
		if n > 0 {
			_, _ = buf.Write(readBuf[:n])
			data := buf.Bytes()
			for lineStart < len(data) {
				idx := bytes.IndexByte(data[lineStart:], '\n')
				if idx < 0 {
					break
				}
				lineEnd := lineStart + idx + 1
				if sseLineEmpty(data[lineStart:lineEnd]) {
					// 空行 = 事件块结束；此时事件已完整收齐，可判定终态。
					if codexTerminalBlock(data[eventStart:lineEnd]) {
						return buf.Bytes(), nil
					}
					eventStart = lineEnd
				}
				lineStart = lineEnd
			}
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return buf.Bytes(), readErr
			}
			// EOF 也是事件边界（无尾随空行）。
			if codexTerminalBlock(buf.Bytes()[eventStart:]) {
				return buf.Bytes(), nil
			}
			return buf.Bytes(), nil
		}
	}
}

// sseLineEmpty 只把真正的空行（`\n` 或 `\r\n`）视为事件分隔；仅含空白的行
// 属于事件内容（spec §5.1/§5.2，A20）。
func sseLineEmpty(line []byte) bool {
	if len(line) > 0 && line[len(line)-1] == '\n' {
		line = line[:len(line)-1]
	}
	if len(line) > 0 && line[len(line)-1] == '\r' {
		line = line[:len(line)-1]
	}
	return len(line) == 0
}

// codexTerminalBlock 判断一个 SSE 事件块是否已是终态事件（response.completed /
// response.failed / error）。data 必须先通过线性深度守卫且能解析成完整 JSON
// （多行 data 已合并）才可信；事件名仅在 data 合法时兜底。
func codexTerminalBlock(block []byte) bool {
	name := ""
	var data bytes.Buffer
	for start := 0; start < len(block); {
		lineEnd := bytes.IndexByte(block[start:], '\n')
		var line []byte
		if lineEnd < 0 {
			line = block[start:]
			start = len(block)
		} else {
			line = block[start : start+lineEnd]
			start += lineEnd + 1
		}
		line = bytes.TrimSpace(line)
		switch {
		case bytes.HasPrefix(line, []byte("event:")):
			name = string(bytes.TrimSpace(line[len("event:"):]))
		case bytes.HasPrefix(line, []byte("data:")):
			chunk := bytes.TrimSpace(line[len("data:"):])
			if data.Len() > 0 {
				data.WriteByte('\n')
			}
			data.Write(chunk)
		}
	}
	// 深度守卫必须先于递归的 gjson 解析：超深事件不是终态。
	if data.Len() == 0 || !domain.ShallowJSON(data.Bytes(), domain.ProbeMaxJSONDepth) || !gjson.ValidBytes(data.Bytes()) {
		return false
	}
	typ := gjson.GetBytes(data.Bytes(), "type").String()
	if typ == "" {
		typ = name
	}
	return typ == "response.completed" || typ == "response.failed" || typ == "error"
}
