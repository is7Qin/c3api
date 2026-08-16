// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"

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

// stream 值判定边界（spec 2026-08-16-single-pass-parse-design）：字面
// true/false/null → 放行（false/null → 非流式）；`"true"` 字符串（值区间含
// 引号）与其他形态（数字/对象）→ 400 同现状 gjson Type 校验。
func TestSkeletonStreamValueBoundaries(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)

	for _, tc := range []struct {
		name string
		body string
	}{
		{"string-true", `{"stream":"true","messages":[]}`},
		{"number", `{"stream":1,"messages":[]}`},
		{"object", `{"stream":{},"messages":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer ck-1")
			rec := httptest.NewRecorder()
			p.HandleChat(rec, req)
			require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
			require.Zero(t, p.rec.Pending(), "stream 类型校验失败在 Select 前：无记录")
			ri, ok := p.sched.Runtime(1)
			require.True(t, ok)
			require.Zero(t, ri.Concurrency, "无并发槽")
		})
	}

	for _, tc := range []struct {
		name string
		body string
	}{
		{"true", `{"stream":true,"messages":[]}`},
		{"false", `{"stream":false,"messages":[]}`},
		{"null", `{"stream":null,"messages":[]}`},
	} {
		t.Run("ok-"+tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer ck-1")
			rec := httptest.NewRecorder()
			p.HandleChat(rec, req)
			require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
		})
	}
}

// model/service_tier 显式 null 放行（gate Minor 1 专项：gjson Null 语义——
// 与缺失同零值，不得 400）。
func TestSkeletonModelTierExplicitNull(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":null,"service_tier":null,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
}

// service_tier 非字符串 → 400（tier 校验零回归：HTTP 路径显式用例——校验
// 语义逐字节不变，消息与 resp-ws 同文案）。
func TestSkeletonServiceTierNonString400(t *testing.T) {
	up := fakeOpenAI(t, "")
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)

	for _, tc := range []struct {
		name string
		body string
	}{
		{"number", `{"service_tier":123,"messages":[]}`},
		{"boolean", `{"service_tier":true,"messages":[]}`},
		{"object", `{"service_tier":{"a":1},"messages":[]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body))
			req.Header.Set("Authorization", "Bearer ck-1")
			rec := httptest.NewRecorder()
			p.HandleChat(rec, req)
			require.Equal(t, http.StatusBadRequest, rec.Code, "body=%s", rec.Body.String())
			require.Equal(t, "invalid request body: service_tier must be a string", gjson.Get(rec.Body.String(), "error.message").String())
			require.Zero(t, p.rec.Pending(), "tier 类型校验失败在 Select 前：无记录")
		})
	}
}

// MB 级 body（vision base64 data URL，~900KB——MaxBodySize 1MiB 内）单遍提取
// 正确性：scanKeys 必须正确跳过 MB 字符串值——stream/model/service_tier 判定
// 不因大值区间偏移失真，base64 数据完整到达上游（4 遍 → 2 遍的收益场景）。
func TestSkeletonLargeBodyExtraction(t *testing.T) {
	var mu sync.Mutex
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-upstream" {
			w.WriteHeader(401)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		gotBody, _ = json.Marshal(body)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "c1", "object": "chat.completion", "model": body["model"]})
	}))
	defer up.Close()
	p := newTestProxy(t, up.URL, 1)

	b64 := strings.Repeat("A", 900<<10) // ~900KB base64（vision data URL 形态）
	body := `{"model":"gpt-4o","stream":false,"service_tier":"auto","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,` + b64 + `"}}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer ck-1")
	rec := httptest.NewRecorder()
	p.HandleChat(rec, req)
	require.Equal(t, http.StatusOK, rec.Code, "MB body 不得误 400/误转流式：%s", rec.Body.String()[:min(200, len(rec.Body.String()))])
	require.Equal(t, "application/json", rec.Header().Get("Content-Type"), "stream:false 判定正确（非流式路径）")

	mu.Lock()
	got := string(gotBody)
	mu.Unlock()
	require.NotEmpty(t, got, "上游必须收到请求体")
	var upBody map[string]any
	require.NoError(t, json.Unmarshal([]byte(got), &upBody))
	require.Equal(t, "gpt-4o", upBody["model"], "model 提取正确（Select 命中 gpt-4o）")
	url := upBody["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["image_url"].(map[string]any)["url"].(string)
	require.Equal(t, "data:image/png;base64,"+b64, url, "MB base64 数据完整到达上游（单遍扫描不误改不截断）")
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
