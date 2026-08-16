// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/scheduler"
)

// 等价性专项（Task 2 Step 5）：骨架局部校验路径——流式/非流式 body 非 JSON、
// model 非字符串 → 本地 400（无记录、Select 前无并发槽）。

func TestSkeletonChatBodyNotJSONStreamAndNonStream(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)

	for _, tc := range []struct {
		name string
		body string
	}{
		{"non-json", `not-json`},
		{"non-json-stream", `not-json{`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer ck-1")
			rec := httptest.NewRecorder()
			p.HandleChat(rec, req)
			require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
			require.Zero(t, p.rec.Pending(), "骨架 peek 失败：无记录")
			ri, ok := p.sched.Runtime(1)
			require.True(t, ok)
			require.Zero(t, ri.Concurrency, "peek 在 Select 前：无并发槽")
		})
	}
}

// 计划 I-2/等价性清单：model 非字符串（number/object）→ 400，在 Select 前、
// 无记录。（注意：现状完整 params 解析对这类输入静默宽松——openai-go 解码
// 不报错、model 落空走默认桶；本 400 是计划明确指定的语义收紧，既有测试
// 无覆盖，行为偏差已在实施报告 Task 2 说明。）
func TestSkeletonModelNonString400(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)

	for _, tc := range []struct {
		name string
		body string
	}{
		{"number", `{"model":123,"messages":[]}`},
		{"number-stream", `{"model":123,"stream":true,"messages":[]}`},
		{"object", `{"model":{"a":1},"messages":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer ck-1")
			rec := httptest.NewRecorder()
			p.HandleChat(rec, req)
			require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
			require.Zero(t, p.rec.Pending(), "model 类型校验失败在 Select 前：无记录")
			ri, ok := p.sched.Runtime(1)
			require.True(t, ok)
			require.Zero(t, ri.Concurrency, "无并发槽")
		})
	}
}

// 显式 null 与缺失等同（encoding/json null → 零值语义）：model:null 不得 400，
// 走默认桶正常转发。
func TestSkeletonModelNullLikeMissing(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":null,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
}

// 非流式 params 解析失败 → 本地 400、handled=true、无记录（评审 I-1 附加
// 缺口），Select 已占的并发槽必须释放（caller 内 Release-only）。
// 注：openai-go/anthropic SDK 解码极宽松（探测：messages 类型错、数值溢出
// 等均不报错），端到端（骨架 peek 后）该分支实际不可达；本测试直接驱动
// caller 验证分支语义——它是防御性保留路径（与现状"400 无记录"同语义）。
func TestCallerLocalReject400NoRecordNoLeak(t *testing.T) {
	for _, tc := range []struct {
		name   string
		format domain.RequestFormat
		call   func(p *Proxy, sel *scheduler.Selection, body []byte) (int, []byte, bool, error)
	}{
		{"chat", domain.FormatOpenAIChat, func(p *Proxy, sel *scheduler.Selection, body []byte) (int, []byte, bool, error) {
			return (&chatCaller{p: p}).Call(context.Background(), httptest.NewRecorder(), nil, "rid", 10, time.Now(), sel, "sk", body, false)
		}},
		{"responses", domain.FormatOpenAIResponses, func(p *Proxy, sel *scheduler.Selection, body []byte) (int, []byte, bool, error) {
			return (&responsesCaller{p: p}).Call(context.Background(), httptest.NewRecorder(), nil, "rid", 10, time.Now(), sel, "sk", body, false)
		}},
		{"anthropic", domain.FormatAnthropic, func(p *Proxy, sel *scheduler.Selection, body []byte) (int, []byte, bool, error) {
			return (&anthropicCaller{p: p}).Call(context.Background(), httptest.NewRecorder(), nil, "rid", 10, time.Now(), sel, "sk", body, false)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := fakeOpenAI(t, "") // 本地拒绝不触达上游，fake 只为构造 Proxy
			defer up.Close()
			p := newTestProxyFormat(t, up.URL, tc.format)

			sel, err := p.sched.Select(10, tc.format, "gpt-4o")
			require.NoError(t, err, "Select 占槽")
			ri, ok := p.sched.Runtime(sel.AccountID)
			require.True(t, ok)
			require.Equal(t, int64(1), ri.Concurrency, "Select 后槽已占")

			code, _, handled, _ := tc.call(p, sel, []byte(`{`)) // 非 JSON → params 解析失败
			require.Equal(t, http.StatusBadRequest, code)
			require.True(t, handled, "本地拒绝 = handled=true（骨架直接 return）")
			require.Zero(t, p.rec.Pending(), "本地拒绝：无记录")
			ri, ok = p.sched.Runtime(sel.AccountID)
			require.True(t, ok)
			require.Zero(t, ri.Concurrency, "本地拒绝必须释放并发槽")
		})
	}
}
