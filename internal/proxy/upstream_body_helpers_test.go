// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"io"
	"net/http"

	"github.com/klauspost/compress/zstd"
)

// readFakeUpstreamBody 假上游读请求体的唯一入口：SDK 对 Codex `/responses` 的 HTTP 请求体
// 做 zstd 压缩（`content-encoding: zstd`），断言面要的是解压后的 JSON。其他端点
// （search 等）不压缩，原样返回。解压失败返回 nil，让断言当场失败而不是静默通过。
// 服务端（httptest.Server）收到的头已规范化；RoundTripper 侧看到的是 SDK 写入的
// 原始小写键，两种都认。
func readFakeUpstreamBody(r *http.Request) []byte {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil
	}
	if contentEncoding(r.Header) != "zstd" {
		return raw
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil
	}
	defer dec.Close()
	out, err := dec.DecodeAll(raw, nil)
	if err != nil {
		return nil
	}
	return out
}

// contentEncoding 读 content-encoding：先规范键，再 SDK 直接写入的小写键。
func contentEncoding(h http.Header) string {
	if v := h.Get("Content-Encoding"); v != "" {
		return v
	}
	if v := h["content-encoding"]; len(v) > 0 {
		return v[0]
	}
	return ""
}
