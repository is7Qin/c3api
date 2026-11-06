// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package aiclient

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

// hcOf 取工厂底座 client：七个出栈入口的末参是**本次出口**（生产恒传
// `sel.Egress.HTTP`），本包测试没有调度器，统一用它自己的底座——与生产
// `Pool.Direct().HTTP` 是同一个 client（main 里 hc 同时喂 pool 与 factory）。
// 只改调用点、不改任何既有断言。
func hcOf(f *Factory) *http.Client { return f.hc }

// TestRawPostRejectsNilEgressClient 跨包契约违例：出口 client 缺失 ⇒ 可诊断的
// errNilEgressClient，**不 panic**、不静默退回工厂底座（spec §4.4 的唯一防御点）。
// 六条 HTTP 出栈入口共用 rawPostCT，故对每一条都钉一遍（防止将来有人给某面开
// 后门绕过防御点）。
func TestRawPostRejectsNilEgressClient(t *testing.T) {
	f := NewFactory(nil, Config{})
	ctx := context.Background()
	body := []byte(`{}`)

	_, err := f.RawPost(ctx, 1, "http://127.0.0.1:1", "/v1/x", "Authorization", "Bearer k", body, nil, nil)
	require.ErrorIs(t, err, errNilEgressClient, "RawPost: 缺出口 client 必须报契约错")

	_, err = f.ChatCompletionRaw(ctx, 1, "http://127.0.0.1:1", "/v1/chat/completions", "k", body, nil, nil)
	require.ErrorIs(t, err, errNilEgressClient, "ChatCompletionRaw: 缺出口 client 必须报契约错")

	_, err = f.ResponseRaw(ctx, 1, "http://127.0.0.1:1", "/v1/responses", "k", body, nil, nil)
	require.ErrorIs(t, err, errNilEgressClient, "ResponseRaw: 缺出口 client 必须报契约错")

	_, err = f.SearchRaw(ctx, 1, "http://127.0.0.1:1", "/v1/alpha/search", "k", body, nil, nil)
	require.ErrorIs(t, err, errNilEgressClient, "SearchRaw: 缺出口 client 必须报契约错")

	_, err = f.AnthMessageRaw(ctx, 1, "http://127.0.0.1:1", "/v1/messages", "k", body, nil, nil)
	require.ErrorIs(t, err, errNilEgressClient, "AnthMessageRaw: 缺出口 client 必须报契约错")

	_, err = f.ImagesRaw(ctx, 1, "http://127.0.0.1:1", "/v1/images/generations", "k", "", body, nil, nil)
	require.ErrorIs(t, err, errNilEgressClient, "ImagesRaw: 缺出口 client 必须报契约错")
}

// TestResponsesWSDialRejectsNilEgressClient WS 面同一个防御点：缺出口 client ⇒
// errNilEgressClient（不是 nil 指针 panic，也不是退回默认 transport——那会让
// 绑了代理的账号从本机 IP 拨号）。
func TestResponsesWSDialRejectsNilEgressClient(t *testing.T) {
	f := NewFactory(nil, Config{})
	conn, resp, err := f.ResponsesWSDial(context.Background(), 1, "http://127.0.0.1:1", "/v1/responses", "k", http.Header{}, nil)
	require.ErrorIs(t, err, errNilEgressClient)
	require.Nil(t, conn)
	require.Nil(t, resp)
}

// TestNilEgressClientIsNotPanic 防御点必须可被调用方识别为普通错误（failover 分类
// 要吃它）：错误文本是固定串、可 errors.Is，且不是 *http.Error 之外的东西。
func TestNilEgressClientIsNotPanic(t *testing.T) {
	f := NewFactory(nil, Config{})
	require.NotPanics(t, func() {
		_, err := f.ChatCompletionRaw(context.Background(), 1, "http://127.0.0.1:1", "/p", "k", nil, nil, nil)
		require.True(t, errors.Is(err, errNilEgressClient))
		require.Equal(t, "aiclient: nil egress client", err.Error())
	})
}
