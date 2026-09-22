// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package proxy

import (
	"net/http"

	"github.com/is7qin/c3api/internal/domain"
)

// HandleResponses 转发 /v1/responses（openai-responses 格式）。全部逻辑在
// 通用骨架 handleFormat + responsesCaller（转发骨架通用化），本方法
// 只是端点入口委托。测试按处理函数直接调用（forward_ext_test.go 等），故入口保留。
func (p *Proxy) HandleResponses(w http.ResponseWriter, r *http.Request) {
	p.handleFormat(domain.FormatOpenAIResponses, w, r)
}
