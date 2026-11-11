// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"bufio"
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
//   - 200：status=200，body 读取至多 2 MiB+1 字节；读到 response.completed /
//     response.failed 终态事件即停止读取并关闭（上游不关流不得拖到超时）；body
//     恰好关闭一次，len > 2 MiB 由调用方判溢出失败；
//   - 非 200：*codexsdk.HTTPError → 其 StatusCode/Raw（Raw 经调用方 capTransport
//     截断到 ≤2 MiB）；*codexsdk.AuthPermanentlyRevokedError → status=401 +
//     Error()/Raw；
//   - 传输失败（拨号/超时/守卫拒绝）：status=0，err 为底层错误。
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
	body, readErr := readProbeBody(resp.Body)
	if readErr != nil {
		return resp.StatusCode, body, readErr
	}
	return resp.StatusCode, body, nil
}

// readProbeBody 增量读取 200 body，读到 SSE 终态（response.completed /
// response.failed）立即返回；总量上限 2 MiB+1（到限即停，由调用方判溢出）。
// body 在函数内关闭且只关闭一次。
func readProbeBody(rc io.ReadCloser) ([]byte, error) {
	defer rc.Close()

	limit := int64(domain.ProbeMaxBodyBytes) + 1
	var buf bytes.Buffer
	reader := bufio.NewReaderSize(rc, 32*1024)
	var event []byte
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			room := limit - int64(buf.Len())
			if room <= 0 {
				return buf.Bytes(), nil
			}
			if int64(len(line)) > room {
				buf.Write(line[:room])
				return buf.Bytes(), nil
			}
			buf.Write(line)

			trimmed := bytes.TrimSpace(line)
			if len(trimmed) == 0 {
				// 空行 = 事件块结束；块内有终态即停。
				if codexTerminalBlock(event) {
					return buf.Bytes(), nil
				}
				event = event[:0]
			} else {
				event = append(event, line...)
				// 单行 data 已构成完整终态事件时立即停（不必等空行/EOF）。
				if bytes.HasPrefix(trimmed, []byte("data:")) && codexTerminalBlock(event) {
					return buf.Bytes(), nil
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return buf.Bytes(), nil
			}
			return buf.Bytes(), err
		}
	}
}

// codexTerminalBlock 判断一个 SSE 事件块是否已是终态事件。data 必须能解析成
// 完整 JSON（多行 data 已合并）才可信；事件名仅在 data 合法时兜底。
func codexTerminalBlock(block []byte) bool {
	name := ""
	var data bytes.Buffer
	for _, rawLine := range bytes.Split(block, []byte("\n")) {
		line := bytes.TrimSpace(rawLine)
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
	if data.Len() == 0 || !gjson.ValidBytes(data.Bytes()) {
		return false
	}
	typ := gjson.GetBytes(data.Bytes(), "type").String()
	if typ == "" {
		typ = name
	}
	return typ == "response.completed" || typ == "response.failed"
}
