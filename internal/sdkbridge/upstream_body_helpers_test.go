// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package sdkbridge

import (
	"io"
	"net/http"

	"github.com/klauspost/compress/zstd"
)

// readUpstreamBody 假上游读请求体的唯一入口：SDK 对 Codex `/responses` 的 HTTP 请求体
// 做 zstd 压缩（`content-encoding: zstd`），断言面要的是解压后的 JSON。其他端点
// （search 等）不压缩，原样返回。解压失败返回 nil，让断言当场失败而不是静默通过。
func readUpstreamBody(r *http.Request) []byte {
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return nil
	}
	if r.Header.Get("Content-Encoding") != "zstd" {
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
